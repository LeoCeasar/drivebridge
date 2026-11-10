// Command preview serves synthetic UI data. It never constructs a cloud client,
// authorizes an account, mounts a filesystem or emits native notifications.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/ui"
)

const conflictID = model.ConflictID("c288f5c6-67b7-4aca-b195-631745ce5e3e")
const mountID = model.MountID("8d5909da-27fa-43fc-8b37-7763ab57d02d")
const entryID = model.EntryID("a8db3ba3-b0c2-4cd5-8c77-c605f2301cf8")

type backend struct {
	mu       sync.Mutex
	draft    []byte
	resolved bool
}

func (b *backend) Status(context.Context) (ui.StatusView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	conflicts := []ui.ConflictSummary{}
	if !b.resolved {
		conflicts = append(conflicts, ui.ConflictSummary{ID: conflictID, MountID: mountID, Name: "产品设计/协作文档.md"})
	}
	return ui.StatusView{SchemaVersion: 1, Mounts: []ui.MountView{{ID: mountID, Name: "团队文档（演示）", Mountpoint: "本机演示目录", State: model.MountStatus{SchemaVersion: 1, MountID: mountID, Running: true, LocalSaved: true, CloudState: "conflict", Conflicts: len(conflicts)}}}, Conflicts: conflicts}, nil
}
func (b *backend) Config(context.Context) (ui.ConfigView, error) {
	return ui.ConfigView{Profiles: []ui.ProfileView{{ID: "7b589a5e-d33b-4b57-8bbe-40853f19ed12", Name: "演示账号", Brand: "feishu", Authorized: false}}, Remotes: []ui.RemoteView{}, Mounts: []ui.MountView{{ID: mountID, Name: "团队文档（演示）", Mountpoint: "本机演示目录"}}}, nil
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
func (b *backend) Mount(context.Context, model.MountID, string) error {
	return errors.New("preview cannot mount")
}
func (b *backend) Conflict(_ context.Context, id model.ConflictID) (ui.ConflictInputs, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id != conflictID {
		return ui.ConflictInputs{}, os.ErrNotExist
	}
	base := "# 协作文件夹使用说明\n\n共享目录用于保存产品方案和会议记录。\n\n## 本周安排\n\n- 周三完成初稿\n- 周五评审\n\n联系人：项目负责人\n"
	local := "# 协作文件夹使用说明\n\n共享目录用于保存产品方案和会议记录。\n\n## 本周安排\n\n- 周四完成初稿\n- 周五评审\n\n联系人：设计负责人\n"
	remote := "# 协作文件夹使用说明\n\n共享目录用于保存产品方案、会议记录和验收清单。\n\n## 本周安排\n\n- 周三完成初稿\n- 下周一评审\n\n联系人：项目负责人\n"
	return ui.ConflictInputs{Conflict: model.Conflict{ID: conflictID, EntryID: entryID, Generation: 12, RemoteRevision: "38"}, Entry: model.Entry{ID: entryID, Remote: "产品设计/协作文档.md", Generation: 12}, Base: []byte(base), Local: []byte(local), Remote: []byte(remote), Draft: b.draft, Text: true}, nil
}
func (b *backend) SaveDraft(_ context.Context, _ model.ConflictID, v []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.draft = append([]byte{}, v...)
	return nil
}
func (b *backend) Resolve(_ context.Context, r model.ResolveRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.ExpectedGeneration != 12 || r.ExpectedRemoteRevision != "38" {
		return model.ErrVersionChanged
	}
	b.resolved = true
	return nil
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
	h, err := ui.Start(ctx, ui.Options{Backend: &backend{}, RuntimeDir: dir, ConfigKey: "synthetic-preview", Notifier: silent{}})
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
