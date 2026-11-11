package ui

import (
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

func mountFromConfigRequest(request ConfigRequest) (model.Mount, error) {
	mount := config.DefaultMount()
	mount.Name = request.Name
	mount.RemoteID = request.RemoteID
	mount.Mountpoint = request.Mountpoint
	mount.MacOSBackend = request.MacOSBackend
	if request.CacheMaxAge != "" {
		mount.CacheMaxAge = request.CacheMaxAge
	}
	if _, err := config.EffectiveCacheMaxAge(mount); err != nil {
		return model.Mount{}, manager.ErrConfig
	}
	if request.CacheMaxBytes < 0 || (request.CacheSize != "" && request.CacheMaxBytes != 0) {
		return model.Mount{}, manager.ErrConfig
	}
	if request.CacheSize != "" {
		var size fs.SizeSuffix
		if err := size.Set(request.CacheSize); err != nil || size <= 0 {
			return model.Mount{}, manager.ErrConfig
		}
		mount.CacheMaxBytes = int64(size)
	} else if request.CacheMaxBytes > 0 {
		mount.CacheMaxBytes = request.CacheMaxBytes
	}
	return mount, nil
}
