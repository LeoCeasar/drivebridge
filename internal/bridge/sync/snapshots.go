package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/doccache"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
)

func (e *Engine) read(ctx context.Context, id model.BlobID) ([]byte, error) {
	if id == "" {
		return nil, nil
	}
	r, err := e.store.ReadBlob(ctx, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
func (e *Engine) saveDoc(ctx context.Context, d cloud.Document) (model.Snapshot, model.DocView, error) {
	return e.saveDocProjection(ctx, d, 0)
}

func (e *Engine) saveDocProjection(ctx context.Context, d cloud.Document, version int) (model.Snapshot, model.DocView, error) {
	if d.Token == "" || d.Revision == "" || len(d.Warnings) > 0 {
		return model.Snapshot{}, model.DocView{}, model.ErrUnsupportedContent
	}
	v, err := markdown.DecodeWithOptions(d.XML, d.Revision, markdown.Options{ProjectionVersion: version, SourceURL: d.SourceURL, ReferenceMap: d.ReferenceMap})
	if err != nil {
		return model.Snapshot{}, v, err
	}
	x, err := e.store.PutBlob(ctx, bytes.NewReader(v.XML))
	if err != nil {
		return model.Snapshot{}, v, err
	}
	m, err := e.store.PutBlob(ctx, bytes.NewReader(v.Markdown))
	if err != nil {
		return model.Snapshot{}, v, err
	}
	side, err := json.Marshal(model.DocSidecar{ProjectionVersion: v.ProjectionVersion, Assets: v.Assets, Blocks: v.Blocks, SourceURL: v.SourceURL, ReferenceMap: v.ReferenceMap})
	if err != nil {
		return model.Snapshot{}, v, err
	}
	b, err := e.store.PutBlob(ctx, bytes.NewReader(side))
	if err != nil {
		return model.Snapshot{}, v, err
	}
	sn, err := doccache.Prepare(ctx, e.store, e.source, model.Snapshot{Token: d.Token, Revision: d.Revision, XML: x, Markdown: m, BlockMap: b, Size: int64(len(v.Markdown))}, v.Assets)
	if err != nil {
		return model.Snapshot{}, v, err
	}
	sn, err = e.store.SaveSnapshot(ctx, sn)
	return sn, v, err
}
func (e *Engine) loadDoc(ctx context.Context, id model.SnapshotID) (model.Snapshot, model.DocView, error) {
	sn, err := e.store.Snapshot(ctx, id)
	if err != nil {
		return sn, model.DocView{}, err
	}
	x, err := e.read(ctx, sn.XML)
	if err != nil {
		return sn, model.DocView{}, err
	}
	side, err := e.read(ctx, sn.BlockMap)
	if err != nil {
		return sn, model.DocView{}, err
	}
	var b model.DocSidecar
	if json.Unmarshal(side, &b) != nil {
		return sn, model.DocView{}, model.ErrUnsupportedContent
	}
	// Missing version denotes the original projection. A pending job must keep
	// its original interpretation rather than silently acquiring new syntax.
	version := b.ProjectionVersion
	if version == 0 {
		version = 1
	}
	if version != 1 && version != 2 && version != 3 {
		return sn, model.DocView{}, model.ErrUnsupportedContent
	}
	v, err := markdown.DecodeWithOptions(x, sn.Revision, markdown.Options{ProjectionVersion: version, SourceURL: b.SourceURL, ReferenceMap: b.ReferenceMap})
	if err != nil {
		return sn, v, err
	}
	stored, err := e.read(ctx, sn.Markdown)
	if err != nil {
		return sn, v, err
	}
	if !bytes.Equal(stored, v.Markdown) {
		return sn, v, model.ErrUnsupportedContent
	}
	return sn, v, err
}
func (e *Engine) fetchDoc(ctx context.Context, token string) (model.Snapshot, model.DocView, error) {
	return e.fetchDocProjection(ctx, token, 0)
}

func (e *Engine) fetchDocProjection(ctx context.Context, token string, version int) (model.Snapshot, model.DocView, error) {
	d, err := e.source.FetchDoc(ctx, token, "")
	if err != nil {
		return model.Snapshot{}, model.DocView{}, err
	}
	if version == 2 && markdown.HasYAMLFrontMatterXML(d.XML) {
		version = 3
	}
	return e.saveDocProjection(ctx, d, version)
}

// Conflict review and resolution use the pending entry's interpretation. A
// clean refresh may migrate projection versions, but never the draft's baseline.
func (e *Engine) fetchEntryDoc(ctx context.Context, entry model.Entry) (model.Snapshot, model.DocView, error) {
	version := 0
	if entry.RemoteCheckpoint != "" {
		_, prior, err := e.loadDoc(ctx, entry.RemoteCheckpoint)
		if err != nil {
			return model.Snapshot{}, model.DocView{}, err
		}
		version = prior.ProjectionVersion
	}
	return e.fetchDocProjection(ctx, entry.Token, version)
}
func (e *Engine) observeFile(ctx context.Context, token string) (model.Snapshot, error) {
	o, err := e.source.ObserveFile(ctx, token)
	if err != nil {
		return model.Snapshot{}, err
	}
	if o.Reader == nil {
		return model.Snapshot{}, model.ErrUnsupportedContent
	}
	defer o.Reader.Close()
	if o.Token != token || o.Size < 0 {
		return model.Snapshot{}, model.ErrIdentityChanged
	}
	sn, err := e.store.SaveFileSnapshot(ctx, token, o.Identity, o.Reader)
	if err != nil {
		return sn, err
	}
	if sn.Size != o.Size {
		return sn, errors.New("download count mismatch")
	}
	return sn, nil
}
