package syncer

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type foregroundKey struct{}

// cliCall is one lark-cli process: it waits for the account-wide credential lock,
// then holds it while it runs.
type cliCall struct {
	op                   string
	foreground           bool
	requestedAt, startAt time.Time
	finishedAt           time.Time
	token                string
}

// accountLockSource models the production transport. Every cloud request is two
// processes (auth status --verify, then the docs call) and each one takes the
// exclusive per-account credential lock, which is acquired by polling TryLock
// every 5ms and is therefore not fair. Background and foreground requests share it.
type accountLockSource struct {
	Source
	docs    map[string]*fixtureCloud
	latency atomic.Int64 // nanoseconds per process
	lock    atomic.Bool
	mu      sync.Mutex
	calls   []cliCall
}

func (s *accountLockSource) cli(ctx context.Context, token, op string) error {
	call := cliCall{op: op, token: token, foreground: ctx.Value(foregroundKey{}) != nil, requestedAt: time.Now()}
	for !s.lock.CompareAndSwap(false, true) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	call.startAt = time.Now()
	time.Sleep(time.Duration(s.latency.Load()))
	call.finishedAt = time.Now()
	s.lock.Store(false)
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	return nil
}
func (s *accountLockSource) FetchDoc(ctx context.Context, token, revision string) (cloud.Document, error) {
	for _, op := range []string{"verify", "fetch"} {
		if err := s.cli(ctx, token, op); err != nil {
			return cloud.Document{}, err
		}
	}
	return s.docs[token].FetchDoc(ctx, token, revision)
}
func (s *accountLockSource) ApplyDoc(ctx context.Context, token, revision string, ops []model.Operation) (cloud.ApplyResult, error) {
	for _, op := range []string{"verify", "apply"} {
		if err := s.cli(ctx, token, op); err != nil {
			return cloud.ApplyResult{}, err
		}
	}
	return s.docs[token].ApplyDoc(ctx, token, revision, ops)
}
func (s *accountLockSource) snapshot() []cliCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cliCall(nil), s.calls...)
}

type conflictPair struct {
	s         *store.Store
	e         *Engine
	source    *accountLockSource
	entries   [2]model.Entry
	conflicts [2]model.Conflict
}

