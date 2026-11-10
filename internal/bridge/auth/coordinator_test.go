package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestEnrollmentUnknownAppIDBlocksOtherManagedCalls(t *testing.T) {
	root := privateTemp(t)
	a, err := newCoordinatorAt(root)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newCoordinatorAt(root)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.WithEnrollmentLock(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	called := false
	err = b.WithCredentialLocks(ctx, "app", "user", func() error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("enrollment lock bypassed: %v %v", err, called)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := b.WithCredentialLocks(context.Background(), "app", "user", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestSameAppUsersAndBrandShareCredentialDomain(t *testing.T) {
	root := privateTemp(t)
	a, _ := newCoordinatorAt(root)
	b, _ := newCoordinatorAt(root)
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.WithCredentialLocks(context.Background(), "app", "user1", func() error { close(held); <-release; return nil })
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.WithCredentialLocks(ctx, "app", "user2", func() error { t.Error("different user escaped AppID lock"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := b.WithCredentialLocks(context.Background(), "different-app", "user2", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestGlobalAuthDomainIndependentOfStateDir(t *testing.T) {
	got, err := DefaultDomain()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || filepath.Base(got) != "DriveBridge" {
		t.Fatal(got)
	}
}
