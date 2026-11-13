package overlay

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTyporaReplacementDirectoryStaysLocalUntilPublication(t *testing.T) {
	f, s, _ := fixture(t)
	const scratch = "(A Document Being Saved By Typora)"
	require.NoError(t, f.Mkdir(ctx, scratch))
	require.True(t, f.LocalOnly(scratch))
	temp := put(t, f, scratch+"/document.md", "latest saved body")
	entry, err := s.EntryByRemote(ctx, temp.Remote())
	require.NoError(t, err)
	should, err := f.ShouldSync(ctx, entry.ID)
	require.NoError(t, err)
	require.False(t, should, "replacement bodies must not upload under the scratch directory")
	result, err := f.Move(ctx, temp, "document.md")
	require.NoError(t, err)
	entry, err = s.EntryByRemote(ctx, result.Remote())
	require.NoError(t, err)
	should, err = f.ShouldSync(ctx, entry.ID)
	require.NoError(t, err)
	require.True(t, should)
	require.Equal(t, "latest saved body", body(t, result))

	// A regular document with the same name or a user-created near match is
	// not a temporary directory and remains eligible for synchronization.
	other, _, _ := fixture(t)
	put(t, other, scratch, "ordinary user file")
	require.False(t, other.LocalOnly(scratch))
	require.NoError(t, other.Mkdir(ctx, "(A Document Being Saved By Me)"))
	require.False(t, other.LocalOnly("(A Document Being Saved By Me)"))
}
