package manager

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestBackgroundPullPolicyChangesWithoutRestart(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	before, err := m.ResolveMount(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(context.Context, model.EntryID) (model.Entry, error) {
		return model.Entry{Remote: "logs/application.LOG", Kind: model.KindFile}, nil
	}
	check := func(want bool) {
		t.Helper()
		got, err := backgroundPullAllowed(ctx, m.LoadConfig, lookup, id, "entry")
		if err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	check(false)
	if err = m.SetBackgroundPullPolicy(ctx, id, []string{}); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err = m.SetBackgroundPullPolicy(ctx, id, []string{"log", " .DB "}); err != nil {
		t.Fatal(err)
	}
	check(false)
	after, err := m.ResolveMount(ctx, string(id))
	if err != nil || after.ID != before.ID || after.Mountpoint != before.Mountpoint || after.RemoteID != before.RemoteID {
		t.Fatal(after, err)
	}
	if err = m.SetBackgroundPullPolicy(ctx, id, []string{"../log"}); err == nil {
		t.Fatal("unsafe suffix accepted")
	}
	check(false)
}
