package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	bolt "go.etcd.io/bbolt"
)

var ctx = context.Background()

type fixtureGuard struct {
	path  string
	calls int
	deny  bool
}

func (g *fixtureGuard) WithQuiescence(ctx context.Context, fn func(context.Context) error) error {
	g.calls++
	if g.deny {
		return ErrNotQuiescent
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l := flock.New(g.path)
	if err := l.Lock(); err != nil {
		return err
	}
	defer l.Unlock()
	return fn(ctx)
}

type harness struct {
	s                *Service
	st               *store.Store
	guard            *fixtureGuard
	p                config.Paths
	scope            store.Scope
	root, storeDir   string
	pending, unknown model.JobID
}

func fixture(t *testing.T) harness {
	t.Helper()
	d, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	p := config.Paths{ConfigFile: filepath.Join(d, "conf", "config.json"), StateDir: filepath.Join(d, "state"), CacheDir: filepath.Join(d, "cache")}
	profile := model.Profile{ID: model.ProfileID(uuid.NewString()), Name: "Verified", Brand: "feishu", AuthRef: "auth/identities/" + uuid.NewString() + "/lark", IdentityHash: "verified-identity"}
	remote := model.Remote{ID: model.RemoteID(uuid.NewString()), Name: "Current", ProfileID: profile.ID, FolderToken: "root", NewMDPolicy: "docx"}
	mount := config.DefaultMount()
	mount.ID = model.MountID(uuid.NewString())
	mount.Name = "Current"
	mount.RemoteID = remote.ID
	mount.Mountpoint = filepath.Join(d, "mounted")
	c := config.Default()
	c.Profiles = append(c.Profiles, profile)
	c.Remotes = append(c.Remotes, remote)
	c.Mounts = append(c.Mounts, mount)
	if e = config.SaveWithPaths(p, c); e != nil {
		t.Fatal(e)
	}
	sd := filepath.Join(p.StateDir, "mounts", string(mount.ID))
	st, e := store.Open(sd)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	sc := store.Scope{MountID: mount.ID, ProfileID: profile.ID, RemoteID: remote.ID, IdentityHash: profile.IdentityHash, FolderToken: remote.FolderToken}
	if e = st.BindScope(ctx, sc); e != nil {
		t.Fatal(e)
	}
	stage := func(name string) (model.Entry, model.Job) {
		entry, e := st.EnsureEntry(ctx, name, model.KindFile)
		if e != nil {
			t.Fatal(e)
		}
		x, j, e := st.Stage(ctx, entry.ID, nil, strings.NewReader("bytes:"+name))
		if e != nil {
			t.Fatal(e)
		}
		return x, j
	}
	_, pending := stage("pending")
	_, unknown := stage("unknown")
	if _, _, e = st.Prepare(ctx, unknown.ID); e != nil {
		t.Fatal(e)
	}
	if e = st.MarkUnknown(ctx, unknown.ID); e != nil {
		t.Fatal(e)
	}
	ce, _ := stage("conflict")
	if e = st.SaveConflict(ctx, model.Conflict{EntryID: ce.ID, Generation: ce.Generation, Local: ce.Local, Reason: "fixture"}); e != nil {
		t.Fatal(e)
	}
	g := &fixtureGuard{path: filepath.Join(d, "lifecycle.lock")}
	s, e := New(Options{Paths: p, Guard: g, Mounts: []Mount{{Store: st, Dir: sd}}})
	if e != nil {
		t.Fatal(e)
	}
	return harness{s, st, g, p, sc, d, sd, pending.ID, unknown.ID}
}
func TestUpgradeKeepsPendingAndConflicts(t *testing.T) {
	h := fixture(t)
	bundle := filepath.Join(h.root, "backup")
	m, e := h.s.PrepareUpgrade(ctx, bundle)
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Files) < 4 || len(m.Before) == 0 || m.Digest == "" || h.guard.calls != 1 {
		t.Fatal(m, h.guard.calls)
	}
	restore := filepath.Join(h.root, "restored")
	if _, e = h.s.Restore(ctx, bundle, restore, map[model.MountID]store.Scope{h.scope.MountID: h.scope}); e != nil {
		t.Fatal(e)
	}
	st, e := store.Open(filepath.Join(restore, "mounts", string(h.scope.MountID)))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if e = st.BindScope(ctx, h.scope); e != nil {
		t.Fatal(e)
	}
	jobs, e := st.Pending(ctx)
	if e != nil || len(jobs) != 3 {
		t.Fatal(jobs, e)
	}
	unknown, e := st.Job(ctx, h.unknown)
	if e != nil || unknown.State != model.StatusAwaitingVerification {
		t.Fatal(unknown, e)
	}
	conflicts, e := st.Conflicts(ctx)
	if e != nil || len(conflicts) != 1 {
		t.Fatal(conflicts, e)
	}
	for _, j := range jobs {
		r, e := st.ReadBlob(ctx, j.Local)
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(r)
		r.Close()
		if e != nil || !strings.HasPrefix(string(b), "bytes:") {
			t.Fatal(string(b), e)
		}
	}
	if e = st.Validate(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestRollbackRefusesNewSchema(t *testing.T) {
	h := fixture(t)
	original, e := os.ReadFile(h.p.ConfigFile)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(h.p.ConfigFile, []byte(`{"schema_version":99}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = h.s.PrepareUpgrade(ctx, filepath.Join(h.root, "refused")); !errors.Is(e, config.ErrUnknownSchema) {
		t.Fatal(e)
	}
	got, e := os.ReadFile(h.p.ConfigFile)
	if e != nil || string(got) != `{"schema_version":99}` {
		t.Fatal(string(got), e)
	}
	os.WriteFile(h.p.ConfigFile, original, 0600)
	bundle := filepath.Join(h.root, "backup")
	m, e := h.s.PrepareUpgrade(ctx, bundle)
	if e != nil {
		t.Fatal(e)
	}
	m.SchemaVersion = 99
	b, _ := json.Marshal(m)
	if e = os.WriteFile(filepath.Join(bundle, "manifest.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = h.s.Restore(ctx, bundle, filepath.Join(h.root, "new"), map[model.MountID]store.Scope{h.scope.MountID: h.scope}); !errors.Is(e, ErrUnknownSchema) {
		t.Fatal(e)
	}
}
func TestLegacyImportDoesNotExecuteCommands(t *testing.T) {
	h := fixture(t)
	data, e := os.ReadFile("testdata/macos.settings")
	if e != nil {
		t.Fatal(e)
	}
	legacy, e := ReadLegacy(strings.NewReader(string(data)), LegacyMacOS)
	if e != nil {
		t.Fatal(e)
	}
	if legacy.Remote != "Legacy Feishu" || len(legacy.Ignored) == 0 {
		t.Fatal(legacy)
	}
	before, e := os.ReadFile(h.p.ConfigFile)
	if e != nil {
		t.Fatal(e)
	}
	r, e := ReadRcloneRemote(strings.NewReader("[Legacy Feishu]\ntype = feishu\nroot_folder_token = imported-root\ncommand = $(touch /never-run)\n"), legacy.Remote)
	if e != nil {
		t.Fatal(e)
	}
	plan, e := h.s.PreviewImport(ctx, legacy, r, ImportOptions{ProfileID: h.scope.ProfileID, Mountpoint: filepath.Join(h.root, "import-mount")})
	if e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(h.p.ConfigFile)
	if string(before) != string(after) {
		t.Fatal("preview wrote config")
	}
	if _, e = h.s.ApplyImport(ctx, plan, "wrong-digest", filepath.Join(h.root, "wrong-backup")); !errors.Is(e, ErrPreviewChanged) {
		t.Fatal(e)
	}
	result, e := h.s.ApplyImport(ctx, plan, plan.Digest, filepath.Join(h.root, "import-backup"))
	if e != nil || !result.ConfigChanged || !result.NeedsStartupHandoff {
		t.Fatal(result, e)
	}
	c, e := config.LoadWithPaths(h.p)
	if e != nil || len(c.Mounts) != 2 || c.Mounts[1].Autostart {
		t.Fatal(c, e)
	}
	if _, e = os.Stat("/never-execute-imported-command"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("import executed command", e)
	}
}
func TestIdentityChangeRequiresNewNamespace(t *testing.T) {
	h := fixture(t)
	bundle := filepath.Join(h.root, "backup")
	if _, e := h.s.PrepareUpgrade(ctx, bundle); e != nil {
		t.Fatal(e)
	}
	wrong := h.scope
	wrong.IdentityHash = "other"
	target := filepath.Join(h.root, "bad-restore")
	if _, e := h.s.Restore(ctx, bundle, target, map[model.MountID]store.Scope{h.scope.MountID: wrong}); !errors.Is(e, model.ErrIdentityChanged) {
		t.Fatal(e)
	}
	if _, e := os.Stat(target); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("published wrong identity", e)
	}
}
func TestUninstallKeepsUserData(t *testing.T) {
	h := fixture(t)
	before, e := os.ReadFile(h.p.ConfigFile)
	if e != nil {
		t.Fatal(e)
	}
	result, e := h.s.Uninstall(ctx)
	if e != nil || !result.PreserveData || !result.NeedsStartupHandoff {
		t.Fatal(result, e)
	}
	after, e := os.ReadFile(h.p.ConfigFile)
	if e != nil || string(before) != string(after) {
		t.Fatal(e)
	}
	if e = h.st.Validate(ctx); e != nil {
		t.Fatal(e)
	}
	j, e := h.st.Job(ctx, h.unknown)
	if e != nil || j.State != model.StatusAwaitingVerification {
		t.Fatal(j, e)
	}
}
func TestGuardRequiredAndBackupTamperRefused(t *testing.T) {
	h := fixture(t)
	if _, e := New(Options{Paths: h.p}); !errors.Is(e, ErrNotQuiescent) {
		t.Fatal(e)
	}
	h.guard.deny = true
	if _, e := h.s.PrepareUpgrade(ctx, filepath.Join(h.root, "denied")); !errors.Is(e, ErrNotQuiescent) {
		t.Fatal(e)
	}
	h.guard.deny = false
	bundle := filepath.Join(h.root, "backup")
	m, e := h.s.PrepareUpgrade(ctx, bundle)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(bundle, filepath.FromSlash(m.Files[0].Path)), []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = h.s.VerifyBackup(ctx, bundle); e == nil {
		t.Fatal("tampered bundle accepted")
	}
}
func TestPreviewCASAndLegacyWindowsData(t *testing.T) {
	h := fixture(t)
	b, e := os.ReadFile("testdata/windows.settings.json")
	if e != nil {
		t.Fatal(e)
	}
	l, e := ReadLegacy(strings.NewReader(string(b)), LegacyWindows)
	if e != nil || l.Remote != "Legacy Feishu" {
		t.Fatal(l, e)
	}
	r := LegacyRemote{Name: l.Remote, Type: "feishu", FolderToken: "imported"}
	p, e := h.s.PreviewImport(ctx, l, r, ImportOptions{ProfileID: h.scope.ProfileID, Mountpoint: filepath.Join(h.root, "import")})
	if e != nil {
		t.Fatal(e)
	}
	c, e := config.LoadWithPaths(h.p)
	if e != nil {
		t.Fatal(e)
	}
	c.Mounts[0].Name = "changed after preview"
	if e = config.SaveWithPaths(h.p, c); e != nil {
		t.Fatal(e)
	}
	if _, e = h.s.ApplyImport(ctx, p, p.Digest, filepath.Join(h.root, "backup")); !errors.Is(e, ErrPreviewChanged) {
		t.Fatal(e)
	}
}

type skippedGuard struct{}

func (skippedGuard) WithQuiescence(context.Context, func(context.Context) error) error { return nil }
func TestGuardCannotSilentlySkipExclusiveOperation(t *testing.T) {
	h := fixture(t)
	s, e := New(Options{Paths: h.p, Guard: skippedGuard{}, Mounts: []Mount{{Store: h.st, Dir: h.storeDir}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.PrepareUpgrade(ctx, filepath.Join(h.root, "never-created")); !errors.Is(e, ErrNotQuiescent) {
		t.Fatalf("no-op guard reported backup success: %v", e)
	}
}
func TestMaliciousLegacyCommandsAreLiteralData(t *testing.T) {
	h := fixture(t)
	marker := filepath.Join(h.root, "executed")
	input := "BACKEND='feishu'\nREMOTE='Existing'\nMOUNT_COMMAND='$(touch " + marker + ")'\n"
	l, e := ReadLegacy(strings.NewReader(input), LegacyMacOS)
	if e != nil || l.Remote != "Existing" {
		t.Fatal(l, e)
	}
	if _, e = os.Stat(marker); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("shell substitution executed", e)
	}
	if _, e = ReadLegacy(strings.NewReader("REMOTE='Existing'\ntouch "+marker+"\n"), LegacyMacOS); e == nil {
		t.Fatal("standalone shell command accepted")
	}
	if _, e = os.Stat(marker); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
}
func TestRecoveryAndGCReuseDurableStore(t *testing.T) {
	h := fixture(t)
	v, e := h.s.Recovery(ctx, h.scope.MountID)
	if e != nil || len(v.Jobs) != 3 || len(v.Conflicts) != 1 {
		t.Fatal(v, e)
	}
	if _, e = h.s.GC(ctx, h.scope.MountID, store.GCOptions{Now: time.Now().Add(100 * 24 * time.Hour), CleanBudgetBytes: 1}); e != nil {
		t.Fatal(e)
	}
	if e = h.st.Validate(ctx); e != nil {
		t.Fatal(e)
	}
	j, e := h.st.Job(ctx, h.unknown)
	if e != nil || j.State != model.StatusAwaitingVerification {
		t.Fatal(j, e)
	}
	var exported strings.Builder
	if e = h.s.ExportRecoveryBlob(ctx, h.scope.MountID, j.Local, &exported); e != nil || exported.String() != "bytes:unknown" {
		t.Fatal(exported.String(), e)
	}
}
func TestBackupSymlinkAndUnreviewedMutationRefused(t *testing.T) {
	h := fixture(t)
	bundle := filepath.Join(h.root, "backup")
	m, e := h.s.PrepareUpgrade(ctx, bundle)
	if e != nil {
		t.Fatal(e)
	}
	victim := filepath.Join(bundle, filepath.FromSlash(m.Files[0].Path))
	if e = os.Remove(victim); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(h.p.ConfigFile, victim); e != nil {
		t.Fatal(e)
	}
	if _, e = h.s.VerifyBackup(ctx, bundle); e == nil {
		t.Fatal("symlink accepted")
	}
	l := Legacy{Backend: "feishu", Remote: "Imported"}
	r := LegacyRemote{Name: l.Remote, Type: "feishu", FolderToken: "imported-root"}
	p, e := h.s.PreviewImport(ctx, l, r, ImportOptions{ProfileID: h.scope.ProfileID, Mountpoint: filepath.Join(h.root, "import")})
	if e != nil {
		t.Fatal(e)
	}
	p.After.Mounts[1].Mountpoint = filepath.Join(h.root, "changed")
	if _, e = h.s.ApplyImport(ctx, p, p.Digest, filepath.Join(h.root, "unreviewed")); !errors.Is(e, ErrPreviewChanged) {
		t.Fatal(e)
	}
}

func TestCanceledBackupDoesNotPublish(t *testing.T) {
	h := fixture(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	target := filepath.Join(h.root, "canceled")
	if _, e := h.s.PrepareUpgrade(c, target); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := os.Stat(target); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
}
func TestFutureDatabaseBackupIsReadOnlyRefused(t *testing.T) {
	h := fixture(t)
	bundle := filepath.Join(h.root, "backup")
	m, e := h.s.PrepareUpgrade(ctx, bundle)
	if e != nil {
		t.Fatal(e)
	}
	dbPath := filepath.Join(bundle, "mounts", string(h.scope.MountID), "store.db")
	db, e := bolt.Open(dbPath, 0600, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("999999")) }); e != nil {
		t.Fatal(e)
	}
	db.Close()
	m.Files, e = inventory(ctx, bundle)
	if e != nil {
		t.Fatal(e)
	}
	m.Digest = manifestDigest(m)
	b, _ := json.Marshal(m)
	if e = os.WriteFile(filepath.Join(bundle, "manifest.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	before, e := fileDigest(dbPath)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.s.VerifyBackup(ctx, bundle); e == nil {
		t.Fatal("future DB accepted")
	}
	after, e := fileDigest(dbPath)
	if e != nil || before != after {
		t.Fatal("readonly auditor changed future DB", e)
	}
}
