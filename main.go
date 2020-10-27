package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"code.cloudfoundry.org/goshims/ioutilshim"

	"github.com/benbjohnson/clock"
	uuid "github.com/satori/go.uuid"

	"github.com/cloudfoundry/uptimer/app"
	"github.com/cloudfoundry/uptimer/appLogValidator"
	"github.com/cloudfoundry/uptimer/cfCmdGenerator"
	"github.com/cloudfoundry/uptimer/cfWorkflow"
	"github.com/cloudfoundry/uptimer/cmdRunner"
	"github.com/cloudfoundry/uptimer/cmdStartWaiter"
	"github.com/cloudfoundry/uptimer/config"
	"github.com/cloudfoundry/uptimer/measurement"
	"github.com/cloudfoundry/uptimer/orchestrator"
	"github.com/cloudfoundry/uptimer/syslogSink"
	"github.com/cloudfoundry/uptimer/version"
	"github.com/cloudfoundry/uptimer/winapp"
)

func main() {
	logger := log.New(os.Stdout, "\n[UPTIMER] ", log.Ldate|log.Ltime|log.LUTC)

	useBuildpackDetection := flag.Bool("useBuildpackDetection", false, "Use buildpack detection (defaults to false)")
	configPath := flag.String("configFile", "", "Path to the config file")
	resultPath := flag.String("resultFile", "", "Path to the result file")
	showVersion := flag.Bool("v", false, "Prints the version of uptimer and exits")
	flag.Parse()

	if *showVersion {
		fmt.Printf("version: %s\n", version.Version)
		os.Exit(0)
	}

	if *configPath == "" {
		logger.Println("Failed to load config: ", fmt.Errorf("'-configFile' flag required"))
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Println("Failed to load config: ", err)
		os.Exit(1)
	}

	err = cfg.Validate()
	if err != nil {
		logger.Println(err)
		os.Exit(1)
	}

	performMeasurements := true

	logger.Println("Preparing included app...")
	appPath, err := prepareIncludedApp("app", app.Source, *useBuildpackDetection)
	if err != nil {
		logger.Println("Failed to prepare included app: ", err)
		performMeasurements = false
	}
	logger.Println("Finished preparing included app")
	defer os.RemoveAll(appPath)

	logger.Println("Preparing included Windows app...")
	winAppPath, err := prepareIncludedWinApp("winapp", *useBuildpackDetection)
	if err != nil {
		logger.Println("Failed to prepare included Windows app: ", err)
		performMeasurements = false
	}
	logger.Println("Finished preparing included Windows app")
	defer os.RemoveAll(winAppPath)

	var sinkAppPath string
	if cfg.OptionalTests.RunAppSyslogAvailability {
		logger.Println("Preparing included syslog sink app...")
		sinkAppPath, err = prepareIncludedApp("syslogSink", syslogSink.Source, *useBuildpackDetection)
		if err != nil {
			logger.Println("Failed to prepare included syslog sink app: ", err)
		}
		logger.Println("Finished preparing included syslog sink app")
	}
	orcTmpDir, recentLogsTmpDir, streamingLogsTmpDir, pushTmpDir, sinkTmpDir, winPushTmpDir, err := createTmpDirs()
	if err != nil {
		logger.Println("Failed to create temp dirs:", err)
		performMeasurements = false
	}

	bufferedRunner, runnerOutBuf, runnerErrBuf := createBufferedRunner()

	pushCmdGenerator := cfCmdGenerator.New(pushTmpDir)
	pushWorkflow := createWorkflow(cfg.CF, appPath)
	logger.Printf("Setting up push workflow with org %s ...", pushWorkflow.Org())
	if err := bufferedRunner.RunInSequence(pushWorkflow.Setup(pushCmdGenerator)...); err != nil {
		logBufferedRunnerFailure(logger, "push workflow setup", err, runnerOutBuf, runnerErrBuf)
		performMeasurements = false
	} else {
		logger.Println("Finished setting up push workflow")
	}
	pushWorkflowGeneratorFunc := func() cfWorkflow.CfWorkflow {
		return cfWorkflow.New(
			cfg.CF,
			pushWorkflow.Org(),
			pushWorkflow.Space(),
			pushWorkflow.Quota(),
			fmt.Sprintf("uptimer-app-%s", uuid.NewV4().String()),
			appPath,
		)
	}

	winPushCmdGenerator := cfCmdGenerator.New(winPushTmpDir)
	winPushWorkflow := createWorkflow(cfg.CF, winAppPath)
	logger.Printf("Setting up Windows push workflow with org %s ...", winPushWorkflow.Org())
	if err := bufferedRunner.RunInSequence(winPushWorkflow.Setup(winPushCmdGenerator)...); err != nil {
		logBufferedRunnerFailure(logger, "push Windows workflow setup", err, runnerOutBuf, runnerErrBuf)
		performMeasurements = false
	} else {
		logger.Println("Finished setting up Windows push workflow")
	}
	winPushWorkflowGeneratorFunc := func() cfWorkflow.CfWorkflow {
		return cfWorkflow.New(
			cfg.CF,
			winPushWorkflow.Org(),
			winPushWorkflow.Space(),
			winPushWorkflow.Quota(),
			fmt.Sprintf("uptimer-app-%s", uuid.NewV4().String()),
			appPath,
		)
	}

	var sinkWorkflow cfWorkflow.CfWorkflow
	var sinkCmdGenerator cfCmdGenerator.CfCmdGenerator
	if cfg.OptionalTests.RunAppSyslogAvailability {
		sinkCmdGenerator = cfCmdGenerator.New(sinkTmpDir)
		sinkWorkflow = createWorkflow(cfg.CF, sinkAppPath)
		logger.Printf("Setting up sink workflow with org %s ...", sinkWorkflow.Org())
		err = bufferedRunner.RunInSequence(
			append(append(
				sinkWorkflow.Setup(sinkCmdGenerator),
				sinkWorkflow.Push(sinkCmdGenerator)...),
				sinkWorkflow.MapRoute(sinkCmdGenerator)...)...)
		if err != nil {
			logBufferedRunnerFailure(logger, "sink workflow setup", err, runnerOutBuf, runnerErrBuf)
			performMeasurements = false
		} else {
			logger.Println("Finished setting up sink workflow")
		}
	}

	orcCmdGenerator := cfCmdGenerator.New(orcTmpDir)
	orcWorkflow := createWorkflow(cfg.CF, appPath)

	authFailedRetryFunc := func(stdOut, stdErr string) bool {
		authFailedMessage := "Authentication has expired.  Please log back in to re-authenticate."
		return strings.Contains(stdOut, authFailedMessage) || strings.Contains(stdErr, authFailedMessage)
	}
	clock := clock.New()
	measurements := createMeasurements(
		clock,
		logger,
		orcWorkflow,
		pushWorkflowGeneratorFunc,
		winPushWorkflowGeneratorFunc,
		cfCmdGenerator.New(recentLogsTmpDir),
		cfCmdGenerator.New(streamingLogsTmpDir),
		pushCmdGenerator,
		winPushCmdGenerator,
		cfg.AllowedFailures,
		authFailedRetryFunc,
	)

	if cfg.OptionalTests.RunAppSyslogAvailability {
		measurements = append(
			measurements,
			createAppSyslogAvailabilityMeasurement(
				clock,
				logger,
				sinkWorkflow,
				sinkCmdGenerator,
				cfg.AllowedFailures,
				authFailedRetryFunc,
			),
		)
	}

	logger.Printf("Setting up main workflow with org %s ...", orcWorkflow.Org())
	orc := orchestrator.New(cfg.While, logger, orcWorkflow, cmdRunner.New(os.Stdout, os.Stderr, io.Copy), measurements, &ioutilshim.IoutilShim{})
	if err = orc.Setup(bufferedRunner, orcCmdGenerator, cfg.OptionalTests); err != nil {
		logBufferedRunnerFailure(logger, "main workflow setup", err, runnerOutBuf, runnerErrBuf)
		performMeasurements = false
	} else {
		logger.Println("Finished setting up main workflow")
	}

	if !cfg.OptionalTests.RunAppSyslogAvailability {
		logger.Println("*NOT* running measurement: App syslog availability")
	}

	exitCode, err := orc.Run(performMeasurements, *resultPath)
	if err != nil {
		logger.Println("Failed run:", err)
	}

	logger.Println("Tearing down...")
	tearDown(
		orc,
		orcCmdGenerator,
		logger,
		pushWorkflow,
		pushCmdGenerator,
		winPushWorkflow,
		winPushCmdGenerator,
		sinkWorkflow,
		sinkCmdGenerator,
		bufferedRunner,
		runnerOutBuf,
		runnerErrBuf,
	)
	logger.Println("Finished tearing down")

	os.Exit(exitCode)
}

