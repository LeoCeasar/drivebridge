package overlay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type Object struct {
	mu      sync.RWMutex
	f       *Fs
	e       model.Entry
	m       store.Metadata
	size    int64
	virtual []byte
}

func (f *Fs) object(ctx context.Context, e model.Entry) (*Object, error) {
	m, err := f.store.Metadata(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	if m.Tombstone {
		return nil, fs.ErrorObjectNotFound
	}
	if m.VirtualType != "" {
		content := virtualContent(m)
		return &Object{f: f, e: e, m: m, size: int64(len(content)), virtual: content}, nil
	}
	size := int64(-1)
	if e.Visible != "" {
		r, err := f.store.ReadBlob(ctx, e.Visible)
		if err != nil {
			return nil, err
		}
		stat, ok := r.(interface{ Stat() (os.FileInfo, error) })
		if !ok {
			r.Close()
			return nil, errors.New("durable blob does not expose size")
		}
		info, err := stat.Stat()
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		size = info.Size()
	}
	return &Object{f: f, e: e, m: m, size: size}, nil
}
func (o *Object) Fs() fs.Info    { return o.f }
func (o *Object) Remote() string { o.mu.RLock(); defer o.mu.RUnlock(); return o.e.Remote }
func (o *Object) String() string { return o.Remote() }
func (o *Object) ModTime(context.Context) time.Time {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.m.ModTime
}
func (o *Object) Size() int64    { o.mu.RLock(); defer o.mu.RUnlock(); return o.size }
func (o *Object) Storable() bool { o.mu.RLock(); defer o.mu.RUnlock(); return o.m.VirtualType == "" }
func (o *Object) ID() string     { o.mu.RLock(); defer o.mu.RUnlock(); return string(o.e.ID) }
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t != hash.SHA256 {
		return "", hash.ErrUnsupported
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.virtual != nil {
		sum := sha256.Sum256(o.virtual)
		return hex.EncodeToString(sum[:]), nil
	}
	return string(o.e.Visible), nil
}
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, err := o.f.store.Entry(ctx, o.e.ID)
	if err != nil {
		return err
	}
	if e.Remote != o.e.Remote {
		return fs.ErrorObjectNotFound
	}
	if err = o.f.requireWritable(ctx, e); err != nil {
		return err
	}
	_, err = o.f.store.ObserveEntry(ctx, store.Observation{Remote: e.Remote, Kind: e.Kind, ModTime: t})
	if err == nil {
		o.m.ModTime = t
	}
	return err
}
func checkOptions(opts []fs.OpenOption, read bool) error {
	for _, opt := range opts {
		switch opt.(type) {
		case *fs.RangeOption, *fs.SeekOption:
			if read {
				continue
			}
		}
		if opt.Mandatory() {
			return errors.New("unsupported mandatory open option")
		}
	}
	return nil
}

type readLease struct {
	ctx context.Context
	io.Reader
	closer io.Closer
}

func (r *readLease) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
func (r *readLease) Close() error { return r.closer.Close() }
func (o *Object) Open(ctx context.Context, opts ...fs.OpenOption) (io.ReadCloser, error) {
	if err := checkOptions(opts, true); err != nil {
		return nil, err
	}
	o.mu.RLock()
	e, size, generated := o.e, o.size, o.virtual
	o.mu.RUnlock()
	if generated != nil {
		return rangeReader(ctx, io.NopCloser(bytes.NewReader(generated)), size, opts)
	}
	if e.Visible == "" {
		resolved, err := o.f.MaterializeManaged(ctx, e.Remote)
		if err != nil {
			return nil, err
		}
		m := resolved.(*Object)
		m.mu.RLock()
		e, size = m.e, m.size
		metadata := m.m
		m.mu.RUnlock()
		o.mu.Lock()
		if o.e.Visible == "" {
			o.e = e
			o.size = size
			o.m = metadata
		} else {
			e, size = o.e, o.size
		}
		o.mu.Unlock()
	}
	r, err := o.f.store.ReadBlob(ctx, e.Visible)
	o.f.touch(e)
	if err != nil {
		return nil, err
	}
	return rangeReader(ctx, r, size, opts)
}

