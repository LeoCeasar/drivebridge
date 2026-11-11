package vfs

import (
	"github.com/stretchr/testify/require"
	"io"
	"testing"
)

func TestManagedCacheBytesCountsReadBody(t *testing.T) {
	v, _, _ := newManagedFS(t, true)
	require.Equal(t, int64(0), v.ManagedCacheBytes())
	h, err := v.Open("file")
	require.NoError(t, err)
	body, err := io.ReadAll(h)
	require.NoError(t, err)
	require.NoError(t, h.Close())
	require.Equal(t, int64(len(body)), v.ManagedCacheBytes())
}
