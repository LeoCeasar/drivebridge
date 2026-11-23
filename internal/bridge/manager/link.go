package manager

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	ErrLinkUnsupported   = errors.New("path has no Feishu resource")
	ErrLinkNotUploaded   = errors.New("resource has not been uploaded")
	ErrLinkUnavailable   = errors.New("provider resource link unavailable")
	ErrLinkLookupTimeout = errors.New("uncached provider link lookup timed out")
)

type LinkResult struct {
	SchemaVersion  int           `json:"schema_version"`
	MountID        model.MountID `json:"mount_id"`
	EntryID        model.EntryID `json:"entry_id"`
	URL            string        `json:"url"`
	Cached         bool          `json:"cached"`
	PendingChanges bool          `json:"pending_changes"`
}

type linkTarget struct {
	Entry    model.Entry    `json:"entry"`
	Metadata store.Metadata `json:"metadata"`
}
type linkTargetRequest struct {
	Remote string `json:"remote"`
}
type linkCacheRequest struct {
	Expected    model.Entry `json:"expected"`
	VirtualType string      `json:"virtual_type"`
	URL         string      `json:"url"`
}

func cacheLinkTarget(ctx context.Context, s *store.Store, in linkCacheRequest) error {
	if larkcli.SafeResourceURL(in.URL) == "" {
		return ErrLinkUnavailable
	}
	current, err := readLinkTarget(ctx, s, in.Expected.Remote)
	if err != nil {
		return err
	}
	if !sameLinkIdentity(linkTarget{Entry: in.Expected, Metadata: store.Metadata{VirtualType: in.VirtualType}}, current) {
		return model.ErrIdentityChanged
	}
	return s.CacheSourceURL(ctx, in.Expected, in.VirtualType, in.URL)
}

