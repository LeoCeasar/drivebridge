package overlay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

const docAssetsDirectory = ".drivebridge-assets"

// DocMediaSource is optional: ordinary metadata/file sources need not implement it.
// A media read owns its temporary download until Close. Stat or io.Seeker lets
// the overlay determine its size without copying an already downloaded body.
type DocMediaSource interface {
	OpenDocMedia(context.Context, string) (io.ReadCloser, error)
}

func (s cloudSource) OpenDocMedia(ctx context.Context, token string) (io.ReadCloser, error) {
	return s.c.OpenDocMedia(ctx, token)
}
func (s *accountSource) OpenDocMedia(ctx context.Context, token string) (io.ReadCloser, error) {
	source, ok := s.Source.(DocMediaSource)
	if !ok {
		return nil, model.ErrUnsupportedContent
	}
	return source.OpenDocMedia(ctx, token)
}

// ValidateManagedOpen keeps readonly assets out of the writable VFS cache,
// including unknown children that must never become ordinary upload candidates.
func (f *Fs) ValidateManagedOpen(_ context.Context, remote string, write bool) error {
	if write && isDocAssetPath(remote) {
		return fs.ErrorPermissionDenied
	}

	return nil
}

func isDocAssetPath(remote string) bool {
	for _, part := range strings.Split(remote, "/") {
		if foldName(part) == docAssetsDirectory {
			return true
		}
	}
	return false
}

// assetDirectoryParent accepts exactly the reserved directory or one flat child.
func assetDirectoryParent(remote string) (parent string, directory bool, ok bool) {
	parts := strings.Split(remote, "/")
	for i, p := range parts {
		if p == docAssetsDirectory {
			if len(parts)-i > 2 {
				return "", false, false
			}
			return strings.Join(parts[:i], "/"), len(parts)-i == 1, true
		}
	}
	return "", false, false
}

type docAssetReference struct {
	asset   model.DocAsset
	owner   model.Entry
	modTime time.Time
	blob    model.BlobID
	size    int64
}

// docAssets reads only immutable sidecars bound to current accepted checkpoints
// in this Store. Merely saving an unrelated/unpublished snapshot grants nothing.
func (f *Fs) docAssets(ctx context.Context, parent string) (map[string]docAssetReference, error) {
	return f.readDocAssets(ctx, parent, false)
}

