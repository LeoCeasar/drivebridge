package vfs

import (
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestDirectoryReadersDoNotBlockManagedStop(t *testing.T) {
	v, _ := managedTestVFS(t)
	reader, err := v.root.Open(os.O_RDONLY)
	require.NoError(t, err)
	readers, writers, reservations := v.ManagedActivity()
	require.Equal(t, 1, readers)
	require.Zero(t, writers+reservations)
	require.False(t, v.ManagedCachePending())
	t.Logf("real root-directory reader: readers=%d writers=%d reservations=%d cache_pending=%v", readers, writers, reservations, v.ManagedCachePending())
	stopReaders, stopWriters, stopReservations := v.ManagedStopActivity()
	require.Zero(t, stopReaders+stopWriters+stopReservations)
	file, err := v.Open("file")
	require.NoError(t, err)
	stopReaders, stopWriters, stopReservations = v.ManagedStopActivity()
	require.Equal(t, 1, stopReaders, "real file readers must still drain before stop")
	require.Zero(t, stopWriters+stopReservations)
	require.NoError(t, file.Release())
	require.NoError(t, reader.Release())
	readers, writers, reservations = v.ManagedActivity()
	require.Zero(t, readers+writers+reservations)
}
