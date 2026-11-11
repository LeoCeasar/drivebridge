package tray

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMenuPreservesMountIdentityAndSeparatesQuitFromStop(t *testing.T) {
	entries := menu(View{State: "conflict", Mounts: []Mount{{ID: "a", Name: "同名", Running: true, Desired: true, Pending: 2, Conflicts: 1}, {ID: "b", Name: "同名"}}})
	actions := map[Action]menuEntry{}
	for _, entry := range entries {
		if entry.Action.Kind != "" {
			actions[entry.Action] = entry
		}
	}
	for _, action := range []Action{{Kind: "open_manager"}, {Kind: "open_folder", MountID: "a"}, {Kind: "retry", MountID: "a"}, {Kind: "stop", MountID: "a"}, {Kind: "start", MountID: "b"}, {Kind: "enable_autostart"}, {Kind: "stop_all"}, {Kind: "quit"}} {
		if _, ok := actions[action]; !ok {
			t.Errorf("missing action %#v", action)
		}
	}
	if got := actions[Action{Kind: "quit"}].Label; got != "退出托盘（继续同步）" {
		t.Fatalf("quit label = %q", got)
	}
	if !actions[Action{Kind: "stop", MountID: "a"}].Enabled || !actions[Action{Kind: "start", MountID: "b"}].Enabled {
		t.Fatal("running and stopped mounts must have separate controls")
	}
}

func TestBusyMenuKeepsOpenAndQuitUsable(t *testing.T) {
	for _, entry := range menu(View{Busy: true, Autostart: true, Mounts: []Mount{{ID: "m", Running: true, Mountpoint: "/tmp/m"}}}) {
		switch entry.Action.Kind {
		case "open_manager", "open_folder", "quit":
			if !entry.Enabled {
				t.Errorf("%s disabled", entry.Action.Kind)
			}
		case "disable_autostart", "stop_all", "stop", "retry":
			if entry.Enabled {
				t.Errorf("%s enabled while busy", entry.Action.Kind)
			}
		}
	}
}

func TestServeRemainsResponsiveWithBlockedActionConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clicks := make(chan string, 4)
	updates := make(chan View, 1)
	renders := make(chan []menuEntry, 4)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, View{}, updates, clicks, make(chan Action), func(_ View, entries []menuEntry) { renders <- entries })
	}()
	initial := <-renders
	for _, entry := range initial {
		if entry.Action.Kind == "open_manager" {
			clicks <- entry.Key
		}
	}
	updates <- View{Busy: true}
	select {
	case <-renders:
	case <-time.After(time.Second):
		t.Fatal("blocked action consumer stalled view updates")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked action consumer stalled cancellation")
	}
}

func TestServeRejectsDisabledAndStaleClicks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clicks := make(chan string, 4)
	actions := make(chan Action, 4)
	renders := make(chan []menuEntry, 4)
	updates := make(chan View)
	go func() {
		_ = serve(ctx, View{Mounts: []Mount{{ID: "m"}}}, updates, clicks, actions, func(_ View, entries []menuEntry) { renders <- entries })
	}()
	entries := <-renders
	var stale, open string
	for _, e := range entries {
		if e.Action.Kind == "start" {
			stale = e.Key
		}
		if e.Action.Kind == "open_manager" {
			open = e.Key
		}
		if e.Action.Kind == "stop" {
			clicks <- e.Key
		}
	}
	updates <- View{}
	<-renders
	clicks <- stale
	clicks <- open
	select {
	case got := <-actions:
		if got != (Action{Kind: "open_manager"}) {
			t.Fatalf("dispatched disabled/stale action: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no open action")
	}
}

func TestAccountMenuShowsLoginStatusAndKeepsProfileIdentity(t *testing.T) {
	entries := menu(View{Accounts: []Account{{ID: "a", Name: "同名", State: "complete", Configured: true, Enabled: true, CanRescan: true}, {ID: "b", Name: "同名", State: "needs_login", Configured: true, Enabled: true}}})
	rescan := map[string]menuEntry{}
	login := map[string]bool{}
	for _, entry := range entries {
		if entry.Action.Kind == "rescan_discovery" {
			rescan[entry.Action.ProfileID] = entry
		}
		if entry.Label == "应用登录：需要重新登录" || entry.Label == "应用登录：已保存" {
			login[entry.Label] = true
		}
	}
	if len(rescan) != 2 || !rescan["a"].Enabled || rescan["b"].Enabled || len(login) != 2 {
		t.Fatal(entries)
	}
	if rescan["a"].Key == rescan["b"].Key || rescan["a"].Action.MountID != "" {
		t.Fatal(rescan)
	}
	for _, entry := range menu(View{Busy: true, Accounts: []Account{{ID: "a", CanRescan: true}}}) {
		if entry.Action.Kind == "rescan_discovery" && entry.Enabled {
			t.Fatal("rescan enabled while busy")
		}
		if entry.Action.Kind == "open_manager" && !entry.Enabled {
			t.Fatal("management disabled while busy")
		}
	}
}

func TestShutdownWaitsForOwnerCleanupAcrossNativeQuitAndExit(t *testing.T) {
	entered, allowClose := make(chan struct{}), make(chan struct{})
	browserClosed := make(chan struct{})
	shutdown := shutdownOnce(func() { close(entered); <-allowClose; close(browserClosed) })
	normalQuit, nativeExit := make(chan struct{}), make(chan struct{})
	go func() { shutdown(); close(normalQuit) }()
	<-entered
	go func() { shutdown(); close(nativeExit) }()
	select {
	case <-normalQuit:
		t.Fatal("Quit passed unfinished owner cleanup")
	default:
	}
	select {
	case <-nativeExit:
		t.Fatal("native exit passed unfinished owner cleanup")
	default:
	}
	close(allowClose)
	for _, done := range []chan struct{}{normalQuit, nativeExit} {
		select {
		case <-done:
			select {
			case <-browserClosed:
			default:
				t.Fatal("exit preceded browser close")
			}
		case <-time.After(time.Second):
			t.Fatal("native shutdown path did not finish")
		}
	}
}

func TestNativeLifetimeOutlivesCanceledAppUntilCleanupCompletes(t *testing.T) {
	appCtx, cancelApp := context.WithCancel(context.Background())
	entered, finishCleanup := make(chan struct{}), make(chan struct{})
	nativeCtx, cancelNative, shutdown := nativeLifetime(appCtx, func() { close(entered); <-finishCleanup })
	defer cancelNative()
	cancelApp()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("app cancellation did not start cleanup")
	}
	if nativeCtx.Err() != nil {
		t.Fatal("native loop canceled before owner cleanup")
	}
	close(finishCleanup)
	select {
	case <-nativeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("native loop did not stop after owner cleanup")
	}
	shutdown()
}

func TestAccountLoginLabelsUseApplicationWindow(t *testing.T) {
	for _, state := range []string{"needs_extension", "browser_disconnected", "needs_login"} {
		label := accountLoginLabel(Account{State: state, Configured: true})
		if !strings.Contains(label, "重新登录") || strings.Contains(label, "扩展") || strings.Contains(label, "浏览器") {
			t.Fatalf("unexpected login guidance for %s: %s", state, label)
		}
	}
	if got := discoveryLabel("browser_disconnected"); got != "目录发现：等待重新登录" {
		t.Fatal(got)
	}
}
