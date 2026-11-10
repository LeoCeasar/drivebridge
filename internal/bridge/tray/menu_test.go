package tray

import (
	"context"
	"errors"
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
