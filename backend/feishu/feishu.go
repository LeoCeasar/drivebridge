// Package feishu provides an rclone backend backed by lark-cli.
package feishu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/larkcli"
	"github.com/rclone/rclone/lib/readers"
)

const (
	defaultCommand = "lark-cli"
	urlSuffix      = ".url"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "feishu",
		Description: "Feishu/Lark Drive via lark-cli.",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:    "command",
			Default: defaultCommand,
			Help:    "Path to the lark-cli executable.",
		}, {
			Name:     "root_folder_token",
			Default:  "",
			Advanced: true,
			Help: `Folder token to use as the remote root.

Leave empty to use the current user's Drive root. When empty the backend
omits --folder-token for root listings because Feishu rejects an explicit
empty folder token with pagination parameters.`,
		}, {
			Name:     "temp_dir",
			Default:  "",
			Advanced: true,
			Help: `Directory for temporary upload and download files.

The backend passes lark-cli relative paths from this directory because
lark-cli rejects absolute local file paths for Drive upload/download.`,
		}, {
			Name:     "docs_as_url",
			Default:  true,
			Advanced: true,
			Help: `Expose Feishu online documents as read-only .url files.

Ordinary Drive files are readable and writable. Online documents, sheets,
bitables, slides, and shortcuts are represented as Windows InternetShortcut
files pointing at the Feishu web URL.`,
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	Command         string `config:"command"`
	RootFolderToken string `config:"root_folder_token"`
	TempDir         string `config:"temp_dir"`
	DocsAsURL       bool   `config:"docs_as_url"`
}

// Fs represents a Feishu Drive remote.
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	cli      *cliClient

	mu           sync.Mutex
	dirCache     map[string]string
	observations []*larkcli.FileObservation
}

// Object describes a Feishu Drive file.
type Object struct {
	fs          *Fs
	remote      string
	token       string
	fileType    string
	url         string
	size        int64
	modTime     time.Time
	virtual     bool
	content     []byte
	mu          sync.Mutex
	observation *larkcli.FileObservation
}

type driveItem larkcli.DriveItem

func parsePath(root string) string {
	return strings.Trim(root, "/")
}

// NewFs constructs an Fs from the path.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	return newFs(ctx, name, root, m, nil)
}

// NewFsWithRunner allows the product worker to reuse the native adapter with a
// bound managed runner. Nil is rejected rather than silently falling back.
func NewFsWithRunner(ctx context.Context, name, root string, m configmap.Mapper, runner *larkcli.Runner) (fs.Fs, error) {
	if _, ok := runner.ManagedIdentity(); !ok {
		return nil, larkcli.ErrIdentity
	}
	return newFs(ctx, name, root, m, runner)
}
func newFs(ctx context.Context, name, root string, m configmap.Mapper, runner *larkcli.Runner) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if opt.Command == "" {
		opt.Command = defaultCommand
	}

	if runner == nil {
		runner = larkcli.NewDirect(opt.Command)
	}
	root = parsePath(root)
	f := &Fs{
		name:     name,
		root:     root,
		opt:      *opt,
		cli:      &cliClient{drive: larkcli.NewDrive(runner, opt.TempDir)},
		dirCache: map[string]string{"": opt.RootFolderToken},
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	if root != "" {
		if _, err := f.resolveDir(ctx, root, false); err != nil {
			obj, objErr := f.objectFromPath(ctx, root)
			if objErr == nil {
				f.root = path.Dir(root)
				if f.root == "." {
					f.root = ""
				}
				_ = obj
				return f, fs.ErrorIsFile
			}
			return f, nil
		}
	}
	return f, nil
}

// Name of the remote.
func (f *Fs) Name() string { return f.name }

// Root of the remote.
func (f *Fs) Root() string { return f.root }

func (f *Fs) String() string {
	return fmt.Sprintf("Feishu Drive root '%s'", f.root)
}

func (f *Fs) Features() *fs.Features { return f.features }

func (f *Fs) Precision() time.Duration { return time.Second }

func (f *Fs) Hashes() hash.Set { return hash.Set(hash.None) }

func (f *Fs) abs(remote string) string {
	return strings.Trim(path.Join(f.root, remote), "/")
}

func splitPath(p string) (dir, leaf string) {
	dir, leaf = path.Split(strings.Trim(p, "/"))
	return strings.Trim(dir, "/"), leaf
}

