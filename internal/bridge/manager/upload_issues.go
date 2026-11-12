package manager

import (
	"context"
	"io"
	"path"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

const maxUploadCopyBytes = 10 << 20

type UploadCopy struct {
	Name    string `json:"name"`
	Content []byte `json:"content"`
}

// Read only the immutable captured job, never a caller-supplied local path.
func pendingUploadCopy(ctx context.Context, s *store.Store, id model.JobID) (UploadCopy, error) {
	jobs, err := s.Pending(ctx)
	if err != nil {
		return UploadCopy{}, err
	}
	for _, j := range jobs {
		if j.ID != id {
			continue
		}
		en, err := s.Entry(ctx, j.EntryID)
		if err != nil {
			return UploadCopy{}, err
		}
		r, err := s.ReadBlob(ctx, j.Local)
		if err != nil {
			return UploadCopy{}, err
		}
		defer r.Close()
		content, err := io.ReadAll(io.LimitReader(r, maxUploadCopyBytes+1))
		if err != nil {
			return UploadCopy{}, err
		}
		if len(content) > maxUploadCopyBytes {
			return UploadCopy{}, model.ErrUnsupportedContent
		}
		return UploadCopy{Name: path.Base(en.Remote), Content: content}, nil
	}
	return UploadCopy{}, store.ErrNotFound
}
func (m *Manager) UploadLocalCopy(ctx context.Context, mount model.MountID, job model.JobID) (out UploadCopy, err error) {
	err = m.withStore(ctx, mount, "/upload-copy", struct {
		ID model.JobID `json:"id"`
	}{job}, &out, func(s *store.Store) error { var e error; out, e = pendingUploadCopy(ctx, s, job); return e })
	return
}