func createTmpDirs() (string, string, string, string, string, string, error) {
	orcTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", "", err
	}
	recentLogsTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", "", err
	}
	streamingLogsTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", "", err
	}
	pushTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", "", err
	}
	sinkTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", "", err
	}
	winTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", "", err
	}

	return orcTmpDir, recentLogsTmpDir, streamingLogsTmpDir, pushTmpDir, sinkTmpDir, winTmpDir, nil
}

func prepareIncludedApp(name, source string, useBuildpackDetection bool) (string, error) {
	dir, err := ioutil.TempDir("", "uptimer-sample-*")
	if err != nil {
		return "", err
	}

	fmt.Printf("Creating go app in %s", dir)

	if err := ioutil.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	manifest := goManifest(name, useBuildpackDetection)
	if err := ioutil.WriteFile(filepath.Join(dir, "manifest.yml"), []byte(manifest), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	return dir, nil
}

func prepareIncludedWinApp(name string, useBuildpackDetection bool) (string, error) {
	dir, err := ioutil.TempDir("", "uptimer-sample-*")
	if err != nil {
		return "", err
	}

	fmt.Printf("Creating Windows app in %s", dir)

	if err := ioutil.WriteFile(filepath.Join(dir, "Web.config"), []byte(winapp.WebConfig), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	if err := ioutil.WriteFile(filepath.Join(dir, "Global.asax"), []byte(winapp.GlobalAsax), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	if err := ioutil.WriteFile(filepath.Join(dir, "Default.aspx.cs"), []byte(winapp.DefaultAspxCs), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	if err := ioutil.WriteFile(filepath.Join(dir, "Default.aspx"), []byte(winapp.DefaultAspx), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	manifest := winManifest(name, useBuildpackDetection)
	if err := ioutil.WriteFile(filepath.Join(dir, "manifest.yml"), []byte(manifest), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	return dir, nil
}

func winManifest(appName string, useBuildpackDetection bool) string {
	m := fmt.Sprintf(`applications:
- name: %s
  memory: 1024m
  stack: windows
`, appName)

	if !useBuildpackDetection {
		m += "  buildpacks:  - hwc_buildpack\n"
	}
	return m
}

func goManifest(appName string, useBuildpackDetection bool) string {
	m := fmt.Sprintf(`applications:
- name: %s
  memory: 64M
  disk: 16M
  env:
    GOPACKAGENAME: github.com/cloudfoundry/uptimer/%s
`, appName, appName)

	if !useBuildpackDetection {
		m += "  buildpacks:\n  - go_buildpack\n"
	}
	return m
}

func createWorkflow(cfc *config.Cf, appPath string) cfWorkflow.CfWorkflow {
	return cfWorkflow.New(
		cfc,
		fmt.Sprintf("uptimer-org-%s", uuid.NewV4().String()),
		fmt.Sprintf("uptimer-space-%s", uuid.NewV4().String()),
		fmt.Sprintf("uptimer-quota-%s", uuid.NewV4().String()),
		fmt.Sprintf("uptimer-app-%s", uuid.NewV4().String()),
		appPath,
	)
}

func createMeasurements(
	clock clock.Clock,
	logger *log.Logger,
	orcWorkflow cfWorkflow.CfWorkflow,
	pushWorkFlowGeneratorFunc func() cfWorkflow.CfWorkflow,
	winPushWorkFlowGeneratorFunc func() cfWorkflow.CfWorkflow,
	recentLogsCmdGenerator, streamingLogsCmdGenerator,
	pushCmdGenerator cfCmdGenerator.CfCmdGenerator,
	winPushCmdGenerator cfCmdGenerator.CfCmdGenerator,
	allowedFailures config.AllowedFailures,
	authFailedRetryFunc func(stdOut, stdErr string) bool,
) []measurement.Measurement {
	recentLogsBufferRunner, recentLogsRunnerOutBuf, recentLogsRunnerErrBuf := createBufferedRunner()
	recentLogsMeasurement := measurement.NewRecentLogs(
		func() []cmdStartWaiter.CmdStartWaiter {
			return orcWorkflow.RecentLogs(recentLogsCmdGenerator)
		},
		recentLogsBufferRunner,
		recentLogsRunnerOutBuf,
		recentLogsRunnerErrBuf,
		appLogValidator.New(),
	)

	streamingLogsBufferRunner, streamingLogsRunnerOutBuf, streamingLogsRunnerErrBuf := createBufferedRunner()
	streamingLogsMeasurement := measurement.NewStreamingLogs(
		func() (context.Context, context.CancelFunc, []cmdStartWaiter.CmdStartWaiter) {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 15*time.Second)
			return ctx, cancelFunc, orcWorkflow.StreamLogs(ctx, streamingLogsCmdGenerator)
		},
		streamingLogsBufferRunner,
		streamingLogsRunnerOutBuf,
		streamingLogsRunnerErrBuf,
		appLogValidator.New(),
	)

	pushRunner, pushRunnerOutBuf, pushRunnerErrBuf := createBufferedRunner()
	appPushabilityMeasurement := measurement.NewAppPushability(
		func() []cmdStartWaiter.CmdStartWaiter {
			w := pushWorkFlowGeneratorFunc()
			return append(
				w.Push(pushCmdGenerator),
				w.Delete(pushCmdGenerator)...,
			)
		},
		pushRunner,
		pushRunnerOutBuf,
		pushRunnerErrBuf,
	)

	winPushRunner, winPushRunnerOutBuf, winPushRunnerErrBuf := createBufferedRunner()
	winAppPushabilityMeasurement := measurement.NewAppPushability(
		func() []cmdStartWaiter.CmdStartWaiter {
			w := winPushWorkFlowGeneratorFunc()
			return append(
				w.Push(winPushCmdGenerator),
				w.Delete(winPushCmdGenerator)...,
			)
		},
		winPushRunner,
		winPushRunnerOutBuf,
		winPushRunnerErrBuf,
	)

	httpAvailabilityMeasurement := measurement.NewHTTPAvailability(
		orcWorkflow.AppUrl(),
		&http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				DisableKeepAlives: true,
			},
		},
	)

	return []measurement.Measurement{
		measurement.NewPeriodic(
			logger,
			clock,
			time.Second,
			httpAvailabilityMeasurement,
			measurement.NewResultSet(),
			allowedFailures.HttpAvailability,
			func(string, string) bool { return false },
		),
		measurement.NewPeriodic(
			logger,
			clock,
			time.Minute,
			appPushabilityMeasurement,
			measurement.NewResultSet(),
			allowedFailures.AppPushability,
			authFailedRetryFunc,
		),
		measurement.NewPeriodic(
			logger,
			clock,
			time.Minute,
			winAppPushabilityMeasurement,
			measurement.NewResultSet(),
			allowedFailures.AppPushability,
			authFailedRetryFunc,
		),
		measurement.NewPeriodic(
			logger,
			clock,
			10*time.Second,
			recentLogsMeasurement,
			measurement.NewResultSet(),
			allowedFailures.RecentLogs,
			authFailedRetryFunc,
		),
		measurement.NewPeriodic(
			logger,
			clock,
			30*time.Second,
			streamingLogsMeasurement,
			measurement.NewResultSet(),
			allowedFailures.StreamingLogs,
			authFailedRetryFunc,
		),
	}
}

func createAppSyslogAvailabilityMeasurement(
	clock clock.Clock,
	logger *log.Logger,
	sinkWorkflow cfWorkflow.CfWorkflow,
	sinkCmdGenerator cfCmdGenerator.CfCmdGenerator,
	allowedFailures config.AllowedFailures,
	authFailedRetryFunc func(stdOut, stdErr string) bool,
) measurement.Measurement {
	syslogAvailabilityBufferRunner, syslogAvailabilityRunnerOutBuf, syslogAvailabilityRunnerErrBuf := createBufferedRunner()
	syslogAvailabilityMeasurement := measurement.NewSyslogDrain(
		func() []cmdStartWaiter.CmdStartWaiter {
			return sinkWorkflow.RecentLogs(sinkCmdGenerator)
		},
		syslogAvailabilityBufferRunner,
		syslogAvailabilityRunnerOutBuf,
		syslogAvailabilityRunnerErrBuf,
		appLogValidator.New(),
	)

	return measurement.NewPeriodicWithoutMeasuringImmediately(
		logger,
		clock,
		30*time.Second,
		syslogAvailabilityMeasurement,
		measurement.NewResultSet(),
		allowedFailures.AppSyslogAvailability,
		authFailedRetryFunc,
	)
}

func createBufferedRunner() (cmdRunner.CmdRunner, *bytes.Buffer, *bytes.Buffer) {
	outBuf := bytes.NewBuffer([]byte{})
	errBuf := bytes.NewBuffer([]byte{})

	return cmdRunner.New(outBuf, errBuf, io.Copy), outBuf, errBuf
}

func logBufferedRunnerFailure(
	logger *log.Logger,
	whatFailed string,
	err error,
	outBuf, errBuf *bytes.Buffer,
) {
	logger.Printf(
		"Failed %s: %v\nstdout:\n%s\nstderr:\n%s\n",
		whatFailed,
		err,
		outBuf.String(),
		errBuf.String(),
	)
	outBuf.Reset()
	errBuf.Reset()
}

func tearDown(
	orc orchestrator.Orchestrator,
	orcCmdGenerator cfCmdGenerator.CfCmdGenerator,
	logger *log.Logger,
	pushWorkflow cfWorkflow.CfWorkflow,
	pushCmdGenerator cfCmdGenerator.CfCmdGenerator,
	winPushWorkflow cfWorkflow.CfWorkflow,
	winPushCmdGenerator cfCmdGenerator.CfCmdGenerator,
	sinkWorkflow cfWorkflow.CfWorkflow,
	sinkCmdGenerator cfCmdGenerator.CfCmdGenerator,
	runner cmdRunner.CmdRunner,
	runnerOutBuf *bytes.Buffer,
	runnerErrBuf *bytes.Buffer,
) {
	if err := orc.TearDown(runner, orcCmdGenerator); err != nil {
		logBufferedRunnerFailure(logger, "main teardown", err, runnerOutBuf, runnerErrBuf)
	}

	if err := runner.RunInSequence(pushWorkflow.TearDown(pushCmdGenerator)...); err != nil {
		logBufferedRunnerFailure(logger, "push workflow teardown", err, runnerOutBuf, runnerErrBuf)
	}

	if err := runner.RunInSequence(winPushWorkflow.TearDown(winPushCmdGenerator)...); err != nil {
		logBufferedRunnerFailure(logger, "push workflow teardown", err, runnerOutBuf, runnerErrBuf)
	}

	if sinkWorkflow != nil {
		if err := runner.RunInSequence(sinkWorkflow.TearDown(sinkCmdGenerator)...); err != nil {
			logBufferedRunnerFailure(logger, "sink workflow teardown", err, runnerOutBuf, runnerErrBuf)
		}
	}
}
