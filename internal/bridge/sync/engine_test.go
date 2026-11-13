package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/rclone/rclone/internal/bridge/platform"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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
	uploadedName    string
	guardDenied     bool
	directory       string
	deletes         int
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
	if f.guardDenied {
		return cloud.ApplyResult{}, errors.Join(cloud.ErrNotDispatched, model.ErrIdentityChanged)
	}
	f.writes++
	op := ops[0]
	var p model.OperationPayload
	_ = json.Unmarshal(op.Payload, &p)
	before, _ := markdown.Decode([]byte(f.xml), strconv.Itoa(f.revision))
	newXML := p.XML
	if newXML != "" {
		at := strings.IndexByte(newXML, '>')
		if at < 0 {
			return cloud.ApplyResult{}, model.ErrUnsupportedContent
		}
		newXML = newXML[:at] + ` id="new` + strconv.Itoa(f.writes) + `"` + newXML[at:]
	}
	switch op.Kind {
	case model.OperationBlockReplace, model.OperationBlockDelete:
		for _, b := range before.Blocks {
			if b.RemoteID == op.BlockID {
				f.xml = strings.Replace(f.xml, string(b.RawXML), newXML, 1)
			}
		}
	case model.OperationBlockInsertAfter:
		if op.AfterBlockID == "0" {
			at := strings.Index(f.xml, "</title>") + len("</title>")
			f.xml = f.xml[:at] + newXML + f.xml[at:]
		} else {
			for _, b := range before.Blocks {
				if b.RemoteID == op.AfterBlockID {
					f.xml = strings.Replace(f.xml, string(b.RawXML), string(b.RawXML)+newXML, 1)
				}
			}
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
	if f.guardDenied {
		return cloud.Document{}, errors.Join(cloud.ErrNotDispatched, model.ErrIdentityChanged)
	}
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
	if f.guardDenied {
		return larkcli.UploadResult{}, errors.Join(cloud.ErrNotDispatched, model.ErrIdentityChanged)
	}
	f.writes++
	f.uploadedName = name
	f.file, _ = io.ReadAll(in)
	if token == "" {
		token = "created-file"
		f.items = append(f.items, larkcli.DriveItem{Token: token, Name: name, Type: "file", ParentToken: parent})
	}
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
func (f *fixtureCloud) Delete(_ context.Context, token string, kind model.Kind) error {
	f.deletes++
	var items []larkcli.DriveItem
	for _, item := range f.items {
		if item.Token != token {
			items = append(items, item)
		}
	}
	f.items = items
	if f.loseResponse {
		f.loseResponse = false
		return model.ErrUnknownResult
	}
	return nil
}
func (f *fixtureCloud) CreateFolder(context.Context, string, string) (string, error) {
	return "folder", nil
}

func setup(t *testing.T, kind model.Kind) (*store.Store, *Engine, *fixtureCloud, model.Entry) {
	return setupOptions(t, kind, store.Options{})
}
func setupOptions(t *testing.T, kind model.Kind, options store.Options) (*store.Store, *Engine, *fixtureCloud, model.Entry) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenWithOptions(root, options)
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
	source := &fixtureCloud{directory: root, xml: `<title id="doc">Title</title><p id="a">alpha one</p><p id="b">beta two</p>`, revision: 1, file: []byte("old"), grants: true}
	source.items = []larkcli.DriveItem{{Token: e.Token, Name: e.CloudName, Type: string(kind), ParentToken: sc.FolderToken}}
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

func TestManualResolutionRebasesAgainstCapturedRemote(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha local\n\nbeta two\n")
	c.xml = strings.Replace(c.xml, "alpha one", "alpha remote", 1)
	c.revision++
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("overlap accepted")
	}
	conflicts, err := s.Conflicts(bg)
	if err != nil || len(conflicts) != 1 {
		t.Fatal(conflicts, err)
	}
	conflict := conflicts[0]
	_, _, err = e.Resolve(bg, model.ResolveRequest{ConflictID: conflict.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: conflict.RemoteRevision, Content: []byte("alpha merged\n\nbeta two\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Status != model.StatusSynced || !strings.Contains(c.xml, "alpha merged") {
		t.Fatal(current, c.xml)
	}
}

func TestEmptyFileGateRetainsBytesAndNonemptySaveResumes(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "")
	if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Status != model.StatusBlocked || blobText(t, s, got.Local) != "" || c.writes != 0 {
		t.Fatal(got, c.writes)
	}
	entry = stageContent(t, s, got, "nonempty")
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Entry(bg, entry.ID)
	if got.Status != model.StatusSynced || string(c.file) != "nonempty" {
		t.Fatal(got, string(c.file))
	}
}

func TestInsertDeleteResponseLossNeverReplays(t *testing.T) {
	for _, local := range []string{"first\n\nalpha one\n\nbeta two\n", "alpha one\n"} {
		t.Run(local, func(t *testing.T) {
			s, e, c, entry := setup(t, model.KindDocx)
			entry = stageContent(t, s, entry, local)
			c.loseResponse = true
			if err := e.SyncEntry(bg, entry.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.SyncEntry(bg, entry.ID); err != nil || c.writes != 1 {
				t.Fatal(err, c.writes)
			}
		})
	}
}
func TestUnknownDuplicateBodyPausesWithoutReplay(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	c.loseResponse = true
	c.applyHook = func() { c.xml += `<p id="duplicate">alpha changed</p>` }
	for range 2 {
		if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnknownResult) {
			t.Fatal(err)
		}
	}
	if c.writes != 1 {
		t.Fatal(c.writes)
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Status != model.StatusAwaitingVerification {
		t.Fatal(current)
	}
}
func TestNamespaceUnknownDeleteDoesNotRepeat(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	c.items = []larkcli.DriveItem{{Token: entry.Token, Name: entry.Remote, Type: "file"}}
	c.loseResponse = true
	op, err := s.PrepareNamespace(bg, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = e.syncNamespace(bg); !errors.Is(err, model.ErrUnknownResult) {
			t.Fatal(err)
		}
	}
	if c.deletes != 1 {
		t.Fatal(c.deletes)
	}
}
func TestCreateUnknownMultipleMatchesDoesNotBind(t *testing.T) {
	s, e, c, _ := setup(t, model.KindDocx)
	entry, err := s.EnsureEntry(bg, "new.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "body\n")
	c.loseResponse = true
	if err = e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	copy := c.items[len(c.items)-1]
	copy.Token = "ambiguous-other"
	c.items = append(c.items, copy)
	if err = e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Token != "" || c.creates != 1 {
		t.Fatal(got, c.creates)
	}
}

type faultIO struct{ fail bool }

func (f *faultIO) SyncFile(file *os.File) error {
	if f.fail {
		return syscall.ENOSPC
	}
	return file.Sync()
}
func (f *faultIO) Rename(a, b string) error { return platform.ReplaceFile(a, b) }
func (f *faultIO) SyncDir(dir string) error { return platform.SyncDir(dir) }
func TestDiskFailurePausesWithoutCloudMutation(t *testing.T) {
	faults := &faultIO{}
	s, e, c, entry := setupOptions(t, model.KindDocx, store.Options{IO: faults})
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	faults.fail = true
	if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	pending, err := s.Pending(bg)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	journal, err := s.Journal(bg, pending[0].ID)
	if err != nil || journal.ErrorCode != "insufficient_space" || journal.Job.State != model.StatusPaused || c.writes != 0 {
		t.Fatal(journal, err, c.writes)
	}
	faults.fail = false
	if err = e.Retry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
}
func TestPartialPatchRestartReconcilesPersistedOperation(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	c.xml += `<p id="c">gamma three</p>`
	c.revision++
	if err := e.RefreshEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	entry, _ = s.Entry(bg, entry.ID)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n\ngamma changed\n")
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	c.applyHook = cancel
	if err := e.SyncEntry(ctx, entry.ID); err == nil {
		t.Fatal("canceled mutation unexpectedly confirmed")
	}
	scope, err := s.Scope(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(c.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.BindScope(bg, scope); err != nil {
		t.Fatal(err)
	}
	restored, err := New(Dependencies{Store: reopened, Source: c, Publisher: PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := reopened.CommitPublication(ctx, c)
		return err
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	if c.writes != 2 {
		t.Fatal("replayed first operation", c.writes)
	}
}

type zeroInput struct{}

func (zeroInput) Read(b []byte) (int, error) { clear(b); return len(b), nil }

type streamingSource struct {
	*fixtureCloud
	size int64
	sum  string
}

func (f *streamingSource) UploadFile(_ context.Context, parent, token, name string, in io.Reader) (larkcli.UploadResult, error) {
	h := sha256.New()
	size, err := io.Copy(h, in)
	if err != nil {
		return larkcli.UploadResult{}, err
	}
	f.size = size
	f.sum = hex.EncodeToString(h.Sum(nil))
	f.items = []larkcli.DriveItem{{Token: "streamed", Type: "file", Name: name, ParentToken: parent}}
	return larkcli.UploadResult{FileToken: "streamed", Size: size}, nil
}
func (f *streamingSource) ObserveFile(_ context.Context, token string) (ObservedFile, error) {
	now := time.Now().UTC()
	return ObservedFile{Token: token, Size: f.size, Identity: model.FileIdentity{Kind: model.FileIdentityObservation, ObservationID: uuid.NewString(), SHA256: f.sum, StartedAt: now, CompletedAt: now}, Reader: io.NopCloser(io.LimitReader(zeroInput{}, f.size))}, nil
}
func TestOrdinaryCreateStreamsLargeBlob(t *testing.T) {
	s, e, c, _ := setupOptions(t, model.KindFile, store.Options{SpacePolicy: func(context.Context, string, int64) error { return nil }})
	e.source = &streamingSource{fixtureCloud: c}
	entry, err := s.EnsureEntry(bg, "large.bin", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	const size = 32 << 20
	entry, _, err = s.Stage(bg, entry.ID, nil, io.LimitReader(zeroInput{}, size))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= size {
		t.Fatalf("ordinary creation buffered a whole file: allocated %d for %d-byte blob", allocated, size)
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Status != model.StatusSynced {
		t.Fatal(current)
	}
}

func TestIdentityCheckStopsQueueBeforePrepare(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	e.deps.CheckIdentity = func(context.Context) error { return model.ErrIdentityChanged }
	if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	pending, _ := s.Pending(bg)
	if len(pending) != 1 || pending[0].State != model.StatusQueued || c.writes != 0 {
		t.Fatal(pending, c.writes)
	}
}

func TestRejectedBeforeDispatchCanSafelyRetry(t *testing.T) {
	for _, kind := range []model.Kind{model.KindDocx, model.KindFile} {
		t.Run(string(kind), func(t *testing.T) {
			s, e, c, entry := setup(t, kind)
			local := "bytes"
			if kind == model.KindDocx {
				local = "alpha changed\n\nbeta two\n"
			}
			entry = stageContent(t, s, entry, local)
			c.guardDenied = true
			if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrIdentityChanged) {
				t.Fatal(err)
			}
			current, _ := s.Entry(bg, entry.ID)
			if current.Status != model.StatusBlocked || c.writes != 0 {
				t.Fatal(current, c.writes)
			}
			c.guardDenied = false
			if err := e.Retry(bg, entry.ID); err != nil {
				t.Fatal(err)
			}
			current, _ = s.Entry(bg, entry.ID)
			if current.Status != model.StatusSynced || c.writes != 1 {
				t.Fatal(current, c.writes)
			}
		})
	}
}
func TestRejectedCreateCanSafelyRetryWithoutUnknownResource(t *testing.T) {
	s, e, c, _ := setup(t, model.KindDocx)
	entry, err := s.EnsureEntry(bg, "new.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "body\n")
	c.guardDenied = true
	if err = e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Status != model.StatusBlocked || c.creates != 0 {
		t.Fatal(current, c.creates)
	}
	c.guardDenied = false
	if err = e.Retry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	if c.creates != 1 {
		t.Fatal(c.creates)
	}
}

func TestRefreshConflictUpdatesInputsAndKeepsDraft(t *testing.T) {
	s, e, c, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha local\n\nbeta two\n")
	c.xml = strings.Replace(c.xml, "alpha one", "alpha remote", 1)
	c.revision++
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("overlap accepted")
	}
	conflicts, _ := s.Conflicts(bg)
	if len(conflicts) != 1 {
		t.Fatal(conflicts)
	}
	before := conflicts[0]
	if err := s.SaveDraft(bg, before.ID, strings.NewReader("unfinished user draft")); err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "alpha local newer\n\nbeta two\n")
	c.xml = strings.Replace(c.xml, "alpha remote", "alpha remote newer", 1)
	c.revision++
	refreshed, err := e.RefreshConflict(bg, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Generation != entry.Generation || refreshed.RemoteRevision != strconv.Itoa(c.revision) || blobText(t, s, refreshed.Draft) != "unfinished user draft" || !strings.Contains(blobText(t, s, refreshed.Local), "local newer") {
		t.Fatal(refreshed)
	}
	if _, _, err = e.Resolve(bg, model.ResolveRequest{ConflictID: before.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: refreshed.RemoteRevision, Content: []byte("alpha final\n\nbeta two\n")}); err != nil {
		t.Fatal(err)
	}
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNewSaveSupersedesUnsentUnsupportedContent(t *testing.T) {
	s, e, c, _ := setup(t, model.KindDocx)
	entry, err := s.EnsureEntry(bg, "new.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "<div>unsupported raw HTML</div>\n")
	if err = e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("unsupported content accepted")
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Status != model.StatusBlocked {
		t.Fatal(current)
	}
	entry = stageContent(t, s, current, "supported body\n")
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Entry(bg, entry.ID)
	if current.Status != model.StatusSynced || c.creates != 1 {
		t.Fatal(current, c.creates)
	}
}

func TestExplicitManualResolutionCanReplaceAmbiguousSupportedBody(t *testing.T) {
	s, e, _, entry, _, conflict := legacyFormatFixture(t, "replacement\n")
	if _, _, err := e.Resolve(bg, model.ResolveRequest{ConflictID: conflict.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: conflict.RemoteRevision, Content: []byte("replacement\n")}); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Status != model.StatusSynced || current.Token != entry.Token || blobText(t, s, current.Visible) != "replacement\n" {
		t.Fatal(current)
	}
}

func TestRestartDuringReadPreparationDoesNotInventMutation(t *testing.T) {
	for _, kind := range []model.Kind{model.KindDocx, model.KindFile} {
		t.Run(string(kind), func(t *testing.T) {
			s, _, c, entry := setup(t, kind)
			local := "bytes"
			if kind == model.KindDocx {
				local = "alpha changed\n\nbeta two\n"
			}
			entry = stageContent(t, s, entry, local)
			pending, _ := s.Pending(bg)
			if _, _, err := s.Prepare(bg, pending[0].ID); err != nil {
				t.Fatal(err)
			}
			scope, _ := s.Scope(bg)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(c.directory)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err = reopened.BindScope(bg, scope); err != nil {
				t.Fatal(err)
			}
			restored, err := New(Dependencies{Store: reopened, Source: c, Publisher: PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
				_, err := reopened.CommitPublication(ctx, c)
				return err
			})}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err = restored.SyncEntry(bg, entry.ID); err != nil {
				t.Fatal(err)
			}
			got, _ := reopened.Entry(bg, entry.ID)
			if got.Status != model.StatusSynced || c.writes != 1 {
				t.Fatal(got, c.writes)
			}
		})
	}
}

func TestFileOverwriteKeepsObservedCloudName(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	entry, err := s.ObserveEntry(bg, store.Observation{Remote: entry.Remote, Kind: entry.Kind, Token: entry.Token, CloudName: "remote:literal%.md"})
	if err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "updated bytes")
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	if c.uploadedName != "remote:literal%.md" {
		t.Fatal("local display name leaked into cloud overwrite", c.uploadedName)
	}
}
func TestCloudFolderGainingChildIsNotRecursivelyDeleted(t *testing.T) {
	s, e, c, _ := setup(t, model.KindFile)
	folder, err := s.ObserveEntry(bg, store.Observation{Remote: "folder", Kind: model.KindFolder, Token: "folder-token"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareNamespace(bg, model.NamespaceOp{Kind: "delete", Src: folder.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	c.items = []larkcli.DriveItem{{Token: "new-sheet", Name: "new child", Type: "sheet", ParentToken: folder.Token}}
	if err = e.syncNamespace(bg); !errors.Is(err, store.ErrBusy) {
		t.Fatal(err)
	}
	if c.deletes != 0 {
		t.Fatal("nonempty cloud folder deleted", c.deletes)
	}
}
