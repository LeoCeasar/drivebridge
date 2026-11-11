package ui

import (
	"context"

	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

func (b *ManagerBackend) DiscoveryViews(ctx context.Context) ([]manager.DiscoveryView, error) {
	return b.manager.DiscoveryViews(ctx)
}
func (b *ManagerBackend) StartDiscoveryLogin(ctx context.Context, id model.ProfileID, origin string) (manager.DiscoveryView, error) {
	return b.manager.StartDiscoveryLogin(ctx, id, origin)
}
func (b *ManagerBackend) CompleteDiscoveryLogin(ctx context.Context, id model.ProfileID) (manager.DiscoveryView, error) {
	return b.manager.CompleteDiscoveryLogin(ctx, id)
}
func (b *ManagerBackend) RescanDiscovery(ctx context.Context, id model.ProfileID) (manager.DiscoveryView, error) {
	return b.manager.RescanDiscovery(ctx, id)
}
func (b *ManagerBackend) SearchDiscoveryFolders(ctx context.Context, id model.ProfileID, query string) ([]manager.DiscoveryCandidate, error) {
	return b.manager.SearchDiscoveryFolders(ctx, id, query)
}
func (b *ManagerBackend) AddDiscoveryFolder(ctx context.Context, id model.ProfileID, input string) (manager.DiscoveryView, error) {
	return b.manager.AddDiscoveryFolder(ctx, id, input)
}

var _ DiscoveryBackend = (*ManagerBackend)(nil)

func (b *ManagerBackend) SetHiddenSharedSync(ctx context.Context, id model.ProfileID, enabled bool) (manager.DiscoveryView, error) {
	return b.manager.SetHiddenSharedSync(ctx, id, enabled)
}

var _ HiddenSharedSyncConfigurer = (*ManagerBackend)(nil)
