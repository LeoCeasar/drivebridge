//go:build unix

package nfs

import (
	"context"
	billy "github.com/go-git/go-billy/v5"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedNFSReadOnlyUntilStableWriteVerified(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("saved"), 0600))
	f, e := fs.NewFs(context.Background(), root)
	require.NoError(t, e)
	v, e := vfs.NewManaged(context.Background(), f, nil)
	require.NoError(t, e)
	t.Cleanup(func() { _ = v.CleanUp(); v.Shutdown() })
	n := &FS{vfs: v}
	require.Zero(t, n.Capabilities()&billy.WriteCapability)
	_, e = n.Create("new")
	require.ErrorIs(t, e, vfs.EROFS)
	_, e = n.OpenFile("file", os.O_RDWR, 0600)
	require.ErrorIs(t, e, vfs.EROFS)
	require.ErrorIs(t, n.Rename("file", "renamed"), vfs.EROFS)
	require.ErrorIs(t, n.Remove("file"), vfs.EROFS)
	h, e := n.Open("file")
	require.NoError(t, e)
	_, e = h.Write([]byte("bad"))
	require.ErrorIs(t, e, vfs.EROFS)
	require.ErrorIs(t, h.Truncate(0), vfs.EROFS)
	b, e := io.ReadAll(h)
	require.NoError(t, e)
	require.Equal(t, "saved", string(b))
	require.NoError(t, h.Close())
}
