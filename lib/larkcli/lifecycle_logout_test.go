package larkcli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLogoutLifecycleShapeAndLock(t *testing.T) {
	for _, scenario := range []string{"logout", "logout-not-logged", "logout-malformed", "logout-partial"} {
		t.Run(scenario, func(t *testing.T) {
			c := &testCoordinator{}
			r, err := NewManaged(ManagedOptions{Executable: testExecutable(t, scenario), AuthRef: "ref", AuthRoot: t.TempDir(), Identity: Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: c})
			if err != nil {
				t.Fatal(err)
			}
			err = r.Logout(context.Background())
			if scenario == "logout" || scenario == "logout-not-logged" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnknownResult) {
				t.Fatalf("mutation ambiguity discarded: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("raw failure leaked")
			}
			if c.calls != 1 {
				t.Fatal("logout outside credential locks")
			}
		})
	}
	if err := NewDirect("not-executed").Logout(context.Background()); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}
