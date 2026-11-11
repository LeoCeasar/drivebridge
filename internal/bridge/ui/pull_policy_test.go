package ui

import (
	"encoding/json"
	"github.com/google/uuid"
	"testing"
)

func TestPullPolicyRequestRetainsExplicitEmptyAndCustomRules(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	for _, extensions := range [][]string{{".log", ".sqlite"}, {}} {
		res := request(t, c, h, "POST", "/api/config", map[string]any{"action": "mount_policy", "mount_id": uuid.NewString(), "background_pull_excluded_extensions": extensions}, csrf, h.URL())
		res.Body.Close()
		b.mu.Lock()
		r := b.lastConfig
		b.mu.Unlock()
		if res.StatusCode != 200 || r.Action != "mount_policy" || r.BackgroundPullExcludedExtensions == nil || len(*r.BackgroundPullExcludedExtensions) != len(extensions) {
			t.Fatal(res.Status, r)
		}
	}
	raw, _ := json.Marshal(b.lastConfig)
	if len(raw) == 0 {
		t.Fatal("no config request")
	}
}
