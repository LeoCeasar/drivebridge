//go:build !darwin && !linux && !windows

package platform

import (
	"context"
	"errors"
)

func SetStartup(context.Context, StartupSpec, bool) error {
	return errors.New("user startup is unsupported on this platform")
}
