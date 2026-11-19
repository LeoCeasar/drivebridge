//go:build !windows

package platform

import (
	"context"
	"github.com/atotto/clipboard"
)

// Finder's native service owns the macOS pasteboard change-count guard.
func NewLinkClipboardWriter(context.Context) (func(string) error, error) {
	return clipboard.WriteAll, nil
}
