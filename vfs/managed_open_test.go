package vfs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

type rejectingOpenFS struct {
	fs.Fs
	calls int
}

func (f *rejectingOpenFS) ValidateManagedOpen(context.Context, string, bool) error {
	f.calls++
	return fs.ErrorPermissionDenied
}

func TestManagedOpenValidatorIgnoredForNativeVFS(t *testing.T) {
	run, v := newTestVFS(t)
	run.WriteObject(v.ctx, "file", "ordinary body", time.Now())
	backend := &rejectingOpenFS{Fs: v.f}
	v.f = backend
	handle, err := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, err)
	require.NoError(t, handle.Close())
	root, err := v.Root()
	require.NoError(t, err)
	_, err = root.Create("new", os.O_CREATE|os.O_WRONLY)
	require.NoError(t, err)
	require.Zero(t, backend.calls)
}
