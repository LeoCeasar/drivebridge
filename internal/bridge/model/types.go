// Package model defines the canonical, transport-independent DriveBridge data types.
package model

import (
	"encoding/json"
	"errors"
	"time"
)

type ProfileID string
type RemoteID string
type MountID string
type EntryID string
type BlobID string
type SnapshotID string
type JobID string
type ConflictID string

type Kind string

const (
	KindDocx   Kind = "docx"
	KindFile   Kind = "file"
	KindFolder Kind = "folder"
)

type Status string

const (
	StatusQueued               Status = "queued"
	StatusMerging              Status = "merging"
	StatusUploading            Status = "uploading"
	StatusAwaitingVerification Status = "awaiting_verification"
	StatusAwaitingPublish      Status = "awaiting_publish"
	StatusConflict             Status = "conflict"
	StatusSynced               Status = "synced"
	StatusBlocked              Status = "blocked"
	StatusPaused               Status = "paused"
)

var (
	ErrGenerationChanged  = errors.New("generation changed")
	ErrVersionChanged     = errors.New("version changed")
	ErrUnsupportedContent = errors.New("unsupported content")
	ErrUnknownResult      = errors.New("unknown result")
	ErrIdentityChanged    = errors.New("identity changed")
)

type Config struct {
	SchemaVersion int       `json:"schema_version"`
	Profiles      []Profile `json:"profiles"`
	Remotes       []Remote  `json:"remotes"`
	Mounts        []Mount   `json:"mounts"`
	StateDir      string    `json:"state_dir,omitempty"`
	CacheDir      string    `json:"cache_dir,omitempty"`
}
type Profile struct {
	History      *HistoryPolicy `json:"history,omitempty"`
	ID           ProfileID      `json:"id"`
	Name         string         `json:"name"`
	Brand        string         `json:"brand"`
	AuthRef      string         `json:"auth_ref,omitempty"`
	IdentityHash string         `json:"identity_hash,omitempty"`
}
type Remote struct {
	MountRemoved bool      `json:"mount_removed,omitempty"`
	ID           RemoteID  `json:"id"`
	Name         string    `json:"name"`
	ProfileID    ProfileID `json:"profile_id"`
	FolderToken  string    `json:"folder_token"`
	NewMDPolicy  string    `json:"new_md_policy"`
	AccountRoot  bool      `json:"account_root,omitempty"`
}
type Mount struct {
	BackgroundPullExcludedExtensions []string `json:"background_pull_excluded_extensions"`
	ID                               MountID  `json:"id"`
	Name                             string   `json:"name"`
	RemoteID                         RemoteID `json:"remote_id"`
	Mountpoint                       string   `json:"mountpoint"`
	CacheMode                        string   `json:"cache_mode"`
	CacheMaxBytes                    int64    `json:"cache_max_bytes"`
	CacheMaxAge                      string   `json:"cache_max_age,omitempty"`
	WriteBack                        string   `json:"write_back"`
	PollDocs                         string   `json:"poll_docs"`
	RefreshDirs                      string   `json:"refresh_dirs"`
	MaxDocBytes                      int64    `json:"max_doc_bytes"`
	Notify                           bool     `json:"notify"`
	Autostart                        bool     `json:"autostart"`
	Archived                         bool     `json:"archived,omitempty"`
	MountEngine                      string   `json:"mount_engine"`
	MacOSBackend                     string   `json:"macos_backend,omitempty"`
}

// VerifiedIdentity contains identity metadata, never CLI credentials.
type VerifiedIdentity struct {
	Brand        string `json:"brand"`
	AppID        string `json:"app_id"`
	UserOpenID   string `json:"user_open_id"`
	IdentityHash string `json:"identity_hash"`
}
type IdentityRecord struct {
	ID string `json:"id"`
	VerifiedIdentity
	AuthRef string `json:"auth_ref"`
}
type IdentityRegistry struct {
	SchemaVersion int              `json:"schema_version"`
	Identities    []IdentityRecord `json:"identities"`
}

// Entry keeps the latest accepted Local snapshot separate from Visible and the two checkpoints.
// LocalCheckpoint and actual cloud RemoteCheckpoint can differ after an automatic merge.
type Entry struct {
	ID               EntryID    `json:"id"`
	MountID          MountID    `json:"mount_id"`
	ProfileID        ProfileID  `json:"profile_id"`
	Remote           string     `json:"remote"`
	CloudName        string     `json:"cloud_name"`
	Token            string     `json:"token"`
	Kind             Kind       `json:"kind"`
	Generation       uint64     `json:"generation"`
	Local            BlobID     `json:"local"`
	Visible          BlobID     `json:"visible"`
	LocalCheckpoint  BlobID     `json:"local_checkpoint"`
	Desired          BlobID     `json:"desired"`
	RemoteCheckpoint SnapshotID `json:"remote_checkpoint"`
	Status           Status     `json:"status"`
	FrozenBy         string     `json:"frozen_by"`
}

