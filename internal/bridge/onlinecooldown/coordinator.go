// Package onlinecooldown coordinates service waits in an OS user's private domain.
// Its fixed data-service bucket is independent of configuration and profile IDs.
package onlinecooldown

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/internal/bridge/platform"
)

// Errors deliberately exclude paths, identities, references, and underlying OS text.
var (
	ErrInvalid = errors.New("invalid online cooldown configuration or delay")
	ErrState   = errors.New("online cooldown state unavailable")
)

// WaitError reports the effective lower bound for the next request. It includes
// the process-local monotonic wait, which can exceed the shared UTC lower bound.
type WaitError struct{ retryAt time.Time }

func (e *WaitError) Error() string      { return "online service cooldown active" }
func (e *WaitError) RetryAt() time.Time { return e.retryAt }

// Coordinator must be constructed with the verified canonical binding and the
// shared OS-user domain, never a profile-specific configuration directory.
// The auth reference is opaque binding metadata, not a filesystem path.
type Coordinator struct {
	root, statePath, lockPath string
	identityHash, authRef     string
	now                       func() time.Time
	mu                        sync.Mutex
	eventID                   string
	revision                  uint64
	waitStarted               time.Time
	waitDuration              time.Duration
}

const maxStateBytes = 64 << 10
const maxDuration = time.Duration(1<<63 - 1)

type state struct {
	SchemaVersion        int    `json:"schema_version"`
	IdentityHash         string `json:"identity_hash"`
	AuthRef              string `json:"auth_ref"`
	EventID              string `json:"event_id"`
	Revision             uint64 `json:"revision"`
	OriginalDelaySeconds int64  `json:"original_delay_seconds"`
	// Retain subsecond precision without converting a large Duration to float64.
	OriginalDelayNanos int64 `json:"original_delay_nanoseconds,omitempty"`
	// Existing live coordinators need only the newest actual server duration;
	// fresh coordinators conservatively rewait OriginalDelay instead.
	EventDelaySeconds int64     `json:"event_delay_seconds,omitempty"`
	EventDelayNanos   int64     `json:"event_delay_nanoseconds,omitempty"`
	NotBeforeUTC      time.Time `json:"not_before_utc"`
	ObservedAtUTC     time.Time `json:"observed_at_utc"`
	Cause             string    `json:"cause"`
	FulfilledEventID  string    `json:"fulfilled_event_id,omitempty"`
	FulfilledRevision uint64    `json:"fulfilled_revision,omitempty"`
}

// New prepares private storage. It does not read or change a cooldown event.
// now supplies UTC wall-clock readings; elapsed waits always use time.Now's
// monotonic clock independently so wall-clock corrections cannot release them.
func New(root, identityHash, authRef string, now func() time.Time) (*Coordinator, error) {
	if !filepath.IsAbs(root) || strings.ContainsRune(root, 0) || filepath.Clean(root) == filepath.VolumeName(root)+string(filepath.Separator) || !canonicalHex(identityHash, 64) || authRef == "" || len(authRef) > 4096 {
		return nil, ErrInvalid
	}
	root = filepath.Clean(root)
	if platform.EnsurePrivateDir(root) != nil {
		return nil, ErrState
	}
	dir := filepath.Join(root, "onlinecooldown", "data")
	if platform.EnsurePrivateDir(dir) != nil {
		return nil, ErrState
	}
	if now == nil {
		now = time.Now
	}
	return &Coordinator{root: dir, statePath: filepath.Join(dir, identityHash+".json"), lockPath: filepath.Join(dir, identityHash+".lock"), identityHash: identityHash, authRef: authRef, now: now}, nil
}

func canonicalHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Check returns immediately after a short disk lock, with no service-time sleep.
func (c *Coordinator) Check(ctx context.Context) error {
	return c.locked(ctx, func() error {
		s, err := c.read()
		if err != nil {
			return err
		}
		if s == nil {
			if c.eventID != "" {
				return ErrState
			}
			return nil
		}
		if err = c.observe(s); err != nil {
			return err
		}
		if s.fulfilled() {
			return nil
		}
		wall := c.now().UTC()
		retryAt := s.NotBeforeUTC
		remaining := c.remaining()
		if remaining > 0 {
			local := wall.Add(remaining)
			if local.After(retryAt) {
				retryAt = local
			}
		}
		if remaining > 0 || wall.Before(s.NotBeforeUTC) {
			return &WaitError{retryAt: retryAt}
		}
		// Only a live process that has satisfied BOTH bounds may acknowledge
		// this exact event/revision. Fresh short-lived clients then consume the
		// evidence instead of replaying the original duration forever.
		s.FulfilledEventID = s.EventID
		s.FulfilledRevision = s.Revision
		return c.write(s)
	})
}

