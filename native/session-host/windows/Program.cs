using System.Text.Json;
using System.Text.Encodings.Web;
using Microsoft.Web.WebView2.Core;

namespace DriveBridge.SessionHost;

internal static class Program
{
    private static readonly Stream Output = Console.OpenStandardOutput();
    private static readonly object OutputLock = new();
    private static readonly JsonSerializerOptions JsonOptions = new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping };

    internal static void Write(object value)
    {
        var bytes = JsonSerializer.SerializeToUtf8Bytes(value, JsonOptions);
        if (bytes.Length > Protocol.MaxOutputBytes) throw new InvalidDataException("Response too large");
        lock (OutputLock) { Output.Write(bytes); Output.WriteByte(10); Output.Flush(); }
    }

    internal static void Ready(string? error = null) => Write(new {
        @event = "ready", protocol = Protocol.Version, bundle_id = Protocol.BundleId,
        persistent = error is null, error
    });

    private static int RunWindowSmoke(bool watchPipe)
    {
        Application.EnableVisualStyles();
        using var context = new ApplicationContext();
        using var window = new SessionWindow("https://drivebridge-smoke.feishu.cn", Guid.NewGuid().ToString("D"), context);
        var exit = 0;
        _ = window.Handle;
        window.BeginInvoke(() => {
            try
            {
                window.AssertWindowBehavior();
                if (watchPipe) { Write(new { @event = "smoke_ready", ok = true }); window.WatchParentForSmoke(); }
                else { Write(new { @event = "smoke_complete", ok = true, checks = new[] { "hidden_start", "no_taskbar", "tool_window", "show_hide", "close_hides", "minimize_hides" } }); context.ExitThread(); }
            }
            catch { exit = 1; Write(new { @event = "smoke_complete", ok = false }); context.ExitThread(); }
        });
        Application.Run(context);
        return exit;
    }

    [STAThread]
    private static int Main(string[] args)
    {
        if (args.SequenceEqual(new[] { "--capabilities" }))
        {
            Write(new { protocol = Protocol.Version, bundle_id = Protocol.BundleId,
                provider = "WebView2", persistent_per_account = true,
                operations = new[] { "show", "hide", "fetch", "complete", "shutdown" } });
            return 0;
        }
        if (args.SequenceEqual(new[] { "--self-test" }) || args.SequenceEqual(new[] { "--self-test-pipe" })) return RunWindowSmoke(args[0] == "--self-test-pipe");
        if (!Protocol.TryArguments(args, out var origin, out var profile)) return 2;
        // WebView2 supports environment overrides for its data folder, runtime and
        // debug arguments. Never let inherited shell settings replace our store.
        foreach (System.Collections.DictionaryEntry entry in Environment.GetEnvironmentVariables())
            if (entry.Key is string key && key.StartsWith("WEBVIEW2_", StringComparison.OrdinalIgnoreCase))
                Environment.SetEnvironmentVariable(key, null);
        try { CoreWebView2Environment.GetAvailableBrowserVersionString(); }
        catch (WebView2RuntimeNotFoundException) { Ready("missing_runtime"); return 3; }
        catch { Ready("unavailable"); return 3; }

        Application.SetHighDpiMode(HighDpiMode.PerMonitorV2);
        Application.EnableVisualStyles();
        Application.SetCompatibleTextRenderingDefault(false);
        using var context = new ApplicationContext();
        using var window = new SessionWindow(origin, profile, context);
        _ = window.Handle; // A hidden HWND and message loop are sufficient to initialize WebView2.
        window.BeginInvoke(async () => await window.InitializeAsync());
        Application.Run(context); // No MainForm: the login window never appears until an explicit show.
        return 0;
    }
}
