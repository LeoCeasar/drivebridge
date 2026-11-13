//go:build !darwin

package platform

import "os"

func SyncFile(f *os.File) error { return f.Sync() }
