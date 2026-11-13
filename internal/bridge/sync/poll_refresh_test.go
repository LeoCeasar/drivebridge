package syncer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// This source holds one real RefreshEntry at its body observation boundary,
// without holding another entry's upload or readback calls.
type blockedActiveSource struct {
	Source
	token                                     string
	started, cancelled, allowReturn, returned chan struct{}
	once                                      sync.Once
}

func (s *blockedActiveSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	if token != s.token {
		return s.Source.ObserveFile(ctx, token)
	}
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	close(s.cancelled)
	<-s.allowReturn
	close(s.returned)
	return ObservedFile{}, ctx.Err()
}

func TestRunBlockedActiveRefreshAllowsOtherUploadAndDrainsOnCancel(t *testing.T) {
	s, engine, source, slow := setup(t, model.KindFile)
	target, err := s.ObserveEntry(bg, store.Observation{Remote: "other.bin", Kind: model.KindFile, Token: "upload-resource"})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := engine.observeFile(bg, target.Token)
	if err != nil {
		t.Fatal(err)
	}
	target, err = s.AdoptFileSnapshot(bg, target.ID, target.Generation, sn.ID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockedActiveSource{Source: source, token: slow.Token, started: make(chan struct{}), cancelled: make(chan struct{}), allowReturn: make(chan struct{}), returned: make(chan struct{})}
	engine.source = blocked
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	engine.opt.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	engine.opt.PollInterval = 10 * time.Millisecond
	engine.opt.DirectoryInterval = time.Hour
	engine.deps.ActiveEntries = func() []model.EntryID { return []model.EntryID{slow.ID} }
	published := make(chan struct{}, 1)
	engine.publisher = PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := s.CommitPublication(ctx, c)
		if err == nil && c.EntryID == target.ID {
			published <- struct{}{}
		}
		return err
	})
	runCtx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	runErr := make(chan error, 1)
	go func() { defer close(done); runErr <- engine.Run(runCtx) }()
	var release sync.Once
	defer func() {
		cancel()
		release.Do(func() { close(blocked.allowReturn) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run leaked after cleanup")
		}
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("active refresh never started")
	}
	stageContent(t, s, target, "upload while another refresh is blocked")
	clock.Add(int64(5 * time.Minute))
	select {
	case <-published:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("slow active refresh blocked another entry's pending upload")
	}
	current, err := s.Entry(bg, target.ID)
	if err != nil || current.Status != model.StatusSynced || blobText(t, s, current.Visible) != "upload while another refresh is blocked" {
		t.Fatalf("upload did not commit: %+v %v", current, err)
	}
	cancel()
	select {
	case <-blocked.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not cancel background refresh")
	}
	select {
	case <-done:
		t.Fatal("Run returned before background refresh drained")
	case <-time.After(25 * time.Millisecond):
	}
	release.Do(func() { close(blocked.allowReturn) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish after refresh drained")
	}
	if err = <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancellation returned %v", err)
	}
	select {
	case <-blocked.returned:
	default:
		t.Fatal("Run outlived its active refresh")
	}
	if err = s.Validate(bg); err != nil {
		t.Fatal(err)
	}
}

type orderedActiveSource struct {
	Source
	requests chan string
	proceed  chan struct{}
}

func (s *orderedActiveSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	select {
	case s.requests <- token:
	case <-ctx.Done():
		return ObservedFile{}, ctx.Err()
	}
	select {
	case <-s.proceed:
		return s.Source.ObserveFile(ctx, token)
	case <-ctx.Done():
		return ObservedFile{}, ctx.Err()
	}
}
func TestRunActiveRefreshIsBoundedAndReachesSnapshotTail(t *testing.T) {
	s, engine, source, first := setup(t, model.KindFile)
	entries := []model.Entry{first}
	for _, name := range []string{"middle.bin", "tail.bin"} {
		en, err := s.ObserveEntry(bg, store.Observation{Remote: name, Kind: model.KindFile, Token: name})
		if err != nil {
			t.Fatal(err)
		}
		sn, err := engine.observeFile(bg, en.Token)
		if err != nil {
			t.Fatal(err)
		}
		en, err = s.AdoptFileSnapshot(bg, en.ID, en.Generation, sn.ID)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, en)
	}
	ordered := &orderedActiveSource{Source: source, requests: make(chan string, 10), proceed: make(chan struct{})}
	engine.source = ordered
	engine.opt.PollInterval = time.Millisecond
	engine.opt.DirectoryInterval = time.Hour
	engine.deps.ActiveEntries = func() []model.EntryID {
		ids := make([]model.EntryID, len(entries))
		for i, en := range entries {
			ids[i] = en.ID
		}
		return ids
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Run did not drain refresh")
		}
	}()
	for _, en := range entries {
		select {
		case token := <-ordered.requests:
			if token != en.Token {
				t.Fatalf("refresh restarted before snapshot tail: got %q want %q", token, en.Token)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("snapshot tail starved")
		}
		// Several polling ticks elapse while this fetch is held. They must neither
		// create more workers nor restart scanning from the first active entry.
		select {
		case token := <-ordered.requests:
			t.Fatalf("unbounded concurrent refresh for %q", token)
		case <-time.After(10 * time.Millisecond):
		}
		ordered.proceed <- struct{}{}
	}
}
