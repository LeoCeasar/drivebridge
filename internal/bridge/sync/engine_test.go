package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

var bg = context.Background()

type fixtureCloud struct {
	readErr         error
	xml             string
	revision        int
	file            []byte
	writes, creates int
	applyHook       func()
	loseResponse    bool
	mismatch        bool
	drift           bool
	deny            bool
	grants          bool
	items           []larkcli.DriveItem
}

func (f *fixtureCloud) FetchDoc(_ context.Context, token, revision string) (cloud.Document, error) {
	if f.readErr != nil {
		return cloud.Document{}, f.readErr
	}
	if f.deny {
		return cloud.Document{}, &larkcli.Error{Code: "permission_denied"}
	}
	if f.drift {
		f.revision++
	}
	return cloud.Document{Token: token, Revision: strconv.Itoa(f.revision), XML: []byte(f.xml)}, nil
}
func (f *fixtureCloud) Capabilities() []cloud.CapabilityGrant {
	if !f.grants {
		return nil
	}
	var out []cloud.CapabilityGrant
	for _, kind := range []string{model.OperationBlockReplace, model.OperationBlockInsertAfter, model.OperationBlockDelete} {
		out = append(out, cloud.CapabilityGrant{Descriptor: model.OperationCapability{ID: kind, Kind: kind, Route: cloud.ShortcutXMLRoute}, CLISourceRevision: cloud.CLIRevision, Evidence: "offline: fixture", Verified: true})
	}
	return out
}
func (f *fixtureCloud) ApplyDoc(_ context.Context, token, revision string, ops []model.Operation) (cloud.ApplyResult, error) {
	f.writes++
	op := ops[0]
	var p model.OperationPayload
	_ = json.Unmarshal(op.Payload, &p)
	before, _ := markdown.Decode([]byte(f.xml), strconv.Itoa(f.revision))
	for _, b := range before.Blocks {
		if b.RemoteID == op.BlockID {
			f.xml = strings.Replace(f.xml, string(b.RawXML), strings.Replace(p.XML, ">", ` id="new`+strconv.Itoa(f.writes)+`">`, 1), 1)
		}
	}
	if f.applyHook != nil {
		f.applyHook()
		f.applyHook = nil
	}
	f.revision++
	if f.loseResponse {
		f.loseResponse = false
		return cloud.ApplyResult{}, model.ErrUnknownResult
	}
	return cloud.ApplyResult{Revision: strconv.Itoa(f.revision)}, nil
}
func (f *fixtureCloud) CreateDoc(_ context.Context, parent, title string, xml []byte) (cloud.Document, error) {
	f.creates++
	f.xml = `<title id="doc">` + title + `</title>` + string(xml)
	next := 0
	f.xml = regexp.MustCompile(`<p>`).ReplaceAllStringFunc(f.xml, func(s string) string { next++; return `<p id="created` + strconv.Itoa(next) + `">` })
	f.items = append(f.items, larkcli.DriveItem{Token: "created", Name: title, Type: "docx", ParentToken: parent})
	f.revision = 1
	if f.loseResponse {
		f.loseResponse = false
		return cloud.Document{}, model.ErrUnknownResult
	}
	return cloud.Document{Token: "created", Revision: "1"}, nil
}
func (f *fixtureCloud) ObserveFile(_ context.Context, token string) (ObservedFile, error) {
	data := bytes.Clone(f.file)
	if f.mismatch && f.writes > 0 {
		data = []byte("wrong")
	}
	h := sha256.Sum256(data)
	now := time.Now().UTC()
	return ObservedFile{Token: token, Identity: model.FileIdentity{Kind: model.FileIdentityObservation, ObservationID: uuid.NewString(), SHA256: hex.EncodeToString(h[:]), StartedAt: now, CompletedAt: now}, Size: int64(len(data)), Reader: io.NopCloser(bytes.NewReader(data))}, nil
}
func (f *fixtureCloud) UploadFile(_ context.Context, parent, token, name string, in io.Reader) (larkcli.UploadResult, error) {
	f.writes++
	f.file, _ = io.ReadAll(in)
	if f.loseResponse {
		f.loseResponse = false
		return larkcli.UploadResult{}, model.ErrUnknownResult
	}
	return larkcli.UploadResult{FileToken: token, Size: int64(len(f.file))}, nil
}
func (f *fixtureCloud) List(context.Context, string) ([]larkcli.DriveItem, error) {
	return f.items, nil
}
func (f *fixtureCloud) Rename(_ context.Context, token string, kind model.Kind, name string) error {
	for i := range f.items {
		if f.items[i].Token == token {
			f.items[i].Name = name
		}
	}
	return nil
}
func (f *fixtureCloud) Move(context.Context, string, model.Kind, string) error { return nil }
func (f *fixtureCloud) Delete(context.Context, string, model.Kind) error       { return nil }
func (f *fixtureCloud) CreateFolder(context.Context, string, string) (string, error) {
	return "folder", nil
}

