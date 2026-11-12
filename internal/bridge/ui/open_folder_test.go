package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

func TestFolderOpenLabelFollowsHostPlatform(t *testing.T) {
	for goos, want := range map[string]string{"windows": "在资源管理器中打开", "darwin": "在 Finder 中打开", "linux": "在文件管理器中打开", "freebsd": "在文件管理器中打开"} {
		if got := FolderOpenLabel(goos); got != want {
			t.Fatalf("%s: %q", goos, got)
		}
	}
}

type openFixture struct {
	*fakeBackend
	opened []model.MountID
	err    error
}

func (b *openFixture) OpenMountFolder(_ context.Context, id model.MountID) error {
	b.opened = append(b.opened, id)
	return b.err
}

func TestOpenFolderEndpointIsAuthenticatedPostOnlyAndHostLabelled(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	source := &openFixture{fakeBackend: b}
	h.backend = source
	old := hostGOOS
	hostGOOS = "darwin"
	t.Cleanup(func() { hostGOOS = old })
	id := uuid.NewString()
	route := "/api/mounts/" + id + "/open"

	res := request(t, c, h, "POST", route, map[string]any{}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 || len(source.opened) != 1 || source.opened[0] != model.MountID(id) {
		t.Fatal(res.Status, source.opened)
	}
	for name, tc := range map[string]struct {
		method, csrf, origin string
		body                 any
		want                 int
	}{
		"missing csrf":   {"POST", "", h.URL(), map[string]any{}, 403},
		"foreign origin": {"POST", csrf, "http://evil.example", map[string]any{}, 403},
		"get":            {"GET", "", "", nil, 405},
		"path in body":   {"POST", csrf, h.URL(), map[string]any{"path": `C:\Windows`}, 400},
	} {
		res = request(t, c, h, tc.method, route, tc.body, tc.csrf, tc.origin)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatal(name, res.Status)
		}
	}
	res = request(t, c, h, "POST", "/api/mounts/not-a-valid-id/open", map[string]any{}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 404 || len(source.opened) != 1 {
		t.Fatal("invalid mount identifier reached the backend", res.Status, source.opened)
	}
	anonymous := &http.Client{}
	res = request(t, anonymous, h, "POST", route, map[string]any{}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 || len(source.opened) != 1 {
		t.Fatal("unauthenticated open", res.Status)
	}

	res = request(t, c, h, "GET", "/api/status", nil, "", "")
	defer res.Body.Close()
	var status struct {
		Platform string `json:"platform"`
		Label    string `json:"open_folder_label"`
		Can      bool   `json:"can_open_folder"`
	}
	if json.NewDecoder(res.Body).Decode(&status) != nil || status.Platform != "darwin" || status.Label != "在 Finder 中打开" || !status.Can {
		t.Fatal(status)
	}
}

func TestOpenFolderErrorsAreActionable(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	source := &openFixture{fakeBackend: b}
	h.backend = source
	id := uuid.NewString()
	for err, want := range map[error][2]any{
		ErrMountNotRunning:        {409, "mount_not_running"},
		manager.ErrArchived:       {409, "mount_archived"},
		platform.ErrNoDesktop:     {503, "no_desktop"},
		platform.ErrInvalidFolder: {409, "folder_unavailable"},
	} {
		source.err = err
		res := request(t, c, h, "POST", "/api/mounts/"+id+"/open", map[string]any{}, csrf, h.URL())
		var got map[string]any
		json.NewDecoder(res.Body).Decode(&got)
		res.Body.Close()
		if res.StatusCode != want[0] || got["code"] != want[1] || got["message"] == "" {
			t.Fatal(err, res.Status, got)
		}
	}
	// A fixture host without the capability answers unavailable, not a panic.
	h.backend = b
	res := request(t, c, h, "POST", "/api/mounts/"+id+"/open", map[string]any{}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatal(res.Status)
	}
}

func TestManagerOpenMountFolderOnlyOpensConfiguredRunningMounts(t *testing.T) {
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
	profile := model.Profile{ID: model.ProfileID(uuid.NewString()), Name: "工作飞书", Brand: "feishu"}
	remote := model.Remote{ID: model.RemoteID(uuid.NewString()), ProfileID: profile.ID, Name: accountRootPrefix + string(profile.ID), FolderToken: "drivebridge-account-" + string(profile.ID), NewMDPolicy: "docx", AccountRoot: true}
	stopped := config.DefaultMount()
	stopped.ID = model.MountID(uuid.NewString())
	stopped.Name = remote.Name
	stopped.RemoteID = remote.ID
	stopped.Mountpoint = filepath.Join(root, "mount")
	other := model.Remote{ID: model.RemoteID(uuid.NewString()), ProfileID: profile.ID, Name: "团队文档", FolderToken: "team", NewMDPolicy: "docx"}
	archived := stopped
	archived.ID = model.MountID(uuid.NewString())
	archived.RemoteID = other.ID
	archived.Mountpoint = filepath.Join(root, "archived")
	archived.Archived = true
	cfg.Profiles = []model.Profile{profile}
	cfg.Remotes = []model.Remote{remote, other}
	cfg.Mounts = []model.Mount{stopped, archived}
	file := filepath.Join(root, "config", "config.json")
	if err = config.Save(file, cfg); err != nil {
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
	var opened []string
	backend.openFolder = func(path string) error { opened = append(opened, path); return nil }
	if err = backend.OpenMountFolder(context.Background(), stopped.ID); err != ErrMountNotRunning {
		t.Fatal("stopped mount must not open", err)
	}
	if err = backend.OpenMountFolder(context.Background(), archived.ID); err != manager.ErrArchived {
		t.Fatal("archived mount must not open", err)
	}
	if err = backend.OpenMountFolder(context.Background(), model.MountID(uuid.NewString())); err == nil {
		t.Fatal("unknown mount opened")
	}
	if len(opened) != 0 {
		t.Fatal(opened)
	}
	view, err := backend.Config(context.Background())
	if err != nil || len(view.Mounts) != 2 || view.Mounts[0].DisplayName != "DriveBridge — 工作飞书" || view.Mounts[0].Name != remote.Name || !view.Mounts[0].AccountRoot || view.Mounts[0].AccountName != "工作飞书" || !view.CanOpenFolder || view.OpenFolderLabel == "" {
		t.Fatal(view, err)
	}
}

func TestDescribeMountKeepsRawNameAndNamesOnlyAccountRoots(t *testing.T) {
	profile := model.Profile{ID: "c143343f-4a52-4999-b7b0-27d4f7fc625b", Name: "工作飞书"}
	legacy := model.Remote{ID: "r1", ProfileID: profile.ID}
	folder := model.Remote{ID: "r2", ProfileID: profile.ID}
	account := MountView{Name: accountRootPrefix + string(profile.ID), RemoteID: "r1"}
	describeMount(&account, []model.Remote{legacy, folder}, []model.Profile{profile})
	if account.DisplayName != "DriveBridge — 工作飞书" || !account.AccountRoot {
		t.Fatal("legacy generated name is not recognised", account)
	}
	shared := MountView{Name: "团队文档", RemoteID: "r2"}
	describeMount(&shared, []model.Remote{legacy, folder}, []model.Profile{profile})
	if shared.DisplayName != "" || shared.AccountRoot || shared.AccountName != "工作飞书" {
		t.Fatal(shared)
	}
	orphan := MountView{Name: accountRootPrefix + "x", RemoteID: "missing"}
	describeMount(&orphan, nil, nil)
	if orphan.AccountRoot || orphan.DisplayName != "" {
		t.Fatal(orphan)
	}
}
