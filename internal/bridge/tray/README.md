# Native tray adapter

The ordinary build has no GUI imports and returns `ErrUnavailable`. Build with
`-tags desktop` for the native adapter. macOS and Linux additionally need
`CGO_ENABLED=1`; Windows uses upstream's native Win32 implementation and can
build with CGO disabled.

The dependency is pinned to `github.com/getlantern/systray v1.2.2`.
[Upstream build requirements](https://github.com/getlantern/systray/tree/v1.2.2)
include GTK3 and libayatana-appindicator3 development headers on Linux. The
optional `legacy_appindicator` tag selects libappindicator3 instead.

Call `Run` once, on the startup goroutine, preserving the initial OS thread.
`Available` describes the build only. Linux `Run` checks GTK display access
before upstream's fatal `gtk_init`, then requires a live
`org.kde.StatusNotifierWatcher` on the session bus. A desktop without this
capability returns an `ErrUnavailable` diagnostic. These probes establish
indicator support; actual visibility still requires native session inspection.

`Action{Kind: "ready"}` is an internal lifecycle event emitted after native
initialization and the initial menu/icon setup, before user actions. It is not a
menu action. A receiver must consume actions while `Run` is active.

Process owners should use `RunWithShutdown` and supply cleanup that cancels and
drains application work, closes owned UI/browser resources, and releases their
singleton. The callback completes once before normal native Quit, and also runs
for platform exit callbacks. macOS upstream Quit calls `NSApp terminate`, so Go
callers may never return or run deferred cleanup. Native preflight failures do
not invoke the callback. The desktop uses this hook; synchronization workers
remain independent processes and are not stopped when quitting the tray.

PNG and Windows ICO assets are local 32×32 geometric status symbols: check for
ready, arrows for syncing, cross for conflicts, exclamation for errors, dash for
unavailable, and square for stopped. They use no downloaded icons or fonts.
