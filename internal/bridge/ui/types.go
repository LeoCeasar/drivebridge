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
	AccountRoot  bool            `json:"account_root,omitempty"`
	MountRemoved bool            `json:"mount_removed,omitempty"`
	ID           model.RemoteID  `json:"id"`
	Name         string          `json:"name"`
	ProfileID    model.ProfileID `json:"profile_id"`
	FolderToken  string          `json:"folder_token"`
}
type MountView struct {
	Archived                         bool              `json:"archived,omitempty"`
	BackgroundPullExcludedExtensions []string          `json:"background_pull_excluded_extensions"`
	StatusWarning                    string            `json:"status_warning,omitempty"`
	StatusUpdatedAt                  string            `json:"status_updated_at,omitempty"`
	CacheMaxBytes                    int64             `json:"cache_max_bytes"`
	CacheMaxAge                      string            `json:"cache_max_age"`
	ID                               model.MountID     `json:"id"`
	Name                             string            `json:"name"`
	DisplayName                      string            `json:"display_name,omitempty"`
	AccountName                      string            `json:"account_name,omitempty"`
	AccountRoot                      bool              `json:"account_root,omitempty"`
	ProfileID                        model.ProfileID   `json:"profile_id,omitempty"`
	RemoteID                         model.RemoteID    `json:"remote_id"`
	Mountpoint                       string            `json:"mountpoint"`
	State                            model.MountStatus `json:"state"`
}
type ConfigView struct {
	Platform   string `json:"platform"`
	CanRecover bool   `json:"can_recover"`
	// OpenFolderLabel is the host's own file-manager name; the browser never
	// guesses the platform. CanOpenFolder is false for fixture hosts.
	OpenFolderLabel string        `json:"open_folder_label,omitempty"`
	CanOpenFolder   bool          `json:"can_open_folder"`
	Profiles        []ProfileView `json:"profiles"`
	Remotes         []RemoteView  `json:"remotes"`
	Mounts          []MountView   `json:"mounts"`
}
type ConflictSummary struct {
	SourceRevision string           `json:"source_revision,omitempty"`
	Revision       string           `json:"revision,omitempty"`
	ID             model.ConflictID `json:"id"`
	Name           string           `json:"name"`
	MountID        model.MountID    `json:"mount_id"`
}
type StatusView struct {
	Platform         string            `json:"platform,omitempty"`
	OpenFolderLabel  string            `json:"open_folder_label,omitempty"`
	CanOpenFolder    bool              `json:"can_open_folder"`
	Partial          bool              `json:"partial,omitempty"`
	SchemaVersion    int               `json:"schema_version"`
	CanRecoverUpload bool              `json:"can_recover_upload"`
	CanRetry         bool              `json:"can_retry"`
	Mounts           []MountView       `json:"mounts"`
	Conflicts        []ConflictSummary `json:"conflicts"`
}
type ConfigRequest struct {
	MountID                          model.MountID   `json:"mount_id,omitempty"`
	BackgroundPullExcludedExtensions *[]string       `json:"background_pull_excluded_extensions,omitempty"`
	CacheMaxAge                      string          `json:"cache_max_age,omitempty"`
	CacheSize                        string          `json:"cache_size,omitempty"`
	CacheMaxBytes                    int64           `json:"cache_max_bytes,omitempty"`
	Action                           string          `json:"action"`
	Name                             string          `json:"name"`
	Brand                            string          `json:"brand,omitempty"`
	ProfileID                        model.ProfileID `json:"profile_id,omitempty"`
	RemoteID                         model.RemoteID  `json:"remote_id,omitempty"`
	FolderToken                      string          `json:"folder_token,omitempty"`
	Mountpoint                       string          `json:"mountpoint,omitempty"`
	MacOSBackend                     string          `json:"macos_backend,omitempty"`
}
type ConflictInputs struct {
	Conflict                   model.Conflict
	Entry                      model.Entry
	Base, Local, Remote, Draft []byte
	Text                       bool
	SourceURL                  string
	Stale                      bool
	RefreshWarning             string
}
type DiffChange struct {
	Start, End         int
	BaseStart, BaseEnd int
	Kind               string
	Side               string
}
type DiffHunk struct {
	ID                                                   int `json:"id"`
	BaseStart, BaseEnd                                   int
	LocalStart, LocalEnd                                 int
	RemoteStart, RemoteEnd                               int
	Base, Local, Remote                                  string
	BaseChanges, LocalChanges, RemoteChanges             []DiffChange
	LocalAdded, LocalRemoved, RemoteAdded, RemoteRemoved int
	WholeDocument                                        bool
}

type ConflictView struct {
	Revision                   string           `json:"revision,omitempty"`
	SourceRevision             string           `json:"source_revision,omitempty"`
	Reason                     string           `json:"reason"`
	RemoteDeleted              bool             `json:"remote_deleted"`
	LocalDeletePending         bool             `json:"local_delete_pending"`
	Stale                      bool             `json:"stale"`
	RefreshWarning             string           `json:"refresh_warning,omitempty"`
	CanExportCopies            bool             `json:"can_export_copies"`
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

// Optional capabilities keep fixture/offline hosts honest about available actions.
type SyncRetrier interface {
	Retry(context.Context, model.MountID) error
}
type ConflictCopies struct {
	Directory string `json:"directory"`
	Base      string `json:"base,omitempty"`
	Local     string `json:"local,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Draft     string `json:"draft,omitempty"`
	Manifest  string `json:"manifest"`
}
type ConflictExporter interface {
	ExportConflict(context.Context, model.ConflictID) (ConflictCopies, error)
}

// ConflictRefresher is an explicit cloud check. Ordinary Conflict reads durable
// local captures; preparation and ordinary opening never require cloud access.
type ConflictRefresher interface {
	RefreshConflict(context.Context, model.ConflictID) (ConflictInputs, error)
}

// ResolveVersionReader checks stored versions before Backend.Resolve performs
// the authoritative cloud recheck.
type ResolveVersionReader interface {
	ConflictForResolve(context.Context, model.ConflictID) (ConflictInputs, error)
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

// UploadRecoverer is available only when a live worker can safely recover a creation.
type UploadRecoverer interface {
	RecoverUpload(context.Context, model.MountID, model.CreateRecoveryRequest) (model.CreateRecoveryResult, error)
}
