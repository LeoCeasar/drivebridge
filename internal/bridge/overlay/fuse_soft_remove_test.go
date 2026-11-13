package overlay

import (
	"testing"

	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
	"github.com/stretchr/testify/require"
)

func TestFuseSoftRemoveDirectoryRefreshKeepsCanonicalIdentityReserved(t *testing.T) {
	f, st, _ := fixture(t)
	original := acceptAssets(t, st, "document.md", "stable-doc", "old", nil)
	object, err := f.NewObject(ctx, original.Remote)
	require.NoError(t, err)
	hidden := ".fuse_hidden0000006900000002"
	_, err = f.Move(ctx, object, hidden)
	require.NoError(t, err)
	require.NoError(t, f.observeItems(ctx, "", "root", []larkcli.DriveItem{{Token: original.Token, Name: "document", Type: "docx", ParentToken: "root"}}))
	current, err := st.EntryByToken(ctx, original.Token)
	require.NoError(t, err)
	require.Equal(t, hidden, current.Remote)
	_, err = st.EntryByRemote(ctx, original.Remote)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, f.Mkdir(ctx, "(A Document Being Saved By Typora)"))
	replacement := put(t, f, "(A Document Being Saved By Typora)/document.md", "latest local body")
	published, err := f.Move(ctx, replacement, original.Remote)
	require.NoError(t, err)
	require.Equal(t, "latest local body", body(t, published))
	current, err = st.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Equal(t, original.Token, current.Token)
	old, err := f.NewObject(ctx, hidden)
	require.NoError(t, err)
	require.NoError(t, old.Remove(ctx))
	current, err = st.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Equal(t, original.Token, current.Token)
	require.NoError(t, st.Validate(ctx))
}
