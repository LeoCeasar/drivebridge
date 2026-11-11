package overlay

import (
	"context"
	"errors"
	"strings"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type accountSyncPolicy struct{ blocked []string }

func (p accountSyncPolicy) allows(remote string) bool {
	for _, root := range p.blocked {
		if remote == root || strings.HasPrefix(remote, root+"/") {
			return false
		}
	}
	return true
}

// Policy hides a local projection, never deletes the observed catalog or Store
// rows. Resolve entrance tokens to actual mapped paths, including stable naming
// and old canonical placements, rather than guessing paths from display names.
func (f *Fs) accountPolicy(ctx context.Context) (accountSyncPolicy, error) {
	var policy accountSyncPolicy
	source, ok := f.source.(*accountSource)
	if !ok || source.options.SyncHiddenShared == nil {
		return policy, nil
	}
	enabled, err := source.options.SyncHiddenShared(ctx)
	if err != nil || enabled {
		return policy, err
	}
	snapshot, err := source.options.ReadSnapshot(ctx)
	if err != nil {
		return policy, err
	}
	// An explicit child may share a canonical root instead of receiving a
	// duplicate mount path. Keep that container so disabling its hidden
	// discovery provenance does not strand the separately selected child.
	retainedAnchors := map[string]bool{}
	for _, entry := range snapshot.Entries {
		if !discovery.HiddenSharedOnly(entry) && entry.CanonicalRootToken != "" {
			retainedAnchors[entry.CanonicalRootToken] = true
		}
	}
	for _, entry := range snapshot.Entries {
		if !accountEntrance(entry) || !discovery.HiddenSharedOnly(entry) || retainedAnchors[entry.Token] {
			continue
		}
		mapped, err := f.store.EntryByToken(ctx, entry.Token)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return policy, err
		}
		if mapped.Kind != model.KindFolder || mapped.Remote == "" {
			continue
		}
		policy.blocked = append(policy.blocked, mapped.Remote)
	}
	return policy, nil
}

func (f *Fs) SyncPathAllowed(ctx context.Context, remote string) (bool, error) {
	policy, err := f.accountPolicy(ctx)
	return err == nil && policy.allows(remote), err
}

// SyncEntryAllowed also consults accepted namespace intent. A local rename out
// of a disabled root must not let its pending content bypass that root's pause.
func (f *Fs) SyncEntryAllowed(ctx context.Context, id model.EntryID) (bool, error) {
	policy, err := f.accountPolicy(ctx)
	if err != nil {
		return false, err
	}
	if len(policy.blocked) == 0 {
		return true, nil
	}
	entry, err := f.store.Entry(ctx, id)
	if err != nil {
		return false, err
	}
	if !policy.allows(entry.Remote) {
		return false, nil
	}
	actions, err := f.store.NamespaceActions(ctx)
	if err != nil {
		return false, err
	}
	for _, action := range actions {
		work, err := f.store.NamespaceJournal(ctx, action.ID)
		if err != nil {
			return false, err
		}
		relevant := id == action.SrcID || id == action.DstID || id == action.ResultID
		for _, before := range work.Before {
			if before.ID == id {
				relevant = true
				if !policy.allows(before.Remote) {
					return false, nil
				}
			}
		}
		if relevant && (!policy.allows(action.Src) || !policy.allows(action.Dst)) {
			return false, nil
		}
	}
	return true, nil
}
