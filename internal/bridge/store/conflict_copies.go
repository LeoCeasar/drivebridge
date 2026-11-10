package store

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
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	bolt "go.etcd.io/bbolt"
)

var ErrConflictCopyChanged = errors.New("existing conflict copy was modified; preserved without overwrite")

type ConflictCopies struct{ Directory, Base, Local, Remote, Draft, Manifest string }
type ConflictCopyFile struct {
	Role      string       `json:"role"`
	Name      string       `json:"name,omitempty"`
	Blob      model.BlobID `json:"blob,omitempty"`
	SHA256    string       `json:"sha256,omitempty"`
	Size      int64        `json:"size"`
	Available bool         `json:"available"`
}
type ConflictCopyManifest struct {
	SchemaVersion  int                `json:"schema_version"`
	ConflictID     model.ConflictID   `json:"conflict_id"`
	EntryID        model.EntryID      `json:"entry_id"`
	Generation     uint64             `json:"generation"`
	RemoteRevision string             `json:"remote_revision"`
	Kind           model.Kind         `json:"kind"`
	Files          []ConflictCopyFile `json:"files"`
}

func copyExtension(e model.Entry) string {
	if e.Kind == model.KindDocx {
		return ".md"
	}
	ext := strings.ToLower(path.Ext(e.Remote))
	switch ext {
	case ".txt", ".md", ".csv", ".json", ".xml", ".log", ".yaml", ".yml", ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".zip", ".docx", ".xlsx":
		return ext
	}
	return ".bin"
}
func copyDescriptor(c model.Conflict, e model.Entry) ConflictCopyManifest {
	m := ConflictCopyManifest{SchemaVersion: 1, ConflictID: c.ID, EntryID: c.EntryID, Generation: c.Generation, RemoteRevision: c.RemoteRevision, Kind: e.Kind}
	ext := copyExtension(e)
	for _, r := range []struct {
		role string
		blob model.BlobID
	}{{"base", c.Base}, {"local", c.Local}, {"remote", c.Remote}, {"draft", c.Draft}} {
		f := ConflictCopyFile{Role: r.role, Blob: r.blob, Available: r.blob != ""}
		if f.Available {
			f.Name = f.Role + ext
			f.SHA256 = string(f.Blob)
		}
		m.Files = append(m.Files, f)
	}
	return m
}
func copiesAt(root string, m ConflictCopyManifest) ConflictCopies {
	out := ConflictCopies{Directory: root, Manifest: filepath.Join(root, "manifest.json")}
	for _, f := range m.Files {
		if !f.Available {
			continue
		}
		p := filepath.Join(root, f.Name)
		switch f.Role {
		case "base":
			out.Base = p
		case "local":
			out.Local = p
		case "remote":
			out.Remote = p
		case "draft":
			out.Draft = p
		}
	}
	return out
}