func linkMountRoot(root, goos string) string {
	if goos == "windows" && len(root) == 2 && root[1] == ':' {
		return root + `\`
	}
	return root
}

func sameLinkIdentity(before, after linkTarget) bool {
	return before.Entry.ID == after.Entry.ID && before.Entry.Token == after.Entry.Token && before.Entry.Kind == after.Entry.Kind && before.Entry.Remote == after.Entry.Remote && before.Metadata.VirtualType == after.Metadata.VirtualType
}

// DesktopConfigForShell resolves the current OS user's active configuration.
// A shell entry has no --bridge-config argument; trust only a live authenticated
// desktop endpoint, never a stale file or an environment-selected configuration.
func DesktopConfigForShell(ctx context.Context) (string, error) {
	domain, err := auth.DefaultDomain()
	if err != nil {
		return "", err
	}
	return (&Manager{domain: domain}).desktopConfigForShell(ctx)
}

func (m *Manager) desktopConfigForShell(ctx context.Context) (string, error) {
	locked, err := m.desktopLocked()
	if err != nil || !locked {
		return "", err
	}
	ep, key, err := m.readDesktopEndpoint(false)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err = callEndpoint(ctx, ep, key, "/status", nil, nil); err != nil {
		return "", err
	}
	return ep.ConfigFile, nil
}

// readLinkTarget uses the projection index, not cloud names or filesystem reads.
// macOS decomposes Unicode paths; Windows selection may differ in case.
func readLinkTarget(ctx context.Context, s *store.Store, remote string) (linkTarget, error) {
	e, err := s.EntryByRemote(ctx, remote)
	if errors.Is(err, store.ErrNotFound) && (runtime.GOOS == "windows" || runtime.GOOS == "darwin") && remote != "" {
		parent := ""
		for _, part := range strings.Split(remote, "/") {
			entries, eerr := s.Entries(ctx, parent)
			if eerr != nil {
				return linkTarget{}, eerr
			}
			found := false
			for _, candidate := range entries {
				name := candidate.Remote[strings.LastIndex(candidate.Remote, "/")+1:]
				equal := norm.NFC.String(name) == norm.NFC.String(part)
				if runtime.GOOS == "windows" {
					equal = cases.Fold().String(norm.NFC.String(name)) == cases.Fold().String(norm.NFC.String(part))
				}
				if equal {
					if found {
						return linkTarget{}, ErrLinkUnsupported
					}
					e, parent, found = candidate, candidate.Remote, true
				}
			}
			if !found {
				return linkTarget{}, ErrLinkUnsupported
			}
		}
		err = nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return linkTarget{}, ErrLinkUnsupported
	}
	if err != nil {
		return linkTarget{}, err
	}
	metadata, err := s.Metadata(ctx, e.ID)
	if err != nil {
		return linkTarget{}, err
	}
	if metadata.Tombstone || metadata.FuseSoftRemove != "" {
		return linkTarget{}, ErrLinkUnsupported
	}
	if e.FrozenBy != "" || metadata.SafeSavePending != "" || metadata.SafeSaveParked {
		return linkTarget{}, ErrPending
	}
	if strings.HasPrefix(e.Token, "drivebridge-account-") {
		return linkTarget{}, ErrLinkUnsupported
	}
	if e.Token == "" {
		return linkTarget{}, ErrLinkNotUploaded
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil {
		return linkTarget{}, err
	}
	for _, conflict := range conflicts {
		if conflict.EntryID == e.ID && conflict.Reason == model.ConflictRemoteDeleted {
			return linkTarget{}, ErrLinkUnavailable
		}
	}
	return linkTarget{e, metadata}, nil
}

func (m *Manager) linkTarget(ctx context.Context, id model.MountID, remote string) (out linkTarget, err error) {
	err = m.withStore(ctx, id, "/link/target", linkTargetRequest{remote}, &out, func(s *store.Store) error {
		var err error
		out, err = readLinkTarget(ctx, s, remote)
		return err
	})
	return
}

func (m *Manager) Link(ctx context.Context, absolutePath string) (LinkResult, error) {
	return m.link(ctx, absolutePath, m.lookupResourceURL)
}

func (m *Manager) link(ctx context.Context, absolutePath string, lookup func(context.Context, mountSpec, linkTarget) (string, error)) (LinkResult, error) {
	var out LinkResult
	if !filepath.IsAbs(absolutePath) || strings.ContainsRune(absolutePath, 0) {
		return out, ErrLinkUnsupported
	}
	local := filepath.Clean(absolutePath)
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return out, err
	}
	var selected model.Mount
	var relative string
	for _, mount := range c.Mounts {
		if mount.Archived {
			continue
		}
		root, selectedPath := linkMountRoot(mount.Mountpoint, runtime.GOOS), local
		if runtime.GOOS == "darwin" {
			root, selectedPath = norm.NFC.String(root), norm.NFC.String(selectedPath)
		}
		rel, err := filepath.Rel(root, selectedPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		if len(mount.Mountpoint) > len(selected.Mountpoint) {
			selected, relative = mount, rel
		}
	}
	if selected.ID == "" {
		return out, ErrLinkUnsupported
	}
	spec, err := m.spec(ctx, selected.ID)
	if err != nil {
		return out, err
	}
	if spec.Mount.Mountpoint != selected.Mountpoint || spec.Mount.RemoteID != selected.RemoteID || spec.Mount.Archived {
		return out, model.ErrIdentityChanged
	}
	remote := filepath.ToSlash(relative)
	if relative == "." {
		remote = ""
	}
	if spec.Remote.AccountRoot && (remote == "" || remote == "个人目录" || remote == "共享目录" || remote == "手动添加") {
		return out, ErrLinkUnsupported
	}
	target, err := m.linkTarget(ctx, selected.ID, remote)
	if err != nil {
		return out, err
	}
	if target.Entry.MountID != selected.ID || target.Entry.ProfileID != spec.Profile.ID {
		return out, model.ErrIdentityChanged
	}
	link := ""
	// Metadata survives remote deletion and recreation. A URL without explicit
	// ownership evidence (including legacy metadata) must be queried afresh.
	if target.Metadata.SourceURLToken == target.Entry.Token {
		link = larkcli.SafeResourceURL(target.Metadata.SourceURL)
	}
	kind := string(target.Entry.Kind)
	if target.Metadata.VirtualType != "" {
		kind = target.Metadata.VirtualType
	}
	if link == "" && target.Metadata.CachedLinkToken == target.Entry.Token && target.Metadata.CachedLinkKind == kind {
		link = larkcli.SafeResourceURL(target.Metadata.CachedLinkURL)
	}
	cached := link != ""
	if !cached {
		link, err = lookup(ctx, spec, target)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return out, online.AtPhase(errors.Join(ErrLinkLookupTimeout, err), "link", "link_lookup")
			}
			return out, online.AtPhase(err, "link", "link_lookup")
		}
		link = larkcli.SafeResourceURL(link)
		if link == "" {
			return out, ErrLinkUnavailable
		}
	}
	current, err := m.linkTarget(ctx, selected.ID, remote)
	if err != nil {
		return out, err
	}
	if !sameLinkIdentity(target, current) {
		return out, model.ErrIdentityChanged
	}
	// A configuration/identity change during a remote lookup must not cross accounts.
	latest, err := m.spec(ctx, selected.ID)
	if err != nil {
		return out, err
	}
	if latest.Mount.Mountpoint != spec.Mount.Mountpoint || latest.Mount.RemoteID != spec.Mount.RemoteID || latest.Mount.Archived || latest.Profile.ID != spec.Profile.ID || latest.Profile.IdentityHash != spec.Profile.IdentityHash || latest.Profile.Brand != spec.Profile.Brand || latest.Profile.AuthRef != spec.Profile.AuthRef || latest.Remote.FolderToken != spec.Remote.FolderToken {
		return out, model.ErrIdentityChanged
	}
	if !cached {
		in := linkCacheRequest{Expected: current.Entry, VirtualType: current.Metadata.VirtualType, URL: link}
		if err = m.withStore(ctx, selected.ID, "/link/cache", in, nil, func(s *store.Store) error { return cacheLinkTarget(ctx, s, in) }); err != nil {
			return out, err
		}
	}
	e := current.Entry
	pending := e.Local != "" && e.Local != e.LocalCheckpoint || e.Status == model.StatusQueued || e.Status == model.StatusMerging || e.Status == model.StatusUploading || e.Status == model.StatusAwaitingVerification || e.Status == model.StatusAwaitingPublish || e.Status == model.StatusConflict || e.Status == model.StatusBlocked
	return LinkResult{1, selected.ID, e.ID, link, cached, pending}, nil
}

func (m *Manager) lookupResourceURL(ctx context.Context, spec mountSpec, target linkTarget) (string, error) {
	a, err := m.Auth(ctx)
	if err != nil {
		return "", err
	}
	runner, err := a.ManagedRunner(ctx, spec.Profile.ID)
	if err != nil {
		return "", err
	}
	kind := string(target.Entry.Kind)
	if target.Metadata.VirtualType != "" {
		kind = target.Metadata.VirtualType
	}
	link, err := larkcli.NewDrive(runner, "").ResourceURL(ctx, target.Entry.Token, kind)
	if errors.Is(err, larkcli.ErrMetadataUnknown) {
		return "", ErrLinkUnavailable
	}
	if errors.Is(err, larkcli.ErrIdentity) {
		return "", model.ErrIdentityChanged
	}
	return link, err
}
