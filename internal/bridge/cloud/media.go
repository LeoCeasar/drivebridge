package cloud

import (
	"context"
	"io"
)

// OpenDocMedia returns a complete authenticated image download. The reader owns
// its private temporary file; Close or context cancellation releases that file.
func (c *Client) OpenDocMedia(ctx context.Context, token string) (io.ReadCloser, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	reader, err := c.drive.OpenDocMedia(ctx, token)
	return reader, canonicalError(err)
}
