package syncer

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestRunNextBoundaryAdmitsHealthyFileWhileOlderFileWaits(t *testing.T) {
	s, e, source, slow := setup(t, model.KindFile)
	healthy, err := s.ObserveEntry(bg, store.Observation{Remote: "later.bin", Kind: model.KindFile, Token: "later"})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := e.observeFile(bg, healthy.Token)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err = s.AdoptFileSnapshot(bg, healthy.ID, healthy.Generation, sn.ID)
	if err != nil {
		t.Fatal(err)
	}
	held := &heldPendingFile{Source: source, token: slow.Token, started: make(chan struct{}), release: make(chan struct{})}
	e.source = held
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	e.opt.Now = func() time.Time { return time.Unix(0, now.Load()) }
	initialized := make(chan struct{})
	var init sync.Once
	e.opt.PollInterval = 5 * time.Millisecond
	e.opt.DirectoryInterval = time.Hour
	e.deps.ActiveEntries = func() []model.EntryID { init.Do(func() { close(initialized) }); return nil }
	published := make(chan struct{}, 1)
	e.publisher = PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := s.CommitPublication(ctx, c)
		if err == nil && c.EntryID == healthy.ID {
			published <- struct{}{}
		}
		return err
	})
	stageContent(t, s, slow, "slow change")
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("dispatcher leaked")
		}
	}()
	select {
	case <-initialized:
	case <-time.After(time.Second):
		t.Fatal("Run not initialized")
	}
	now.Add(int64(5 * time.Minute))
	select {
	case <-held.started:
	case <-time.After(2 * time.Second):
		t.Fatal("old file not started")
	}
	stageContent(t, s, healthy, "new boundary change")
	now.Add(int64(5 * time.Minute))
	select {
	case <-published:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("older cycle prevented next boundary's healthy upload")
	}
}

func TestDispatcherReleasesThreeTimedOutSlotsForHealthyFile(t *testing.T) {
	s, e, source, first := setup(t, model.KindFile)
	var slow []model.Entry
	slow = append(slow, first)
	for i, token := range []string{"slow-two", "slow-three", "healthy-last"} {
		en, err := s.ObserveEntry(bg, store.Observation{Remote: token + ".bin", Kind: model.KindFile, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		sn, err := e.observeFile(bg, en.Token)
		if err != nil {
			t.Fatal(err)
		}
		en, err = s.AdoptFileSnapshot(bg, en.ID, en.Generation, sn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			slow = append(slow, en)
		}
	}
	healthy, err := s.EntryByRemote(bg, "healthy-last.bin")
	if err != nil {
		t.Fatal(err)
	}
	gate := &deadlineFileSource{Source: source, tokens: map[string]bool{}, started: make(chan string, 3)}
	for _, en := range slow {
		gate.tokens[en.Token] = true
		stageContent(t, s, en, "slow edit")
	}
	e.source = gate
	e.opt.TransferTimeout = 2 * time.Second
	d := newBackgroundDispatcher(bg, e)
	defer d.close()
	d.epoch = 1
	for _, en := range slow {
		current, _ := s.Entry(bg, en.ID)
		d.admit(en.ID, current.Generation)
	}
	d.pump()
	for range slow {
		select {
		case <-gate.started:
		case <-time.After(time.Second):
			t.Fatal("slow file not started")
		}
	}
	healthy = stageContent(t, s, healthy, "healthy latest")
	d.admit(healthy.ID, healthy.Generation)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case result := <-d.results:
			d.completed(result)
			d.pump()
		case <-deadline:
			t.Fatal("timed-out files retained all slots")
		}
		current, _ := s.Entry(bg, healthy.ID)
		if current.Status == model.StatusSynced {
			break
		}
	}
	for _, en := range slow {
		current, _ := s.Entry(bg, en.ID)
		if current.Status != model.StatusPaused {
			t.Fatalf("slow file not retained for retry: %s", current.Status)
		}
	}
}

type deadlineFileSource struct {
	Source
	tokens  map[string]bool
	started chan string
}

func (s *deadlineFileSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	if !s.tokens[token] {
		return s.Source.ObserveFile(ctx, token)
	}
	s.started <- token
	<-ctx.Done()
	return ObservedFile{}, ctx.Err()
}

