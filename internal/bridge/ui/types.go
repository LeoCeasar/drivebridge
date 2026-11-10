package ui

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/model"
)

type ProfileView struct {
	ID         model.ProfileID `json:"id"`
	Name       string          `json:"name"`
	Brand      string          `json:"brand"`
	Authorized bool            `json:"authorized"`
}
type RemoteView struct {
	ID          model.RemoteID  `json:"id"`
	Name        string          `json:"name"`
	ProfileID   model.ProfileID `json:"profile_id"`
	FolderToken string          `json:"folder_token"`
}
type MountView struct {
	ID         model.MountID     `json:"id"`
	Name       string            `json:"name"`
	RemoteID   model.RemoteID    `json:"remote_id"`
	Mountpoint string            `json:"mountpoint"`
	State      model.MountStatus `json:"state"`
}
type ConfigView struct {
	CanRecover bool          `json:"can_recover"`
	Profiles   []ProfileView `json:"profiles"`
	Remotes    []RemoteView  `json:"remotes"`
	Mounts     []MountView   `json:"mounts"`
}
type ConflictSummary struct {
	ID      model.ConflictID `json:"id"`
	Name    string           `json:"name"`
	MountID model.MountID    `json:"mount_id"`
}
type StatusView struct {
	SchemaVersion int               `json:"schema_version"`
	Mounts        []MountView       `json:"mounts"`
	Conflicts     []ConflictSummary `json:"conflicts"`
}
type ConfigRequest struct {
	Action      string          `json:"action"`
	Name        string          `json:"name"`
	Brand       string          `json:"brand,omitempty"`
	ProfileID   model.ProfileID `json:"profile_id,omitempty"`
	RemoteID    model.RemoteID  `json:"remote_id,omitempty"`
	FolderToken string          `json:"folder_token,omitempty"`
	Mountpoint  string          `json:"mountpoint,omitempty"`
}
type ConflictInputs struct {
	Conflict                   model.Conflict
	Entry                      model.Entry
	Base, Local, Remote, Draft []byte
	Text                       bool
	SourceURL                  string
}
type DiffHunk struct {
	ID                  int `json:"id"`
	BaseStart, BaseEnd  int
	Base, Local, Remote string
}
type ConflictView struct {
	HasDraft                   bool             `json:"has_draft"`
	ID                         model.ConflictID `json:"id"`
	EntryID                    model.EntryID    `json:"entry_id"`
	Generation                 uint64           `json:"generation"`
	RemoteRevision             string           `json:"remote_revision"`
	Name                       string           `json:"name"`
	Base, Local, Remote, Draft string
	Text                       bool       `json:"text"`
	Hunks                      []DiffHunk `json:"hunks"`
	SourceURL                  string     `json:"source_url,omitempty"`
}
type Backend interface {
	Status(context.Context) (StatusView, error)
	Config(context.Context) (ConfigView, error)
	Configure(context.Context, ConfigRequest) (ConfigView, error)
	AuthStart(context.Context, model.ProfileID) (auth.AuthFlow, error)
	AuthComplete(context.Context, string) error
	Conflict(context.Context, model.ConflictID) (ConflictInputs, error)
	SaveDraft(context.Context, model.ConflictID, []byte) error
	Resolve(context.Context, model.ResolveRequest) error
	Mount(context.Context, model.MountID, string) error
}
type Notifier interface {
	Available() bool
	Notify(context.Context, string, string) error
	Open(context.Context, string) error
}
type Options struct {
	Backend               Backend
	RuntimeDir, ConfigKey string
	Notifier              Notifier
}
