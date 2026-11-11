package manager

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/lib/larkcli"
	"time"
)

// ErrDiscoveryUnclassified means no complete metadata proof was obtained. The
// caller must keep the prior catalog and must not invent a new directory root.
var ErrDiscoveryUnclassified = errors.New("directory placement is not yet verified")

const (
	classificationMaxFolders = 256
	classificationMaxItems   = 50000
	classificationMaxDepth   = 64
)

type classificationLister interface {
	List(context.Context, string) ([]larkcli.DriveItem, error)
}
type classificationMetadata interface {
	FolderMeta(context.Context, string) (DiscoveryCandidate, error)
}
type officialClassificationSource struct {
	runner *larkcli.Runner
	drive  *larkcli.Drive
}

func (s officialClassificationSource) List(ctx context.Context, token string) ([]larkcli.DriveItem, error) {
	return s.drive.List(ctx, token)
}
func (s officialClassificationSource) FolderMeta(ctx context.Context, token string) (DiscoveryCandidate, error) {
	return discoveryFolder(ctx, s.runner, token, "")
}

type manualPlacement struct {
	Placement, CanonicalRootToken, RootName string
	RelativePath                            []string
}
type classificationNode struct {
	token, name, root, rootName string
	path                        []string
	ancestors                   map[string]bool
	depth                       int
}
type classificationWalk struct {
	source          classificationLister
	requests, items int
	anchors         map[string]bool
}

