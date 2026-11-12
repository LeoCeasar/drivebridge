// Command preview serves synthetic UI data. It never constructs a cloud client,
// authorizes an account, mounts a filesystem or emits native notifications.
//
// Scenarios: DRIVEBRIDGE_UI_PREVIEW_EMPTY=1 shows a first-run workspace and
// DRIVEBRIDGE_UI_PREVIEW_STALE=1 marks conflict sources as not re-verified.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/ui"
)

const (
	conflictID = model.ConflictID("c288f5c6-67b7-4aca-b195-631745ce5e3e")
	conflictB  = model.ConflictID("0f6a9a4e-7f0b-4e0c-9a28-6e1bd2c7a001")
	conflictC  = model.ConflictID("0f6a9a4e-7f0b-4e0c-9a28-6e1bd2c7a002")
	mountID    = model.MountID("8d5909da-27fa-43fc-8b37-7763ab57d02d")
	folderID   = model.MountID("5c1d6a53-3bd1-4d7a-8b5a-2d1b7a1c0003")
	archivedID = model.MountID("5c1d6a53-3bd1-4d7a-8b5a-2d1b7a1c0004")
	entryID    = model.EntryID("a8db3ba3-b0c2-4cd5-8c77-c605f2301cf8")
	entryB     = model.EntryID("2b3d7e44-5a5a-4f43-8d51-90f8b0c1a001")
	entryC     = model.EntryID("2b3d7e44-5a5a-4f43-8d51-90f8b0c1a002")
	profileID  = model.ProfileID("c143343f-4a52-4999-b7b0-27d4f7fc625b")
)

type fixtureConflict struct {
	id                        model.ConflictID
	entry                     model.EntryID
	name, base, local, remote string
	generation                uint64
	revision                  string
}

type backend struct {
	mu       sync.Mutex
	drafts   map[model.ConflictID][]byte
	resolved map[model.ConflictID]bool
	running  map[model.MountID]bool
	stale    bool
	empty    bool
	copyRoot string
	opened   []model.MountID
}

