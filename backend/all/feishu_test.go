//go:build drivebridge_feishu

package all_test

import (
	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/fs"
	"testing"
)

func TestFeishuProductBackendRegistry(t *testing.T) {
	seen := map[string]bool{}
	for _, backend := range fs.Registry {
		seen[backend.Name] = true
	}
	if len(seen) != 2 || !seen["feishu"] || !seen["local"] {
		t.Fatalf("unrelated backends included: %v", seen)
	}
}
