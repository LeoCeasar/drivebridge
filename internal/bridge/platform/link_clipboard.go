package platform

import (
	"context"
	"errors"
)

var ErrLinkClipboardChanged = errors.New("clipboard changed during link lookup")

type linkClipboard interface {
	sequence() (uint32, error)
	withLock(context.Context, func() error) error
	write(context.Context, string) error
}

func newLinkClipboardWriter(ctx context.Context, clipboard linkClipboard) (func(string) error, error) {
	expected, err := clipboard.sequence()
	if err != nil {
		return nil, err
	}
	return func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return clipboard.withLock(ctx, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			current, err := clipboard.sequence()
			if err != nil {
				return err
			}
			if current != expected {
				return ErrLinkClipboardChanged
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return clipboard.write(ctx, text)
		})
	}, nil
}
