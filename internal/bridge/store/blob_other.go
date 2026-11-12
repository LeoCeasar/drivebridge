//go:build !windows

package store

// replaceDenied is always false: POSIX rename replaces an open file safely.
func replaceDenied(error) bool { return false }
