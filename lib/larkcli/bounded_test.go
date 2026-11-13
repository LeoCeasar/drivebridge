package larkcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type boundedCoordinator struct {
	run func(context.Context, func() error) error
}

func (c boundedCoordinator) WithCredentialLocks(ctx context.Context, _, _ string, run func() error) error {
	return c.run(ctx, run)
}

func TestBusinessBudgetExpiresWhileCredentialGateOccupied(t *testing.T) {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	c := boundedCoordinator{run: func(ctx context.Context, run func() error) error {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
			return run()
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	r := boundedManaged(t, testExecutable(t, "echo"), c)
	started := time.Now()
	_, err := r.Run(context.Background(), Request{Timeout: 50 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrNotDispatched) || errors.Is(err, ErrUnknownResult) || time.Since(started) > time.Second {
		t.Fatalf("waiting request classification or budget: %v", err)
	}
	<-gate
	if _, err := r.Run(context.Background(), Request{}); err != nil {
		t.Fatalf("healthy request after occupied gate: %v", err)
	}
}

func TestBusinessStartFailureIsNotDispatchedAndRedacted(t *testing.T) {
	r := NewDirect(filepath.Join(t.TempDir(), "SECRET-missing-cli"))
	_, err := r.Run(context.Background(), Request{Args: []string{"SECRET-argument"}})
	if !errors.Is(err, ErrNotDispatched) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("start failure classification or redaction: %v", err)
	}
}

func TestTransferRequestsUseLongerFiniteBudget(t *testing.T) {
	for _, kind := range []string{"upload", "download", "media"} {
		t.Run(kind, func(t *testing.T) {
			gateErr := errors.New("stop at credential gate")
			c := boundedCoordinator{run: func(ctx context.Context, _ func() error) error {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) < 14*time.Minute || time.Until(deadline) > 15*time.Minute {
					t.Errorf("transfer has no separate finite budget")
				}
				return gateErr
			}}
			drive := NewDrive(boundedManaged(t, "must-not-start", c), t.TempDir())
			var err error
			switch kind {
			case "upload":
				_, err = drive.Upload(context.Background(), "folder", "file", "name", strings.NewReader("bytes"))
			case "download":
				_, err = drive.ObserveFile(context.Background(), "file")
			case "media":
				_, err = drive.OpenDocMedia(context.Background(), "media")
			}
			if !errors.Is(err, gateErr) || !errors.Is(err, ErrNotDispatched) || errors.Is(err, ErrUnknownResult) {
				t.Fatalf("transfer pre-start classification: %v", err)
			}
		})
	}
}

func TestMutationTimeoutRemainsUnknownAfterStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := NewDocx(NewDirect(testExecutable(t, "cancel"))).Apply(ctx, "doc", "1", DocMutation{Kind: "block_delete", BlockID: "block"})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnknownResult) || errors.Is(err, ErrNotDispatched) {
		t.Fatalf("started mutation no longer unknown: %v", err)
	}
}

func TestBusinessTimeoutReapsChildAndReleasesCredentialGate(t *testing.T) {
	gate := make(chan struct{}, 1)
	c := boundedCoordinator{run: func(ctx context.Context, run func() error) error {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
			return run()
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	r := boundedManaged(t, testExecutable(t, "cancel"), c)
	_, err := r.Run(context.Background(), Request{Timeout: 100 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotDispatched) {
		t.Fatalf("started child incorrectly classified: %v", err)
	}
	healthy := boundedManaged(t, testExecutable(t, "echo"), c)
	if _, err := healthy.Run(context.Background(), Request{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("credential gate retained after reap: %v", err)
	}
}

func TestBusinessEarlierParentDeadlineWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := NewDirect(testExecutable(t, "cancel")).Run(ctx, Request{Timeout: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("parent deadline ignored: %v", err)
	}
}

func TestBusinessInheritedPipesDoNotRetainGateOrAllowLateDescendantWrite(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late-write")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled := make(chan time.Time, 1)
	go func() {
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			if _, err := os.Stat(marker + ".ready"); err == nil {
				time.Sleep(50 * time.Millisecond) // let the parent exit with pipes inherited
				break
			}
			time.Sleep(time.Millisecond)
		}
		canceled <- time.Now()
		cancel()
	}()
	_, err := NewDirect(testExecutable(t, "descendant")).Run(ctx, Request{Args: []string{marker}})
	if time.Since(<-canceled) > 600*time.Millisecond {
		t.Errorf("inherited pipes exceeded request deadline: %v", err)
	}
	if _, statErr := os.Stat(marker + ".ready"); statErr != nil {
		t.Fatal("descendant fixture did not start")
	}
	if errors.Is(err, ErrNotDispatched) {
		t.Errorf("started child classified as undispatched: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("descendant survived the credential-lock boundary")
	}
}
func (c boundedCoordinator) WithEnrollmentLock(ctx context.Context, run func() error) error {
	return c.run(ctx, run)
}

func boundedManaged(t *testing.T, executable string, c Coordinator) *Runner {
	t.Helper()
	r, err := NewManaged(ManagedOptions{Executable: executable, AuthRef: "ref", AuthRoot: t.TempDir(), Identity: Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: c})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBusinessRequestHasDeadlineBeforeCredentialWait(t *testing.T) {
	gateErr := errors.New("gate occupied")
	c := boundedCoordinator{run: func(ctx context.Context, _ func() error) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Minute {
			t.Errorf("credential wait has no finite business budget")
		}
		return gateErr
	}}
	r := boundedManaged(t, "must-not-start", c)
	if _, err := r.Run(context.Background(), Request{}); !errors.Is(err, gateErr) {
		t.Fatal(err)
	}
}

func TestNoninteractiveLifecycleHasFiniteCredentialWait(t *testing.T) {
	c := boundedCoordinator{run: func(ctx context.Context, _ func() error) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Minute {
			t.Error("noninteractive metadata can wait indefinitely for credentials")
		}
		return context.DeadlineExceeded
	}}
	r := boundedManaged(t, "must-not-start", c)
	if _, err := r.VerifyIdentity(context.Background()); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("metadata pre-start result: %v", err)
	}
}
