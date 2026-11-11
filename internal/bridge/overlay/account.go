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

// AccountOptions projects a discovery catalog and the official personal root
// into a single account namespace. AccountToken must match the store scope.
type AccountOptions struct {
	AccountToken  string
	PersonalToken string
	ReadSnapshot  func(context.Context) (discovery.Snapshot, error)
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
	personal  []larkcli.DriveItem
	groups    map[string][]larkcli.DriveItem
}

func newAccountSource(base Source, options AccountOptions) (*accountSource, error) {
	if options.AccountToken == "" || options.PersonalToken == "" || options.AccountToken == options.PersonalToken || options.ReadSnapshot == nil || options.PersonalToken == options.AccountToken+"-shared" || options.PersonalToken == options.AccountToken+"-manual" {
		return nil, errors.New("invalid account source options")
	}
	return &accountSource{Source: base, options: options, entries: map[string]discovery.Entry{}, protected: map[string]bool{}, groups: map[string][]larkcli.DriveItem{}}, nil
}
func (s *accountSource) top() []larkcli.DriveItem {
	return []larkcli.DriveItem{
		{Token: s.options.PersonalToken, Name: "个人目录", Type: "folder", ParentToken: s.options.AccountToken},
		{Token: s.options.AccountToken + "-shared", Name: "共享目录", Type: "folder", ParentToken: s.options.AccountToken},
		{Token: s.options.AccountToken + "-manual", Name: "手动添加", Type: "folder", ParentToken: s.options.AccountToken},
	}
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
	// Persisted category entrances remain protected even before the first refresh.
	return s.protected[e.Token] || (e.Token != "" && e.Kind == model.KindFolder && (parentPath(e.Remote) == "个人目录" || parentPath(e.Remote) == "共享目录" || parentPath(e.Remote) == "手动添加"))
}
func (s *accountSource) List(ctx context.Context, token string) ([]larkcli.DriveItem, error) {
	if token == s.options.AccountToken {
		return s.top(), nil
	}
	s.mu.RLock()
	if token == s.options.PersonalToken {
		items := append([]larkcli.DriveItem(nil), s.personal...)
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
	rank := 4
	for _, m := range e.Memberships {
		switch m {
		case discovery.Personal:
			return 0
		case discovery.SharedVisible, discovery.SharedHidden:
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

// Refresh performs metadata I/O before any store observation. All catalog roots
// are bound before ordinary traversal, so nested aliases cannot claim a token.
// Missing rows and failed refreshes never remove an existing durable mapping.
func (s *accountSource) accountDirectories(ctx context.Context) ([]accountDirectory, error) {
	snapshot, err := s.options.ReadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	personal, err := s.Source.List(ctx, "")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make(map[string]discovery.Entry, len(s.entries)+len(snapshot.Entries))
	for token, e := range s.entries {
		entries[token] = e
	}
	seen := map[string]bool{}
	for _, e := range snapshot.Entries {
		if e.Token == "" || e.Name == "" || seen[e.Token] || accountRank(e) == 4 || s.accountSynthetic(e.Token) || e.Token == s.options.PersonalToken {
			return nil, errors.New("invalid account catalog metadata")
		}
		seen[e.Token] = true
		entries[e.Token] = e
	}
	personal = append([]larkcli.DriveItem(nil), personal...)
	personalTokens := map[string]bool{}
	for i := range personal {
		if personal[i].Token == "" || personal[i].Name == "" || personalTokens[personal[i].Token] || s.accountSynthetic(personal[i].Token) || personal[i].Token == s.options.PersonalToken {
			return nil, errors.New("invalid personal root metadata")
		}
		personalTokens[personal[i].Token] = true
		personal[i].ParentToken = s.options.PersonalToken
	}
	groups := map[string][]larkcli.DriveItem{}
	for token, e := range entries {
		s.protected[token] = true
		if personalTokens[token] {
			continue
		}
		parent := s.options.PersonalToken
		switch accountRank(e) {
		case 1:
			parent = s.options.AccountToken + "-shared"
		case 2:
			parent = s.options.AccountToken + "-manual"
		}
		item := larkcli.DriveItem{Token: token, Name: e.Name, Type: "folder", ParentToken: parent}
		if parent == s.options.PersonalToken {
			personal = append(personal, item)
		} else {
			groups[parent] = append(groups[parent], item)
		}
	}
	for token := range groups {
		sort.Slice(groups[token], func(i, j int) bool { return groups[token][i].Token < groups[token][j].Token })
	}
	s.entries, s.personal, s.groups = entries, personal, groups
	return []accountDirectory{
		{remote: "", token: s.options.AccountToken, items: s.top()},
		{remote: "个人目录", token: s.options.PersonalToken, items: personal},
		{remote: "共享目录", token: s.options.AccountToken + "-shared", items: groups[s.options.AccountToken+"-shared"]},
		{remote: "手动添加", token: s.options.AccountToken + "-manual", items: groups[s.options.AccountToken+"-manual"]},
	}, nil
}
