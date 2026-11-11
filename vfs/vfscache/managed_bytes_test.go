package vfscache

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestManagedBytesRefreshesCurrentDataUsage(t *testing.T) {
	cache, item := managedItem(t)
	require.Equal(t, int64(0), cache.Stats()["bytesUsed"], "periodic Stats can be stale before a cleaner pass")
	require.Equal(t, int64(5), cache.ManagedBytes())
	_, err := item.WriteAt([]byte("more"), 5)
	require.NoError(t, err)
	require.Equal(t, int64(9), cache.ManagedBytes(), "include dirty/open bytes even though they cannot be evicted")
	require.NoError(t, item.Close(nil))
}