func rangeReader(ctx context.Context, r io.ReadCloser, size int64, opts []fs.OpenOption) (io.ReadCloser, error) {
	var err error
	offset, limit := int64(0), int64(-1)
	ranges := 0
	for _, opt := range opts {
		switch x := opt.(type) {
		case *fs.RangeOption:
			ranges++
			offset, limit = x.Decode(size)
			if x.Start >= 0 && x.End >= 0 && x.End < x.Start {
				r.Close()
				return nil, errors.New("invalid range")
			}
		case *fs.SeekOption:
			ranges++
			offset = x.Offset
		}
	}
	if ranges > 1 || offset < 0 {
		r.Close()
		return nil, errors.New("invalid or multiple byte ranges")
	}
	if offset > size {
		offset = size
	}
	if limit < 0 || limit > size-offset {
		limit = size - offset
	}
	if seek, ok := r.(io.Seeker); ok {
		if _, err = seek.Seek(offset, io.SeekStart); err != nil {
			r.Close()
			return nil, err
		}
	} else if _, err = io.CopyN(io.Discard, r, offset); err != nil {
		r.Close()
		return nil, err
	}
	return &readLease{ctx: ctx, Reader: io.LimitReader(r, limit), closer: r}, nil
}
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, opts ...fs.OpenOption) error {
	if src == nil {
		return errors.New("missing object metadata")
	}
	if err := checkOptions(opts, false); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	current, err := o.f.store.Entry(ctx, o.e.ID)
	if err != nil {
		return err
	}
	if current.Remote != o.e.Remote {
		return fs.ErrorObjectNotFound
	}
	if err = o.f.requireWritable(ctx, current); err != nil {
		return err
	}
	expectedGeneration := current.Generation
	if current.Kind == model.KindDocx {
		in = io.LimitReader(in, store.LargeSnapshotBytes+1)
	}
	blob, err := o.f.store.PutBlob(ctx, in)
	if err != nil {
		return err
	}
	r, err := o.f.store.ReadBlob(ctx, blob)
	if err != nil {
		return err
	}
	defer r.Close()
	info, err := r.(interface{ Stat() (os.FileInfo, error) }).Stat()
	if err != nil {
		return err
	}
	if current.Kind == model.KindDocx && info.Size() > store.LargeSnapshotBytes {
		return model.ErrUnsupportedContent
	}
	// Re-read after importing: identical Flush retries reuse accepted content,
	// while differing writes use generation CAS and cannot silently overwrite a race.
	current, err = o.f.store.Entry(ctx, o.e.ID)
	if err != nil {
		return err
	}
	if current.Remote != o.e.Remote {
		return fs.ErrorObjectNotFound
	}
	if current.Local != blob {
		if current.Generation != expectedGeneration {
			return model.ErrGenerationChanged
		}
		current, _, err = o.f.store.StagePreparedBlob(ctx, current.ID, &expectedGeneration, blob, o.f.LocalOnly(current.Remote))
		if err != nil {
			return err
		}
	}
	metadata := src.ModTime(ctx)
	if !metadata.IsZero() {
		if _, err = o.f.store.ObserveEntry(ctx, store.Observation{Remote: current.Remote, Kind: current.Kind, ModTime: metadata}); err != nil {
			return &CommittedError{Err: err}
		}
	}
	updated, err := o.f.object(ctx, current)
	if err != nil {
		return &CommittedError{Err: err}
	}
	o.e = updated.e
	o.m = updated.m
	o.size = updated.size
	return nil
}
func (o *Object) Remove(ctx context.Context) error { return o.f.remove(ctx, o.Remote(), false) }