func TestDocQuantumYieldsOnlyAfterVerifiedReadbackAndKeepsDesired(t *testing.T) {
	s, e, source, en := setup(t, model.KindDocx)
	en = stageContent(t, s, en, "alpha changed\n\nbeta changed\n")
	ctx := context.WithValue(bg, quantumKey{}, dispatchQuantum{operations: 1, duration: time.Hour})
	err := e.syncEntry(ctx, en.ID, nil, true)
	if !errors.Is(err, errDispatchYield) {
		t.Fatalf("did not yield: %v", err)
	}
	jobs, _ := s.Pending(bg)
	journal, _ := s.Journal(bg, jobs[0].ID)
	if len(journal.Operations) != 1 || len(journal.Results) != 1 || journal.Desired == "" {
		t.Fatalf("unverified yield %+v", journal)
	}
	desired := journal.Desired
	stageContent(t, s, en, "alpha changed\n\nbeta newer save\n")
	cutoff := en.Generation
	for i := 0; i < 8; i++ {
		err = e.syncEntry(ctx, en.ID, &cutoff, true)
		if !errors.Is(err, errDispatchYield) {
			break
		}
	}
	if err != nil && !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	if strings.Contains(source.xml, "newer save") {
		t.Fatal("later save entered old quantum")
	}
	journal, _ = s.Journal(bg, jobs[0].ID)
	if journal.Desired != desired {
		t.Fatal("resumed another intent")
	}
	current, _ := s.Entry(bg, en.ID)
	if !strings.Contains(blobText(t, s, current.Local), "newer save") {
		t.Fatal("newer local save lost")
	}
}

func TestInteractiveResolveDeadlinePreservesDraft(t *testing.T) {
	p := newConflictPair(t)
	p.e.opt.InteractiveTimeout = time.Nanosecond
	p.e.source = &deadlineDocSource{Source: p.source}
	_, _, err := p.e.Resolve(bg, p.request(0, "kept user draft\n"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline missing: %v", err)
	}
	conflicts, _ := p.s.Conflicts(bg)
	for _, c := range conflicts {
		if c.ID == p.conflicts[0].ID && blobText(t, p.s, c.Draft) != "kept user draft\n" {
			t.Fatal("draft lost on timeout")
		}
	}
}

type deadlineDocSource struct{ Source }

func (s *deadlineDocSource) FetchDoc(ctx context.Context, token, revision string) (cloud.Document, error) {
	<-ctx.Done()
	return cloud.Document{}, ctx.Err()
}

func TestDispatcherBoundaryNeverAdmitsSaveAfterCutoff(t *testing.T) {
	s, e, source, en := setup(t, model.KindFile)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	e.opt.Now = func() time.Time { return time.Unix(0, now.Load()) }
	d := newBackgroundDispatcher(bg, e)
	defer d.close()
	en = stageContent(t, s, en, "at boundary")
	var once sync.Once
	e.deps.ShouldSync = func(context.Context, model.EntryID) (bool, error) {
		once.Do(func() { stageContent(t, s, en, "after boundary") })
		return true, nil
	}
	now.Add(int64(5 * time.Minute))
	if err := d.tick(); err != nil {
		t.Fatal(err)
	}
	waitPass := func() {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case r := <-d.results:
				d.completed(r)
				if r.id == en.ID {
					return
				}
			case <-deadline:
				t.Fatal("file pass did not finish")
			}
		}
	}
	waitPass()
	if source.writes != 0 {
		t.Fatal("post-boundary generation uploaded early")
	}
	now.Add(int64(5 * time.Minute))
	if err := d.tick(); err != nil {
		t.Fatal(err)
	}
	waitPass()
	if string(source.file) != "after boundary" {
		t.Fatal("next boundary missed latest saved body")
	}
}