// A normal directory listing only needs to know whether the reserved asset
// directory exists; stop at the first verified reference without statting media.
func (f *Fs) readDocAssets(ctx context.Context, parent string, firstOnly bool) (map[string]docAssetReference, error) {
	if _, err := f.directory(ctx, parent); err != nil {
		return nil, err
	}
	policy, err := f.accountPolicy(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := f.store.Entries(ctx, parent)
	if err != nil {
		return nil, err
	}
	result := make(map[string]docAssetReference)
	for _, e := range entries {
		if e.Kind != model.KindDocx || !policy.allows(e.Remote) {
			continue
		}
		assetMetadata, err := f.store.Metadata(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		checkpoint := e.RemoteCheckpoint
		restoredAssets := assetMetadata.HistorySnapshot != "" && assetMetadata.HistoryMediaBody == e.Local
		if restoredAssets {
			checkpoint = assetMetadata.HistorySnapshot
		}
		if checkpoint == "" {
			continue
		}
		sn, err := f.store.Snapshot(ctx, checkpoint)
		if err != nil {
			return nil, err
		}
		if (!restoredAssets && sn.Token != e.Token) || sn.BlockMap == "" {
			continue
		}
		assets, err := f.verifiedDocAssets(ctx, sn)
		if err != nil {
			return nil, err
		}
		if len(assets) == 0 {
			continue
		}
		metadata, err := f.store.Metadata(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if metadata.Tombstone || restoredAssets && (metadata.HistorySnapshot != checkpoint || metadata.HistoryMediaBody != e.Local) {
			continue
		}
		current, err := f.store.Entry(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if current.Remote != e.Remote || current.Token != e.Token || current.RemoteCheckpoint != e.RemoteCheckpoint {
			continue
		}
		if firstOnly && len(assets) > 0 {
			asset := assets[0]
			result[path.Join(parent, asset.Path)] = docAssetReference{asset: asset, owner: e}
			return result, nil
		}
		cached := sn.DocMedia
		if len(cached) < len(assets) {
			cached, err = f.store.CachedDocMedia(ctx, sn.Token, sn.Revision)
			if err != nil {
				return nil, err
			}
		}
		for _, asset := range assets {
			remote := path.Join(parent, asset.Path)
			ref := docAssetReference{asset: asset, owner: e, modTime: metadata.ModTime, size: -1}
			if b := cached[asset.Path]; b != "" {
				r, readErr := f.store.ReadBlob(ctx, b)
				if readErr == nil {
					info, statErr := r.(interface{ Stat() (os.FileInfo, error) }).Stat()
					closeErr := r.Close()
					if statErr != nil {
						return nil, statErr
					}
					if closeErr != nil {
						return nil, closeErr
					}
					ref.blob, ref.size = b, info.Size()
				} else if !errors.Is(readErr, os.ErrNotExist) {
					return nil, readErr
				}
			}
			result[remote] = ref
		}
	}
	return result, nil
}

// verifiedDocAssets caches the immutable XML/sidecar agreement, never the
// mutable entry authority or body leases. Each caller still revalidates its
// accepted checkpoint, tombstone and history restore state.
func (f *Fs) verifiedDocAssets(ctx context.Context, sn model.Snapshot) ([]model.DocAsset, error) {
	f.docManifestMu.Lock()
	assets, ok := f.docManifests[sn.ID]
	f.docManifestMu.Unlock()
	if ok {
		return assets, nil
	}
	r, err := f.store.ReadBlob(ctx, sn.BlockMap)
	if err != nil {
		return nil, err
	}
	var sidecar model.DocSidecar
	err = json.NewDecoder(r).Decode(&sidecar)
	closeErr := r.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if sidecar.ProjectionVersion == 2 && len(sidecar.Assets) > 0 && sn.XML != "" {
		raw, err := f.store.ReadBlob(ctx, sn.XML)
		if err != nil {
			return nil, err
		}
		xml, err := io.ReadAll(raw)
		closeErr = raw.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		view, decodeErr := markdown.DecodeWithOptions(xml, sn.Revision, markdown.Options{ProjectionVersion: 2, SourceURL: sidecar.SourceURL, ReferenceMap: sidecar.ReferenceMap})
		if decodeErr == nil {
			original := make(map[model.DocAsset]bool, len(view.Assets))
			for _, asset := range view.Assets {
				original[asset] = true
			}
			for _, asset := range sidecar.Assets {
				if original[asset] && asset.Token != "" && !strings.ContainsAny(asset.Token, "/\\:\x00 \t\n\r") && asset.Path == markdown.AssetPath(asset.Token, asset.Name, asset.MIME) {
					assets = append(assets, asset)
				}
			}
		}
	}
	f.docManifestMu.Lock()
	defer f.docManifestMu.Unlock()
	if f.docManifests == nil {
		f.docManifests = make(map[model.SnapshotID][]model.DocAsset)
	}
	if existing, ok := f.docManifests[sn.ID]; ok {
		return existing, nil
	}
	const manifestLimit = 128
	if len(f.docManifestOrder) == manifestLimit {
		delete(f.docManifests, f.docManifestOrder[0])
		f.docManifestOrder = f.docManifestOrder[1:]
	}
	f.docManifests[sn.ID] = assets
	f.docManifestOrder = append(f.docManifestOrder, sn.ID)
	return assets, nil
}

func (f *Fs) listDocAssets(ctx context.Context, remote string) (fs.DirEntries, error) {
	parent, directory, ok := assetDirectoryParent(remote)
	if !ok {
		return nil, fs.ErrorDirNotFound
	}
	if !directory {
		return nil, fs.ErrorIsFile
	}
	assets, err := f.docAssets(ctx, parent)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return nil, fs.ErrorDirNotFound
	}
	result := make(fs.DirEntries, 0, len(assets))
	for name, ref := range assets {
		result = append(result, &docAssetObject{f: f, remote: name, ref: ref, size: ref.size})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Remote() < result[j].Remote() })
	return result, nil
}
func (f *Fs) newDocAsset(ctx context.Context, remote string) (*docAssetObject, error) {
	if err := validPath(remote, false); err != nil {
		return nil, err
	}
	parent, directory, ok := assetDirectoryParent(remote)
	if !ok {
		return nil, fs.ErrorObjectNotFound
	}
	assets, err := f.docAssets(ctx, parent)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if directory {
		if len(assets) > 0 {
			return nil, fs.ErrorIsDir
		}
		return nil, fs.ErrorObjectNotFound
	}
	ref, ok := assets[remote]
	if !ok {
		return nil, fs.ErrorObjectNotFound
	}
	return &docAssetObject{f: f, remote: remote, ref: ref, size: ref.size}, nil
}

type docAssetObject struct {
	mu     sync.RWMutex
	f      *Fs
	remote string
	ref    docAssetReference
	size   int64
}

func (o *docAssetObject) Fs() fs.Info                       { return o.f }
func (o *docAssetObject) Remote() string                    { return o.remote }
func (o *docAssetObject) String() string                    { return o.remote }
func (o *docAssetObject) ID() string                        { return "doc-media:" + o.remote }
func (o *docAssetObject) ModTime(context.Context) time.Time { return o.ref.modTime }
func (o *docAssetObject) Size() int64                       { o.mu.RLock(); defer o.mu.RUnlock(); return o.size }
func (*docAssetObject) Storable() bool                      { return false }
func (o *docAssetObject) MimeType(context.Context) string   { return o.ref.asset.MIME }
func (*docAssetObject) Hash(context.Context, hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}
func (*docAssetObject) SetModTime(context.Context, time.Time) error { return fs.ErrorPermissionDenied }
func (*docAssetObject) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return fs.ErrorPermissionDenied
}
func (*docAssetObject) Remove(context.Context) error { return fs.ErrorPermissionDenied }
func (o *docAssetObject) Open(ctx context.Context, opts ...fs.OpenOption) (io.ReadCloser, error) {
	if err := checkOptions(opts, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := o.f.newDocAsset(ctx, o.remote)
	if err != nil {
		return nil, err
	}
	if current.ref.asset.Token != o.ref.asset.Token {
		return nil, fs.ErrorObjectNotFound
	}
	if current.ref.blob == "" {
		// Upgrade pre-existing caches as one document bundle, not a standalone
		// image download. New documents always arrive with complete bodies.
		if err := o.f.ensureDocImages(ctx, current.ref.owner); err != nil {
			return nil, err
		}
		current, err = o.f.newDocAsset(ctx, o.remote)
		if err != nil {
			return nil, err
		}
	}
	if current.ref.blob == "" {
		return nil, store.ErrNotFound
	}
	r, err := o.f.store.ReadBlob(ctx, current.ref.blob)
	if err != nil {
		return nil, err
	}
	size := current.ref.size
	o.mu.Lock()
	o.size = size
	o.mu.Unlock()
	if err := o.f.store.TouchBodyAccess(ctx, current.ref.owner.ID, time.Now().UTC()); err != nil {
		r.Close()
		return nil, err
	}
	return rangeReader(ctx, r, size, opts)
}

// Streams without size metadata get an ephemeral read lease, never a durable
// cache blob. The production transport already exposes Stat and needs no copy.
func sizedDocMedia(ctx context.Context, r io.ReadCloser) (io.ReadCloser, int64, error) {
	if stat, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
		info, err := stat.Stat()
		if err != nil {
			r.Close()
			return nil, 0, err
		}
		if info.Size() < 0 {
			r.Close()
			return nil, 0, errors.New("invalid media size")
		}
		return r, info.Size(), nil
	}
	if seeker, ok := r.(io.Seeker); ok {
		size, err := seeker.Seek(0, io.SeekEnd)
		if err == nil {
			_, err = seeker.Seek(0, io.SeekStart)
		}
		if err != nil || size < 0 {
			r.Close()
			if err == nil {
				err = errors.New("invalid media size")
			}
			return nil, 0, err
		}
		return r, size, nil
	}
	temp, err := os.CreateTemp("", "drivebridge-media-read-*")
	if err != nil {
		r.Close()
		return nil, 0, err
	}
	lease := &temporaryDocMedia{File: temp}
	size, err := io.Copy(temp, &readLease{ctx: ctx, Reader: r, closer: r})
	closeErr := r.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		_, err = temp.Seek(0, io.SeekStart)
	}
	if err != nil {
		lease.Close()
		return nil, 0, err
	}
	return lease, size, nil
}

type temporaryDocMedia struct{ *os.File }

func (r *temporaryDocMedia) Close() error {
	err := r.File.Close()
	removeErr := os.Remove(r.Name())
	return errors.Join(err, removeErr)
}

var _ fs.Object = (*docAssetObject)(nil)
