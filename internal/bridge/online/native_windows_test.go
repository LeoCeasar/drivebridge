//go:build windows

package online_test

import (
	"fmt"
	"testing"

	"github.com/rclone/rclone/internal/bridge/online"
	"golang.org/x/sys/windows"
)

// Real Win32 filesystem errors differ from the POSIX-compatible syscall
// constants; falling back to unclassified would hide a recoverable local fault.
func TestWin32FilesystemCauses(t *testing.T) {
	for _, err := range []error{windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL, windows.ERROR_DISK_QUOTA_EXCEEDED} {
		if got := online.Cause(fmt.Errorf("wrapped: %w", err)); got != "insufficient_space" {
			t.Fatalf("error=%v cause=%s", err, got)
		}
	}
	for _, err := range []error{windows.ERROR_IO_DEVICE, windows.ERROR_READ_FAULT, windows.ERROR_WRITE_FAULT, windows.ERROR_CRC} {
		if got := online.Cause(fmt.Errorf("wrapped: %w", err)); got != "io_error" {
			t.Fatalf("error=%v cause=%s", err, got)
		}
	}
}
