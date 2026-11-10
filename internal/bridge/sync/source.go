package syncer

import (
	"context"
	"errors"
	"io"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

// ObservedFile owns one complete acquired download, including its private lease.
type ObservedFile struct {
	Token    string
	Identity model.FileIdentity
	Size     int64
	Reader   io.ReadCloser
}
type Source interface {
	FetchDoc(context.Context, string, string) (cloud.Document, error)
	CreateDoc(context.Context, string, string, []byte) (cloud.Document, error)
	ApplyDoc(context.Context, string, string, []model.Operation) (cloud.ApplyResult, error)
	Capabilities() []cloud.CapabilityGrant
	ObserveFile(context.Context, string) (ObservedFile, error)
	UploadFile(context.Context, string, string, string, io.Reader) (larkcli.UploadResult, error)
	List(context.Context, string) ([]larkcli.DriveItem, error)
	Rename(context.Context, string, model.Kind, string) error
	Move(context.Context, string, model.Kind, string) error
	Delete(context.Context, string, model.Kind) error
	CreateFolder(context.Context, string, string) (string, error)
}
type cloudSource struct{ *cloud.Client }
type observedReader struct {
	io.ReadCloser
	release func() error
}

func (r *observedReader) Close() error { return errors.Join(r.ReadCloser.Close(), r.release()) }
func (c cloudSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	o, err := c.Client.ObserveFile(ctx, token)
	if err != nil {
		return ObservedFile{}, err
	}
	r, err := o.Open()
	if err != nil {
		_ = o.Close()
		return ObservedFile{}, err
	}
	return ObservedFile{Token: o.Token, Identity: o.Identity, Size: o.Size, Reader: &observedReader{r, o.Close}}, nil
}
