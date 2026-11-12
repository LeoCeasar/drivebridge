//go:build drivebridge_feishu

package operations

import (
	"errors"
	"github.com/rclone/rclone/fs/config/configmap"
)

func newListJSONCipher(configmap.Mapper) (filenameCipher, error) {
	return nil, errors.New("crypt is not included in the Feishu edition")
}
