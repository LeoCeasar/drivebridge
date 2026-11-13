//go:build darwin

package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// SyncFile preserves Go's Darwin FULLFSYNC durability, using a syscall wrapper
// that announces blocking to the scheduler. Go 1.25 os.File.Sync calls the
// runtime's nonblocking fcntl trampoline; using it on a FUSE mount served by the
// same process can deadlock a GC stop-the-world. Control pins the descriptor
// against concurrent Close for the entire operation.
func SyncFile(f *os.File) error {
	if f == nil {
		return os.ErrInvalid
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var syncErr error
	err = raw.Control(func(fd uintptr) {
		_, syncErr = unix.FcntlInt(fd, unix.F_FULLFSYNC, 0)
		if errors.Is(syncErr, unix.ENOTSUP) {
			syncErr = unix.Fsync(int(fd))
		}
	})
	if err != nil {
		if _, statErr := f.Stat(); errors.Is(statErr, os.ErrClosed) {
			return &os.PathError{Op: "sync", Path: f.Name(), Err: os.ErrClosed}
		}
	}
	return errors.Join(err, syncErr)
}
