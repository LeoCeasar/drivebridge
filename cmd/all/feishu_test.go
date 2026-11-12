//go:build drivebridge_feishu

package all_test

import (
	"bytes"
	"encoding/json"
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"
	"testing"
)

func TestFeishuProductCommandsAndOfflineEdition(t *testing.T) {
	for _, name := range []string{"config", "desktop", "feishu", "version"} {
		if found, _, err := cmd.Root.Find([]string{name}); err != nil || found == cmd.Root {
			t.Fatalf("missing %s", name)
		}
	}
	for _, name := range []string{"bisync", "serve", "cryptcheck"} {
		if found, _, err := cmd.Root.Find([]string{name}); err == nil && found != cmd.Root {
			t.Fatalf("unrelated command %s included", name)
		}
	}
	edition, _, err := cmd.Root.Find([]string{"feishu", "edition"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	edition.SetOut(&out)
	if err := edition.RunE(edition, nil); err != nil {
		t.Fatal(err)
	}
	var value map[string]string
	if err := json.Unmarshal(out.Bytes(), &value); err != nil || value["edition"] != "feishu" {
		t.Fatalf("wrong product edition %s %v", out.String(), err)
	}
}
