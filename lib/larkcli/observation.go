package larkcli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileObservation owns a private complete-download lease; no server revision is
// asserted. Open returns the same captured bytes, never another remote download.
type FileObservation struct {
	Token, ObservationID, SHA256 string
	Size                         int64
	StartedAt, CompletedAt       time.Time
	mu                           sync.Mutex
	path, dir                    string
	readers                      int
	closed                       bool
}

func (o *FileObservation) Open() (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, os.ErrClosed
	}
	f, err := os.Open(o.path)
	if err != nil {
		return nil, err
	}
	o.readers++
	return &observationReader{File: f, owner: o}, nil
}
func (o *FileObservation) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	if o.readers == 0 {
		return os.RemoveAll(o.dir)
	}
	return nil
}

type observationReader struct {
	*os.File
	owner *FileObservation
	once  sync.Once
	err   error
}

func (r *observationReader) Close() error {
	r.once.Do(func() {
		r.err = r.File.Close()
		r.owner.mu.Lock()
		defer r.owner.mu.Unlock()
		r.owner.readers--
		if r.owner.closed && r.owner.readers == 0 {
			removeErr := os.RemoveAll(r.owner.dir)
			if r.err == nil {
				r.err = removeErr
			}
		}
	})
	return r.err
}

func observationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}
func (d *Drive) ObserveFile(ctx context.Context, token string) (obs *FileObservation, err error) {
	if token == "" {
		return nil, ErrProtocol
	}
	dir, err := os.MkdirTemp(d.tempDir, "drivebridge-observation-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if obs == nil {
			os.RemoveAll(dir)
		}
	}()
	started := time.Now().UTC()
	r, err := d.runner.Run(ctx, Request{Args: []string{"drive", "+download", "--file-token", token, "--output", "download", "--overwrite"}, Dir: dir, Timeout: transferTimeout})
	if err != nil {
		return nil, err
	}
	var raw struct {
		Path string          `json:"saved_path"`
		Size json.RawMessage `json:"size_bytes"`
	}
	if singleJSON(r.Data, &raw) != nil || raw.Path == "" {
		return nil, ErrProtocol
	}
	size, err := strictInt(raw.Size, false)
	if err != nil {
		return nil, err
	}
	p := raw.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, ErrProtocol
	}
	resolvedRoot, rootErr := filepath.EvalSymlinks(dir)
	resolvedFile, fileErr := filepath.EvalSymlinks(p)
	if rootErr != nil || fileErr != nil {
		return nil, ErrProtocol
	}
	resolvedRel, relErr := filepath.Rel(resolvedRoot, resolvedFile)
	if relErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return nil, ErrProtocol
	}
	current := dir
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrProtocol
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil, ErrProtocol
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx: ctx, reader: f})
	if err != nil || n != size {
		return nil, ErrProtocol
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	id, err := observationID()
	if err != nil {
		return nil, err
	}
	return &FileObservation{Token: token, ObservationID: id, SHA256: hex.EncodeToString(h.Sum(nil)), Size: size, StartedAt: started, CompletedAt: time.Now().UTC(), path: p, dir: dir}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
