//go:build windows

package online

import (
	"errors"

	"golang.org/x/sys/windows"
)

func nativeCause(err error) string {
	switch {
	case errors.Is(err, windows.ERROR_DISK_FULL), errors.Is(err, windows.ERROR_HANDLE_DISK_FULL), errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED):
		return "insufficient_space"
	case errors.Is(err, windows.ERROR_IO_DEVICE), errors.Is(err, windows.ERROR_READ_FAULT), errors.Is(err, windows.ERROR_WRITE_FAULT), errors.Is(err, windows.ERROR_CRC):
		return "io_error"
	}
	return ""
}