// newConflictPair builds two independent Feishu documents with overlapping edits,
// so the engine holds two conflicts exactly like the reported batch.
func newConflictPair(t *testing.T) *conflictPair {
	t.Helper()
	s, e, a, entryA := setup(t, model.KindDocx)
	b := &fixtureCloud{directory: a.directory, xml: a.xml, revision: 1, file: []byte("old"), grants: true}
	routed := &accountLockSource{Source: a, docs: map[string]*fixtureCloud{"resource": a, "resource-b": b}}
	e.source = routed
	entryB, err := s.ObserveEntry(bg, store.Observation{Remote: "other.md", Kind: model.KindDocx, Token: "resource-b"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := b.FetchDoc(bg, entryB.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	sn, _, err := e.saveDoc(bg, doc)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.OfferRemote(bg, entryB.ID, entryB.Generation, sn, sn.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitPublication(bg, candidate); err != nil {
		t.Fatal(err)
	}
	p := &conflictPair{s: s, e: e, source: routed}
	for i, item := range []struct {
		entry model.Entry
		doc   *fixtureCloud
	}{{entryA, a}, {entryB, b}} {
		en, err := s.Entry(bg, item.entry.ID)
		if err != nil {
			t.Fatal(err)
		}
		en = stageContent(t, s, en, "alpha local\n\nbeta two\n")
		item.doc.xml = strings.Replace(item.doc.xml, "alpha one", "alpha remote", 1)
		item.doc.revision++
		if err = e.SyncEntry(bg, en.ID); err == nil {
			t.Fatal("overlapping edits were accepted")
		}
		p.entries[i] = en
	}
	list, err := s.Conflicts(bg)
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	for _, c := range list {
		for i := range p.entries {
			if c.EntryID == p.entries[i].ID {
				p.conflicts[i] = c
			}
		}
	}
	if p.conflicts[0].ID == "" || p.conflicts[1].ID == "" {
		t.Fatal(list)
	}
	return p
}

func (p *conflictPair) request(i int, text string) model.ResolveRequest {
	c := p.conflicts[i]
	return model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte(text)}
}

// batchStall accepts file A, then file B, while a background loop ticks like
// Engine.Run does. It returns how many background processes asked for the account
// lock while B was being accepted, and B's wall time.
func batchStall(t *testing.T, grace time.Duration) (competing int, duration time.Duration, p *conflictPair) {
	t.Helper()
	p = newConflictPair(t)
	p.e.opt.WriteBack = 60 * time.Millisecond
	p.e.opt.ForegroundGrace = grace
	p.source.latency.Store(int64(120 * time.Millisecond))
	ctx, cancel := context.WithCancel(bg)
	var loops sync.WaitGroup
	loops.Add(1)
	go func() {
		defer loops.Done()
		for ctx.Err() == nil {
			_ = p.e.cycle(ctx)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	defer func() { cancel(); loops.Wait() }()
	fg := context.WithValue(ctx, foregroundKey{}, true)
	if _, _, err := p.e.Resolve(fg, p.request(0, "alpha final A\n\nbeta two\n")); err != nil {
		t.Fatal("accept A:", err)
	}
	started := time.Now()
	if _, _, err := p.e.Resolve(fg, p.request(1, "alpha final B\n\nbeta two\n")); err != nil {
		t.Fatal("accept B:", err)
	}
	finished := time.Now()
	for _, call := range p.source.snapshot() {
		if !call.foreground && !call.requestedAt.Before(started) && call.requestedAt.Before(finished) {
			competing++
		}
	}
	return competing, finished.Sub(started), p
}

// Reproduces the report: with no foreground priority, file A's post-accept
// write-back becomes due while file B is still verifying, and its lark-cli
// processes queue on the same account lock as B's reads.
func TestBatchSecondAcceptCompetesWithFirstFilesWriteBackWithoutForegroundGate(t *testing.T) {
	competing, took, _ := batchStall(t, -1)
	t.Logf("legacy: accepting B took %s, %d background lock requests during it", took, competing)
	if competing == 0 {
		// Documents the legacy race only; scheduling under parallel test load may not
		// reproduce it. The regression guard is TestBatchSecondAcceptIsNotDelayedByFirstFilesWriteBack.
		t.Skip("timing-dependent legacy reproduction did not race in this run")
	}
}

// The fix: background cycles yield while a batch is being accepted, then run the
// deferred work; nothing is lost.
func TestBatchSecondAcceptIsNotDelayedByFirstFilesWriteBack(t *testing.T) {
	competing, took, p := batchStall(t, 150*time.Millisecond)
	t.Logf("gated: accepting B took %s, %d background lock requests during it", took, competing)
	if competing != 0 {
		t.Fatalf("%d background process(es) took the account lock while the batch was being accepted", competing)
	}
	// A deferred upload still happens once the foreground goes quiet.
	deadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(bg, deadline)
	defer cancel()
	for {
		_ = p.e.cycle(ctx)
		a, errA := p.s.Entry(bg, p.entries[0].ID)
		b, errB := p.s.Entry(bg, p.entries[1].ID)
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if a.Status == model.StatusSynced && b.Status == model.StatusSynced {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("deferred write-back never ran: A=%s B=%s", a.Status, b.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestForegroundGateIsBoundedAndNeverBlocksForeground(t *testing.T) {
	_, e, _, _ := setup(t, model.KindDocx)
	now := time.Unix(1000, 0)
	e.opt.Now = func() time.Time { return now }
	e.opt.ForegroundGrace = 2 * time.Second
	if e.backgroundShouldYield() {
		t.Fatal("idle engine must not yield")
	}
	done := e.beginForeground()
	if !e.backgroundShouldYield() {
		t.Fatal("background must yield while a foreground request runs")
	}
	done()
	done() // release is idempotent
	if !e.backgroundShouldYield() {
		t.Fatal("grace window covers the gap before the next batch item")
	}
	now = now.Add(3 * time.Second)
	if e.backgroundShouldYield() {
		t.Fatal("background must resume after the grace window")
	}
	// A foreground request that hangs cannot starve durable jobs forever.
	hung := e.beginForeground()
	defer hung()
	if !e.backgroundShouldYield() {
		t.Fatal("expected yield at start of deferral")
	}
	now = now.Add(maxBackgroundDeferral + time.Second)
	if e.backgroundShouldYield() {
		t.Fatal("deferral must be capped")
	}
	// Disabled gate is the legacy behaviour.
	e.opt.ForegroundGrace = -1
	if e.backgroundShouldYield() {
		t.Fatal("negative grace disables the gate")
	}
}

// Safety rechecks are unchanged: a cloud edit between review and acceptance keeps
// the draft and reports a version change instead of staging the merge.
func TestBatchAcceptRechecksCloudAndKeepsDraftWhenChanged(t *testing.T) {
	p := newConflictPair(t)
	if err := p.s.SaveDraft(bg, p.conflicts[1].ID, strings.NewReader("alpha final B\n\nbeta two\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.e.Resolve(bg, p.request(0, "alpha final A\n\nbeta two\n")); err != nil {
		t.Fatal(err)
	}
	b := p.source.docs["resource-b"]
	b.xml = strings.Replace(b.xml, "alpha remote", "alpha remote edited again", 1)
	b.revision++
	_, _, err := p.e.Resolve(bg, p.request(1, "alpha final B\n\nbeta two\n"))
	if err == nil || !strings.Contains(err.Error(), model.ErrVersionChanged.Error()) {
		t.Fatalf("want version changed, got %v", err)
	}
	list, err := p.s.Conflicts(bg)
	if err != nil || len(list) != 1 || list[0].ID != p.conflicts[1].ID || list[0].Resolved {
		t.Fatalf("B must stay open for review: %+v %v", list, err)
	}
	if got := blobText(t, p.s, list[0].Draft); got != "alpha final B\n\nbeta two\n" {
		t.Fatalf("draft lost: %q", got)
	}
}