func conflicts() []fixtureConflict {
	base := "# 协作文件夹使用说明\n\n共享目录用于保存产品方案和会议记录。\n\n## 本周安排\n\n- 周三完成初稿\n- 周五评审\n\n联系人：项目负责人\n"
	local := "# 协作文件夹使用说明\n\n共享目录用于保存产品方案和会议记录。\n\n## 本周安排\n\n- 周四完成初稿\n- 周五评审\n\n联系人：设计负责人\n"
	remote := "# 协作文件夹使用说明\n\n共享目录用于保存产品方案、会议记录和验收清单。\n\n## 本周安排\n\n- 周三完成初稿\n- 下周一评审\n\n联系人：项目负责人\n"
	return []fixtureConflict{
		{conflictID, entryID, "产品设计/协作文档.md", base, local, remote, 12, "38"},
		{conflictB, entryB, "会议纪要/2026-11-周会.md", "# 周会\n\n- 议题一：发布节奏\n- 议题二：缓存策略\n", "# 周会\n\n- 议题一：发布节奏\n- 议题二：缓存策略（5 GiB 软上限）\n", "# 周会\n\n- 议题一：发布节奏（延期一周）\n- 议题二：缓存策略\n", 3, "17"},
		{conflictC, entryC, "README.md", "# 项目\n\n说明待补充。\n", "# 项目\n\n说明：本地补充了安装步骤。\n", "# 项目\n\n说明：云端补充了使用限制。\n", 5, "9"},
	}
}
func (b *backend) find(id model.ConflictID) (fixtureConflict, bool) {
	for _, c := range conflicts() {
		if c.id == id {
			return c, true
		}
	}
	return fixtureConflict{}, false
}
func (b *backend) mounts() []ui.MountView {
	if b.empty {
		return []ui.MountView{}
	}
	open := 0
	for _, c := range conflicts() {
		if !b.resolved[c.id] {
			open++
		}
	}
	usedAccount, usedFolder := int64(3_650_000_000), int64(612_000_000)
	account := ui.MountView{ID: mountID, Name: "账号盘-" + string(profileID), DisplayName: "DriveBridge — 工作飞书", AccountName: "工作飞书", AccountRoot: true, ProfileID: profileID, Mountpoint: `C:\Users\demo\DriveBridge\` + string(profileID), CacheMaxBytes: 5 << 30, CacheMaxAge: "168h", BackgroundPullExcludedExtensions: []string{".log", ".db"}, State: model.MountStatus{SchemaVersion: 1, MountID: mountID, Running: b.running[mountID], LocalSaved: true, CloudState: "conflict", Conflicts: open, Queued: 2, CacheBytes: &usedAccount, DirectoryIndex: &model.DirectoryIndexStatus{Known: 128, Cached: 96, Refreshing: 1}}}
	if !b.running[mountID] {
		account.State.CloudState = "stopped"
	}
	folder := ui.MountView{ID: folderID, Name: "团队文档", Mountpoint: `D:\Sync\团队文档`, CacheMaxBytes: 10 << 30, CacheMaxAge: "720h", BackgroundPullExcludedExtensions: []string{}, State: model.MountStatus{SchemaVersion: 1, MountID: folderID, Running: b.running[folderID], LocalSaved: true, CloudState: "synced", CacheBytes: &usedFolder, DirectoryIndex: &model.DirectoryIndexStatus{Known: 40, Cached: 40}}}
	if !b.running[folderID] {
		folder.State.CloudState = "stopped"
	}
	old := ui.MountView{ID: archivedID, Name: "旧项目资料", Mountpoint: `D:\Sync\旧项目`, Archived: true, CacheMaxBytes: 5 << 30, CacheMaxAge: "168h", BackgroundPullExcludedExtensions: []string{}, State: model.MountStatus{SchemaVersion: 1, MountID: archivedID, LocalSaved: true, CloudState: "stopped", Queued: 1}}
	return []ui.MountView{account, folder, old}
}
func (b *backend) Status(context.Context) (ui.StatusView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := []ui.ConflictSummary{}
	if !b.empty {
		for _, c := range conflicts() {
			if !b.resolved[c.id] {
				list = append(list, ui.ConflictSummary{ID: c.id, MountID: mountID, Name: c.name})
			}
		}
	}
	return ui.StatusView{SchemaVersion: 1, Mounts: b.mounts(), Conflicts: list}, nil
}
func (b *backend) Config(context.Context) (ui.ConfigView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	profiles := []ui.ProfileView{{ID: profileID, Name: "工作飞书", Brand: "feishu", Authorized: true}}
	if b.empty {
		profiles = []ui.ProfileView{}
	}
	return ui.ConfigView{Platform: runtime.GOOS, CanRecover: true, Profiles: profiles, Remotes: []ui.RemoteView{}, Mounts: b.mounts()}, nil
}
func (b *backend) Configure(context.Context, ui.ConfigRequest) (ui.ConfigView, error) {
	return ui.ConfigView{}, errors.New("preview has no configuration authority")
}
func (b *backend) AuthStart(context.Context, model.ProfileID) (auth.AuthFlow, error) {
	return auth.AuthFlow{}, errors.New("preview has no authentication authority")
}
func (b *backend) AuthComplete(context.Context, string) error {
	return errors.New("preview has no authentication authority")
}
func (b *backend) Mount(_ context.Context, id model.MountID, action string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch action {
	case "start":
		b.running[id] = true
	case "stop":
		b.running[id] = false
	case "archive", "restore":
	default:
		return errors.New("preview cannot mount")
	}
	return nil
}
func (b *backend) Conflict(_ context.Context, id model.ConflictID) (ui.ConflictInputs, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.find(id)
	if !ok {
		return ui.ConflictInputs{}, os.ErrNotExist
	}
	return ui.ConflictInputs{Conflict: model.Conflict{ID: c.id, EntryID: c.entry, Generation: c.generation, RemoteRevision: c.revision}, Entry: model.Entry{ID: c.entry, Remote: c.name, Generation: c.generation}, Base: []byte(c.base), Local: []byte(c.local), Remote: []byte(c.remote), Draft: b.drafts[id], Text: true, Stale: b.stale, RefreshWarning: "offline"}, nil
}

// OpenMountFolder records the request; the preview never starts a file manager.
func (b *backend) OpenMountFolder(_ context.Context, id model.MountID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running[id] {
		return ui.ErrMountNotRunning
	}
	b.opened = append(b.opened, id)
	return nil
}
func (b *backend) Retry(context.Context, model.MountID) error {
	b.mu.Lock()
	b.stale = false
	b.mu.Unlock()
	return nil
}

// ExportConflict writes only synthetic preview bytes into this preview's temp
// root. Production exports use Store.ExportConflictCopies through Manager.
func (b *backend) ExportConflict(ctx context.Context, id model.ConflictID) (ui.ConflictCopies, error) {
	in, err := b.Conflict(ctx, id)
	if err != nil {
		return ui.ConflictCopies{}, err
	}
	dir, err := os.MkdirTemp(b.copyRoot, "synthetic-copies-")
	if err != nil {
		return ui.ConflictCopies{}, err
	}
	out := ui.ConflictCopies{Directory: dir, Manifest: filepath.Join(dir, "manifest.json")}
	for _, part := range []struct {
		name string
		data []byte
		dest *string
	}{{"base.md", in.Base, &out.Base}, {"local.md", in.Local, &out.Local}, {"remote.md", in.Remote, &out.Remote}, {"draft.md", in.Draft, &out.Draft}} {
		if part.data == nil {
			continue
		}
		*part.dest = filepath.Join(dir, part.name)
		if err = os.WriteFile(*part.dest, part.data, 0600); err != nil {
			return ui.ConflictCopies{}, err
		}
	}
	manifest, _ := json.Marshal(map[string]any{"fixture": true, "notice": "synthetic UI preview files only"})
	err = os.WriteFile(out.Manifest, manifest, 0600)
	return out, err
}
func (b *backend) SaveDraft(_ context.Context, id model.ConflictID, v []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drafts[id] = append([]byte{}, v...)
	return nil
}
func (b *backend) Resolve(_ context.Context, r model.ResolveRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.find(r.ConflictID)
	if !ok || r.ExpectedGeneration != c.generation || r.ExpectedRemoteRevision != c.revision {
		return model.ErrVersionChanged
	}
	b.resolved[r.ConflictID] = true
	return nil
}

func (b *backend) views() []manager.DiscoveryView {
	if b.empty {
		return []manager.DiscoveryView{}
	}
	entries := []discovery.Entry{
		{Token: "p1", Name: "我的文档", Memberships: []string{"personal"}},
		{Token: "p2", Name: "设计稿", Memberships: []string{"personal"}},
		{Token: "s1", Name: "团队文档", Memberships: []string{"shared_visible"}},
		{Token: "s2", Name: "产品评审", Memberships: []string{"shared_visible"}},
		{Token: "h1", Name: "归档共享", Memberships: []string{"shared_hidden"}},
	}
	scanned := time.Now().Add(-12 * time.Minute)
	snapshot := discovery.Snapshot{ScannedAt: scanned, Complete: true, Entries: entries, Sources: map[string]discovery.SourceState{
		discovery.Personal:      {Status: "complete", Count: 0, Pages: 1},
		discovery.SharedVisible: {Status: "complete", Count: 12, Pages: 2},
		discovery.SharedHidden:  {Status: "complete", Count: 3, Pages: 1},
	}}
	return []manager.DiscoveryView{{ProfileID: profileID, Configured: true, Enabled: true, SyncHiddenShared: true, State: "complete", LastScan: scanned, Snapshot: snapshot, MountID: mountID, Mountpoint: `C:\Users\demo\DriveBridge\` + string(profileID), Browser: &manager.BrowserBridgeView{Connected: true, HelperAvailable: true}}}
}
func (b *backend) DiscoveryViews(context.Context) ([]manager.DiscoveryView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.views(), nil
}
func (b *backend) one() (manager.DiscoveryView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.views()
	if len(v) == 0 {
		return manager.DiscoveryView{}, os.ErrNotExist
	}
	return v[0], nil
}
func (b *backend) StartDiscoveryLogin(context.Context, model.ProfileID, string) (manager.DiscoveryView, error) {
	return b.one()
}
func (b *backend) CompleteDiscoveryLogin(context.Context, model.ProfileID) (manager.DiscoveryView, error) {
	return b.one()
}
func (b *backend) RescanDiscovery(context.Context, model.ProfileID) (manager.DiscoveryView, error) {
	return b.one()
}
func (b *backend) SearchDiscoveryFolders(context.Context, model.ProfileID, string) ([]manager.DiscoveryCandidate, error) {
	return []manager.DiscoveryCandidate{}, nil
}
func (b *backend) AddDiscoveryFolder(context.Context, model.ProfileID, string) (manager.DiscoveryView, error) {
	return b.one()
}

type silent struct{}

func (silent) Available() bool                              { return false }
func (silent) Notify(context.Context, string, string) error { return nil }
func (silent) Open(context.Context, string) error           { return nil }
func main() {
	dir, err := os.MkdirTemp("", "drivebridge-ui-preview-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		panic(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	b := &backend{copyRoot: dir, stale: os.Getenv("DRIVEBRIDGE_UI_PREVIEW_STALE") == "1", empty: os.Getenv("DRIVEBRIDGE_UI_PREVIEW_EMPTY") == "1", drafts: map[model.ConflictID][]byte{}, resolved: map[model.ConflictID]bool{}, running: map[model.MountID]bool{mountID: true, folderID: false}}
	h, err := ui.Start(ctx, ui.Options{Backend: b, RuntimeDir: dir, ConfigKey: "synthetic-preview", Notifier: silent{}})
	if err != nil {
		panic(err)
	}
	defer h.Close(context.Background())
	u, err := h.BootstrapURL(conflictID)
	if err != nil {
		panic(err)
	}
	fmt.Println("PREVIEW_URL=" + u)
	fmt.Println("Synthetic local UI only. Ctrl-C stops this preview.")
	<-ctx.Done()
}
