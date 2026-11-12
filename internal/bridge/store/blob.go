package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/shirou/gopsutil/v4/disk"
)

var blobName = regexp.MustCompile(`^[0-9a-f]{64}$`)

func defaultSpacePolicy(ctx context.Context, dir string, size int64) error {
	if size < LargeSnapshotBytes {
		return nil
	}
	d, err := disk.UsageWithContext(ctx, dir)
	if err != nil {
		return err
	}
	if d.Free < MinimumFreeBytes {
		return ErrSpace
	}
	return nil
}
func (s *Store) blobPath(id model.BlobID) string { return filepath.Join(s.dir, "blobs", string(id)) }
func (s *Store) PutBlob(ctx context.Context, src io.Reader) (model.BlobID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	bound, closed := s.bound, s.closed
	s.mu.Unlock()
	if !bound {
		return "", ErrUnbound
	}
	if closed {
		return "", errors.New("store closed")
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "blobs"), ".prepared-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	hash := sha256.New()
	var size int64
	buf := make([]byte, 128*1024)
	for {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		var n int
		n, err = src.Read(buf)
		if n > 0 {
			size += int64(n)
			if e := s.space(ctx, s.dir, size); e != nil {
				return "", e
			}
			if _, e := f.Write(buf[:n]); e != nil {
				return "", e
			}
			hash.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = s.io.SyncFile(f); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	id := model.BlobID(hex.EncodeToString(hash.Sum(nil)))
	// Final publication and its fresh preparation timestamp must exclude GC.
	// Hashing/copying remains outside the Store lock, even for large files.
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound {
		return "", ErrUnbound
	}
	if s.closed {
		return "", errors.New("store closed")
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	target := s.blobPath(id)
	// Rewriting an equal immutable blob is safe and re-establishes durability after an earlier sync failure.
	if err = platform.CheckNoSymlinks(target); err != nil {
		return "", err
	}
	if err = s.io.Rename(name, target); err != nil {
		// Windows refuses to replace a file another handle holds open (readers
		// open blobs without FILE_SHARE_DELETE). The name is the SHA-256 of the
		// content, so an existing regular file of the same length already holds
		// exactly these bytes: keep it, drop the redundant temp file and still
		// refresh the grace timestamp and directory durability below.
		if !replaceDenied(err) || !s.sameBlobExists(target, size) {
			return "", err
		}
	}
	// Equal content can reuse an old hash while its accepting transaction is
	// still waiting. Refresh its grace before letting any collector observe it.
	now := time.Now().UTC()
	if err = os.Chtimes(target, now, now); err != nil {
		return "", err
	}
	if err = s.io.SyncDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	return id, nil
}

type leasedReader struct {
	*os.File
	once    sync.Once
	release func()
}

func (r *leasedReader) Close() error {
	var err error
	r.once.Do(func() { err = r.File.Close(); r.release() })
	return err
}
func (s *Store) ReadBlob(ctx context.Context, id model.BlobID) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !blobName.MatchString(string(id)) {
		return nil, fmt.Errorf("invalid blob ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound {
		return nil, ErrUnbound
	}
	if s.closed {
		return nil, errors.New("store closed")
	}
	if err := platform.CheckNoSymlinks(s.blobPath(id)); err != nil {
		return nil, err
	}
	f, err := os.Open(s.blobPath(id))
	if err != nil {
		return nil, err
	}
	s.leases[id]++
	return &leasedReader{File: f, release: func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.leases[id]--
		if s.leases[id] == 0 {
			delete(s.leases, id)
		}
	}}, nil
}
func (s *Store) checkBlob(id model.BlobID) error {
	if id == "" {
		return nil
	}
	if !blobName.MatchString(string(id)) {
		return errors.New("invalid blob ID")
	}
	if err := platform.CheckNoSymlinks(s.blobPath(id)); err != nil {
		return err
	}
	f, err := os.Stat(s.blobPath(id))
	if err != nil {
		return err
	}
	if !f.Mode().IsRegular() {
		return errors.New("blob is not regular file")
	}
	return nil
}

// sameBlobExists reports whether target is already a regular, non-symlink file
// of the expected length. Content addressing makes that sufficient to reuse it.
func (s *Store) sameBlobExists(target string, size int64) bool {
	info, err := os.Lstat(target)
	return err == nil && info.Mode().IsRegular() && info.Size() == size
}
