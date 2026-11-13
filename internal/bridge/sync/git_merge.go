package syncer

import (
	"bytes"
	"context"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"unicode/utf8"
)

// Git clean is authoritative for text. Keep the existing finer-grained merge
// for changes Git groups into a conflict (for example two independent edits on
// one line). Conversion and remote-identity safety remain separate checks.
func (e *Engine) mergeText(ctx context.Context, base, local, remote []byte) ([]byte, string, error) {
	for _, b := range [][]byte{base, local, remote} {
		if len(b) > markdown.MaxMarkdownBytes {
			return nil, "", history.ErrLimit
		}
		if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			return nil, "", history.ErrIntegrity
		}
	}
	inputs := [][]byte{base, local, remote}
	for i, b := range inputs {
		normalized, err := markdown.NormalizeForMerge(b)
		if err != nil {
			return nil, "", err
		}
		inputs[i] = normalized
	}
	base, local, remote = inputs[0], inputs[1], inputs[2]
	// Canonical Markdown cannot express every trailing/consecutive break. The
	// existing codec can: compare complete supported bodies before treating a
	// representation-only delta as concurrent content.
	if markdown.EquivalentForSync(base, remote) {
		return bytes.Clone(local), "", nil
	}
	if markdown.EquivalentForSync(base, local) {
		return bytes.Clone(remote), "", nil
	}
	if markdown.EquivalentForSync(local, remote) {
		return bytes.Clone(local), "", nil
	}
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
