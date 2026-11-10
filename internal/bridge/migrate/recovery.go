package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
)

const ManifestSchema = 1

type InventoryFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type MountBackup struct {
	Scope     store.Scope `json:"scope"`
	Directory string      `json:"directory"`
}
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	CreatedAt     time.Time       `json:"created_at"`
	ConfigSchema  int             `json:"config_schema"`
	Mounts        []MountBackup   `json:"mounts"`
	Before        []InventoryFile `json:"before"`
	Files         []InventoryFile `json:"files"`
	Digest        string          `json:"digest"`
}

func manifestDigest(m Manifest) string { m.Digest = ""; return digest(m) }
func fileDigest(p string) (string, error) {
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func record(ctx context.Context, root, rel string) (InventoryFile, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := platform.CheckNoSymlinks(p); err != nil {
		return InventoryFile{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return InventoryFile{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return InventoryFile{}, err
	}
	if !info.Mode().IsRegular() {
		return InventoryFile{}, errors.New("backup contains nonregular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, f})
	return InventoryFile{rel, n, hex.EncodeToString(h.Sum(nil))}, err
}
func writeFile(ctx context.Context, p string, src io.Reader) error {
	if err := platform.EnsurePrivateDir(filepath.Dir(p)); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = platform.SecurePrivate(p, false); err == nil {
		_, err = io.Copy(f, contextReader{ctx, src})
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func copyFile(ctx context.Context, src, dst string) error {
	if err := platform.CheckNoSymlinks(src); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("source is not regular")
	}
	return writeFile(ctx, dst, f)
}
func validRelative(p string) bool {
	if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, v := range strings.Split(p, "/") {
		if v == ".." || v == "" {
			return false
		}
	}
	return true
}
func within(a, b string) bool {
	r, err := filepath.Rel(a, b)
	return err == nil && (r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))))
}
func (s *Service) destination(target string) error {
	if !filepath.IsAbs(target) {
		return errors.New("absolute destination required")
	}
	if err := platform.CheckNoSymlinks(target); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return errors.New("destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, m := range s.mounts {
		if within(m.Dir, target) || within(target, m.Dir) {
			return errors.New("destination overlaps accepted store")
		}
	}
	if within(target, s.paths.ConfigFile) || within(filepath.Dir(s.paths.ConfigFile), target) {
		return errors.New("destination overlaps configuration")
	}
	return nil
}
func stageDirectory(target string) (string, error) {
	if err := platform.EnsurePrivateDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	p, err := os.MkdirTemp(filepath.Dir(target), ".migration-")
	if err == nil {
		err = platform.SecurePrivate(p, true)
	}
	return p, err
}
func syncTree(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if err := platform.SyncDir(d); err != nil {
			return err
		}
	}
	return nil
}
func publish(stage, target string) error {
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		return errors.New("destination changed during staging")
	}
	if err := syncTree(stage); err != nil {
		return err
	}
	if err := platform.ReplaceFile(stage, target); err != nil {
		return err
	}
	return platform.SyncDir(filepath.Dir(target))
}

// PrepareUpgrade validates known schemas and publishes a complete immutable backup.
// There is no invented schema transition: the current supported version is one.
func (s *Service) PrepareUpgrade(ctx context.Context, target string) (m Manifest, err error) {
	err = s.quiesce(ctx, func(ctx context.Context) error { var er error; m, er = s.backup(ctx, target); return er })
	return
}
func (s *Service) backup(ctx context.Context, target string) (Manifest, error) {
	c, err := s.validateCurrent(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if err = s.destination(target); err != nil {
		return Manifest{}, err
	}
	stage, err := stageDirectory(target)
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(stage)
	manifest := Manifest{SchemaVersion: ManifestSchema, CreatedAt: time.Now().UTC(), ConfigSchema: c.SchemaVersion}
	if err = copyFile(ctx, s.paths.ConfigFile, filepath.Join(stage, "config.json")); err != nil {
		return manifest, err
	}
	before, err := record(ctx, filepath.Dir(s.paths.ConfigFile), filepath.Base(s.paths.ConfigFile))
	if err != nil {
		return manifest, err
	}
	before.Path = "config.json"
	manifest.Before = append(manifest.Before, before)
	for _, mount := range s.mounts {
		scope, err := mount.Store.Scope(ctx)
		if err != nil {
			return manifest, err
		}
		rel := path.Join("mounts", string(scope.MountID))
		manifest.Mounts = append(manifest.Mounts, MountBackup{scope, rel})
		dbDst := filepath.Join(stage, filepath.FromSlash(rel), "store.db")
		if err = platform.EnsurePrivateDir(filepath.Dir(dbDst)); err != nil {
			return manifest, err
		}
		db, err := os.OpenFile(dbDst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return manifest, err
		}
		err = mount.Store.Backup(ctx, db)
		if err == nil {
			err = db.Sync()
		}
		closeErr := db.Close()
		if err != nil {
			return manifest, err
		}
		if closeErr != nil {
			return manifest, closeErr
		}
		before, err := record(ctx, mount.Dir, "store.db")
		if err != nil {
			return manifest, err
		}
		before.Path = path.Join(rel, "store.db")
		manifest.Before = append(manifest.Before, before)
		blobDir := filepath.Join(mount.Dir, "blobs")
		if err = filepath.WalkDir(blobDir, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := platform.CheckNoSymlinks(p); err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			r, err := filepath.Rel(mount.Dir, p)
			if err != nil {
				return err
			}
			logical := path.Join(rel, filepath.ToSlash(r))
			if err = copyFile(ctx, p, filepath.Join(stage, filepath.FromSlash(logical))); err != nil {
				return err
			}
			b, err := record(ctx, mount.Dir, filepath.ToSlash(r))
			if err != nil {
				return err
			}
			b.Path = logical
			manifest.Before = append(manifest.Before, b)
			return nil
		}); err != nil {
			return manifest, err
		}
		if err = platform.EnsurePrivateDir(filepath.Join(stage, filepath.FromSlash(rel), "blobs")); err != nil {
			return manifest, err
		}
	}
	manifest.Files, err = inventory(ctx, stage)
	if err != nil {
		return manifest, err
	}
	sort.Slice(manifest.Before, func(i, j int) bool { return manifest.Before[i].Path < manifest.Before[j].Path })
	sort.Slice(manifest.Mounts, func(i, j int) bool { return manifest.Mounts[i].Scope.MountID < manifest.Mounts[j].Scope.MountID })
	manifest.Digest = manifestDigest(manifest)
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	if err = writeFile(ctx, filepath.Join(stage, "manifest.json"), strings.NewReader(string(b))); err != nil {
		return manifest, err
	}
	if _, err = s.VerifyBackup(ctx, stage); err != nil {
		return manifest, err
	}
	if err = publish(stage, target); err != nil {
		return manifest, err
	}
	return manifest, nil
}
func inventory(ctx context.Context, root string) (out []InventoryFile, err error) {
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, er error) error {
		if er != nil {
			return er
		}
		if er = platform.CheckNoSymlinks(p); er != nil {
			return er
		}
		if d.IsDir() {
			return nil
		}
		r, er := filepath.Rel(root, p)
		if er != nil {
			return er
		}
		rel := filepath.ToSlash(r)
		if rel == "manifest.json" {
			return nil
		}
		v, er := record(ctx, root, rel)
		if er != nil {
			return er
		}
		out = append(out, v)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return
}

// VerifyBackup is read-only: unknown schema, symlinks, inventory/hash errors and
// identity/reference mismatches fail before any destination is created.
func (s *Service) VerifyBackup(ctx context.Context, root string) (Manifest, error) {
	if !filepath.IsAbs(root) {
		return Manifest{}, errors.New("absolute backup path required")
	}
	if err := platform.CheckNoSymlinks(root); err != nil {
		return Manifest{}, err
	}
	p := filepath.Join(root, "manifest.json")
	if err := platform.CheckNoSymlinks(p); err != nil {
		return Manifest{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	var m Manifest
	dec := json.NewDecoder(io.LimitReader(f, 64<<20))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return m, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return m, errors.New("trailing backup manifest")
	}
	if m.SchemaVersion != ManifestSchema || m.ConfigSchema != config.SchemaVersion {
		return m, ErrUnknownSchema
	}
	if m.Digest == "" || manifestDigest(m) != m.Digest {
		return m, errors.New("backup inventory digest mismatch")
	}
	seen := map[string]bool{}
	for _, v := range m.Files {
		hash, er := hex.DecodeString(v.SHA256)
		if !validRelative(v.Path) || v.Path == "manifest.json" || seen[v.Path] || v.Size < 0 || er != nil || len(hash) != sha256.Size {
			return m, errors.New("invalid backup inventory")
		}
		seen[v.Path] = true
	}
	if !seen["config.json"] {
		return m, errors.New("backup missing config")
	}
	actual, err := inventory(ctx, root)
	if err != nil {
		return m, err
	}
	if digest(actual) != digest(m.Files) {
		return m, errors.New("backup file inventory or content differs")
	}
	paths := config.Paths{ConfigFile: filepath.Join(root, "config.json"), StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache")}
	c, err := config.LoadWithPaths(paths)
	if err != nil {
		return m, err
	}
	known := map[model.MountID]bool{}
	for _, mount := range m.Mounts {
		expected := path.Join("mounts", string(mount.Scope.MountID))
		if mount.Directory != expected || known[mount.Scope.MountID] || !validRelative(mount.Directory) {
			return m, errors.New("invalid backup mount namespace")
		}
		known[mount.Scope.MountID] = true
		if err = binding(c, mount.Scope); err != nil {
			return m, err
		}
		if err = store.ValidateDirectory(ctx, filepath.Join(root, filepath.FromSlash(mount.Directory)), mount.Scope); err != nil {
			return m, err
		}
	}
	for _, v := range m.Files {
		if strings.HasPrefix(v.Path, "mounts/") {
			parts := strings.Split(v.Path, "/")
			if len(parts) < 3 || !known[model.MountID(parts[1])] {
				return m, errors.New("unbound store file in backup")
			}
		} else if v.Path != "config.json" {
			return m, errors.New("unexpected file in backup")
		}
	}
	return m, nil
}

// Restore publishes a verified copy into a new, unused namespace. Activation and
// replacement of a stopped installation are separate manager operations.
func (s *Service) Restore(ctx context.Context, backup, target string, expected map[model.MountID]store.Scope) (m Manifest, err error) {
	err = s.quiesce(ctx, func(ctx context.Context) error {
		var er error
		m, er = s.VerifyBackup(ctx, backup)
		if er != nil {
			return er
		}
		if len(expected) != len(m.Mounts) {
			return model.ErrIdentityChanged
		}
		for _, mount := range m.Mounts {
			if expected[mount.Scope.MountID] != mount.Scope {
				return model.ErrIdentityChanged
			}
		}
		if er = s.destination(target); er != nil {
			return er
		}
		if within(backup, target) || within(target, backup) {
			return errors.New("restore overlaps backup")
		}
		stage, er := stageDirectory(target)
		if er != nil {
			return er
		}
		defer os.RemoveAll(stage)
		for _, file := range m.Files {
			if er = copyFile(ctx, filepath.Join(backup, filepath.FromSlash(file.Path)), filepath.Join(stage, filepath.FromSlash(file.Path))); er != nil {
				return er
			}
		}
		for _, mount := range m.Mounts {
			if er = platform.EnsurePrivateDir(filepath.Join(stage, filepath.FromSlash(mount.Directory), "blobs")); er != nil {
				return er
			}
		}
		if er = copyFile(ctx, filepath.Join(backup, "manifest.json"), filepath.Join(stage, "manifest.json")); er != nil {
			return er
		}
		if _, er = s.VerifyBackup(ctx, stage); er != nil {
			return er
		}
		return publish(stage, target)
	})
	return
}

type RecoveryView struct {
	Scope        store.Scope
	Jobs         []model.Job
	Conflicts    []model.Conflict
	Namespace    []model.NamespaceOp
	Creates      []model.CreateOp
	Publications []model.PublishCandidate
}

func (s *Service) mount(ctx context.Context, id model.MountID) (Mount, store.Scope, error) {
	for _, m := range s.mounts {
		sc, err := m.Store.Scope(ctx)
		if err != nil {
			return Mount{}, sc, err
		}
		if sc.MountID == id {
			return m, sc, nil
		}
	}
	return Mount{}, store.Scope{}, fmt.Errorf("mount not found: %s", id)
}
func (s *Service) Recovery(ctx context.Context, id model.MountID) (v RecoveryView, err error) {
	m, sc, err := s.mount(ctx, id)
	if err != nil {
		return v, err
	}
	v.Scope = sc
	if v.Jobs, err = m.Store.Pending(ctx); err != nil {
		return
	}
	if v.Conflicts, err = m.Store.Conflicts(ctx); err != nil {
		return
	}
	if v.Namespace, err = m.Store.PendingNamespace(ctx); err != nil {
		return
	}
	if v.Creates, err = m.Store.PendingCreates(ctx); err != nil {
		return
	}
	v.Publications, err = m.Store.PendingPublications(ctx)
	return
}
func (s *Service) GC(ctx context.Context, id model.MountID, o store.GCOptions) (result store.GCResult, err error) {
	err = s.quiesce(ctx, func(ctx context.Context) error {
		m, _, err := s.mount(ctx, id)
		if err != nil {
			return err
		}
		if err = m.Store.Validate(ctx); err != nil {
			return err
		}
		result, err = m.Store.GC(ctx, o)
		return err
	})
	return
}
func (s *Service) ExportRecoveryBlob(ctx context.Context, id model.MountID, blob model.BlobID, w io.Writer) error {
	m, _, err := s.mount(ctx, id)
	if err != nil {
		return err
	}
	r, err := m.Store.ReadBlob(ctx, blob)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, contextReader{ctx, r})
	return err
}