func (f *Fs) MaterializeManaged(ctx context.Context, remote string) (fs.Object, error) {
	if isDocAssetPath(remote) {
		o, err := f.newDocAsset(ctx, remote)
		if err != nil {
			return nil, err
		}
		r, err := o.Open(ctx)
		if err != nil {
			return nil, err
		}
		if err := r.Close(); err != nil {
			return nil, err
		}
		return o, nil
	}
	if allowed, err := f.SyncPathAllowed(ctx, remote); err != nil {
		return nil, err
	} else if !allowed {
		return nil, fs.ErrorObjectNotFound
	}
	e, err := f.find(ctx, remote)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if e.Kind == model.KindFolder {
		return nil, fs.ErrorIsDir
	}
	metadata, err := f.store.Metadata(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	if metadata.VirtualType != "" || e.Visible != "" {
		return f.object(ctx, e)
	}
	if e.FrozenBy != "" {
		return nil, store.ErrFrozen
	}
	if e.Token == "" {
		return nil, errors.New("unmaterialized local object has no accepted content")
	}
	f.materialMu.Lock()
	if flight := f.materializing[e.ID]; flight != nil {
		f.materialMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			return flight.object, flight.err
		}
	}
	flight := &materialization{done: make(chan struct{})}
	f.materializing[e.ID] = flight
	f.materialMu.Unlock()
	flight.object, flight.err = f.materialize(ctx, e)
	f.materialMu.Lock()
	delete(f.materializing, e.ID)
	close(flight.done)
	f.materialMu.Unlock()
	return flight.object, flight.err
}
func mapNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fs.ErrorObjectNotFound
	}
	return err
}
func (f *Fs) materialize(ctx context.Context, e model.Entry) (fs.Object, error) {
	current, err := f.store.Entry(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	if current.Visible != "" {
		return f.object(ctx, current)
	}
	var sn model.Snapshot
	switch e.Kind {
	case model.KindFile:
		obs, err := f.source.ObserveFile(ctx, e.Token)
		if err != nil {
			return nil, err
		}
		if obs.Close == nil {
			return nil, errors.New("observation missing ownership lease")
		}
		defer obs.Close()
		if obs.Token != e.Token {
			return nil, model.ErrIdentityChanged
		}
		if obs.Open == nil || obs.Size < 0 {
			return nil, errors.New("invalid observation")
		}
		r, err := obs.Open()
		if err != nil {
			return nil, err
		}
		sn, err = f.store.SaveFileSnapshot(ctx, e.Token, obs.Identity, r)
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if sn.Size != obs.Size {
			return nil, errors.New("observation declared size mismatch")
		}
		current, err = f.store.AdoptFileSnapshot(ctx, e.ID, e.Generation, sn.ID)
		if err != nil {
			return f.racedMaterialization(ctx, e, err)
		}
	case model.KindDocx:
		doc, err := f.source.FetchDoc(ctx, e.Token, "")
		if err != nil {
			return nil, err
		}
		if doc.Token != e.Token {
			return nil, model.ErrIdentityChanged
		}
		if doc.Revision == "" {
			return nil, errors.New("document missing revision")
		}
		view, err := markdown.DecodeWithOptions(doc.XML, doc.Revision, markdown.Options{SourceURL: doc.SourceURL, ReferenceMap: doc.ReferenceMap})
		if err != nil {
			return nil, err
		}
		xml, err := f.store.PutBlob(ctx, bytes.NewReader(view.XML))
		if err != nil {
			return nil, err
		}
		md, err := f.store.PutBlob(ctx, bytes.NewReader(view.Markdown))
		if err != nil {
			return nil, err
		}
		sidecar, err := json.Marshal(model.DocSidecar{ProjectionVersion: view.ProjectionVersion, Assets: view.Assets, Blocks: view.Blocks, SourceURL: view.SourceURL, ReferenceMap: view.ReferenceMap})
		if err != nil {
			return nil, err
		}
		blockMap, err := f.store.PutBlob(ctx, bytes.NewReader(sidecar))
		if err != nil {
			return nil, err
		}
		sn = model.Snapshot{Token: e.Token, Revision: doc.Revision, XML: xml, Markdown: md, BlockMap: blockMap, Size: int64(len(view.Markdown))}
		candidate, err := f.store.OfferRemote(ctx, e.ID, e.Generation, sn, md)
		if err != nil {
			return f.racedMaterialization(ctx, e, err)
		}
		current, err = f.store.CommitPublication(ctx, candidate)
		if err != nil {
			return f.racedMaterialization(ctx, e, err)
		}
	default:
		return nil, model.ErrUnsupportedContent
	}
	return f.object(ctx, current)
}
func (f *Fs) racedMaterialization(ctx context.Context, e model.Entry, cause error) (fs.Object, error) {
	current, err := f.store.Entry(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	if current.Remote == e.Remote && current.Visible != "" {
		return f.object(ctx, current)
	}
	return nil, cause
}

var _ fs.Object = (*Object)(nil)
