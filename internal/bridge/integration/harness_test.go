// Package integration composes real durable Store/Overlay/VFS/Engine without a
// driver or account. Synthetic provider capability evidence is offline-only.
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/migrate"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/overlay"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
	"github.com/rclone/rclone/lib/larkcli"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

type resource struct {
	kind              model.Kind
	name, parent, xml string
	data              []byte
	revision          int
}
type uploadGate struct {
	entered, resume chan struct{}
	once            sync.Once
}
type provider struct {
	beforeCreate                                              func(context.Context, string, string, []byte) error
	uploadLoseAndOffline                                      bool
	mu                                                        sync.Mutex
	resources                                                 map[string]resource
	root                                                      string
	permission, offline, loseApplyResponse                    bool
	uploads, applies, creates, deletes, fetches, observations int
	applied                                                   []model.Operation
	uploadTokens                                              []string
	gate                                                      *uploadGate
	active                                                    atomic.Int32
}

func (p *provider) readError() error {
	if p.permission {
		return &larkcli.Error{Code: "permission_denied"}
	}
	if p.offline {
		return errors.New("synthetic provider offline")
	}
	return nil
}
func (p *provider) List(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.readError(); err != nil {
		return nil, err
	}
	var out []larkcli.DriveItem
	for token, r := range p.resources {
		if r.parent == parent {
			out = append(out, larkcli.DriveItem{Token: token, Name: r.name, Type: string(r.kind), ParentToken: parent})
		}
	}
	return out, nil
}
func (p *provider) FetchDoc(ctx context.Context, token, revision string) (cloud.Document, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return cloud.Document{}, err
	}
	if err := p.readError(); err != nil {
		return cloud.Document{}, err
	}
	r, ok := p.resources[token]
	if !ok {
		return cloud.Document{}, os.ErrNotExist
	}
	p.fetches++
	return cloud.Document{Token: token, Revision: strconv.Itoa(r.revision), XML: []byte(r.xml)}, nil
}
func (p *provider) observe(ctx context.Context, token string) (model.FileIdentity, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return model.FileIdentity{}, nil, err
	}
	if err := p.readError(); err != nil {
		return model.FileIdentity{}, nil, err
	}
	r, ok := p.resources[token]
	if !ok {
		return model.FileIdentity{}, nil, os.ErrNotExist
	}
	p.observations++
	data := bytes.Clone(r.data)
	hash := sha256.Sum256(data)
	now := time.Now().UTC()
	return model.FileIdentity{Kind: model.FileIdentityObservation, ObservationID: uuid.NewString(), SHA256: hex.EncodeToString(hash[:]), StartedAt: now, CompletedAt: now}, data, nil
}

type overlaySource struct{ *provider }

