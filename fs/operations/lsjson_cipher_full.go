//go:build !drivebridge_feishu

package operations

import (
	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"
)

func newListJSONCipher(config configmap.Mapper) (filenameCipher, error) {
	return crypt.NewCipher(config)
}
