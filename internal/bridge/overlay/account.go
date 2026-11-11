package overlay

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

// AccountOptions projects a local discovery catalog and the official personal
// root into one account namespace. AccountToken must match the store scope.
type AccountOptions struct {
	AccountToken     string
	PersonalToken    string
	ReadSnapshot     func(context.Context) (discovery.Snapshot, error)
	SyncHiddenShared func(context.Context) (bool, error)
}

type accountDirectory struct {
	remote, token string
	items         []larkcli.DriveItem
}
type accountCatalog interface {
	accountDirectories(context.Context) ([]accountDirectory, error)
}
type accountProtection interface {
	accountProtected(model.Entry) bool
	accountSynthetic(string) bool
}

type accountSource struct {
	Source
	options   AccountOptions
	mu        sync.RWMutex
	entries   map[string]discovery.Entry
	protected map[string]bool
	groups    map[string][]larkcli.DriveItem
	legacy    bool
}

func newAccountSource(base Source, options AccountOptions) (*accountSource, error) {
	if options.AccountToken == "" || options.PersonalToken == "" || options.AccountToken == options.PersonalToken || options.ReadSnapshot == nil || options.PersonalToken == options.AccountToken+"-shared" || options.PersonalToken == options.AccountToken+"-manual" {
		return nil, errors.New("invalid account source options")
	}
	return &accountSource{Source: base, options: options, entries: map[string]discovery.Entry{}, protected: map[string]bool{}, groups: map[string][]larkcli.DriveItem{}}, nil
}
func (s *accountSource) categoryItems(legacy bool) []larkcli.DriveItem {
	items := []larkcli.DriveItem{
		{Token: s.options.PersonalToken, Name: "个人目录", Type: "folder", ParentToken: s.options.AccountToken},
		{Token: s.options.AccountToken + "-shared", Name: "共享目录", Type: "folder", ParentToken: s.options.AccountToken},
	}
	if legacy {
		items = append(items, larkcli.DriveItem{Token: s.options.AccountToken + "-manual", Name: "手动添加", Type: "folder", ParentToken: s.options.AccountToken})
	}
	return items
}
func (s *accountSource) accountSynthetic(token string) bool {
	return token == s.options.AccountToken || token == s.options.AccountToken+"-shared" || token == s.options.AccountToken+"-manual"
}
func (s *accountSource) accountProtected(e model.Entry) bool {
	if s.accountSynthetic(e.Token) || e.Token == s.options.PersonalToken {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entry, ok := s.entries[e.Token]; ok && !accountEntrance(entry) && parentPath(e.Remote) != "手动添加" {
		return false
	}
	// Old canonical entrances are never silently rewritten while VFS cache or
	// accepted edits may still reference them. They retain their original identity.
	return s.protected[e.Token] || (e.Token != "" && e.Kind == model.KindFolder && (parentPath(e.Remote) == "个人目录" || parentPath(e.Remote) == "共享目录" || parentPath(e.Remote) == "手动添加"))
}
func (s *accountSource) List(ctx context.Context, token string) ([]larkcli.DriveItem, error) {
	if token == s.options.PersonalToken {
		// This is the only route that reads official root metadata. It is invoked
		// for this cold/explicitly refreshed folder, never for another directory.
		items, err := s.Source.List(ctx, "")
		if err != nil {
			return nil, err
		}
		items = append([]larkcli.DriveItem(nil), items...)
		for i := range items {
			if s.accountSynthetic(items[i].Token) || items[i].Token == s.options.PersonalToken {
				return nil, errors.New("invalid personal root metadata")
			}
			items[i].ParentToken = s.options.PersonalToken
		}
		return items, nil
	}
	s.mu.RLock()
	if token == s.options.AccountToken {
		items := s.categoryItems(s.legacy)
		s.mu.RUnlock()
		return items, nil
	}
	if token == s.options.AccountToken+"-shared" || token == s.options.AccountToken+"-manual" {
		items := append([]larkcli.DriveItem(nil), s.groups[token]...)
		s.mu.RUnlock()
		return items, nil
	}
	s.mu.RUnlock()
	return s.Source.List(ctx, token)
}
func accountRank(e discovery.Entry) int {
	switch e.Placement {
	case "personal":
		return 0
	case "shared":
		return 1
	}
	rank := 4
	for _, membership := range e.Memberships {
		switch membership {
		case discovery.Personal:
			return 0
		case discovery.SharedVisible, discovery.SharedHidden, "shared":
			if rank > 1 {
				rank = 1
			}
		case discovery.Manual:
			if rank > 2 {
				rank = 2
			}
		}
	}
	if e.Manual && rank > 2 {
		rank = 2
	}
	return rank
}
func accountEntrance(e discovery.Entry) bool {
	return e.CanonicalRootToken == "" || e.CanonicalRootToken == e.Token
}

// accountDirectories only reads the local snapshot. Projection never depends on
// official personal-root/network availability and never downloads file bodies.
func (s *accountSource) accountDirectories(ctx context.Context) ([]accountDirectory, error) {
	snapshot, err := s.options.ReadSnapshot(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		entries := make(map[string]discovery.Entry, len(s.entries)+len(snapshot.Entries))
		for token, e := range s.entries {
			entries[token] = e
		}
		seen := map[string]bool{}
		for _, e := range snapshot.Entries {
			if e.Token == "" || e.Name == "" || seen[e.Token] || accountRank(e) == 4 || s.accountSynthetic(e.Token) || e.Token == s.options.PersonalToken {
				err = errors.New("invalid account catalog metadata")
				break
			}
			seen[e.Token] = true
			entries[e.Token] = e
		}
		if err == nil {
			s.entries = entries
		}
	}
	groups := map[string][]larkcli.DriveItem{}
	protected := map[string]bool{}
	for token, e := range s.entries {
		if !accountEntrance(e) {
			continue
		}
		protected[token] = true
		parent := s.options.PersonalToken
		switch accountRank(e) {
		case 1:
			parent = s.options.AccountToken + "-shared"
		case 2:
			parent = s.options.AccountToken + "-manual"
		}
		groups[parent] = append(groups[parent], larkcli.DriveItem{Token: token, Name: e.Name, Type: "folder", ParentToken: parent})
	}
	for token := range groups {
		sort.Slice(groups[token], func(i, j int) bool { return groups[token][i].Token < groups[token][j].Token })
	}
	s.groups, s.protected, s.legacy = groups, protected, len(groups[s.options.AccountToken+"-manual"]) > 0
	dirs := []accountDirectory{
		{remote: "", token: s.options.AccountToken, items: s.categoryItems(s.legacy)},
		{remote: "个人目录", token: s.options.PersonalToken, items: groups[s.options.PersonalToken]},
		{remote: "共享目录", token: s.options.AccountToken + "-shared", items: groups[s.options.AccountToken+"-shared"]},
	}
	if s.legacy {
		dirs = append(dirs, accountDirectory{remote: "手动添加", token: s.options.AccountToken + "-manual", items: groups[s.options.AccountToken+"-manual"]})
	}
	return dirs, err
}
