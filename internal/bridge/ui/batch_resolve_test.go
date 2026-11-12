package ui

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
)

// batchBackend is a two-conflict backend that counts Feishu reads. A cloud read
// costs readCost, like the verify+fetch pair of lark-cli processes it models.
type batchBackend struct {
	*fakeBackend
	mu        sync.Mutex
	readCost  time.Duration
	cloudRead int
	files     map[model.ConflictID]*batchFile
	order     []model.ConflictID
}
type batchFile struct {
	input   ConflictInputs
	draft   []byte
	changed error // returned by the cloud recheck inside Resolve
}

func newBatchBackend(base *fakeBackend, names ...string) *batchBackend {
	b := &batchBackend{fakeBackend: base, files: map[model.ConflictID]*batchFile{}}
	for _, name := range names {
		id, entry := model.ConflictID(uuid.NewString()), model.EntryID(uuid.NewString())
		b.files[id] = &batchFile{input: ConflictInputs{Conflict: model.Conflict{ID: id, EntryID: entry, Generation: 4, RemoteRevision: "r1"}, Entry: model.Entry{ID: entry, Remote: name, Generation: 4}, Base: []byte("base\n"), Local: []byte("local\n"), Remote: []byte("remote\n"), Text: true}}
		b.order = append(b.order, id)
	}
	return b
}
func (b *batchBackend) cloudReadCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cloudRead
}
func (b *batchBackend) read() {
	b.mu.Lock()
	b.cloudRead++
	cost := b.readCost
	b.mu.Unlock()
	time.Sleep(cost)
}
func (b *batchBackend) local(id model.ConflictID) (ConflictInputs, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.files[id]
	if !ok {
		return ConflictInputs{}, model.ErrVersionChanged
	}
	v := f.input
	v.Draft = f.draft
	return v, nil
}

// Conflict is the refreshing read used when opening a conflict.
func (b *batchBackend) Conflict(_ context.Context, id model.ConflictID) (ConflictInputs, error) {
	b.read()
	return b.local(id)
}
func (b *batchBackend) SaveDraft(_ context.Context, id model.ConflictID, v []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.files[id]
	if !ok {
		return model.ErrVersionChanged
	}
	f.draft = append([]byte(nil), v...)
	return nil
}
func (b *batchBackend) Resolve(_ context.Context, r model.ResolveRequest) error {
	b.read() // the single authoritative cloud recheck
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.files[r.ConflictID].changed
}

// localReadBackend adds the cheap local version read the host prefers.
type localReadBackend struct{ *batchBackend }

func (b localReadBackend) ConflictForResolve(_ context.Context, id model.ConflictID) (ConflictInputs, error) {
	return b.local(id)
}

func startWith(t *testing.T, backend Backend) (*Host, *http.Client, string) {
	t.Helper()
	d := t.TempDir()
	h, err := Start(ctx, Options{Backend: backend, RuntimeDir: filepath.Join(d, "ui"), ConfigKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(ctx) })
	client, csrf := bootstrap(t, h)
	return h, client, csrf
}

func submitBatch(t *testing.T, h *Host, c *http.Client, csrf string, b *batchBackend) []*http.Response {
	t.Helper()
	var out []*http.Response
	for _, id := range b.order {
		r := model.ResolveRequest{ConflictID: id, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1", Content: []byte("merged " + string(id))}
		res := request(t, c, h, "POST", "/api/conflicts/"+string(id)+"/resolve", r, csrf, h.URL())
		res.Body.Close()
		out = append(out, res)
	}
	return out
}

// Accepting one file used to read the cloud twice: once to refresh the conflict
// before the check and once inside Resolve. Every read is two serialized lark-cli
// processes on one account lock, so a two-file batch queued eight of them.
func TestBatchAcceptReadsCloudOncePerFile(t *testing.T) {
	b := newBatchBackend(&fakeBackend{}, "A.md", "B.md")
	h, c, csrf := startWith(t, localReadBackend{b})
	for i, res := range submitBatch(t, h, c, csrf, b) {
		if res.StatusCode != 200 {
			t.Fatalf("file %d: %s", i, res.Status)
		}
		timing := res.Header.Get("Server-Timing")
		for _, stage := range []string{"draft;dur=", "check;dur=", "accept;dur="} {
			if !strings.Contains(timing, stage) {
				t.Fatalf("file %d: Server-Timing %q lacks %s", i, timing, stage)
			}
		}
	}
	if got := b.cloudReadCount(); got != 2 {
		t.Fatalf("cloud reads for a two-file batch: got %d, want 2 (one authoritative recheck per file)", got)
	}
}

// Hosts without the local reader keep the earlier, slower behaviour.
func TestBatchAcceptWithoutLocalReaderStillRefreshesFirst(t *testing.T) {
	b := newBatchBackend(&fakeBackend{}, "A.md", "B.md")
	h, c, csrf := startWith(t, b)
	for _, res := range submitBatch(t, h, c, csrf, b) {
		if res.StatusCode != 200 {
			t.Fatal(res.Status)
		}
	}
	// Without a local reader every version read refreshes from the cloud: the
	// pre-read, the post-draft reason recheck (deletion conflicts) and Resolve's
	// own recheck, i.e. three per file.
	if got := b.cloudReadCount(); got != 6 {
		t.Fatalf("legacy path cloud reads: got %d, want 6", got)
	}
}

// With the cheap read a file whose stored version moved is rejected before any
// cloud request, and the authoritative recheck still turns a cloud edit into a
// review request that keeps the saved draft.
func TestBatchAcceptKeepsSafetyChecksAndDraft(t *testing.T) {
	b := newBatchBackend(&fakeBackend{}, "moved.md", "edited.md")
	h, c, csrf := startWith(t, localReadBackend{b})
	moved, edited := b.order[0], b.order[1]
	b.files[moved].input.Conflict.Generation = 5
	b.files[moved].input.Entry.Generation = 5
	b.files[edited].changed = model.ErrVersionChanged
	responses := submitBatch(t, h, c, csrf, b)
	if responses[0].StatusCode != 409 || responses[1].StatusCode != 409 {
		t.Fatalf("both files need review: %s, %s", responses[0].Status, responses[1].Status)
	}
	if got := b.cloudReadCount(); got != 1 {
		t.Fatalf("a stale local version must not cost a cloud read: got %d reads, want 1", got)
	}
	for _, id := range b.order {
		if got := string(b.files[id].draft); got != "merged "+string(id) {
			t.Fatalf("draft not kept for %s: %q", id, got)
		}
	}
}

// Per-file latency is one cloud read, not two.
func TestBatchAcceptLatencyIsOneCloudReadPerFile(t *testing.T) {
	b := newBatchBackend(&fakeBackend{}, "A.md", "B.md")
	b.readCost = 150 * time.Millisecond
	h, c, csrf := startWith(t, localReadBackend{b})
	var durations []time.Duration
	for _, id := range b.order {
		start := time.Now()
		r := model.ResolveRequest{ConflictID: id, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1", Content: []byte("x")}
		res := request(t, c, h, "POST", "/api/conflicts/"+string(id)+"/resolve", r, csrf, h.URL())
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(res.Status)
		}
		durations = append(durations, time.Since(start))
	}
	for i, d := range durations {
		if d > 3*b.readCost {
			t.Fatalf("file %d took %s, want about one cloud read (%s)", i, d, b.readCost)
		}
	}
	t.Logf("per-file accept: %v", durations)
}
