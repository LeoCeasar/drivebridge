package vfs

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagedStatMetadataDoesNotMaterializeBodies(t *testing.T) {
	v, backend, run := newManagedFS(t, true)
	run.WriteObject(v.ctx, "other", "untouched body", time.Now())
	root, err := v.Root()
	require.NoError(t, err)
	nodes, err := root.ReadDirAll()
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	node, err := root.StatMetadata("file")
	require.NoError(t, err)
	require.Equal(t, int64(-1), node.DirEntry().Size())
	require.Equal(t, int64(0), node.Size())
	node, err = v.StatMetadata("/file")
	require.NoError(t, err)
	require.Equal(t, int32(0), backend.materialized.Load())
	_, err = v.StatMetadata("missing")
	require.ErrorIs(t, err, ENOENT)
	_, err = v.StatMetadata("file/child")
	require.ErrorIs(t, err, ENOENT)
	require.Equal(t, int32(0), backend.materialized.Load())
	handle, err := node.Open(os.O_RDONLY)
	require.NoError(t, err)
	content, err := io.ReadAll(handle)
	require.NoError(t, err)
	require.Equal(t, "hello😀", string(content))
	require.NoError(t, handle.Close())
	require.Equal(t, int32(1), backend.materialized.Load())
	exact, err := v.StatMetadata("file")
	require.NoError(t, err)
	require.Equal(t, int64(len("hello😀")), exact.Size())
	other, err := v.StatMetadata("other")
	require.NoError(t, err)
	require.Equal(t, int64(-1), other.DirEntry().Size())
	require.Equal(t, int32(1), backend.materialized.Load(), "opening one file cannot hydrate its siblings")
}

func TestManagedMetadataLookupDoesNotRequireReadableBody(t *testing.T) {
	v, backend, _ := newManagedFS(t, true)
	backend.materializeErr = errors.New("body temporarily unavailable")
	node, err := v.StatMetadata("file")
	require.NoError(t, err)
	require.Equal(t, int32(0), backend.materialized.Load())
	_, err = node.Open(os.O_RDONLY)
	require.ErrorIs(t, err, backend.materializeErr)
	_, err = v.Stat("file")
	require.ErrorIs(t, err, backend.materializeErr, "legacy Stat must retain its exact-size behavior")
}
