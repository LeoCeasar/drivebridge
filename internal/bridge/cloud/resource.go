package cloud

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
)

func resourceKind(kind model.Kind) bool {
	return kind == model.KindDocx || kind == model.KindFile || kind == model.KindFolder
}
func (c *Client) Rename(ctx context.Context, token string, kind model.Kind, name string) error {
	if !resourceKind(kind) {
		return model.ErrUnsupportedContent
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	return canonicalError(c.drive.Rename(ctx, token, string(kind), name))
}
func (c *Client) Move(ctx context.Context, token string, kind model.Kind, parent string) error {
	if !resourceKind(kind) {
		return model.ErrUnsupportedContent
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	return canonicalError(c.drive.Move(ctx, token, string(kind), parent))
}
func (c *Client) Delete(ctx context.Context, token string, kind model.Kind) error {
	if !resourceKind(kind) {
		return model.ErrUnsupportedContent
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	return canonicalError(c.drive.Delete(ctx, token, string(kind)))
}
