package store

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestClaimConflictNotificationPersistsOnce(t *testing.T) {
	s, d, sc := openTest(t)
	e, _ := stage(t, s, entry(t, s, "conflict"), "bytes")
	c := model.Conflict{ID: "notification-conflict", EntryID: e.ID, Generation: e.Generation, Local: e.Local}
	if err := s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimConflictNotification(ctx, c.ID)
	if err != nil || !first {
		t.Fatal(first, err)
	}
	again, err := s.ClaimConflictNotification(ctx, c.ID)
	if err != nil || again {
		t.Fatal(again, err)
	}
	s.Close()
	s, err = Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.BindScope(ctx, sc)
	again, err = s.ClaimConflictNotification(ctx, c.ID)
	if err != nil || again {
		t.Fatal(again, err)
	}
}
func TestNotificationRequiresExistingConflict(t *testing.T) {
	s, _, _ := openTest(t)
	if _, err := s.ClaimConflictNotification(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
