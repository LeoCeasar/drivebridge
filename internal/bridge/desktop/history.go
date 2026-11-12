package desktop

import (
	"context"
	"sync"

	"github.com/rclone/rclone/internal/bridge/manager"
)

func startHistoryMaintenance(ctx context.Context, m *manager.Manager, operations *sync.WaitGroup) {
	operations.Add(1)
	go func() { defer operations.Done(); m.RunHistoryMaintenance(ctx) }()
}
