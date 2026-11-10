//go:build desktop && (windows || (darwin && cgo))

package tray

func preflight() error { return nil }
