package larkcli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

const maxDocMediaSize int64 = 64 << 20

// Media tokens are opaque identifiers, never provider URLs, file paths or flags.
var docMediaToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,255}$`)

// The pinned CLI appends an extension inferred from Content-Type or
// Content-Disposition when --output has no extension. Only these image suffixes
// may extend our requested basename, and their MIME must match the file bytes.
var docMediaExtensionMIME = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp", ".ico": "image/x-icon",
}

// OpenDocMedia downloads an image through the same managed runner and account
// locks as document reads. Only a complete, bounded image in our private output
// directory is exposed. The caller owns the reader and must Close it; context
// cancellation also releases the temporary file. Stat and Seek are available on
// the returned reader for consumers that need its size or a byte range.
func (d *Drive) OpenDocMedia(ctx context.Context, token string) (out io.ReadCloser, err error) {
	if !docMediaToken.MatchString(token) {
		return nil, ErrProtocol
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(d.tempDir, "drivebridge-media-")
	if err != nil {
		return nil, mediaFileError(err)
	}
	ownedDir := dir
	defer func() {
		if out == nil {
			_ = os.RemoveAll(ownedDir)
		}
	}()
	// Canonicalize the newly created directory before setting the child's cwd;
	// CLI saved_path may be absolute, including on systems with /tmp aliases.
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, mediaFileError(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, mediaFileError(err)
	}
	root, err := os.Lstat(dir)
	if err != nil {
		return nil, mediaFileError(err)
	}
	if !privateMediaDir(root) {
		return nil, ErrProtocol
	}
	response, err := d.runner.Run(ctx, Request{Args: []string{"docs", "+media-download", "--token", token, "--output", "media"}, Dir: dir, Timeout: transferTimeout})
	if err != nil {
		return nil, err
	}
	currentRoot, err := os.Lstat(dir)
	if err != nil || !privateMediaDir(currentRoot) || !os.SameFile(root, currentRoot) {
		return nil, ErrProtocol
	}
	var raw struct {
		Path string          `json:"saved_path"`
		Size json.RawMessage `json:"size_bytes"`
	}
	if singleJSON(response.Data, &raw) != nil {
		return nil, ErrProtocol
	}
	size, err := strictInt(raw.Size, false)
	if err != nil || size <= 0 || size > maxDocMediaSize {
		return nil, ErrProtocol
	}
	extension := filepath.Ext(raw.Path)
	extensionMIME, allowedExtension := docMediaExtensionMIME[strings.ToLower(extension)]
	if extension != "" && !allowedExtension {
		return nil, ErrProtocol
	}
	name := "media" + extension
	path := filepath.Join(dir, name)
	if raw.Path != name && raw.Path != path {
		return nil, ErrProtocol
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil, ErrProtocol
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, mediaFileError(err)
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = file.Close()
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() != size {
		return nil, ErrProtocol
	}
	var header [512]byte
	n, err := io.ReadFull(contextReader{ctx: ctx, reader: file}, header[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, mediaFileError(err)
	}
	detectedMIME := http.DetectContentType(header[:n])
	if extensionMIME != "" && extensionMIME != detectedMIME {
		return nil, ErrProtocol
	}
	switch detectedMIME {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "image/x-icon":
	default:
		return nil, ErrUnsupportedCapability
	}
	// Count to EOF rather than trusting metadata or an HTTP content-type. A
	// failed or truncated transfer never becomes a readable asset lease.
	remaining, err := io.Copy(io.Discard, io.LimitReader(contextReader{ctx: ctx, reader: file}, maxDocMediaSize+1))
	if err != nil {
		return nil, mediaFileError(err)
	}
	if int64(n)+remaining != size {
		return nil, ErrProtocol
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, mediaFileError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader := &docMediaReader{File: file, ctx: ctx, dir: dir}
	stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
	reader.mu.Lock()
	if reader.closed {
		stop()
	} else {
		reader.stop = stop
	}
	reader.mu.Unlock()
	adopted = true
	if err := ctx.Err(); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return reader, nil
}

func privateMediaDir(info os.FileInfo) bool {
	return info != nil && info.IsDir() && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
}

// Keep filesystem paths and upstream prose out of caller-visible diagnostics.
func mediaFileError(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Type: "media_file", Cause: err}
}

type docMediaReader struct {
	*os.File
	ctx    context.Context
	dir    string
	mu     sync.Mutex
	closed bool
	stop   func() bool
	err    error
}

func (r *docMediaReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.File.Read(p)
}
func (r *docMediaReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.err
	}
	r.closed = true
	if r.stop != nil {
		r.stop()
	}
	r.err = mediaFileError(errors.Join(r.File.Close(), os.RemoveAll(r.dir)))
	return r.err
}