func TestRunManualRetryAcceptsWithoutWaitingForCloud(t *testing.T) {
	s, e, source, en := setup(t, model.KindFile)
	stageContent(t, s, en, "manual latest")
	held := &heldPendingFile{Source: source, token: en.Token, started: make(chan struct{}), release: make(chan struct{})}
	e.source = held
	initialized := make(chan struct{})
	var once sync.Once
	e.opt.PollInterval = 5 * time.Millisecond
	e.deps.ActiveEntries = func() []model.EntryID { once.Do(func() { close(initialized) }); return nil }
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Run leaked")
		}
	}()
	select {
	case <-initialized:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	request, stop := context.WithTimeout(bg, 100*time.Millisecond)
	defer stop()
	if err := e.Retry(request, en.ID); err != nil {
		t.Fatalf("retry waited on cloud instead of accepting: %v", err)
	}
	select {
	case <-held.started:
	case <-time.After(time.Second):
		t.Fatal("accepted work did not start")
	}
}

func TestRunMetadataRefreshDoesNotHoldFileAdmission(t *testing.T) {
	s, e, source, en := setup(t, model.KindFile)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	e.opt.Now = func() time.Time { return time.Unix(0, now.Load()) }
	started := make(chan struct{})
	var once sync.Once
	e.opt.DirectoryInterval = 5 * time.Millisecond
	e.opt.BackgroundTimeout = 2 * time.Second
	e.deps.RefreshDirectories = func(ctx context.Context) error { once.Do(func() { close(started) }); <-ctx.Done(); return ctx.Err() }
	published := make(chan struct{}, 1)
	e.publisher = PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := s.CommitPublication(ctx, c)
		if err == nil {
			published <- struct{}{}
		}
		return err
	})
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Run leaked")
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("metadata not started")
	}
	stageContent(t, s, en, "while metadata waits")
	now.Add(int64(5 * time.Minute))
	select {
	case <-published:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("metadata blocked file upload")
	}
	if string(source.file) != "while metadata waits" {
		t.Fatal("wrong body")
	}
}

func TestDispatcherPolicySkipDoesNotSpinOrLoseFutureAdmission(t *testing.T) {
	s, e, source, en := setup(t, model.KindFile)
	en = stageContent(t, s, en, "policy edit")
	allowed := false
	checks := 0
	e.deps.ShouldSync = func(context.Context, model.EntryID) (bool, error) { checks++; return allowed, nil }
	d := newBackgroundDispatcher(bg, e)
	defer d.close()
	d.epoch = 1
	d.admit(en.ID, en.Generation)
	d.pump()
	select {
	case r := <-d.results:
		d.completed(r)
	case <-time.After(time.Second):
		t.Fatal("skip did not return")
	}
	if len(d.queue) != 0 || d.active != 0 {
		t.Fatal("unchanged skipped job immediately requeued")
	}
	if checks != 1 || source.writes != 0 {
		t.Fatal("policy skip caused cloud work")
	}
	allowed = true
	d.epoch++
	d.admit(en.ID, en.Generation)
	d.pump()
	select {
	case r := <-d.results:
		d.completed(r)
	case <-time.After(2 * time.Second):
		t.Fatal("later admission did not run")
	}
	current, _ := s.Entry(bg, en.ID)
	if current.Status != model.StatusSynced {
		t.Fatalf("later admission lost: %s", current.Status)
	}
}

func TestRunRetryAdmissionDoesNotWaitForCloudIdentityCheck(t *testing.T) {
	s, e, _, en := setup(t, model.KindFile)
	stageContent(t, s, en, "identity check later")
	initialized := make(chan struct{})
	var once sync.Once
	e.opt.PollInterval = 5 * time.Millisecond
	e.deps.ActiveEntries = func() []model.EntryID { once.Do(func() { close(initialized) }); return nil }
	checking := make(chan struct{}, 1)
	e.deps.CheckIdentity = func(ctx context.Context) error {
		select {
		case checking <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Run leaked")
		}
	}()
	select {
	case <-initialized:
	case <-time.After(time.Second):
		t.Fatal("not initialized")
	}
	req, stop := context.WithTimeout(bg, 50*time.Millisecond)
	defer stop()
	if err := e.Retry(req, en.ID); err != nil {
		t.Fatalf("admission waited for cloud identity: %v", err)
	}
	select {
	case <-checking:
	case <-time.After(time.Second):
		t.Fatal("identity was not checked before executing background work")
	}
}
