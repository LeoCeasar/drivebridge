package doccache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"

	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// Ensure backfills an old accepted snapshot without rewriting its identity or
// any entry/job references. Assets are re-derived from the original XML.
func Ensure(ctx context.Context, st *store.Store, source any, sn model.Snapshot) error {
	r, err := st.ReadBlob(ctx, sn.BlockMap)
	if err != nil {
		return err
	}
	var side model.DocSidecar
	err = json.NewDecoder(r).Decode(&side)
	closeErr := r.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if side.ProjectionVersion != 2 || len(side.Assets) == 0 {
		return nil
	}
	// Current snapshots already carry the complete bundle. Legacy snapshots
	// may have a revision-bound backfill saved separately. In either case,
	// reuse its local bodies before reading or decoding the original XML.
	cached := sn.DocMedia
	if len(cached) == 0 {
		cached, err = st.CachedDocMedia(ctx, sn.Token, sn.Revision)
		if err != nil {
			return err
		}
	}
	ready := true
	for _, asset := range side.Assets {
		blob := cached[asset.Path]
		if blob == "" {
			ready = false
			break
		}
		body, readErr := st.ReadBlob(ctx, blob)
		if errors.Is(readErr, os.ErrNotExist) {
			ready = false
			break
		}
		if readErr != nil {
			return readErr
		}
		if err := body.Close(); err != nil {
			return err
		}
	}
	if ready {
		return nil
	}
	r, err = st.ReadBlob(ctx, sn.XML)
	if err != nil {
		return err
	}
	xml, err := io.ReadAll(r)
	closeErr = r.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	view, err := markdown.DecodeWithOptions(xml, sn.Revision, markdown.Options{ProjectionVersion: 2, SourceURL: side.SourceURL, ReferenceMap: side.ReferenceMap})
	if err != nil {
		return err
	}
	prepared, err := Prepare(ctx, st, source, sn, view.Assets)
	if err != nil {
		return err
	}
	if maps.Equal(prepared.DocMedia, sn.DocMedia) {
		return nil
	}
	cached, err = st.CachedDocMedia(ctx, sn.Token, sn.Revision)
	if err != nil {
		return err
	}
	if maps.Equal(cached, prepared.DocMedia) {
		return nil
	}
	prepared.ID = ""
	_, err = st.SaveSnapshot(ctx, prepared)
	return err
}
