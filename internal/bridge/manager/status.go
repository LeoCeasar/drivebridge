package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
)

func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, store.ErrCreateNeedsAttention):
		return "creation_needs_attention"
	case errors.Is(err, model.ErrUnknownResult):
		return "unknown_result"
	case errors.Is(err, ErrArchived):
		return "mount_archived"
	case errors.Is(err, ErrMountUnsynced):
		return "mount_unsynced"
	case errors.Is(err, ErrHistoryPending):
		return "history_pending"
	case errors.Is(err, ErrCleanupPending):
		return "cleanup_pending"
	case errors.Is(err, ErrPending), errors.Is(err, auth.ErrReferencesBusy), errors.Is(err, auth.ErrBusy), errors.Is(err, store.ErrLocked), errors.Is(err, store.ErrBusy):
		return "pending"
	case errors.Is(err, model.ErrIdentityChanged), errors.Is(err, ErrInstanceChanged):
		return "identity_changed"
	case errors.Is(err, model.ErrGenerationChanged), errors.Is(err, model.ErrVersionChanged):
		return "version_changed"
	case errors.Is(err, ErrDriver), errors.Is(err, model.ErrUnsupportedContent):
		return "driver_unavailable"
	case errors.Is(err, auth.ErrUserMissing), errors.Is(err, auth.ErrMissingScopes), errors.Is(err, auth.ErrNeedsReconcile):
		return "unauthorized"
	case errors.Is(err, ErrConfig), errors.Is(err, config.ErrUnknownSchema):
		return "config"
	case errors.Is(err, ErrReadyTimeout):
		return "starting"
	case errors.Is(err, ErrUnavailable):
		return "worker_unavailable"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "network"
	default:
		return "operation_failed"
	}
}
func errorFromCode(code string) error {
	switch code {
	case "creation_needs_attention":
		return store.ErrCreateNeedsAttention
	case "creation_unconfirmed", "unknown_result":
		return model.ErrUnknownResult
	case "mount_archived":
		return ErrArchived
	case "mount_unsynced":
		return ErrMountUnsynced
	case "history_pending":
		return ErrHistoryPending
	case "cleanup_pending":
		return ErrCleanupPending
	case "pending":
		return ErrPending
	case "identity_changed":
		return model.ErrIdentityChanged
	case "version_changed":
		return model.ErrVersionChanged
	case "driver_unavailable":
		return ErrDriver
	case "unauthorized":
		return auth.ErrNeedsReconcile
	case "config":
		return ErrConfig
	case "starting":
		return ErrReadyTimeout
	case "worker_unavailable":
		return ErrUnavailable
	case "network":
		return context.DeadlineExceeded
	default:
		return errors.New("operation failed")
	}
}
func ErrorInfo(err error) *Error {
	code := errorCode(err)
	message := code
	switch code {
	case "mount_unsynced":
		message = "尚有未同步内容，未删除。请先查看 upload status 或 conflict list 并处理后重试。"
	case "history_pending":
		message = "历史尚未完成归档，未删除。请保持应用运行，在 history status 中查看进度后重试。"
	case "cleanup_pending":
		message = "挂载已删除，缓存清理待完成；后台将自动重试。"
	}
	return &Error{Code: code, Message: message, Retryable: code == "network" || code == "worker_unavailable" || code == "history_pending" || code == "cleanup_pending"}
}
func ExitCode(err error) int {
	switch errorCode(err) {
	case "":
		return 0
	case "config":
		return 2
	case "unauthorized":
		return 3
	case "driver_unavailable":
		return 4
	case "network", "worker_unavailable":
		return 5
	case "pending", "mount_unsynced", "history_pending", "cleanup_pending":
		return 6
	case "identity_changed", "version_changed":
		return 7
	default:
		return 1
	}
}
func storeStatus(ctx context.Context, id model.MountID, s *store.Store) (model.MountStatus, error) {
	out := model.MountStatus{CloudSyncInterval: syncer.BackgroundSyncInterval.String(), SchemaVersion: 1, MountID: id, LocalSaved: true, CloudState: "synced"}
	jobs, err := s.Pending(ctx)
	if err != nil {
		return out, err
	}
	awaiting := 0
	for _, j := range jobs {
		journal, journalErr := s.Journal(ctx, j.ID)
		if journalErr != nil {
			return out, journalErr
		}
		if journal.ErrorCode != "" {
			out.LastError = journal.ErrorCode
		}
		switch j.State {
		case model.StatusMerging, model.StatusUploading:
			out.Inflight++
		case model.StatusAwaitingVerification:
			out.Queued++
			awaiting++
		default:
			out.Queued++
		}
		if j.State == model.StatusPaused || j.State == model.StatusBlocked || j.State == model.StatusAwaitingVerification {
			en, err := s.Entry(ctx, j.EntryID)
			if err != nil {
				return out, err
			}
			issue := model.SyncIssue{Diagnostic: journal.Diagnostic, JobID: j.ID, EntryID: j.EntryID, Name: en.Remote, Code: journal.ErrorCode, State: j.State, NewDocument: en.Kind == model.KindDocx && en.Token == ""}
			if j.State == model.StatusAwaitingVerification && issue.Code == "" {
				issue.Code = "unknown_result"
			}
			creation, createErr := s.CreateForJob(ctx, j.ID)
			if createErr != nil && !errors.Is(createErr, store.ErrNotFound) {
				return out, createErr
			}
			if createErr == nil && (creation.State == "unknown" || creation.State == "bound" || creation.State == "renaming" || creation.State == "prepared") {
				reason := creation.CheckReason
				if reason == "" {
					reason = creation.FailureCode
				}
				if reason == "" {
					reason = "unknown_result"
				}
				issue.CreateRecovery = &model.CreateRecoveryInfo{CanRecreate: creation.NeedsAttention && (creation.State == "prepared" || creation.State == "unknown") && creation.Token == "" && creation.CandidateToken == "" && en.Token == "" && en.RemoteCheckpoint == "" && en.LocalCheckpoint == "", CurrentGeneration: en.Generation, CurrentLocal: en.Local, Attempts: creation.CheckAttempts, Reason: reason}
				if issue.Diagnostic == nil {
					failure := map[string]string{"request_timeout": "首次创建请求超时", "permission_denied": "首次创建请求权限不足", "auth_required": "首次创建请求需要重新授权", "unauthorized": "首次创建请求未获授权", "identity_changed": "首次创建请求的账号身份发生变化", "network_or_protocol": "首次创建请求网络或服务响应未获确认", "offline": "首次创建请求无法连接飞书", "rate_limited": "首次创建请求受到频率限制", "canceled": "首次创建请求被中断"}[creation.FailureCode]
					if creation.FailureCode == "" || creation.FailureCode == "unknown_result" {
						failure = "旧记录未保留首次错误原因"
					} else if failure == "" {
						failure = "首次创建请求未获确认"
					}
					check := map[string]string{"not_found": "尚未找到先前创建的文件", "multiple_matches": "找到多个候选文件", "content_mismatch": "候选文件内容与原提交不同", "location_changed": "先前创建的文件名称或位置已变化", "network_or_protocol": "核验时网络或服务响应未获确认", "request_timeout": "核验请求超时", "permission_denied": "核验请求权限不足", "rename_unconfirmed": "云端改名结果尚未确认"}[creation.CheckReason]
					if check != "" {
						failure += "；最近核验：" + check
					}
					issue.Diagnostic = &model.SyncDiagnostic{Kind: "creation_unconfirmed", Message: failure + "。"}
				}
			}
			r, err := s.ReadBlob(ctx, j.Local)
			if err != nil {
				return out, err
			}
			info, err := r.(interface{ Stat() (os.FileInfo, error) }).Stat()
			closeErr := r.Close()
			if err != nil {
				return out, err
			}
			if closeErr != nil {
				return out, closeErr
			}
			issue.LocalSaved = true
			issue.CanDownload = info.Size() <= maxUploadCopyBytes
			out.Issues = append(out.Issues, issue)
			if j.State == model.StatusBlocked || j.State == model.StatusPaused && out.CloudState != "blocked" {
				out.CloudState = string(j.State)
			}
		}
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil {
		return out, err
	}
	out.Conflicts = len(conflicts)
	publications, err := s.PendingPublications(ctx)
	if err != nil {
		return out, err
	}
	out.Queued += len(publications)
	if len(publications) > 0 && out.CloudState == "synced" {
		out.CloudState = "awaiting_publish"
	}
	namespaces, err := s.PendingNamespace(ctx)
	if err != nil {
		return out, err
	}
	out.Queued += len(namespaces)
	actions, err := s.NamespaceActions(ctx)
	if err != nil {
		return out, err
	}
	for _, op := range actions {
		journal, err := s.NamespaceJournal(ctx, op.ID)
		if err != nil {
			return out, err
		}
		if journal.Dispatched {
			out.Inflight++
		} else {
			out.Queued++
		}
	}
	if out.Conflicts > 0 {
		out.CloudState = "conflict"
	} else if out.Inflight > 0 {
		out.CloudState = "uploading"
	} else if awaiting > 0 && out.CloudState != "blocked" && out.CloudState != "paused" {
		out.CloudState = "awaiting_verification"
	} else if out.Queued > 0 && out.CloudState == "synced" {
		out.CloudState = "queued"
	}
	return out, nil
}
func (m *Manager) Status(ctx context.Context, id model.MountID) (model.MountStatus, error) {
	var out model.MountStatus
	var currentHistory *model.HistoryStatus
	err := m.call(ctx, id, "/status", nil, &out)
	if err == nil {
		return out, nil
	}
	if !os.IsNotExist(err) {
		return out, err
	}
	spec, err := m.spec(ctx, id)
	if err != nil {
		return out, err
	}
	if err = m.validateOwnership(ctx, spec); err != nil {
		return out, err
	}
	out = model.MountStatus{CloudSyncInterval: syncer.BackgroundSyncInterval.String(), SchemaVersion: 1, MountID: id, CloudState: "stopped", LocalSaved: true}
	if _, statErr := os.Stat(filepath.Join(spec.Paths.Root, "store.db")); statErr == nil {
		lock, lockErr := privateLock(m.runtimeLockPath(id))
		if lockErr != nil {
			return out, lockErr
		}
		ok, lockErr := lock.TryLock()
		if lockErr != nil {
			return out, lockErr
		}
		if !ok {
			return out, ErrPending
		}
		defer lock.Unlock()
		s, openErr := store.Open(spec.Paths.Root)
		if openErr != nil {
			return out, openErr
		}
		defer s.Close()
		if openErr = s.BindScope(ctx, ownershipFor(m.options.ConfigFile, spec).Scope); openErr != nil {
			return out, openErr
		}
		out, openErr = storeStatus(ctx, id, s)
		if openErr != nil {
			return out, openErr
		}
		out.Running = false
		actual, historyErr := s.HistoryStatus(ctx)
		if historyErr != nil {
			return out, historyErr
		}
		currentHistory = &actual
		pending, cacheErr := offlineCachePending(ownershipFor(m.options.ConfigFile, spec))
		if cacheErr != nil {
			return out, cacheErr
		}
		if pending {
			out.VFSPending++
		}
		out.LocalSaved = out.VFSPending == 0
	} else if !os.IsNotExist(statErr) {
		return out, statErr
	}
	readLastWorkerError(spec.Paths.Root, &out)
	readHistoryState(spec.Paths.Root, &out)
	if currentHistory != nil {
		if cached := out.History; cached != nil {
			currentHistory.Bytes, currentHistory.BudgetBytes, currentHistory.OverBudget, currentHistory.UsageAt = cached.Bytes, cached.BudgetBytes, cached.OverBudget, cached.UsageAt
			currentHistory.MaintenanceError = cached.MaintenanceError
		}
		out.History = currentHistory
	}
	return out, nil
}

// Once a child has been spawned, readiness must not acquire the runtime lock to
// inspect its offline Store. Doing so can defeat that child's first ownership
// claim. Only authenticated IPC can report readiness; a last-error hint does not.
func (m *Manager) startupStatus(ctx context.Context, spec mountSpec) (model.MountStatus, error) {
	var out model.MountStatus
	err := m.call(ctx, spec.Mount.ID, "/status", nil, &out)
	if err == nil {
		return out, nil
	}
	if !os.IsNotExist(err) {
		return out, err
	}
	out = model.MountStatus{CloudSyncInterval: syncer.BackgroundSyncInterval.String(), SchemaVersion: 1, MountID: spec.Mount.ID, CloudState: "starting"}
	readLastWorkerError(spec.Paths.Root, &out)
	return out, nil
}

func readLastWorkerError(root string, out *model.MountStatus) {
	raw, err := os.ReadFile(filepath.Join(root, "last-state.json"))
	if err == nil {
		var state struct {
			LastError string `json:"last_error"`
		}
		if json.Unmarshal(raw, &state) == nil {
			out.LastError = state.LastError
		}
	}
}

type cacheManifest struct {
	SchemaVersion      int           `json:"schema_version"`
	MountID            model.MountID `json:"mount_id"`
	DataRoot, MetaRoot string
}
type CacheStatus struct {
	BlobFiles int   `json:"blob_files"`
	BlobBytes int64 `json:"blob_bytes"`
}

func cacheStatus(root string) (CacheStatus, error) {
	var out CacheStatus
	err := filepath.WalkDir(filepath.Join(root, "blobs"), func(p string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return ErrConfig
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			out.BlobFiles++
			out.BlobBytes += info.Size()
		}
		return nil
	})
	return out, err
}
func storeGCOptions(budget int64) store.GCOptions { return store.GCOptions{CleanBudgetBytes: budget} }
func (m *Manager) UploadStatus(ctx context.Context, id model.MountID) ([]model.Job, error) {
	var out []model.Job
	err := m.withStore(ctx, id, "/uploads", nil, &out, func(s *store.Store) error { var err error; out, err = s.Pending(ctx); return err })
	if errors.Is(err, errNoState) {
		return []model.Job{}, nil
	}
	return out, err
}
func (m *Manager) Retry(ctx context.Context, id model.MountID) error {
	var out struct{}
	return m.call(ctx, id, "/retry", struct{}{}, &out)
}
func (m *Manager) CleanCache(ctx context.Context, id model.MountID) (store.GCResult, error) {
	var out store.GCResult
	err := m.withStore(ctx, id, "/cache/clean", struct{}{}, &out, func(s *store.Store) error {
		var err error
		spec, err := m.spec(ctx, id)
		if err != nil {
			return err
		}
		out, err = s.GC(ctx, storeGCOptions(spec.Mount.CacheMaxBytes))
		return err
	})
	if errors.Is(err, errNoState) {
		return store.GCResult{}, nil
	}
	return out, err
}
func (m *Manager) CacheStatus(ctx context.Context, id model.MountID) (CacheStatus, error) {
	var out CacheStatus
	err := m.withStore(ctx, id, "/cache", nil, &out, func(s *store.Store) error {
		spec, err := m.spec(ctx, id)
		if err == nil {
			out, err = cacheStatus(spec.Paths.Root)
		}
		return err
	})
	if errors.Is(err, errNoState) {
		return CacheStatus{}, nil
	}
	return out, err
}

