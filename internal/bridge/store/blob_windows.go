//go:build windows

package store

import (
	"errors"
	"syscall"
)

// replaceDenied reports a MoveFileEx failure caused by the destination being
// open in another handle without FILE_SHARE_DELETE (or otherwise in use).
func replaceDenied(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	const accessDenied, sharingViolation, lockViolation = 5, 32, 33
	return errno == accessDenied || errno == sharingViolation || errno == lockViolation
}
