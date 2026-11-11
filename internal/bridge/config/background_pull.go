package config

import (
	"fmt"
	"path"
	"strings"
)

// EffectiveBackgroundPullExcludedExtensions preserves defaults for old mount
// configurations. An explicitly empty list disables background exclusions.
// Rules describe one final filename extension, never a path or glob.
func EffectiveBackgroundPullExcludedExtensions(mount Mount) ([]string, error) {
	rules := mount.BackgroundPullExcludedExtensions
	if rules == nil {
		rules = []string{".log", ".db"}
	}
	out := make([]string, 0, len(rules))
	seen := make(map[string]bool, len(rules))
	for _, value := range rules {
		extension := strings.ToLower(strings.TrimSpace(value))
		extension = strings.TrimPrefix(extension, ".")
		if extension == "" {
			return nil, fmt.Errorf("invalid background pull excluded extension %q", value)
		}
		for _, r := range extension {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return nil, fmt.Errorf("invalid background pull excluded extension %q", value)
			}
		}
		extension = "." + extension
		if !seen[extension] {
			seen[extension] = true
			out = append(out, extension)
		}
	}
	return out, nil
}

// BackgroundPullExcluded matches the file's final extension against effective
// normalized rules. Directory names do not affect whether its body is polled.
func BackgroundPullExcluded(remote string, extensions []string) bool {
	extension := strings.ToLower(path.Ext(remote))
	for _, excluded := range extensions {
		if extension == excluded {
			return true
		}
	}
	return false
}
