//go:build !darwin && !windows

package ownedlogin

func supportedPlatform() bool { return false }
