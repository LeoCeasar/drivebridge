package model

import (
	"path"
	"strings"
	"unicode/utf8"
)

// DirectoryRefreshRequest always names a mount-relative directory; empty is root.
type DirectoryRefreshRequest struct {
	Remote string `json:"remote"`
}

// ValidDirectoryRemote uses the same wire format on every OS. Native drive,
// UNC and traversal paths are never interpreted as a mount-relative scope.
func ValidDirectoryRemote(remote string) bool {
	if remote == "" {
		return true
	}
	if len(remote) > 32768 || !utf8.ValidString(remote) || remote == "." || strings.HasPrefix(remote, "/") || strings.ContainsAny(remote, "\\:\x00") || path.Clean(remote) != remote {
		return false
	}
	for _, p := range strings.Split(remote, "/") {
		if p == ".." || p == "." || p == "" {
			return false
		}
	}
	return true
}