func entryPlacement(e discovery.Entry) string {
	if e.Placement == "personal" || e.Placement == "shared" {
		return e.Placement
	}
	for _, member := range e.Memberships {
		if member == discovery.Personal {
			return "personal"
		}
	}
	for _, member := range e.Memberships {
		if member == discovery.SharedVisible || member == discovery.SharedHidden {
			return "shared"
		}
	}
	return ""
}
func projectionRoot(e discovery.Entry) bool {
	return e.CanonicalRootToken == "" || e.CanonicalRootToken == e.Token
}
func rootPlacement(token, name, placement string) manualPlacement {
	return manualPlacement{Placement: placement, CanonicalRootToken: token, RootName: name}
}
func classifyManualFolder(ctx context.Context, source classificationLister, snapshot discovery.Snapshot, target DiscoveryCandidate) (manualPlacement, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	anchors := map[string]bool{}
	for _, entry := range snapshot.Entries {
		if projectionRoot(entry) {
			anchors[entry.Token] = true
			if entry.Token == target.Token {
				if placement := entryPlacement(entry); placement != "" {
					return rootPlacement(target.Token, target.Name, placement), nil
				}
			}
		}
	}
	walk := &classificationWalk{source: source, anchors: anchors}
	// Numeric parent IDs cannot be resolved as folder tokens. Match only against
	// known entrance metadata, then prove the immediate edge with its current list.
	if target.parentID != "" && target.parentID != "0" {
		if metadata, ok := source.(classificationMetadata); ok {
			for _, entry := range snapshot.Entries {
				placement := entryPlacement(entry)
				if !projectionRoot(entry) || placement == "" {
					continue
				}
				if walk.requests >= classificationMaxFolders {
					return manualPlacement{}, ErrDiscoveryUnclassified
				}
				walk.requests++
				parent, err := metadata.FolderMeta(ctx, entry.Token)
				if err != nil {
					continue
				}
				if parent.nodeID == "" || parent.nodeID != target.parentID {
					continue
				}
				items, err := walk.list(ctx, parent.Token)
				if err != nil {
					return manualPlacement{}, ErrDiscoveryUnclassified
				}
				for _, item := range items {
					if item.Type == "folder" && item.Token == target.Token {
						if item.Name != target.Name {
							return manualPlacement{}, ErrDiscoveryUnclassified
						}
						return manualPlacement{Placement: placement, CanonicalRootToken: parent.Token, RootName: parent.Name, RelativePath: []string{item.Name}}, nil
					}
				}
				return manualPlacement{}, ErrDiscoveryUnclassified
			}
		}
	}
	personal, err := walk.list(ctx, "")
	if err != nil {
		return manualPlacement{}, ErrDiscoveryUnclassified
	}
	roots := []classificationNode{}
	for _, item := range personal {
		if item.Type != "folder" {
			continue
		}
		if !classificationItem(item, "") {
			return manualPlacement{}, ErrDiscoveryUnclassified
		}
		roots = append(roots, classificationNode{token: item.Token, name: item.Name, root: item.Token, rootName: item.Name, ancestors: map[string]bool{}})
	}
	found, ok, personalTokens, err := walk.tree(ctx, roots, "personal", target)
	if ok {
		return found, nil
	}
	if err != nil {
		return manualPlacement{}, ErrDiscoveryUnclassified
	}
	// Only complete personal exclusion permits unknown accessible folders to be
	// called shared. Search existing entrances first to retain canonical identity.
	roots = nil
	for _, entry := range snapshot.Entries {
		if !projectionRoot(entry) || personalTokens[entry.Token] {
			continue
		}
		roots = append(roots, classificationNode{token: entry.Token, name: entry.Name, root: entry.Token, rootName: entry.Name, ancestors: map[string]bool{}})
	}
	found, ok, _, err = walk.tree(ctx, roots, "shared", target)
	if ok {
		return found, nil
	}
	if err != nil {
		return manualPlacement{}, ErrDiscoveryUnclassified
	}
	return rootPlacement(target.Token, target.Name, "shared"), nil
}
func classificationItem(item larkcli.DriveItem, parent string) bool {
	return discoveryTokenRE.MatchString(item.Token) && discoveryName(item.Name) && (parent == "" || item.ParentToken == "" || item.ParentToken == parent)
}
func (w *classificationWalk) list(ctx context.Context, token string) ([]larkcli.DriveItem, error) {
	if ctx.Err() != nil || w.requests >= classificationMaxFolders {
		return nil, ErrDiscoveryUnclassified
	}
	w.requests++
	items, err := w.source.List(ctx, token)
	if err != nil {
		return nil, ErrDiscoveryUnclassified
	}
	w.items += len(items)
	if w.items > classificationMaxItems {
		return nil, ErrDiscoveryUnclassified
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !classificationItem(item, token) || seen[item.Token] {
			return nil, ErrDiscoveryUnclassified
		}
		seen[item.Token] = true
	}
	return items, nil
}
func (w *classificationWalk) tree(ctx context.Context, queue []classificationNode, placement string, target DiscoveryCandidate) (manualPlacement, bool, map[string]bool, error) {
	visited := map[string]bool{}
	parents := map[string]string{}
	seeded := map[string]bool{}
	incomplete := false
	for _, node := range queue {
		if seeded[node.token] {
			return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
		}
		seeded[node.token] = true
		if node.token == target.Token {
			return rootPlacement(target.Token, target.Name, placement), true, visited, nil
		}
	}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if visited[node.token] {
			return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
		}
		visited[node.token] = true
		if node.depth > classificationMaxDepth {
			return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
		}
		items, err := w.list(ctx, node.token)
		if err != nil {
			incomplete = true
			if ctx.Err() != nil || w.requests >= classificationMaxFolders {
				return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
			}
			continue
		}
		for _, item := range items {
			if item.Type != "folder" {
				continue
			}
			if item.Token == node.token || node.ancestors[item.Token] {
				return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
			}
			if parent, ok := parents[item.Token]; ok && parent != node.token {
				return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
			}
			for ancestor := node.token; ancestor != ""; ancestor = parents[ancestor] {
				if ancestor == item.Token {
					return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
				}
			}
			parents[item.Token] = node.token
			root, rootName, path := node.root, node.rootName, append(append([]string(nil), node.path...), item.Name)
			if w.anchors[item.Token] {
				root, rootName, path = item.Token, item.Name, nil
			}
			if item.Token == target.Token {
				if item.Name != target.Name {
					return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
				}
				return manualPlacement{Placement: placement, CanonicalRootToken: root, RootName: rootName, RelativePath: path}, true, visited, nil
			}
			// A separately projected entrance is traversed from its own queue slot.
			if seeded[item.Token] {
				continue
			}
			ancestors := make(map[string]bool, len(node.ancestors)+1)
			for token := range node.ancestors {
				ancestors[token] = true
			}
			ancestors[node.token] = true
			queue = append(queue, classificationNode{token: item.Token, name: item.Name, root: root, rootName: rootName, path: path, ancestors: ancestors, depth: node.depth + 1})
		}
	}
	if incomplete {
		return manualPlacement{}, false, visited, ErrDiscoveryUnclassified
	}
	return manualPlacement{}, false, visited, nil
}
