//go:build windows

package integration

import "syscall"

func syscallEIO() error { return syscall.EIO }

func syscallENOSPC() error { return syscall.ENOSPC }