// Record records only a real service wait. Calls with zero delay leave existing
// state untouched. A shorter overlapping reply cannot replace its dominant event.
func (c *Coordinator) Record(ctx context.Context, delay time.Duration) error {
	if delay < 0 {
		return ErrInvalid
	}
	if c == nil || ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay == 0 {
		return nil
	}
	return c.locked(ctx, func() error {
		current, err := c.read()
		if err != nil {
			return err
		}
		if current == nil && c.eventID != "" {
			return ErrState
		}
		if current != nil {
			if err = c.observe(current); err != nil {
				return err
			}
		}
		wall := c.now().UTC()
		deadline := wall.Add(delay)
		if wall.IsZero() || wall.Year() < 1 || deadline.Year() > 9999 {
			return ErrInvalid
		}
		revision := uint64(1)
		duration := delay
		eventID := ""
		if current != nil {
			if current.Revision == ^uint64(0) {
				return ErrState
			}
			revision = current.Revision + 1
			remaining := c.remaining()
			active := !current.fulfilled() && (remaining > 0 || wall.Before(current.NotBeforeUTC))
			if active {
				if current.NotBeforeUTC.After(deadline) {
					deadline = current.NotBeforeUTC
				}
				if prior := current.duration(); prior >= duration {
					duration, eventID = prior, current.EventID
				}
			}
		}
		if eventID == "" {
			var id [16]byte
			if _, err = rand.Read(id[:]); err != nil {
				return ErrState
			}
			eventID = hex.EncodeToString(id[:])
		}
		// Even dominated real server evidence advances the revision: otherwise
		// an older worker could acknowledge before this actual wait completes
		// after a wall-clock correction. Manual zero-delay calls never get here.
		next := &state{SchemaVersion: 1, IdentityHash: c.identityHash, AuthRef: c.authRef, EventID: eventID, Revision: revision, OriginalDelaySeconds: int64(duration / time.Second), OriginalDelayNanos: int64(duration % time.Second), EventDelaySeconds: int64(delay / time.Second), EventDelayNanos: int64(delay % time.Second), NotBeforeUTC: deadline, ObservedAtUTC: wall, Cause: "rate_limited"}
		if err = c.write(next); err != nil {
			return err
		}
		return c.observe(next)
	})
}

// All event comparisons and replacements occur under the same cross-process
// lock, which provides the revision CAS. Local state uses the same order and is
// only held for file operations; lock acquisition never holds the local mutex.
func (c *Coordinator) locked(ctx context.Context, fn func() error) error {
	if c == nil || ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if platform.CheckNoSymlinks(c.root) != nil || platform.CheckNoSymlinks(c.lockPath) != nil {
		return ErrState
	}
	if err := prepareLock(c.lockPath); err != nil {
		return err
	}
	lock := flock.New(c.lockPath)
	ok, err := lock.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil || !ok {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrState
	}
	defer lock.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if platform.CheckNoSymlinks(c.lockPath) != nil || platform.CheckNoSymlinks(c.statePath) != nil {
		return ErrState
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return fn()
}

func prepareLock(path string) error {
	// Refuse directories and other special files before an open that could block.
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return ErrState
	}
	if err == nil && !info.Mode().IsRegular() {
		return ErrState
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return ErrState
	}
	actual, statErr := f.Stat()
	current, lstatErr := os.Lstat(path)
	closeErr := f.Close()
	if statErr != nil || lstatErr != nil || closeErr != nil || !actual.Mode().IsRegular() || !os.SameFile(actual, current) || platform.CheckNoSymlinks(path) != nil || platform.SecurePrivate(path, false) != nil {
		return ErrState
	}
	return nil
}