func (f *Fs) resolveDir(ctx context.Context, dir string, create bool) (string, error) {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return f.opt.RootFolderToken, nil
	}
	f.mu.Lock()
	if token, ok := f.dirCache[dir]; ok {
		f.mu.Unlock()
		return token, nil
	}
	f.mu.Unlock()

	parent, leaf := splitPath(dir)
	parentToken, err := f.resolveDir(ctx, parent, create)
	if err != nil {
		return "", err
	}
	items, err := f.cli.list(ctx, parentToken)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.Type == "folder" && item.Name == leaf {
			f.cacheDir(dir, item.Token)
			return item.Token, nil
		}
	}
	if !create {
		return "", fs.ErrorDirNotFound
	}
	token, err := f.cli.createFolder(ctx, parentToken, leaf)
	if err != nil {
		return "", err
	}
	f.cacheDir(dir, token)
	return token, nil
}

func (f *Fs) cacheDir(dir, token string) {
	f.mu.Lock()
	f.dirCache[strings.Trim(dir, "/")] = token
	f.mu.Unlock()
}

func (f *Fs) flushDir(dir string) {
	dir = strings.Trim(dir, "/")
	f.mu.Lock()
	for k := range f.dirCache {
		if k == dir || strings.HasPrefix(k, dir+"/") {
			delete(f.dirCache, k)
		}
	}
	if dir == "" {
		f.dirCache[""] = f.opt.RootFolderToken
	}
	f.mu.Unlock()
}

func (f *Fs) listItems(ctx context.Context, dir string) ([]driveItem, error) {
	token, err := f.resolveDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	return f.cli.list(ctx, token)
}

// List the objects and directories in dir.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	absDir := f.abs(dir)
	items, err := f.listItems(ctx, absDir)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		remote := path.Join(dir, item.Name)
		modTime := item.modTime()
		switch item.Type {
		case "folder":
			f.cacheDir(path.Join(absDir, item.Name), item.Token)
			entries = append(entries, fs.NewDir(remote, modTime).SetID(item.Token))
		case "file":
			entries = append(entries, f.newObject(remote, item, false))
		default:
			if f.opt.DocsAsURL && item.URL != "" {
				entries = append(entries, f.newURLObject(remote+urlSuffix, item))
			}
		}
	}
	return entries, nil
}

func (f *Fs) objectFromPath(ctx context.Context, absPath string) (*Object, error) {
	parent, leaf := splitPath(absPath)
	items, err := f.listItems(ctx, parent)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Name == leaf {
			if item.Type == "folder" {
				return nil, fs.ErrorIsDir
			}
			if item.Type == "file" {
				remote := strings.Trim(strings.TrimPrefix(absPath, f.root), "/")
				return f.newObject(remote, item, false), nil
			}
		}
		if f.opt.DocsAsURL && item.Name+urlSuffix == leaf && item.URL != "" && item.Type != "file" && item.Type != "folder" {
			remote := strings.Trim(strings.TrimPrefix(absPath, f.root), "/")
			return f.newURLObject(remote, item), nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	obj, err := f.objectFromPath(ctx, f.abs(remote))
	if err != nil {
		return nil, err
	}
	if !obj.virtual {
		if err := obj.materialize(ctx); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

func (f *Fs) newObject(remote string, item driveItem, virtual bool) *Object {
	return &Object{
		fs:       f,
		remote:   remote,
		token:    item.Token,
		fileType: item.Type,
		url:      item.URL,
		size:     -1,
		modTime:  item.modTime(),
		virtual:  virtual,
	}
}

func (f *Fs) newURLObject(remote string, item driveItem) *Object {
	content := []byte("[InternetShortcut]\r\nURL=" + item.URL + "\r\n")
	o := f.newObject(remote, item, true)
	o.content = content
	o.size = int64(len(content))
	return o
}

// Put uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	existingObj, err := f.NewObject(ctx, src.Remote())
	switch err {
	case nil:
		return existingObj, existingObj.Update(ctx, in, src, options...)
	case fs.ErrorObjectNotFound:
		o := &Object{fs: f, remote: src.Remote(), size: -1, modTime: src.ModTime(ctx)}
		return o, o.Update(ctx, in, src, options...)
	default:
		return nil, err
	}
}

// Mkdir creates a directory.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.resolveDir(ctx, f.abs(dir), true)
	return err
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	absDir := f.abs(dir)
	if absDir == "" {
		return errors.New("can't remove root directory")
	}
	token, err := f.resolveDir(ctx, absDir, false)
	if err != nil {
		return err
	}
	items, err := f.cli.list(ctx, token)
	if err != nil {
		return err
	}
	if len(items) != 0 {
		return fs.ErrorDirectoryNotEmpty
	}
	if err := f.cli.delete(ctx, token, "folder"); err != nil {
		return err
	}
	f.flushDir(absDir)
	return nil
}

// Move renames or moves an object server-side through lark-cli.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok || srcObj.virtual {
		return nil, fs.ErrorCantMove
	}
	fs.Debugf(srcObj, "Moving Feishu object token=%q to %q", srcObj.token, remote)
	dstParent, dstLeaf := splitPath(f.abs(remote))
	dstParentToken, err := f.resolveDir(ctx, dstParent, true)
	if err != nil {
		return nil, err
	}
	srcParent, _ := splitPath(f.abs(srcObj.remote))
	srcParentToken, err := f.resolveDir(ctx, srcParent, false)
	if err != nil {
		return nil, err
	}
	if dstParentToken != srcParentToken {
		if err := f.cli.move(ctx, srcObj.token, "file", dstParentToken); err != nil {
			return nil, err
		}
	}
	if path.Base(srcObj.remote) != dstLeaf {
		if err := f.cli.rename(ctx, srcObj.token, "file", dstLeaf); err != nil {
			return nil, err
		}
	}
	f.flushDir(srcParent)
	f.flushDir(dstParent)
	newObj, err := f.NewObject(ctx, remote)
	if err != nil {
		fs.Debugf(srcObj, "Move completed but new object lookup for %q failed: %v", remote, err)
		return nil, err
	}
	fs.Debugf(srcObj, "Moved Feishu object token=%q to %q", srcObj.token, remote)
	return newObj, nil
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info { return o.fs }

