package feishu

import (
	"context"
	"io"

	"github.com/rclone/rclone/lib/larkcli"
)

type cliClient struct{ drive *larkcli.Drive }

func (c *cliClient) list(ctx context.Context, folder string) ([]driveItem, error) {
	items, err := c.drive.List(ctx, folder)
	if err != nil {
		return nil, err
	}
	out := make([]driveItem, len(items))
	for i, item := range items {
		out[i] = driveItem(item)
	}
	return out, nil
}
func (c *cliClient) createFolder(ctx context.Context, parent, name string) (string, error) {
	return c.drive.CreateFolder(ctx, parent, name)
}
func (c *cliClient) upload(ctx context.Context, parent, token, name string, in io.Reader) (string, int64, string, error) {
	r, err := c.drive.Upload(ctx, parent, token, name, in)
	return r.FileToken, r.Size, r.URL, err
}
func (c *cliClient) delete(ctx context.Context, token, kind string) error {
	return c.drive.Delete(ctx, token, kind)
}
func (c *cliClient) move(ctx context.Context, token, kind, parent string) error {
	return c.drive.Move(ctx, token, kind, parent)
}
func (c *cliClient) rename(ctx context.Context, token, kind, name string) error {
	return c.drive.Rename(ctx, token, kind, name)
}
