//go:build drivebridge_feishu

// Package all registers only the backends included in the Feishu product.
package all

import (
	_ "github.com/rclone/rclone/backend/feishu"
	_ "github.com/rclone/rclone/backend/local"
)
