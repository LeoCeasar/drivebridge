using System.Text.Json;
using System.Runtime.InteropServices;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace DriveBridge.SessionHost;

internal sealed class SessionWindow : Form
{
    private readonly string origin;
    private readonly string profile;
    private readonly ApplicationContext context;
    private readonly WebView2 web = new() { Dock = DockStyle.Fill };
    private readonly Panel browserPanel = new() { Dock = DockStyle.Fill };
    private readonly Panel localPanel = new() { Dock = DockStyle.Fill, BackColor = Color.WhiteSmoke, Visible = false };
    private readonly Label address = new() { AutoSize = false, Dock = DockStyle.Fill, TextAlign = ContentAlignment.MiddleLeft, AutoEllipsis = true };
    private readonly Label status = new() { Dock = DockStyle.Bottom, Height = 32, AutoEllipsis = true, Padding = new Padding(12, 0, 12, 0), TextAlign = ContentAlignment.MiddleLeft };
    private readonly Label headline = new() { AutoSize = true, Font = new Font("Segoe UI", 23, FontStyle.Bold), TextAlign = ContentAlignment.MiddleCenter, Margin = new Padding(10, 24, 10, 12) };
    private readonly Label subtitle = new() { AutoSize = true, Font = new Font("Segoe UI", 13), TextAlign = ContentAlignment.MiddleCenter, Margin = new Padding(10) };
    private readonly Label detail = new() { AutoSize = true, MaximumSize = new Size(620, 0), TextAlign = ContentAlignment.MiddleCenter, Margin = new Padding(24, 16, 24, 16) };
    private readonly Button done = new() { Text = "先隐藏到后台", AutoSize = true, Padding = new Padding(16, 8, 16, 8), Margin = new Padding(8) };
    private readonly Queue<Command> fetchQueue = new();
    private bool fetching;
    private bool stopping;
    private bool completed;
    private bool navigated;
    private bool loading = true;
    private int pendingUi;

    internal SessionWindow(string origin, string profile, ApplicationContext context)
    {
        this.origin = origin; this.profile = profile; this.context = context;
        Text = "DriveBridge 飞书登录";
        ShowInTaskbar = false;
        FormBorderStyle = FormBorderStyle.SizableToolWindow; // No second taskbar/Alt+Tab application.
        StartPosition = FormStartPosition.CenterScreen;
        ClientSize = new Size(1000, 760);
        MinimumSize = new Size(700, 540);
        var top = new Panel { Dock = DockStyle.Top, Height = 52, Padding = new Padding(12, 8, 12, 8) };
        var hide = new Button { Text = "隐藏到后台", Dock = DockStyle.Right, Width = 112 };
        var reload = new Button { Text = "重新载入", Dock = DockStyle.Right, Width = 100 };
        hide.Click += (_, _) => Hide(); reload.Click += (_, _) => Reload();
        address.Text = new Uri(origin).Host;
        top.Controls.Add(address); top.Controls.Add(reload); top.Controls.Add(hide);
        browserPanel.Controls.Add(web); browserPanel.Controls.Add(top); browserPanel.Controls.Add(status);
        Controls.Add(browserPanel); Controls.Add(localPanel);
        var stack = new FlowLayoutPanel { FlowDirection = FlowDirection.TopDown, WrapContents = false, AutoSize = true, Anchor = AnchorStyles.None, Padding = new Padding(24), MaximumSize = new Size(700, 0) };
        var mark = new Label { Text = "✓", Font = new Font("Segoe UI", 44), ForeColor = Color.SeaGreen, AutoSize = true, Margin = new Padding(10) };
        var buttons = new FlowLayoutPanel { AutoSize = true, WrapContents = false, Margin = new Padding(10) };
        var retry = new Button { Text = "重新打开登录页", AutoSize = true, Padding = new Padding(16, 8, 16, 8), Margin = new Padding(8) };
        done.Click += (_, _) => Hide(); retry.Click += (_, _) => Reload();
        buttons.Controls.Add(done); buttons.Controls.Add(retry);
        stack.Controls.Add(mark); stack.Controls.Add(headline); stack.Controls.Add(subtitle); stack.Controls.Add(detail); stack.Controls.Add(buttons);
        foreach (Control child in stack.Controls) child.Anchor = AnchorStyles.None;
        localPanel.Controls.Add(stack);
        void CenterStack()
        {
            stack.Location = new Point(Math.Max(0, (localPanel.ClientSize.Width - stack.Width) / 2), Math.Max(0, (localPanel.ClientSize.Height - stack.Height) / 2));
        }
        localPanel.Resize += (_, _) => CenterStack(); stack.SizeChanged += (_, _) => CenterStack();
        FormClosing += (_, e) => { if (!stopping) { e.Cancel = true; Hide(); } };
        Resize += (_, _) => { if (WindowState == FormWindowState.Minimized) { Hide(); WindowState = FormWindowState.Normal; } };
    }

