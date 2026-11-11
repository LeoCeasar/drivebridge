package ui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestMountCacheRequestDefaultsPresetsAndValidation(t *testing.T) {
	for _, request := range []ConfigRequest{{}, {CacheMaxAge: "168h", CacheSize: "5G"}, {CacheMaxAge: "720h", CacheSize: "8G"}, {CacheMaxAge: "0h", CacheMaxBytes: 7 * 1024 * 1024 * 1024}} {
		mount, err := mountFromConfigRequest(request)
		if err != nil {
			t.Fatal(request, err)
		}
		wantAge := request.CacheMaxAge
		if wantAge == "" {
			wantAge = "168h"
		}
		if mount.CacheMaxAge != wantAge || mount.CacheMaxBytes <= 0 {
			t.Fatal(mount)
		}
	}
	for _, request := range []ConfigRequest{{CacheMaxAge: "-1h"}, {CacheMaxAge: "garbage"}, {CacheSize: "off"}, {CacheSize: "0"}, {CacheMaxBytes: -1}, {CacheSize: "5G", CacheMaxBytes: 7}} {
		if _, err := mountFromConfigRequest(request); err == nil {
			t.Fatal("invalid cache request accepted", request)
		}
	}
}
func TestConfigRequestCarriesCachePolicyThroughProtectedAPI(t *testing.T) {
	h, b, client, csrf, _ := fixture(t)
	requestData := ConfigRequest{Action: "mount", Name: "Cache", RemoteID: model.RemoteID(uuid.NewString()), Mountpoint: "/synthetic/mount", CacheSize: "5G", CacheMaxAge: "0h"}
	response := request(t, client, h, "POST", "/api/config", requestData, csrf, h.URL())
	response.Body.Close()
	if response.StatusCode != 200 || b.lastConfig.CacheSize != "5G" || b.lastConfig.CacheMaxAge != "0h" {
		t.Fatal(response.Status, b.lastConfig)
	}
}
func TestConfigViewShowsEffectiveLegacyCachePolicy(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	cfg := config.Default()
	cfg.StateDir = filepath.Join(root, "state")
	cfg.CacheDir = filepath.Join(root, "cache")
	profile := model.Profile{ID: model.ProfileID(uuid.NewString()), Name: "Fixture", Brand: "feishu"}
	remote := model.Remote{ID: model.RemoteID(uuid.NewString()), ProfileID: profile.ID, Name: "Fixture", FolderToken: "fixture", NewMDPolicy: "docx"}
	mount := config.DefaultMount()
	mount.ID = model.MountID(uuid.NewString())
	mount.Name = "Fixture"
	mount.RemoteID = remote.ID
	mount.Mountpoint = filepath.Join(root, "mount")
	mount.CacheMaxAge = ""
	cfg.Profiles = []model.Profile{profile}
	cfg.Remotes = []model.Remote{remote}
	cfg.Mounts = []model.Mount{mount}
	file := filepath.Join(root, "config", "config.json")
	if err := config.Save(file, cfg); err != nil {
		t.Fatal(err)
	}
	m, err := manager.New(manager.Options{ConfigFile: file})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	backend, err := NewManagerBackend(m)
	if err != nil {
		t.Fatal(err)
	}
	view, err := backend.Config(context.Background())
	if err != nil || len(view.Mounts) != 1 || view.Mounts[0].CacheMaxAge != "168h" || view.Mounts[0].CacheMaxBytes != 5*1024*1024*1024 {
		t.Fatal(view, err)
	}
	encoded, err := json.Marshal(view)
	if err != nil || !strings.Contains(string(encoded), `"cache_max_age":"168h"`) {
		t.Fatal(string(encoded), err)
	}
}
func TestMountCacheFormOffersRetentionAndSoftBudget(t *testing.T) {
	body, err := os.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`name="cache_max_age"`, `value="168h"`, `value="720h"`, `value="0h"`, `name="cache_size" value="5G"`, "软上限", "查看列表不会延长"} {
		if !strings.Contains(string(body), expected) {
			t.Fatal("missing cache policy input", expected)
		}
	}
}
