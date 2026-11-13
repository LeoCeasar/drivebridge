//go:build !windows

package platform

func replaceSharedFile(from, to string) error { return ReplaceFile(from, to) }
