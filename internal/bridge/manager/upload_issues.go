package manager

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"

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

// ValidateCreateRecoveryRequest validates the immutable snapshot supplied by the caller.
func ValidateCreateRecoveryRequest(req model.CreateRecoveryRequest) error {
	if !validID(string(req.JobID)) || req.ExpectedGeneration == 0 || len(req.ExpectedLocal) != 64 || strings.ToLower(string(req.ExpectedLocal)) != string(req.ExpectedLocal) {
		return ErrConfig
	}
	if _, err := hex.DecodeString(string(req.ExpectedLocal)); err != nil {
		return ErrConfig
	}
	if req.Action != "recheck" && req.Action != "recreate" {
		return ErrConfig
	}
	if req.Action == "recreate" && !req.ConfirmRecreate {
		return ErrConfig
	}
	return nil
}

// RecoverUpload only calls the running worker, which owns both the database and cloud checks.
func (m *Manager) RecoverUpload(ctx context.Context, mount model.MountID, req model.CreateRecoveryRequest) (out model.CreateRecoveryResult, err error) {
	if !validID(string(mount)) {
		return out, ErrConfig
	}
	if err = ValidateCreateRecoveryRequest(req); err != nil {
		return out, err
	}
	err = m.call(ctx, mount, "/upload-recover", req, &out)
	if errors.Is(err, os.ErrNotExist) {
		err = ErrUnavailable
	}
	return
}
