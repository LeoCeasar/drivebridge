//go:build drivebridge_feishu

// Package all registers product commands and the native mount adapters.
package all

import (
	_ "github.com/rclone/rclone/cmd/cmount"
	_ "github.com/rclone/rclone/cmd/config"
	_ "github.com/rclone/rclone/cmd/desktop"
	_ "github.com/rclone/rclone/cmd/feishu"
	_ "github.com/rclone/rclone/cmd/mount"
	_ "github.com/rclone/rclone/cmd/mount2"
	_ "github.com/rclone/rclone/cmd/version"
)
