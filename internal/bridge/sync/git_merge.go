package syncer

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/markdown"
)

// Git clean is authoritative for text. Keep the existing finer-grained merge
// for changes Git groups into a conflict (for example two independent edits on
// one line). Conversion and remote-identity safety remain separate checks.
func (e *Engine) mergeText(ctx context.Context, base, local, remote []byte) ([]byte, string, error) {
	merged, clean, err := history.MergeText(ctx, base, local, remote)
	if err != nil {
		return nil, "", err
	}
	if clean {
		return merged, "", nil
	}
	merged, reason := markdown.MergePlain(base, local, remote)
	return merged, reason, nil
}