// ExportConflictCopies creates immutable, human-readable copies from captured
// conflict references. A changed draft/input creates a new version directory;
// modified existing copies are never replaced. It performs no network operation.
func (s *Store) ExportConflictCopies(ctx context.Context, id model.ConflictID) (ConflictCopies, error) {
	if !canonicalUUID(string(id)) {
		return ConflictCopies{}, errors.New("canonical conflict UUID required")
	}
	var c model.Conflict
	var e model.Entry
	if err := s.view(ctx, func(tx *bolt.Tx) error {
		if err := get(tx, "conflicts", string(id), &c); err != nil {
			return err
		}
		var err error
		e, err = entryTx(tx, c.EntryID)
		return err
	}); err != nil {
		return ConflictCopies{}, err
	}
	expected := copyDescriptor(c, e)
	encoded, _ := json.Marshal(expected)
	fingerprint := sha256.Sum256(encoded)
	parent := filepath.Join(s.dir, "conflicts", string(id))
	if err := platform.EnsurePrivateDir(parent); err != nil {
		return ConflictCopies{}, err
	}
	lp := filepath.Join(parent, "export.lock")
	if err := platform.CheckNoSymlinks(lp); err != nil {
		return ConflictCopies{}, err
	}
	lock := flock.New(lp)
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return ConflictCopies{}, err
	}
	if !ok {
		if err := ctx.Err(); err != nil {
			return ConflictCopies{}, err
		}
		return ConflictCopies{}, ErrBusy
	}
	defer lock.Unlock()
	if err = platform.SecurePrivate(lp, false); err != nil {
		return ConflictCopies{}, err
	}
	final := filepath.Join(parent, hex.EncodeToString(fingerprint[:]))
	out := copiesAt(final, expected)
	if _, err = os.Lstat(final); err == nil {
		if err = s.verifyConflictCopies(ctx, final, expected); err != nil {
			return out, errors.Join(ErrConflictCopyChanged, err)
		}
		if err = s.syncCopyDirectories(final, parent); err != nil {
			return out, err
		}
		return out, nil
	} else if !os.IsNotExist(err) {
		return ConflictCopies{}, err
	}
	stage, err := os.MkdirTemp(parent, ".export-")
	if err != nil {
		return ConflictCopies{}, err
	}
	defer os.RemoveAll(stage)
	if err = platform.SecurePrivate(stage, true); err != nil {
		return ConflictCopies{}, err
	}
	manifest := expected
	for i, ref := range expected.Files {
		if !ref.Available {
			continue
		}
		if err = ctx.Err(); err != nil {
			return ConflictCopies{}, err
		}
		if err = s.checkBlob(ref.Blob); err != nil {
			return ConflictCopies{}, err
		}
		reader, err := s.ReadBlob(ctx, ref.Blob)
		if err != nil {
			return ConflictCopies{}, err
		}
		file, err := os.OpenFile(filepath.Join(stage, ref.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			reader.Close()
			return ConflictCopies{}, err
		}
		hash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(file, hash), copyContextReader{ctx, reader})
		readerErr := reader.Close()
		if copyErr == nil {
			copyErr = readerErr
		}
		if copyErr == nil && hex.EncodeToString(hash.Sum(nil)) != string(ref.Blob) {
			copyErr = fmt.Errorf("source blob hash mismatch: %s", ref.Role)
		}
		if copyErr == nil {
			copyErr = platform.SecurePrivate(file.Name(), false)
		}
		if copyErr == nil {
			copyErr = s.io.SyncFile(file)
		}
		closeErr := file.Close()
		if copyErr != nil {
			return ConflictCopies{}, copyErr
		}
		if closeErr != nil {
			return ConflictCopies{}, closeErr
		}
		manifest.Files[i].Size = n
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ConflictCopies{}, err
	}
	mf, err := os.CreateTemp(stage, ".manifest-")
	if err != nil {
		return ConflictCopies{}, err
	}
	if err = platform.SecurePrivate(mf.Name(), false); err == nil {
		_, err = mf.Write(append(data, '\n'))
	}
	if err == nil {
		err = s.io.SyncFile(mf)
	}
	closeErr := mf.Close()
	if err != nil {
		return ConflictCopies{}, err
	}
	if closeErr != nil {
		return ConflictCopies{}, closeErr
	}
	if err = s.io.Rename(mf.Name(), filepath.Join(stage, "manifest.json")); err != nil {
		return ConflictCopies{}, err
	}
	if err = s.io.SyncDir(stage); err != nil {
		return ConflictCopies{}, err
	}
	if err = s.verifyConflictCopies(ctx, stage, expected); err != nil {
		return ConflictCopies{}, err
	}
	if _, err = os.Lstat(final); !os.IsNotExist(err) {
		return ConflictCopies{}, ErrConflictCopyChanged
	}
	if err = s.io.Rename(stage, final); err != nil {
		return ConflictCopies{}, err
	}
	if err = s.syncCopyDirectories(final, parent); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Store) syncCopyDirectories(final, parent string) error {
	for _, p := range []string{final, parent, filepath.Dir(parent), s.dir} {
		if err := s.io.SyncDir(p); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) verifyConflictCopies(ctx context.Context, dir string, expected ConflictCopyManifest) error {
	if err := platform.CheckNoSymlinks(dir); err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := platform.CheckNoSymlinks(manifestPath); err != nil {
		return err
	}
	info, err := os.Stat(manifestPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("invalid copy manifest file")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var m ConflictCopyManifest
	if err = json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.SchemaVersion != 1 || m.ConflictID != expected.ConflictID || m.EntryID != expected.EntryID || m.Generation != expected.Generation || m.RemoteRevision != expected.RemoteRevision || m.Kind != expected.Kind || len(m.Files) != len(expected.Files) {
		return errors.New("copy manifest identity changed")
	}
	for i, ref := range expected.Files {
		actual := m.Files[i]
		if actual.Role != ref.Role || actual.Name != ref.Name || actual.Blob != ref.Blob || actual.Available != ref.Available || actual.SHA256 != ref.SHA256 {
			return errors.New("copy manifest references changed")
		}
		if !ref.Available {
			if actual.Size != 0 {
				return errors.New("unobserved copy has bytes")
			}
			continue
		}
		p := filepath.Join(dir, ref.Name)
		if err = platform.CheckNoSymlinks(p); err != nil {
			return err
		}
		info, err = os.Stat(p)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != actual.Size {
			return errors.New("copy size changed")
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, copyContextReader{ctx, f})
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != actual.SHA256 {
			return errors.New("copy content changed")
		}
	}
	return nil
}

type copyContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r copyContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