type Snapshot struct {
	// DocMedia binds local image bodies to this document revision, independently of its projection.
	DocMedia     map[string]BlobID `json:"doc_media,omitempty"`
	FileData     BlobID            `json:"file_data,omitempty"`
	FileIdentity *FileIdentity     `json:"file_identity,omitempty"`
	ID           SnapshotID        `json:"id"`
	Token        string            `json:"token"`
	Revision     string            `json:"revision"`
	XML          BlobID            `json:"xml"`
	Markdown     BlobID            `json:"markdown"`
	BlockMap     BlobID            `json:"block_map"`
	Size         int64             `json:"size"`
	ModTime      time.Time         `json:"mod_time"`
}

type Job struct {
	ID           JobID      `json:"id"`
	EntryID      EntryID    `json:"entry_id"`
	Generation   uint64     `json:"generation"`
	Local        BlobID     `json:"local"`
	BaseLocal    BlobID     `json:"base_local"`
	Plan         BlobID     `json:"plan"`
	BaseRemote   SnapshotID `json:"base_remote"`
	OperationIDs []string   `json:"operation_ids"`
	State        Status     `json:"state"`
}

type Conflict struct {
	ID             ConflictID `json:"id"`
	EntryID        EntryID    `json:"entry_id"`
	Generation     uint64     `json:"generation"`
	Base           BlobID     `json:"base"`
	Local          BlobID     `json:"local"`
	Remote         BlobID     `json:"remote"`
	RemoteRevision string     `json:"remote_revision"`
	Draft          BlobID     `json:"draft"`
	Reason         string     `json:"reason"`
	Resolved       bool       `json:"resolved"`
}

type NamespaceOp struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Src         string  `json:"src"`
	Dst         string  `json:"dst"`
	State       string  `json:"state"`
	SrcID       EntryID `json:"src_id"`
	DstID       EntryID `json:"dst_id"`
	ResultID    EntryID `json:"result_id"`
	ExpectedSrc uint64  `json:"expected_src"`
	ExpectedDst uint64  `json:"expected_dst"`
	DstExists   bool    `json:"dst_exists"`
	Input       BlobID  `json:"input"`
	Previous    BlobID  `json:"previous"`
}

type CreateOp struct {
	ID            string  `json:"id"`
	JobID         JobID   `json:"job_id"`
	EntryID       EntryID `json:"entry_id"`
	Generation    uint64  `json:"generation"`
	Kind          Kind    `json:"kind"`
	ParentToken   string  `json:"parent_token"`
	TemporaryName string  `json:"temporary_name"`
	FinalName     string  `json:"final_name"`
	Token         string  `json:"token"`
	Revision      string  `json:"revision"`
	Input         BlobID  `json:"input"`
	State         string  `json:"state"`
}

// Operation.ID is a local journal ID, never a server idempotency key.
// ClientToken must remain empty for docs shortcuts; missing Route/CapabilityID cannot dispatch.
type Operation struct {
	Kind         string          `json:"kind"`
	BlockID      string          `json:"block_id"`
	AfterBlockID string          `json:"after_block_id"`
	ClientToken  string          `json:"client_token"`
	Payload      json.RawMessage `json:"payload"`
	ID           string          `json:"id,omitempty"`
	Route        string          `json:"route,omitempty"`
	CapabilityID string          `json:"capability_id,omitempty"`
}

// OperationCapability is a descriptor; an empty or populated value grants no authority.
type OperationCapability struct {
	ID    string `json:"id"`
	Route string `json:"route"`
	Kind  string `json:"kind"`
}

// Block Start/End are half-open UTF-8 byte offsets; RawXML and IDs belong in sidecars.
type Block struct {
	Children []Block `json:"children,omitempty"`
	Readable bool    `json:"readable,omitempty"`
	LocalID  string  `json:"local_id"`
	RemoteID string  `json:"remote_id"`
	ParentID string  `json:"parent_id"`
	Kind     string  `json:"kind"`
	Hash     string  `json:"hash"`
	Start    int     `json:"start"`
	End      int     `json:"end"`
	Opaque   bool    `json:"opaque"`
	RawXML   []byte  `json:"raw_xml"`
}

type DocAsset struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	MIME  string `json:"mime"`
}

