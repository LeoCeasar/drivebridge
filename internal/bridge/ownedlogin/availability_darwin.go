package ownedlogin

import (
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func supportedPlatform() bool {
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return false
	}
	major, err := strconv.Atoi(strings.Split(version, ".")[0])
	return err == nil && major >= 14
}