func setup(t *testing.T, kind model.Kind) (*store.Store, *Engine, *fixtureCloud, model.Entry) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sc := store.Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: "verified", FolderToken: "root"}
	if err = s.BindScope(bg, sc); err != nil {
		t.Fatal(err)
	}
	e, err := s.ObserveEntry(bg, store.Observation{Remote: "note.md", Kind: kind, Token: "resource"})
	if err != nil {
		t.Fatal(err)
	}
	source := &fixtureCloud{xml: `<title id="doc">Title</title><p id="a">alpha one</p><p id="b">beta two</p>`, revision: 1, file: []byte("old"), grants: true}
	engine, err := New(Dependencies{Store: s, Source: source, Publisher: PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := s.CommitPublication(ctx, c)
		return err
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if kind == model.KindDocx {
		doc, _ := source.FetchDoc(bg, e.Token, "")
		sn, view, err := engine.saveDoc(bg, doc)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := s.OfferRemote(bg, e.ID, e.Generation, sn, sn.Markdown)
		if err != nil {
			t.Fatal(err)
		}
		_ = view
		if _, err = s.CommitPublication(bg, candidate); err != nil {
			t.Fatal(err)
		}
	} else {
		sn, err := engine.observeFile(bg, e.Token)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AdoptFileSnapshot(bg, e.ID, e.Generation, sn.ID); err != nil {
			t.Fatal(err)
		}
	}
	e, _ = s.Entry(bg, e.ID)
	return s, engine, source, e
}
func stageContent(t *testing.T, s *store.Store, e model.Entry, body string) model.Entry {
	t.Helper()
	e, _, err := s.Stage(bg, e.ID, nil, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func blobText(t *testing.T, s *store.Store, id model.BlobID) string {
	t.Helper()
	r, err := s.ReadBlob(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTimeoutAfterCommitReconciles(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	c.loseResponse = true
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Status != model.StatusSynced || c.writes != 1 {
		t.Fatalf("%+v writes%d", got, c.writes)
	}
	if err := e.SyncEntry(bg, entry.ID); err != nil || c.writes != 1 {
		t.Fatalf("replayed %d %v", c.writes, err)
	}
}
func TestG1UploadG2SaveRebases(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	c.applyHook = func() { stageContent(t, s, entry, "alpha changed\n\nbeta newer\n") }
	if err := e.SyncEntry(bg, entry.ID); err != nil && !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Generation != 2 || !strings.Contains(blobText(t, s, got.Local), "beta newer") {
		t.Fatal(got)
	}
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Entry(bg, entry.ID)
	if got.Status != model.StatusSynced || !strings.Contains(c.xml, "beta newer") {
		t.Fatal(got, c.xml)
	}
}
func TestOrdinaryReadbackMismatchNotConfirmed(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "wanted")
	c.mismatch = true
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("mismatch accepted")
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Status == model.StatusSynced || blobText(t, s, got.Local) != "wanted" {
		t.Fatal(got)
	}
}
func TestRemoteDriftThreeTimesConflicts(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	c.drift = true
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("drift accepted")
	}
	conflicts, _ := s.Conflicts(bg)
	if len(conflicts) != 1 || c.writes != 0 {
		t.Fatal(conflicts, c.writes)
	}
}
func TestUnverifiedRouteNeverDispatches(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	c.grants = false
	if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatal(err)
	}
	if c.writes != 0 {
		t.Fatal(c.writes)
	}
}

func TestCreateUnknownNoDuplicate(t *testing.T) {
	s, e, c, _ := setup(t, model.KindDocx)
	entry, err := s.EnsureEntry(bg, "new.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "# New\n\nfirst\n\nsecond\n")
	c.loseResponse = true
	if err = e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Token != "created" || got.Status != model.StatusSynced || c.creates != 1 {
		t.Fatalf("%+v creates%d", got, c.creates)
	}
}
func TestPermissionAndDiskErrorsPause(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	c.deny = true
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("permission accepted")
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Status != model.StatusBlocked || c.writes != 0 {
		t.Fatal(got, c.writes)
	}
	pending, _ := s.Pending(bg)
	if len(pending) != 1 {
		t.Fatal(pending)
	}
	c.deny = false
	if err := s.ResumeJob(bg, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteRefreshDefersOpenFile(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	busy := errors.New("external update busy")
	e.publisher = PublisherFunc(func(context.Context, model.PublishCandidate) error { return busy })
	c.xml = strings.Replace(c.xml, "alpha one", "remote changed", 1)
	c.revision++
	if err := e.RefreshEntry(bg, entry.ID); !errors.Is(err, busy) {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Visible != entry.Visible {
		t.Fatal("busy reader overwritten")
	}
	list, err := s.PendingPublications(bg)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	e.publisher = PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := s.CommitPublication(ctx, c)
		return err
	})
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Entry(bg, entry.ID)
	if !strings.Contains(blobText(t, s, got.Visible), "remote changed") {
		t.Fatal(got)
	}
}
func TestPartialPatchResumesWithoutDuplicate(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	c.xml += `<p id="c">gamma three</p>`
	c.revision++
	if err := e.RefreshEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	entry, _ = s.Entry(bg, entry.ID)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n\ngamma changed\n")
	c.loseResponse = true
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	if c.writes != 2 || !strings.Contains(c.xml, "gamma changed") {
		t.Fatal(c.writes, c.xml)
	}
}

func TestTransientReadBackoffAndExplicitRetry(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	clock := time.Unix(100, 0)
	e.opt.Now = func() time.Time { return clock }
	c.readErr = errors.New("offline")
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("read error lost")
	}
	c.readErr = nil
	if err := e.SyncEntry(bg, entry.ID); err != nil || c.writes != 0 {
		t.Fatal("backoff bypassed", err, c.writes)
	}
	clock = clock.Add(3 * time.Second)
	if err := e.SyncEntry(bg, entry.ID); err != nil || c.writes != 1 {
		t.Fatal(err, c.writes)
	}
}