type DocView struct {
	ProjectionVersion int             `json:"projection_version,omitempty"`
	Assets            []DocAsset      `json:"assets,omitempty"`
	SourceURL         string          `json:"source_url,omitempty"`
	ReferenceMap      json.RawMessage `json:"reference_map,omitempty"`
	XML               []byte          `json:"xml"`
	Markdown          []byte          `json:"markdown"`
	Revision          string          `json:"revision"`
	Blocks            []Block         `json:"blocks"`
}

// DocSidecar retains projection context alongside separate immutable XML and
// Markdown blobs. References describe source content, never permission to replay it.
type DocSidecar struct {
	ProjectionVersion int             `json:"projection_version,omitempty"`
	Assets            []DocAsset      `json:"assets,omitempty"`
	Blocks            []Block         `json:"blocks"`
	SourceURL         string          `json:"source_url,omitempty"`
	ReferenceMap      json.RawMessage `json:"reference_map,omitempty"`
}

type Hunk struct {
	BaseStart int      `json:"base_start"`
	BaseEnd   int      `json:"base_end"`
	Base      []string `json:"base"`
	Local     []string `json:"local"`
	Remote    []string `json:"remote"`
}

type MergeResult struct {
	Deferred       bool        `json:"deferred,omitempty"` // more insertions require IDs from a verified readback
	Markdown       []byte      `json:"markdown"`
	Operations     []Operation `json:"operations"`
	Hunks          []Hunk      `json:"hunks"`
	ConflictReason string      `json:"conflict_reason"`
}

type PublishCandidate struct {
	EntryID            EntryID    `json:"entry_id"`
	ExpectedGeneration uint64     `json:"expected_generation"`
	ExpectedRemote     SnapshotID `json:"expected_remote"`
	NewRemote          SnapshotID `json:"new_remote"`
	Visible            BlobID     `json:"visible"`
}

type ResolveRequest struct {
	Action                 string     `json:"action,omitempty"`
	ConflictID             ConflictID `json:"conflict_id"`
	ExpectedGeneration     uint64     `json:"expected_generation"`
	ExpectedRemoteRevision string     `json:"expected_remote_revision"`
	Content                []byte     `json:"content"`
}

// DirectoryIndexStatus reports metadata coverage, independently of file bodies
// and upload confirmation. Known is not an exhaustive cloud inventory.
type DirectoryIndexStatus struct {
	Known      int `json:"known"`
	Cached     int `json:"cached"`
	Refreshing int `json:"refreshing"`
	Failed     int `json:"failed"`
}

// SyncIssue is a durable failed upload, distinct from a version conflict.
type SyncIssue struct {
	JobID       JobID   `json:"job_id"`
	EntryID     EntryID `json:"entry_id"`
	Name        string  `json:"name"`
	Code        string  `json:"code"`
	State       Status  `json:"state"`
	LocalSaved  bool    `json:"local_saved"`
	NewDocument bool    `json:"new_document"`
	CanDownload bool    `json:"can_download"`
}

type MountStatus struct {
	History        *HistoryStatus        `json:"history,omitempty"`
	Issues         []SyncIssue           `json:"issues,omitempty"`
	DirectoryIndex *DirectoryIndexStatus `json:"directory_index,omitempty"`
	// CacheBytes is the clean-body cache size observed by the last periodic
	// sweep (store blobs plus fetched VFS data). Nil until measured; it is an
	// observation for display, never a quota decision.
	CacheBytes *int64 `json:"cache_bytes,omitempty"`
	SchemaVersion  int                   `json:"schema_version"`
	MountID        MountID               `json:"mount_id"`
	Running        bool                  `json:"running"`
	LocalSaved     bool                  `json:"local_saved"`
	VFSPending     int                   `json:"vfs_pending"`
	Queued         int                   `json:"queued"`
	Inflight       int                   `json:"inflight"`
	Conflicts      int                   `json:"conflicts"`
	CloudState     string                `json:"cloud_state"`
	LastError      string                `json:"last_error"`
}

// Operation kinds describe logical mutations, never verified transport capabilities.
const (
	OperationBlockReplace     = "block_replace"
	OperationBlockInsertAfter = "block_insert_after"
	OperationBlockDelete      = "block_delete"
)

// OperationPayload hashes semantic content without remote IDs. Delete has empty XML.
type OperationPayload struct {
	ExplicitResolution bool   `json:"explicit_resolution,omitempty"`
	XML                string `json:"xml,omitempty"`
	BeforeHash         string `json:"before_hash,omitempty"`
	AfterHash          string `json:"after_hash,omitempty"`
}