var errNoState = errors.New("mount state has not been created")

func (m *Manager) withStore(ctx context.Context, id model.MountID, route string, input, output any, fn func(*store.Store) error) error {
	if err := m.call(ctx, id, route, input, output); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	spec, err := m.spec(ctx, id)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(spec.Paths.Root, "store.db")); os.IsNotExist(statErr) {
		return errNoState
	} else if statErr != nil {
		return statErr
	}
	l, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		return err
	}
	ok, err := l.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return ErrPending
	}
	defer l.Unlock()
	s, err := store.Open(spec.Paths.Root)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = s.BindScope(ctx, ownershipFor(m.options.ConfigFile, spec).Scope); err != nil {
		return err
	}
	return fn(s)
}

type LogEvent struct {
	At      time.Time     `json:"at"`
	Code    string        `json:"code"`
	MountID model.MountID `json:"mount_id"`
}

func (w *worker) log(code string) {
	if platform.EnsurePrivateDir(w.spec.Paths.Logs) != nil {
		return
	}
	file := filepath.Join(w.spec.Paths.Logs, "events.jsonl")
	if platform.CheckNoSymlinks(file) != nil {
		return
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	if platform.SecurePrivate(file, false) != nil {
		return
	}
	_ = json.NewEncoder(f).Encode(LogEvent{At: time.Now().UTC(), Code: code, MountID: w.spec.Mount.ID})
}
func readLogs(file string) ([]LogEvent, error) {
	if err := platform.CheckNoSymlinks(file); err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return []LogEvent{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, 8<<20))
	out := []LogEvent{}
	for scanner.Scan() {
		var e LogEvent
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			out = append(out, e)
			if len(out) > 200 {
				out = out[1:]
			}
		}
	}
	return out, scanner.Err()
}
func (m *Manager) Logs(ctx context.Context, id model.MountID) ([]LogEvent, error) {
	var out []LogEvent
	if err := m.call(ctx, id, "/logs", nil, &out); err == nil {
		return out, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	spec, err := m.spec(ctx, id)
	if err != nil {
		return nil, err
	}
	return readLogs(filepath.Join(spec.Paths.Logs, "events.jsonl"))
}
