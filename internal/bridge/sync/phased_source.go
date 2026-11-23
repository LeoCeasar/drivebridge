package syncer

import (
	"context"
	"io"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/doccache"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/lib/larkcli"
)

// The wrapper annotates only the failed boundary; the executor supplies the
// enclosing operation and retains all authority over the business journal.
type phasedSource struct{ Source }

func (s phasedSource) FetchDoc(c context.Context, t, r string) (cloud.Document, error) {
	v, e := s.Source.FetchDoc(c, t, r)
	return v, online.AtPhaseIfUnset(e, "", "document_read")
}
func (s phasedSource) CreateDoc(c context.Context, p, n string, b []byte) (cloud.Document, error) {
	v, e := s.Source.CreateDoc(c, p, n, b)
	return v, online.AtPhaseIfUnset(e, "create", "create_dispatch")
}
func (s phasedSource) ApplyDoc(c context.Context, t, r string, o []model.Operation) (cloud.ApplyResult, error) {
	v, e := s.Source.ApplyDoc(c, t, r, o)
	return v, online.AtPhaseIfUnset(e, "update", "write_dispatch")
}
func (s phasedSource) ObserveFile(c context.Context, t string) (ObservedFile, error) {
	v, e := s.Source.ObserveFile(c, t)
	return v, online.AtPhaseIfUnset(e, "", "readback")
}
func (s phasedSource) UploadFile(c context.Context, p, t, n string, r io.Reader) (larkcli.UploadResult, error) {
	v, e := s.Source.UploadFile(c, p, t, n, r)
	return v, online.AtPhaseIfUnset(e, "upload", "write_dispatch")
}
func (s phasedSource) List(c context.Context, p string) ([]larkcli.DriveItem, error) {
	v, e := s.Source.List(c, p)
	return v, online.AtPhaseIfUnset(e, "", "list_parent")
}
func (s phasedSource) Rename(c context.Context, t string, k model.Kind, n string) error {
	return online.AtPhaseIfUnset(s.Source.Rename(c, t, k, n), "rename", "rename_dispatch")
}
func (s phasedSource) Move(c context.Context, t string, k model.Kind, p string) error {
	return online.AtPhaseIfUnset(s.Source.Move(c, t, k, p), "move", "move_dispatch")
}
func (s phasedSource) Delete(c context.Context, t string, k model.Kind) error {
	return online.AtPhaseIfUnset(s.Source.Delete(c, t, k), "delete", "delete_dispatch")
}
func (s phasedSource) CreateFolder(c context.Context, p, n string) (string, error) {
	v, e := s.Source.CreateFolder(c, p, n)
	return v, online.AtPhaseIfUnset(e, "mkdir", "create_dispatch")
}

// Preserve the optional media capability used by the document snapshot cache.
func (s phasedSource) OpenDocMedia(ctx context.Context, token string) (io.ReadCloser, error) {
	source, ok := s.Source.(doccache.MediaSource)
	if !ok {
		return nil, model.ErrUnsupportedContent
	}
	r, err := source.OpenDocMedia(ctx, token)
	return r, online.AtPhaseIfUnset(err, "", "document_read")
}
