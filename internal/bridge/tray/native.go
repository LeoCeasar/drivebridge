//go:build desktop && (windows || (cgo && (darwin || linux)))

package tray

import (
	"context"
	"embed"
	"fmt"
	"runtime"
	"sync/atomic"

	"github.com/getlantern/systray"
)

//go:embed assets/*.png assets/*.ico
var icons embed.FS

var nativeStarted atomic.Bool

// Available reports compiled native support; Run checks the graphical session.
// Linux additionally requires a running StatusNotifierWatcher on session D-Bus.
func Available() bool { return true }

// Run must be called from the startup goroutine on the initial OS thread.
// Upstream systray owns a process-wide native loop, so Run may be used only once.
// Cancellation closes only the tray; the caller owns synchronization workers.
func Run(ctx context.Context, initial View, updates <-chan View, actions chan<- Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if actions == nil {
		return fmt.Errorf("%w: action consumer is required", ErrUnavailable)
	}
	if !nativeStarted.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: native event loop may run only once per process", ErrUnavailable)
	}
	if err := preflight(); err != nil {
		nativeStarted.Store(false)
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	systray.Run(func() {
		clicks := make(chan string, 64)
		items := map[string]*systray.MenuItem{}
		readySent := false
		render := func(v View, entries []menuEntry) {
			visible := make(map[string]bool, len(entries))
			for _, e := range entries {
				visible[e.Key] = true
				item := items[e.Key]
				if item == nil {
					if e.Parent == "" {
						item = systray.AddMenuItem(e.Label, e.Tooltip)
					} else {
						item = items[e.Parent].AddSubMenuItem(e.Label, e.Tooltip)
					}
					items[e.Key] = item
					go watchClicks(runCtx, item.ClickedCh, e.Key, clicks)
				} else {
					item.SetTitle(e.Label)
					item.SetTooltip(e.Tooltip)
					item.Show()
				}
				if e.Enabled {
					item.Enable()
				} else {
					item.Disable()
				}
			}
			for key, item := range items {
				if !visible[key] {
					item.Hide()
				}
			}
			state := v.State
			switch state {
			case "ready", "syncing", "conflict", "error", "stopped":
			case "auth":
				state = "error"
			default:
				state = "unavailable"
			}
			ext := ".png"
			if runtime.GOOS == "windows" {
				ext = ".ico"
			}
			data, _ := icons.ReadFile("assets/" + state + ext)
			systray.SetIcon(data)
			// The menu keeps state explicit even on monochrome desktop themes.
			systray.SetTitle("DB")
			systray.SetTooltip("DriveBridge · " + stateLabel(v.State) + " " + v.Text)
			if !readySent {
				// Upstream runs onReady on a worker goroutine, never the UI thread.
				select {
				case actions <- Action{Kind: "ready"}:
				case <-runCtx.Done():
				}
				readySent = true
			}
		}
		done <- serve(runCtx, initial, updates, clicks, actions, render)
		systray.Quit()
	}, cancel)
	cancel()
	return <-done
}

func watchClicks(ctx context.Context, source <-chan struct{}, key string, clicks chan<- string) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-source:
			if !ok {
				return
			}
			select {
			case clicks <- key:
			case <-ctx.Done():
				return
			}
		}
	}
}