func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

func (o *Object) Remote() string { return o.remote }

func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

func (o *Object) Size() int64 { o.mu.Lock(); defer o.mu.Unlock(); return o.size }

func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

func (o *Object) Storable() bool { return !o.virtual }

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	if o.virtual {
		return openBytes(o.content, options...), nil
	}
	if err := o.materialize(ctx); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	file, err := o.observation.Open()
	if err != nil {
		return nil, err
	}
	return applyRange(file, o.size, options...), nil
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if o.virtual {
		return fs.ErrorPermissionDenied
	}
	parent, leaf := splitPath(o.fs.abs(o.remote))
	parentToken, err := o.fs.resolveDir(ctx, parent, true)
	if err != nil {
		return err
	}
	token, size, url, err := o.fs.cli.upload(ctx, parentToken, o.token, leaf, in)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// Keep an already-open captured observation alive until Fs shutdown; future
	// opens bind a new observation after a successful synchronous upload.
	o.observation = nil
	o.token = token
	o.fileType = "file"
	o.url = url
	o.size = size
	o.modTime = src.ModTime(ctx)
	o.fs.flushDir(parent)
	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	if o.virtual {
		return fs.ErrorPermissionDenied
	}
	if err := o.fs.cli.delete(ctx, o.token, o.deleteType()); err != nil {
		return err
	}
	parent, _ := splitPath(o.fs.abs(o.remote))
	o.fs.flushDir(parent)
	return nil
}

func (o *Object) deleteType() string {
	if o.fileType != "" {
		return o.fileType
	}
	return "file"
}

func (o *Object) MimeType(ctx context.Context) string {
	if o.virtual {
		return "text/plain"
	}
	return fs.MimeTypeFromName(o.remote)
}

func (o *Object) ID() string { return o.token }

func openBytes(b []byte, options ...fs.OpenOption) io.ReadCloser {
	return applyRange(&bytesReadCloser{Reader: bytes.NewReader(b)}, int64(len(b)), options...)
}

type bytesReadCloser struct {
	*bytes.Reader
}

func (b *bytesReadCloser) Close() error {
	return nil
}

type seekReadCloser interface {
	io.ReadCloser
	io.Seeker
}

func applyRange(in io.ReadCloser, size int64, options ...fs.OpenOption) io.ReadCloser {
	var offset int64
	limit := int64(-1)
	for _, option := range options {
		switch x := option.(type) {
		case *fs.RangeOption:
			if size >= 0 {
				offset, limit = x.Decode(size)
			}
		case *fs.SeekOption:
			offset = x.Offset
		default:
			if option.Mandatory() {
				fs.Logf(nil, "Unsupported mandatory option: %v", option)
			}
		}
	}
	if offset > 0 {
		if seeker, ok := in.(seekReadCloser); ok {
			_, _ = seeker.Seek(offset, io.SeekStart)
		}
	}
	return readers.NewLimitedReadCloser(in, limit)
}

var (
	_ fs.Fs        = (*Fs)(nil)
	_ fs.Mover     = (*Fs)(nil)
	_ fs.Object    = (*Object)(nil)
	_ fs.MimeTyper = (*Object)(nil)
	_ fs.IDer      = (*Object)(nil)
)
