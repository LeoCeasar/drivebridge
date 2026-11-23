package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/onlinecooldown"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestManagedRunnersShareCanonicalCooldownAcrossConfigs(t *testing.T) {
	s, _, f, id, root := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	b, err := s.Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	other := model.ProfileID(uuid.NewString())
	o := s.options
	o.ConfigFile = filepath.Join(root, "other.json")
	o.Profiles = &memoryProfiles{profiles: map[model.ProfileID]model.Profile{other: {ID: other, Brand: "feishu", Name: "alias"}}}
	alias, err := newServiceAt(context.Background(), o, root, s.factory)
	if err != nil {
		t.Fatal(err)
	}
	defer alias.Close()
	if _, err = alias.Link(context.Background(), other, b.AuthRef); err != nil {
		t.Fatal(err)
	}
	cool, err := s.onlineCoordinator(b)
	if err != nil {
		t.Fatal(err)
	}
	if err = cool.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	runner, err := alias.ManagedRunner(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), larkcli.Request{Args: []string{"drive", "files", "list"}})
	var wait *onlinecooldown.WaitError
	if !errors.As(err, &wait) || !errors.Is(err, larkcli.ErrNotDispatched) {
		t.Fatalf("alias bypassed cooldown: %v", err)
	}
}
