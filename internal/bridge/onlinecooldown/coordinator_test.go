package onlinecooldown

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

const testIdentity = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const otherIdentity = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testRef = "opaque/canonical-binding"

func privateRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
func coordinator(t *testing.T, root, identity string, now func() time.Time) *Coordinator {
	t.Helper()
	c, err := New(root, identity, testRef, now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func waitUntil(t *testing.T, c *Coordinator) time.Time {
	t.Helper()
	err := c.Check(context.Background())
	var wait *WaitError
	if !errors.As(err, &wait) {
		t.Fatalf("want WaitError, got %v", err)
	}
	if strings.Contains(err.Error(), testIdentity) || strings.Contains(err.Error(), testRef) || strings.Contains(err.Error(), c.root) {
		t.Fatalf("sensitive wait error: %v", err)
	}
	return wait.RetryAt()
}

// A profile-specific storage key or a missing disk read lets the second worker dispatch.
func TestCanonicalIdentitySharesPersistedWait(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	first := coordinator(t, root, testIdentity, clock)
	if err := first.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Record(context.Background(), 10*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	second := coordinator(t, root, testIdentity, clock)
	if got := waitUntil(t, second); got.Before(at.Add(10*24*time.Hour - time.Second)) {
		t.Fatalf("server delay truncated: %v", got)
	}
	if err := coordinator(t, root, otherIdentity, clock).Check(context.Background()); err != nil {
		t.Fatalf("other identity blocked: %v", err)
	}
	raw, err := os.ReadFile(first.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema_version", "identity_hash", "auth_ref", "event_id", "revision", "original_delay_seconds", "not_before_utc", "observed_at_utc", "cause"} {
		if _, ok := state[field]; !ok {
			t.Errorf("missing %s", field)
		}
	}
	if state["original_delay_seconds"] != float64(864000) {
		t.Fatalf("persisted wrong delay: %s", raw)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{root, first.root, first.statePath, first.lockPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(0600)
			if info.IsDir() {
				want = 0700
			}
			if info.Mode().Perm() != want {
				t.Errorf("private mode = %o, want %o", info.Mode().Perm(), want)
			}
		}
	}
}

// Replacing a dominant event with a shorter reply loses the restart lower bound.
func TestShorterReplyCannotReplaceLongEvent(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	long := coordinator(t, root, testIdentity, clock)
	if err := long.Record(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(long.statePath)
	if err != nil {
		t.Fatal(err)
	}
	short := coordinator(t, root, testIdentity, clock)
	if err := short.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(long.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var original, updated state
	if json.Unmarshal(before, &original) != nil || json.Unmarshal(after, &updated) != nil {
		t.Fatal("invalid persisted state")
	}
	if original.EventID != updated.EventID || original.OriginalDelaySeconds != updated.OriginalDelaySeconds || !original.NotBeforeUTC.Equal(updated.NotBeforeUTC) {
		t.Fatalf("short reply replaced dominant event\nbefore %s\nafter %s", before, after)
	}
	waitUntil(t, short)
	if err := short.Record(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, short)
}

// Using UTC alone releases a request after a forward clock correction; rereads must not write.
func TestRestartRewaitsWithoutRewritingAndForwardClockCannotRelease(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	first := coordinator(t, root, testIdentity, clock)
	if err := first.Record(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(first.statePath)
	at = at.Add(24 * time.Hour)
	waitUntil(t, first)
	restarted := coordinator(t, root, testIdentity, clock)
	got := waitUntil(t, restarted)
	if got.Before(at.Add(time.Hour - time.Second)) {
		t.Fatalf("restart lost original delay: %v", got)
	}
	waitUntil(t, restarted)
	after, _ := os.ReadFile(first.statePath)
	if string(before) != string(after) {
		t.Fatal("reading extended shared state")
	}
}

// Re-arming the same event on every read causes a permanent cooldown.
func TestRereadingSameEventDoesNotRestartAndExpiredEventAcceptsFreshDelay(t *testing.T) {
	root := privateRoot(t)
	at := time.Now().UTC()
	c := coordinator(t, root, testIdentity, func() time.Time { return at })
	if err := c.Record(context.Background(), 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		time.Sleep(10 * time.Millisecond)
		_ = c.Check(context.Background())
	}
	at = at.Add(time.Second)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("same event restarted: %v", err)
	}
	if err := c.Record(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, c)
}

// Ignoring new revisions or UTC later bounds lets an existing worker dispatch too early.
func TestExistingWorkerHonorsLaterSharedUpdate(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	first := coordinator(t, root, testIdentity, clock)
	second := coordinator(t, root, testIdentity, clock)
	if err := first.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, second)
	if err := first.Record(context.Background(), 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := waitUntil(t, second); got.Before(at.Add(2*time.Hour - time.Second)) {
		t.Fatalf("missed shared extension: %v", got)
	}
}

// Lock contention must return the original caller cancellation without waiting for service time.
func TestCanceledLockPreservesContext(t *testing.T) {
	root := privateRoot(t)
	c := coordinator(t, root, testIdentity, nil)
	lock := flock.New(c.lockPath)
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.Record(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost cancellation: %v", err)
	}
	ctx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := c.Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost pre-cancellation: %v", err)
	}
}

// Unsafe paths or malformed binding metadata must fail closed with no raw content in errors.
func TestRejectsInvalidConfigurationAndUnsafeState(t *testing.T) {
	root := privateRoot(t)
	for _, args := range [][3]string{{"relative", testIdentity, testRef}, {filepath.VolumeName(root) + string(filepath.Separator), testIdentity, testRef}, {root, "secret-invalid-identity", testRef}, {root, strings.ToUpper(testIdentity), testRef}, {root, strings.Repeat("g", 64), testRef}, {root, testIdentity, ""}} {
		_, err := New(args[0], args[1], args[2], nil)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid configuration got %v", err)
		}
		if err != nil && (strings.Contains(err.Error(), "secret-invalid") || strings.Contains(err.Error(), root)) {
			t.Errorf("unsafe error: %v", err)
		}
	}
	for _, mutation := range []string{"malformed", "identity", "authref", "oversize", "symlink", "lock-symlink", "directory", "overflow", "negative", "schema", "trailing-json"} {
		t.Run(mutation, func(t *testing.T) {
			c := coordinator(t, privateRoot(t), testIdentity, nil)
			if err := c.Record(context.Background(), time.Minute); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(c.statePath)
			switch mutation {
			case "malformed":
				raw = []byte("secret credentials invalid json")
			case "identity":
				raw = []byte(strings.ReplaceAll(string(raw), testIdentity, otherIdentity))
			case "authref":
				raw = []byte(strings.ReplaceAll(string(raw), testRef, "secret-other-reference"))
			case "oversize":
				raw = make([]byte, 100000)
			case "overflow", "negative", "schema":
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if mutation == "overflow" {
					fields["original_delay_seconds"] = json.RawMessage("9223372037")
				} else if mutation == "negative" {
					fields["original_delay_seconds"] = json.RawMessage("-1")
				} else {
					fields["schema_version"] = json.RawMessage("42")
				}
				var err error
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			case "trailing-json":
				raw = append(raw, []byte("{}")...)
			case "directory":
				_ = os.Remove(c.statePath)
				if err := os.Mkdir(c.statePath, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink", "lock-symlink":
				target := filepath.Join(privateRoot(t), "secret")
				if err := os.WriteFile(target, raw, 0600); err != nil {
					t.Fatal(err)
				}
				path := c.statePath
				if mutation == "lock-symlink" {
					path = c.lockPath
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			if mutation != "symlink" && mutation != "lock-symlink" && mutation != "directory" {
				if err := os.WriteFile(c.statePath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, err := range []error{c.Check(context.Background()), c.Record(context.Background(), time.Second)} {
				if !errors.Is(err, ErrState) {
					t.Fatalf("unsafe state did not fail closed: %v", err)
				}
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), c.root) {
					t.Fatalf("unsafe error: %v", err)
				}
			}
		})
	}
}

func TestNegativeDelayRejectedAndMaximumDurationPreserved(t *testing.T) {
	c := coordinator(t, privateRoot(t), testIdentity, nil)
	if err := c.Record(context.Background(), -time.Second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative wait: %v", err)
	}
	if err := c.Record(context.Background(), time.Duration(1<<63-1)); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, c)
	waitUntil(t, coordinator(t, filepath.Dir(filepath.Dir(c.root)), testIdentity, nil))
}

func TestConcurrentRecordsCannotLoseDominatingWait(t *testing.T) {
	root := privateRoot(t)
	var wg sync.WaitGroup
	for _, delay := range []time.Duration{time.Minute, time.Hour, time.Second, 24 * time.Hour, 2 * time.Hour} {
		c := coordinator(t, root, testIdentity, nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Record(context.Background(), delay); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c := coordinator(t, root, testIdentity, nil)
	if got := waitUntil(t, c); got.Before(time.Now().Add(24*time.Hour - time.Second)) {
		t.Fatalf("lost longest record: %v", got)
	}
}

func TestCooldownAcrossProcess(t *testing.T) {
	if root := os.Getenv("DRIVEBRIDGE_COOLDOWN_TEST_ROOT"); root != "" {
		c := coordinator(t, root, testIdentity, nil)
		waitUntil(t, c)
		return
	}
	root := privateRoot(t)
	c := coordinator(t, root, testIdentity, nil)
	if err := c.Record(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCooldownAcrossProcess$")
	cmd.Env = append(os.Environ(), "DRIVEBRIDGE_COOLDOWN_TEST_ROOT="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second process did not share wait: %v\n%s", err, output)
	}
}

// A backward wall correction must not hide a new server wait behind an old UTC bound.
func TestNewServerWaitSurvivesBackwardThenForwardClockCorrection(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	c := coordinator(t, root, testIdentity, func() time.Time { return at })
	if err := c.Record(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	at = at.Add(-24 * time.Hour)
	if err := c.Record(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	at = at.Add(48 * time.Hour)
	got := waitUntil(t, c)
	if got.Before(at.Add(time.Hour - time.Second)) {
		t.Fatalf("new wait lost after clock correction: %v", got)
	}
}

// A shorter extension restarts only the actual new server duration in a live process.
func TestShorterExtensionPreservesRemainingInsteadOfRestartingOriginal(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	c := coordinator(t, root, testIdentity, clock)
	observer := coordinator(t, root, testIdentity, clock)
	if err := c.Record(context.Background(), 60*time.Second); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, observer)
	c.waitStarted = time.Now().Add(-50 * time.Second)
	observer.waitStarted = time.Now().Add(-50 * time.Second)
	at = at.Add(50 * time.Second)
	if err := c.Record(context.Background(), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, live := range []*Coordinator{c, observer} {
		got := waitUntil(t, live)
		if got.Before(at.Add(14*time.Second)) || got.After(at.Add(16*time.Second)) {
			t.Fatalf("live process restarted dominant original wait: remaining %s", got.Sub(at))
		}
	}
	if got := waitUntil(t, coordinator(t, root, testIdentity, clock)); got.Before(at.Add(59 * time.Second)) {
		t.Fatalf("fresh process lost conservative original wait: %v", got)
	}
}

// Persisting completion after both bounds pass prevents every short-lived CLI from rewaiting.
func TestFulfilledWaitPersistsAcrossFreshInstances(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	c := coordinator(t, root, testIdentity, clock)
	observer := coordinator(t, root, testIdentity, clock)
	if err := c.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, observer)
	at = at.Add(70 * time.Second)
	waitUntil(t, c) // UTC alone cannot acknowledge completion.
	waitUntil(t, coordinator(t, root, testIdentity, clock))
	c.waitStarted = time.Now().Add(-70 * time.Second)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, next := range []*Coordinator{observer, coordinator(t, root, testIdentity, clock)} {
		if err := next.Check(context.Background()); err != nil {
			t.Fatalf("fulfilled wait replayed: %v", err)
		}
	}
	wrongRef, err := New(root, testIdentity, "profile-alias-with-wrong-binding", clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongRef.Check(context.Background()); !errors.Is(err, ErrState) {
		t.Fatalf("fulfilled marker bypassed binding: %v", err)
	}
}

// A new rate-limit event invalidates old completion and keeps its own duration.
func TestNewServerWaitInvalidatesFulfillment(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	c := coordinator(t, root, testIdentity, clock)
	if err := c.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	c.waitStarted = time.Now().Add(-70 * time.Second)
	at = at.Add(70 * time.Second)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Record(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, next := range []*Coordinator{c, coordinator(t, root, testIdentity, clock)} {
		got := waitUntil(t, next)
		if got.Before(at.Add(2*time.Second)) || got.After(at.Add(4*time.Second)) {
			t.Fatalf("new event inherited fulfilled duration: %s", got.Sub(at))
		}
	}
	c.waitStarted = time.Now().Add(-4 * time.Second)
	at = at.Add(4 * time.Second)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := coordinator(t, root, testIdentity, clock).Check(context.Background()); err != nil {
		t.Fatalf("new event completion not persisted: %v", err)
	}
}

// Evidence hidden by a backward wall correction must stop another live process acknowledging the old event.
func TestNewShorterEvidencePreventsPrematureFulfillment(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	original := coordinator(t, root, testIdentity, clock)
	if err := original.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	restarted := coordinator(t, root, testIdentity, clock)
	waitUntil(t, restarted)
	// The restarted process has a conservative minute, while the original wait is over.
	original.waitStarted = time.Now().Add(-70 * time.Second)
	if err := restarted.Record(context.Background(), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	at = at.Add(70 * time.Second)
	if got := waitUntil(t, original); got.Before(at.Add(14 * time.Second)) {
		t.Fatalf("new actual delay hidden by old bound: %v", got)
	}
}

// Missing an intermediate reply loses its actual duration, so use the conservative original bound.
func TestSkippedServerEventsKeepConservativeOriginalBound(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	writer := coordinator(t, root, testIdentity, clock)
	observer := coordinator(t, root, testIdentity, clock)
	if err := writer.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, observer)
	if err := writer.Record(context.Background(), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := writer.Record(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	observer.waitStarted = time.Now().Add(-70 * time.Second)
	at = at.Add(200 * time.Second)
	if got := waitUntil(t, observer); got.Before(at.Add(89 * time.Second)) {
		t.Fatalf("skipped reply lost its monotonic bound: %s", got.Sub(at))
	}
}

func TestMonotonicCompletionCannotAcknowledgeBeforeUTCBound(t *testing.T) {
	root := privateRoot(t)
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	c := coordinator(t, root, testIdentity, clock)
	if err := c.Record(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	c.waitStarted = time.Now().Add(-70 * time.Second)
	waitUntil(t, c)
	at = at.Add(70 * time.Second)
	waitUntil(t, coordinator(t, root, testIdentity, clock))
}