    private static string SessionDirectory(string profile)
    {
        var folder = Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData);
        if (string.IsNullOrEmpty(folder) || !Path.IsPathFullyQualified(folder)) throw new IOException("Missing user data directory");
        foreach (var name in new[] { "DriveBridge", "Sessions", profile })
        {
            folder = Path.Combine(folder, name);
            if (Directory.Exists(folder) && (File.GetAttributes(folder) & FileAttributes.ReparsePoint) != 0) throw new IOException("Redirected session directory");
            Directory.CreateDirectory(folder);
            if ((File.GetAttributes(folder) & FileAttributes.ReparsePoint) != 0) throw new IOException("Redirected session directory");
        }
        return folder;
    }

    internal async Task InitializeAsync()
    {
        _ = Task.Run(ReadInputAsync); // Observe parent EOF even while WebView2 is initializing.
        try
        {
            var dataFolder = SessionDirectory(profile);
            var environment = await CoreWebView2Environment.CreateAsync(null, dataFolder);
            if (!string.Equals(Path.GetFullPath(environment.UserDataFolder), Path.GetFullPath(dataFolder), StringComparison.OrdinalIgnoreCase)) throw new IOException("Session data folder overridden");
            if (stopping) return;
            await web.EnsureCoreWebView2Async(environment);
            if (stopping) return;
            var core = web.CoreWebView2;
            core.Settings.AreDevToolsEnabled = false;
            core.Settings.AreDefaultContextMenusEnabled = false;
            core.Settings.AreHostObjectsAllowed = false;
            core.Settings.IsWebMessageEnabled = false;
            core.Settings.AreBrowserAcceleratorKeysEnabled = false;
            core.Settings.IsStatusBarEnabled = false;
            core.Settings.IsPasswordAutosaveEnabled = false;
            core.Settings.IsGeneralAutofillEnabled = false;
            core.NavigationStarting += (_, e) =>
            {
                if (!AllowedNavigation(e.Uri)) { e.Cancel = true; status.Text = "请使用飞书页面提供的网页登录或扫码登录。"; return; }
                loading = true; navigated = false;
            };
            core.SourceChanged += (_, _) =>
            {
                if (Uri.TryCreate(core.Source, UriKind.Absolute, out var current))
                {
                    address.Text = current.Host;
                    if (!completed && IsAuthentication(current)) ShowCloud();
                }
            };
            core.NavigationCompleted += (_, e) =>
            {
                loading = false; navigated = e.IsSuccess;
                if (completed) return;
                if (e.IsSuccess && CurrentUri() is { } current && !IsAuthentication(current)) ShowLocal(false);
                else { ShowCloud(); if (!e.IsSuccess) status.Text = "页面暂时无法加载，请检查网络或点击重新载入。"; }
            };
            core.NewWindowRequested += (_, e) => { e.Handled = true; if (AllowedNavigation(e.Uri)) core.Navigate(e.Uri); };
            core.DownloadStarting += (_, e) => e.Cancel = true;
            core.PermissionRequested += (_, e) => e.State = CoreWebView2PermissionState.Deny;
            core.ServerCertificateErrorDetected += (_, e) => e.Action = CoreWebView2ServerCertificateErrorAction.Cancel;
            core.ProcessFailed += (_, _) => { navigated = false; loading = false; if (!completed) { ShowCloud(); status.Text = "登录页面已中断，请点击重新载入。"; } };
            Reload();
            Program.Ready();
        }
        catch (WebView2RuntimeNotFoundException) { if (!stopping) { Program.Ready("missing_runtime"); Shutdown(); } }
        catch { if (!stopping) { Program.Ready("unavailable"); Shutdown(); } }
    }

    private async Task ReadInputAsync()
    {
        try
        {
            await Protocol.ReadLinesAsync(Console.OpenStandardInput(), async data =>
            {
                var command = Protocol.Parse(data);
                if (command is null) return;
                // Do not let an unbounded stream fill the GUI message queue.
                if (Interlocked.Increment(ref pendingUi) > 16)
                {
                    Interlocked.Decrement(ref pendingUi); Reply(command, false, "request_failed"); return;
                }
                var accepted = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                try
                {
                    BeginInvoke(() => { try { HandleCommand(command); } finally { Interlocked.Decrement(ref pendingUi); accepted.TrySetResult(); } });
                    await accepted.Task;
                }
                catch { Interlocked.Decrement(ref pendingUi); }
            });
        }
        catch { /* EOF or a broken private pipe ends the helper. */ }
        try { BeginInvoke(Shutdown); } catch { }
    }

    private static bool AllowedNavigation(string value) => value == "about:blank" || (Uri.TryCreate(value, UriKind.Absolute, out var uri) && uri.Scheme == "https" && string.IsNullOrEmpty(uri.UserInfo));
    private Uri? CurrentUri() => Uri.TryCreate(web.CoreWebView2?.Source, UriKind.Absolute, out var current) ? current : null;
    private bool SameOrigin(Uri uri) => uri.Scheme == "https" && uri.GetLeftPart(UriPartial.Authority) == origin;
    private bool IsAuthentication(Uri uri) => !SameOrigin(uri) || new[] { "/accounts", "/account/", "/login", "/passport", "/auth" }.Any(prefix => uri.AbsolutePath.StartsWith(prefix, StringComparison.OrdinalIgnoreCase));
    private void ShowCloud() { localPanel.Visible = false; browserPanel.Visible = true; }
    private void ShowLocal(bool success)
    {
        headline.Text = success ? "网页登录已验证" : "正在核验网页登录";
        subtitle.Text = success ? "目录发现已完成" : "正在自动核验账号并发现目录";
        detail.Text = success ? "请返回 DriveBridge 设置查看账号盘和挂载状态。关闭此窗口后，目录发现仍可在后台继续。" : "若等待较久，可返回 DriveBridge 重新验证。";
        done.Text = success ? "完成并隐藏" : "先隐藏到后台";
        Text = success ? "DriveBridge · 网页登录已验证" : "DriveBridge 飞书登录";
        // Keep the browser HWND alive; hiding the control must not discard its persistent session.
        browserPanel.Visible = false; localPanel.Visible = true; localPanel.BringToFront();
    }
    private void ShowOwnedWindow() { Show(); WindowState = FormWindowState.Normal; Activate(); BringToFront(); }
    private void Reload()
    {
        completed = false; navigated = false; loading = true; ShowCloud();
        status.Text = "登录后将自动核验账号并发现目录；若等待较久，可返回 DriveBridge 重新验证。";
        web.CoreWebView2?.Navigate(origin + "/drive/me/");
    }
    private static void Reply(Command command, bool ok, string? error = null) => Program.Write(new { id = command.Id, ok, error });
    private static void Result(Command command, string kind, int status = 0, string contentType = "", string body = "") => Program.Write(new { id = command.Id, ok = true, result = new { kind, status, contentType, body } });

    private void HandleCommand(Command command)
    {
        if (stopping) return;
        switch (command.Op)
        {
            case "show":
                completed = false;
                if (CurrentUri() is { } current && !IsAuthentication(current) && navigated) ShowLocal(false); else ShowCloud();
                ShowOwnedWindow(); Reply(command, true); break;
            case "hide": Hide(); Reply(command, true); break;
            case "complete": completed = true; ShowLocal(true); ShowOwnedWindow(); Reply(command, true); break;
            case "shutdown": Reply(command, true); Shutdown(); break;
            case "fetch":
                if (!Protocol.ValidRequest(command.Path, command.Query)) { Reply(command, false, "protocol"); break; }
                if (CurrentUri() is { } page && page.AbsoluteUri != "about:blank" && IsAuthentication(page)) { Result(command, "login"); break; }
                if (loading || !navigated || CurrentUri() is null) { Result(command, "loading"); break; }
                if (fetchQueue.Count >= 8) { Reply(command, false, "request_failed"); break; }
                fetchQueue.Enqueue(command); _ = PumpFetchAsync(); break;
            default: Reply(command, false, "protocol"); break;
        }
    }

    private async Task PumpFetchAsync()
    {
        if (fetching || stopping) return;
        fetching = true;
        try
        {
            while (fetchQueue.TryDequeue(out var command) && !stopping)
            {
                try
                {
                    if (loading || !navigated) { Result(command, "loading"); continue; }
                    if (CurrentUri() is not { } current || IsAuthentication(current)) { Result(command, "login"); continue; }
                    var core = web.CoreWebView2;
                    // Evaluate in an isolated world, with no page-to-native messaging surface.
                    // No remote-debugging port is opened. CDP remains private to this control.
                    using var tree = JsonDocument.Parse(await core.CallDevToolsProtocolMethodAsync("Page.getFrameTree", "{}"));
                    var frameId = tree.RootElement.GetProperty("frameTree").GetProperty("frame").GetProperty("id").GetString();
                    using var world = JsonDocument.Parse(await core.CallDevToolsProtocolMethodAsync("Page.createIsolatedWorld", JsonSerializer.Serialize(new { frameId, worldName = "drivebridge-directory-reader", grantUniveralAccess = false })));
                    var contextId = world.RootElement.GetProperty("executionContextId").GetInt32();
                    var target = command.Path + (command.Query.Length == 0 ? "" : "?" + command.Query);
                    var expression = FetchScript.Replace("__ORIGIN__", JsonSerializer.Serialize(origin), StringComparison.Ordinal).Replace("__TARGET__", JsonSerializer.Serialize(target), StringComparison.Ordinal);
                    var evaluation = await core.CallDevToolsProtocolMethodAsync("Runtime.evaluate", JsonSerializer.Serialize(new { expression, contextId, awaitPromise = true, returnByValue = true })).WaitAsync(TimeSpan.FromSeconds(35));
                    using var response = JsonDocument.Parse(evaluation);
                    var root = response.RootElement;
                    if (root.TryGetProperty("exceptionDetails", out _) || !root.TryGetProperty("result", out var remote) || !remote.TryGetProperty("value", out var value)) { Reply(command, false, "request_failed"); continue; }
                    var kind = value.GetProperty("kind").GetString() ?? "";
                    var statusCode = value.GetProperty("status").GetInt32();
                    var type = value.GetProperty("contentType").GetString() ?? "";
                    var body = value.GetProperty("body").GetString() ?? "";
                    if (Protocol.Utf8.GetByteCount(body) > Protocol.MaxBodyBytes) { Reply(command, false, "protocol"); continue; }
                    Result(command, kind, statusCode, type, body);
                }
                catch { if (!stopping) Reply(command, false, "request_failed"); }
            }
        }
        finally { fetching = false; }
    }

    private const string FetchScript = """
    (async () => {
      const origin = __ORIGIN__, target = __TARGET__;
      if (location.origin !== origin || /^\/(?:accounts?|login|passport|auth)(?:\/|$)/i.test(location.pathname)) return {kind:'login',status:0,contentType:'',body:''};
      const abort = new AbortController(), timer = setTimeout(() => abort.abort(), 30000);
      try {
        const response = await fetch(target, {method:'GET',mode:'same-origin',credentials:'include',redirect:'error',signal:abort.signal,headers:{Accept:'application/json'}});
        const contentType = response.headers.get('content-type') || '';
        if (response.status === 401 || response.status === 403 || contentType.toLowerCase().includes('text/html')) return {kind:'login',status:response.status,contentType,body:''};
        if (!response.body) return {kind:'request_failed',status:response.status,contentType,body:''};
        const reader = response.body.getReader(), decoder = new TextDecoder('utf-8', {fatal:true});
        let total = 0, body = '';
        for (;;) {
          const part = await reader.read(); if (part.done) break;
          total += part.value.byteLength;
          if (total > 16777216) { await reader.cancel(); return {kind:'request_failed',status:0,contentType:'',body:''}; }
          body += decoder.decode(part.value, {stream:true});
        }
        body += decoder.decode(); return {kind:'',status:response.status,contentType,body};
      } catch (_) { return {kind:'request_failed',status:0,contentType:'',body:''}; }
      finally { clearTimeout(timer); }
    })()
    """;

    [DllImport("user32.dll", EntryPoint = "GetWindowLongW")]
    private static extern int GetWindowLong(IntPtr window, int index);

    internal void AssertWindowBehavior()
    {
        static void Assert(bool value) { if (!value) throw new InvalidOperationException("Window parity smoke failed"); }
        Assert(!Visible && !ShowInTaskbar && FormBorderStyle == FormBorderStyle.SizableToolWindow);
        var styles = GetWindowLong(Handle, -20);
        Assert((styles & 0x80) != 0 && (styles & 0x40000) == 0); // TOOLWINDOW, never APPWINDOW.
        ShowOwnedWindow(); Assert(Visible);
        Hide(); Assert(!Visible && !IsDisposed);
        ShowOwnedWindow(); Close(); Assert(!Visible && !IsDisposed);
        ShowOwnedWindow(); WindowState = FormWindowState.Minimized; Application.DoEvents(); Assert(!Visible && !IsDisposed);
    }
    internal void WatchParentForSmoke() => _ = Task.Run(ReadInputAsync);

    private void Shutdown()
    {
        if (stopping) return;
        stopping = true; fetchQueue.Clear(); Hide();
        web.Dispose(); context.ExitThread();
    }
}
