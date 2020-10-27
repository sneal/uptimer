package winapp

const WebConfig = `<?xml version="1.0" encoding="utf-8"?>
<configuration>
    <system.web>
      <customErrors mode="Off"/>
      <compilation debug="true" targetFramework="4.5" />
      <httpRuntime targetFramework="4.5" />
    </system.web>
</configuration>
`

const GlobalAsax = `<%@ Application Language="C#" %>
<script runat="server"> 
    void Application_Error(object sender, EventArgs e) 
    {
        Exception lastError = Server.GetLastError();
        Console.WriteLine("Unhandled exception: " + lastError.Message + lastError.StackTrace);
    }
</script>
`

const DefaultAspxCs = `using System;
using System.Web;

public partial class _Default : System.Web.UI.Page
{
    protected void Page_Load(object sender, EventArgs e)
    {
        // Stop Caching in IE
        Response.Cache.SetCacheability(HttpCacheability.NoCache);

        // Stop Caching in Firefox
        Response.Cache.SetNoStore();
    }
}
`

const DefaultAspx = `<%@ Page Language="C#" AutoEventWireup="true" CodeFile="Default.aspx.cs" Inherits="_Default" %>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml">
<head runat="server">
    <title>Hello</title>
</head>
<body>
    <form id="form1" runat="server">
        <h2>Hello</h2>
    </form>
</body>
</html>
`