func (p overlaySource) ObserveFile(ctx context.Context, token string) (overlay.FileObservation, error) {
	id, data, err := p.observe(ctx, token)
	if err != nil {
		return overlay.FileObservation{}, err
	}
	return overlay.FileObservation{Token: token, Identity: id, Size: int64(len(data)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, Close: func() error { return nil }}, nil
}

type engineSource struct{ *provider }

func (p engineSource) ObserveFile(ctx context.Context, token string) (syncer.ObservedFile, error) {
	id, data, err := p.observe(ctx, token)
	if err != nil {
		return syncer.ObservedFile{}, err
	}
	return syncer.ObservedFile{Token: token, Identity: id, Size: int64(len(data)), Reader: io.NopCloser(bytes.NewReader(data))}, nil
}
func (p *provider) Capabilities() []cloud.CapabilityGrant {
	var grants []cloud.CapabilityGrant
	for _, kind := range []string{model.OperationBlockReplace, model.OperationBlockInsertAfter, model.OperationBlockDelete} {
		grants = append(grants, cloud.CapabilityGrant{Descriptor: model.OperationCapability{ID: "synthetic-" + kind, Kind: kind, Route: cloud.ShortcutXMLRoute}, Verified: true, CLISourceRevision: cloud.CLIRevision, Evidence: "offline: functional integration provider; not live authority"})
	}
	return grants
}
func (p *provider) ApplyDoc(ctx context.Context, token, revision string, ops []model.Operation) (cloud.ApplyResult, error) {
	p.active.Add(1)
	defer p.active.Add(-1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return cloud.ApplyResult{}, err
	}
	if err := p.readError(); err != nil {
		return cloud.ApplyResult{}, errors.Join(cloud.ErrNotDispatched, err)
	}
	if len(ops) != 1 {
		return cloud.ApplyResult{}, cloud.ErrUnsupportedBatch
	}
	op := ops[0]
	if op.Route != cloud.ShortcutXMLRoute || op.CapabilityID != "synthetic-"+op.Kind || op.ID == "" {
		return cloud.ApplyResult{}, model.ErrUnsupportedContent
	}
	r, ok := p.resources[token]
	if !ok {
		return cloud.ApplyResult{}, os.ErrNotExist
	}
	before, err := markdown.Decode([]byte(r.xml), strconv.Itoa(r.revision))
	if err != nil {
		return cloud.ApplyResult{}, err
	}
	var payload model.OperationPayload
	if err = json.Unmarshal(op.Payload, &payload); err != nil {
		return cloud.ApplyResult{}, err
	}
	p.applies++
	replacement := payload.XML
	if replacement != "" {
		at := strings.IndexByte(replacement, '>')
		if at < 0 {
			return cloud.ApplyResult{}, model.ErrUnsupportedContent
		}
		replacement = replacement[:at] + ` id="applied-` + strconv.Itoa(p.applies) + `"` + replacement[at:]
	}
	matched := false
	switch op.Kind {
	case model.OperationBlockReplace, model.OperationBlockDelete:
		for _, block := range before.Blocks {
			if block.RemoteID == op.BlockID {
				r.xml = strings.Replace(r.xml, string(block.RawXML), replacement, 1)
				matched = true
				break
			}
		}
	case model.OperationBlockInsertAfter:
		if op.AfterBlockID == "0" {
			r.xml = replacement + r.xml
			matched = true
		} else {
			for _, block := range before.Blocks {
				if block.RemoteID == op.AfterBlockID {
					r.xml = strings.Replace(r.xml, string(block.RawXML), string(block.RawXML)+replacement, 1)
					matched = true
					break
				}
			}
		}
	}
	if !matched {
		return cloud.ApplyResult{}, model.ErrVersionChanged
	}
	r.revision++
	p.resources[token] = r
	p.applied = append(p.applied, op)
	if p.loseApplyResponse {
		p.loseApplyResponse = false
		return cloud.ApplyResult{}, model.ErrUnknownResult
	}
	return cloud.ApplyResult{Revision: strconv.Itoa(r.revision)}, nil
}
func (p *provider) CreateDoc(ctx context.Context, parent, name string, xml []byte) (cloud.Document, error) {
	p.mu.Lock()
	hook := p.beforeCreate
	err := p.readError()
	p.mu.Unlock()
	if err != nil {
		return cloud.Document{}, errors.Join(cloud.ErrNotDispatched, err)
	}
	if hook != nil {
		if err = hook(ctx, parent, name, xml); err != nil {
			return cloud.Document{}, errors.Join(cloud.ErrNotDispatched, err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	token := uuid.NewString()
	body := strings.ReplaceAll(string(xml), "<p>", `<p id="created-`+token+`">`)
	p.resources[token] = resource{kind: model.KindDocx, name: name, parent: parent, xml: body, revision: 1}
	return cloud.Document{Token: token, Revision: "1"}, nil
}

func (p *provider) UploadFile(ctx context.Context, parent, token, name string, in io.Reader) (larkcli.UploadResult, error) {
	p.active.Add(1)
	defer p.active.Add(-1)
	data, err := io.ReadAll(in)
	if err != nil {
		return larkcli.UploadResult{}, err
	}
	p.mu.Lock()
	gate := p.gate
	if err = p.readError(); err != nil {
		p.mu.Unlock()
		return larkcli.UploadResult{}, errors.Join(cloud.ErrNotDispatched, err)
	}
	p.mu.Unlock()
	if gate != nil {
		gate.once.Do(func() { close(gate.entered) })
		select {
		case <-ctx.Done():
			return larkcli.UploadResult{}, ctx.Err()
		case <-gate.resume:
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.uploads++
	p.uploadTokens = append(p.uploadTokens, token)
	if token == "" {
		p.creates++
		token = uuid.NewString()
	}
	r := p.resources[token]
	r.kind = model.KindFile
	r.name = name
	if parent != "" {
		r.parent = parent
	}
	r.data = bytes.Clone(data)
	r.revision++
	p.resources[token] = r
	if p.uploadLoseAndOffline {
		p.uploadLoseAndOffline = false
		p.offline = true
		return larkcli.UploadResult{FileToken: token, Size: int64(len(data))}, model.ErrUnknownResult
	}
	return larkcli.UploadResult{FileToken: token, Size: int64(len(data))}, nil
}
func (p *provider) Rename(_ context.Context, token string, kind model.Kind, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.resources[token]
	if !ok {
		return os.ErrNotExist
	}
	r.name = name
	p.resources[token] = r
	return nil
}
func (p *provider) Move(_ context.Context, token string, kind model.Kind, parent string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.resources[token]
	if !ok {
		return os.ErrNotExist
	}
	r.parent = parent
	p.resources[token] = r
	return nil
}
func (p *provider) Delete(_ context.Context, token string, kind model.Kind) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes++
	delete(p.resources, token)
	return nil
}
func (p *provider) CreateFolder(_ context.Context, parent, name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	token := uuid.NewString()
	p.creates++
	p.resources[token] = resource{kind: model.KindFolder, parent: parent, name: name}
	return token, nil
}
func (p *provider) setDoc(token, xml string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.resources[token]
	r.xml = xml
	r.revision++
	p.resources[token] = r
}
func (p *provider) setFile(token string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.resources[token]
	r.data = bytes.Clone(data)
	r.revision++
	p.resources[token] = r
}
func (p *provider) doc(t *testing.T, token string) string {
	t.Helper()
	p.mu.Lock()
	r := p.resources[token]
	p.mu.Unlock()
	v, err := markdown.Decode([]byte(r.xml), strconv.Itoa(r.revision))
	if err != nil {
		t.Fatal(err)
	}
	return string(v.Markdown)
}

type persistence struct {
	fail  atomic.Bool
	errno atomic.Int32
}

func (p *persistence) SyncFile(f *os.File) error {
	if p.fail.Load() {
		if p.errno.Load() == 2 {
			return syscallENOSPC()
		}
		return syscallEIO()
	}
	return f.Sync()
}
func (p *persistence) Rename(a, b string) error { return platform.ReplaceFile(a, b) }
func (p *persistence) SyncDir(pth string) error { return platform.SyncDir(pth) }

type harness struct {
	t                              *testing.T
	ctx                            context.Context
	root, dir, docToken, fileToken string
	scope                          store.Scope
	paths                          config.Paths
	store                          *store.Store
	overlay                        *overlay.Fs
	vfs                            *vfs.VFS
	engine                         *syncer.Engine
	cloud                          *provider
	io                             *persistence
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, root: root, docToken: uuid.NewString(), fileToken: uuid.NewString(), io: &persistence{}}
	h.paths = config.Paths{ConfigFile: filepath.Join(root, "config", "config.json"), StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "clean-cache")}
	h.scope = store.Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: uuid.NewString(), FolderToken: uuid.NewString()}
	h.dir = filepath.Join(h.paths.StateDir, "mounts", string(h.scope.MountID))
	c := config.Default()
	c.Profiles = append(c.Profiles, model.Profile{ID: h.scope.ProfileID, Name: "Synthetic integration profile", Brand: "feishu", AuthRef: "auth/identities/" + uuid.NewString() + "/lark", IdentityHash: h.scope.IdentityHash})
	c.Remotes = append(c.Remotes, model.Remote{ID: h.scope.RemoteID, Name: "Synthetic folder", ProfileID: h.scope.ProfileID, FolderToken: h.scope.FolderToken, NewMDPolicy: "docx"})
	m := config.DefaultMount()
	m.ID = h.scope.MountID
	m.Name = "Synthetic mount"
	m.RemoteID = h.scope.RemoteID
	m.Mountpoint = filepath.Join(root, "kernel-mount-unused")
	c.Mounts = append(c.Mounts, m)
	if err = config.SaveWithPaths(h.paths, c); err != nil {
		t.Fatal(err)
	}
	h.cloud = &provider{root: h.scope.FolderToken, resources: map[string]resource{h.docToken: {kind: model.KindDocx, name: "document", parent: h.scope.FolderToken, xml: `<p id="a">alpha beta gamma</p><p id="anchor">stable anchor 中文😀</p><p id="b">tail</p>`, revision: 1}, h.fileToken: {kind: model.KindFile, name: "data.bin", parent: h.scope.FolderToken, data: []byte("\x00\xffinitial bytes"), revision: 1}}}
	h.ctx, _ = fs.AddConfig(context.Background())
	previousCache := fsconfig.GetCacheDir()
	if err = fsconfig.SetCacheDir(filepath.Join(root, "vfs-cache")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsconfig.SetCacheDir(previousCache) })
	h.open()
	t.Cleanup(func() {
		if h.vfs != nil {
			h.vfs.Shutdown()
		}
		if h.store != nil {
			h.store.Close()
		}
	})
	return h
}
func (h *harness) open() {
	h.t.Helper()
	var err error
	h.store, err = store.OpenWithOptions(h.dir, store.Options{IO: h.io})
	if err != nil {
		h.t.Fatal(err)
	}
	if err = h.store.BindScope(h.ctx, h.scope); err != nil {
		h.t.Fatal(err)
	}
	raw, err := overlay.New(overlay.Dependencies{Store: h.store, Source: overlaySource{h.cloud}, ProfileID: h.scope.ProfileID, MountID: h.scope.MountID})
	if err != nil {
		h.t.Fatal(err)
	}
	h.overlay = raw.(*overlay.Fs)
	opt := vfscommon.Opt
	opt.CachePollInterval = 0
	opt.WriteBack = 0
	opt.HandleCaching = 0
	h.vfs, err = vfs.NewManaged(h.ctx, h.overlay, &opt)
	if err != nil {
		h.t.Fatal(err)
	}
	deletionCommit := func(ctx context.Context, entry model.Entry, commit func() (bool, error)) error {
		return h.vfs.TryExternalReconcile(ctx, entry.Remote, func() (fs.Object, error) {
			removed, err := commit()
			if err != nil || removed {
				return nil, err
			}
			return h.overlay.NewObject(ctx, entry.Remote)
		})
	}
	h.overlay.SetDeletionCommit(deletionCommit)
	h.engine, err = syncer.New(syncer.Dependencies{DeletionCommit: deletionCommit, Store: h.store, Source: engineSource{h.cloud}, Publisher: syncer.PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		entry, err := h.store.Entry(ctx, c.EntryID)
		if err != nil {
			return err
		}
		return h.vfs.TryExternalUpdate(ctx, entry.Remote, func() (fs.Object, error) { return h.overlay.Publish(ctx, c) })
	}), ShouldSync: h.overlay.ShouldSync, LocalOnly: h.overlay.LocalOnly}, syncer.Options{EmptyFileUploadVerified: true})
	if err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) restart() {
	h.t.Helper()
	h.vfs.Shutdown()
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open()
}
func (h *harness) entry(name string) model.Entry {
	h.t.Helper()
	// These integration assertions inspect completed outbox state. Local save
	// itself now returns before this asynchronous handoff (tested separately).
	if err := h.vfs.DrainManagedArchives(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	e, err := h.store.EntryByRemote(h.ctx, name)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}
func (h *harness) read(name string) string {
	h.t.Helper()
	b, err := h.vfs.ReadFile(name)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}
func (h *harness) blob(id model.BlobID) string {
	h.t.Helper()
	r, err := h.store.ReadBlob(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}
func (h *harness) save(name, content string) model.Entry {
	h.t.Helper()
	f, err := h.vfs.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err = f.Write([]byte(content)); err != nil {
		f.Close()
		h.t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		h.t.Fatal(err)
	}
	if err = f.Flush(); err != nil {
		f.Close()
		h.t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		h.t.Fatal(err)
	}
	return h.entry(name)
}
func (h *harness) assertIdle() {
	h.t.Helper()
	r, w, ns := h.vfs.ManagedActivity()
	if r+w+ns != 0 || h.vfs.ManagedCachePending() {
		h.t.Fatalf("not idle: readers=%d writers=%d namespace=%d pending=%t", r, w, ns, h.vfs.ManagedCachePending())
	}
}

type quiescer struct {
	h  *harness
	mu sync.Mutex
}

func (q *quiescer) WithQuiescence(ctx context.Context, fn func(context.Context) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	release, err := q.h.vfs.PauseManagedAdmission()
	if err != nil {
		return err
	}
	defer release()
	r, w, n := q.h.vfs.ManagedActivity()
	if r+w+n != 0 || q.h.vfs.ManagedCachePending() || q.h.cloud.active.Load() != 0 {
		return migrate.ErrNotQuiescent
	}
	return fn(ctx)
}
func (h *harness) migrator() *migrate.Service {
	h.t.Helper()
	m, err := migrate.New(migrate.Options{Paths: h.paths, Guard: &quiescer{h: h}, Mounts: []migrate.Mount{{Store: h.store, Dir: h.dir}}})
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("channel barrier timeout")
		var zero T
		return zero
	}
}

var _ syncer.Source = engineSource{}
var _ overlay.Source = overlaySource{}

func (h *harness) debugState() {
	h.t.Helper()
	jobs, _ := h.store.Pending(h.ctx)
	conflicts, _ := h.store.Conflicts(h.ctx)
	h.t.Logf("jobs=%+v conflicts=%+v", jobs, conflicts)
	for _, j := range jobs {
		journal, _ := h.store.Journal(h.ctx, j.ID)
		h.t.Logf("journal=%+v", journal)
	}
	h.cloud.mu.Lock()
	h.t.Logf("provider applies=%d resources=%+v", h.cloud.applies, h.cloud.resources)
	h.cloud.mu.Unlock()
}
