// Package doccache prepares a complete document/image cache before publication.
package doccache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type MediaSource interface {
	OpenDocMedia(context.Context, string) (io.ReadCloser, error)
}

const maxMediaBytes = 64 << 20

var ErrIncomplete = errors.New("document image cache incomplete")

// Prepare caches all assets before returning a publishable snapshot. Same-
// revision cache bodies are reused without network I/O. Partial failures never
// produce accepted references; PutBlob provides atomic durable file publication.
func Prepare(ctx context.Context, st *store.Store, source any, sn model.Snapshot, assets []model.DocAsset) (result model.Snapshot, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(ErrIncomplete, err)
		}
	}()
	if len(assets) == 0 {
		return sn, nil
	}
	cached, err := st.CachedDocMedia(ctx, sn.Token, sn.Revision)
	if err != nil {
		return sn, err
	}
	media := make(map[string]model.BlobID, len(assets))
	for _, a := range assets {
		if err := ctx.Err(); err != nil {
			return sn, err
		}
		if _, ok := media[a.Path]; ok {
			continue
		}
		if b := cached[a.Path]; b != "" {
			r, err := st.ReadBlob(ctx, b)
			if err == nil {
				err = r.Close()
			}
			if err == nil {
				media[a.Path] = b
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return sn, err
			}
		}
		src, ok := source.(MediaSource)
		if !ok {
			return sn, model.ErrUnsupportedContent
		}
		r, err := src.OpenDocMedia(ctx, a.Token)
		if err != nil {
			return sn, fmt.Errorf("cache document image %s: %w", a.Path, err)
		}
		if r == nil {
			return sn, errors.New("image source returned no body")
		}
		limited := &io.LimitedReader{R: r, N: maxMediaBytes + 1}
		b, err := st.PutBlob(ctx, limited)
		closeErr := r.Close()
		if err != nil {
			return sn, err
		}
		if closeErr != nil {
			return sn, closeErr
		}
		if limited.N == 0 || limited.N == maxMediaBytes+1 {
			return sn, errors.New("invalid document image size")
		}
		media[a.Path] = b
	}
	sn.DocMedia = media
	return sn, nil
}
