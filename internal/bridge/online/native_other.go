//go:build !windows

package online

func nativeCause(error) string { return "" }