func (c *Coordinator) read() (*state, error) {
	if platform.CheckNoSymlinks(c.statePath) != nil {
		return nil, ErrState
	}
	info, err := os.Lstat(c.statePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxStateBytes {
		return nil, ErrState
	}
	f, err := os.Open(c.statePath)
	if err != nil {
		return nil, ErrState
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) || platform.SecurePrivate(c.statePath, false) != nil {
		return nil, ErrState
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil || len(raw) > maxStateBytes {
		return nil, ErrState
	}
	var s state
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&s) != nil || decoder.Decode(new(any)) != io.EOF || !c.valid(&s) {
		return nil, ErrState
	}
	return &s, nil
}

func (c *Coordinator) valid(s *state) bool {
	if s.SchemaVersion != 1 || s.IdentityHash != c.identityHash || s.AuthRef != c.authRef || !canonicalHex(s.EventID, 32) || s.Revision == 0 || s.Cause != "rate_limited" || !validDuration(s.OriginalDelaySeconds, s.OriginalDelayNanos) {
		return false
	}
	if (s.EventDelaySeconds != 0 || s.EventDelayNanos != 0) && (!validDuration(s.EventDelaySeconds, s.EventDelayNanos) || s.eventDuration() > s.duration()) {
		return false
	}
	if (s.FulfilledEventID != "" || s.FulfilledRevision != 0) && !s.fulfilled() {
		return false
	}
	_, observedOffset := s.ObservedAtUTC.Zone()
	_, dueOffset := s.NotBeforeUTC.Zone()
	return !s.ObservedAtUTC.IsZero() && !s.NotBeforeUTC.IsZero() && s.ObservedAtUTC.Year() >= 1 && s.NotBeforeUTC.Year() <= 9999 && observedOffset == 0 && dueOffset == 0 && s.NotBeforeUTC.After(s.ObservedAtUTC)
}

func validDuration(seconds, nanos int64) bool {
	if seconds < 0 || seconds > int64(maxDuration/time.Second) || nanos < 0 || nanos >= int64(time.Second) {
		return false
	}
	base := time.Duration(seconds) * time.Second
	return nanos <= int64(maxDuration-base) && base+time.Duration(nanos) > 0
}

func (s *state) duration() time.Duration {
	return time.Duration(s.OriginalDelaySeconds)*time.Second + time.Duration(s.OriginalDelayNanos)
}

func (s *state) eventDuration() time.Duration {
	if s.EventDelaySeconds == 0 && s.EventDelayNanos == 0 {
		return s.duration() // Compatibility with records written before this field.
	}
	return time.Duration(s.EventDelaySeconds)*time.Second + time.Duration(s.EventDelayNanos)
}

func (s *state) fulfilled() bool {
	return s.FulfilledEventID == s.EventID && s.FulfilledRevision == s.Revision
}

func (c *Coordinator) observe(s *state) error {
	if s.Revision < c.revision || s.Revision == c.revision && c.eventID != "" && s.EventID != c.eventID {
		return ErrState
	}
	if s.fulfilled() {
		c.waitDuration = 0
	} else if c.eventID != s.EventID || c.revision != s.Revision {
		// Never shorten a still-active process-local bound on a new shared event.
		duration := s.eventDuration()
		if c.eventID == "" || s.Revision-c.revision > 1 {
			// A skipped revision may have contained a longer actual wait than
			// the latest reply. Its elapsed time cannot be established locally.
			duration = s.duration()
		}
		if duration > c.remaining() {
			c.waitStarted = time.Now()
			c.waitDuration = duration
		}
	}
	c.eventID = s.EventID
	c.revision = s.Revision
	return nil
}

func (c *Coordinator) remaining() time.Duration {
	if c.waitDuration == 0 {
		return 0
	}
	// Subtract elapsed monotonic time instead of adding a deadline: adding a
	// near-MaxDuration to time.Now can overflow its monotonic representation
	// and silently fall back to the system wall clock.
	elapsed := time.Since(c.waitStarted)
	if elapsed <= 0 {
		return c.waitDuration
	}
	if elapsed >= c.waitDuration {
		return 0
	}
	return c.waitDuration - elapsed
}

func (c *Coordinator) write(s *state) error {
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > maxStateBytes {
		return ErrState
	}
	if platform.CheckNoSymlinks(c.statePath) != nil {
		return ErrState
	}
	f, err := os.CreateTemp(c.root, ".cooldown-")
	if err != nil {
		return ErrState
	}
	defer os.Remove(f.Name())
	if err = platform.SecurePrivate(f.Name(), false); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = platform.SyncFile(f)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrState
	}
	if platform.CheckNoSymlinks(c.statePath) != nil || platform.ReplaceFile(f.Name(), c.statePath) != nil || platform.SyncDir(c.root) != nil {
		return ErrState
	}
	return nil
}
