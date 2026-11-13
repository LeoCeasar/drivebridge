package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

var ErrGitUnavailable = errors.New("history_git_unavailable")
var ErrBodyUnavailable = errors.New("history_body_unavailable")
var ErrLimit = errors.New("history_limit")
var ErrIntegrity = errors.New("history_integrity")

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, ErrLimit
	}
	return b.Buffer.Write(p)
}
func (r *Repository) git(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := r.gitCommand(ctx, input, args...)
	out := &boundedOutput{limit: 32 << 20}
	stderr := &boundedOutput{limit: 16 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("history git %s: %w", args[0], err)
	}
	return out.Bytes(), nil
}

// gitCommand shares the same isolated environment for bounded commands and
// streaming blob reads; hooks, external diff and the user's Git config stay out.
func (r *Repository) gitCommand(ctx context.Context, input io.Reader, args ...string) *exec.Cmd {
	options := []string{"--no-pager", "--git-dir=" + r.gitDir, "-c", "core.fsync=objects,reference", "-c", "core.fsyncMethod=fsync", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "core.useReplaceRefs=false"}
	cmd := exec.CommandContext(ctx, r.executable, append(options, args...)...)
	env := []string{}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if !strings.HasPrefix(k, "GIT_") && k != "PAGER" {
			env = append(env, v)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_AUTHOR_NAME=DriveBridge", "GIT_AUTHOR_EMAIL=local@drivebridge.invalid", "GIT_COMMITTER_NAME=DriveBridge", "GIT_COMMITTER_EMAIL=local@drivebridge.invalid")
	cmd.Stdin = input
	return cmd
}

func (r *Repository) blob(ctx context.Context, in io.Reader) (string, error) {
	b, err := r.git(ctx, in, "hash-object", "-w", "--stdin")
	return strings.TrimSpace(string(b)), err
}

type treeItem struct{ Mode, Type, OID, Name string }

func (r *Repository) tree(ctx context.Context, items []treeItem) (string, error) {
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	var b bytes.Buffer
	for _, v := range items {
		fmt.Fprintf(&b, "%s %s %s\t%s%c", v.Mode, v.Type, v.OID, v.Name, 0)
	}
	out, err := r.git(ctx, &b, "mktree", "-z")
	return strings.TrimSpace(string(out)), err
}
func (r *Repository) replace(ctx context.Context, root string, parts []string, leaf string, asTree ...bool) (string, error) {
	var items []treeItem
	if root != "" {
		raw, err := r.git(ctx, nil, "ls-tree", "-z", root)
		if err != nil {
			return "", err
		}
		for _, row := range bytes.Split(raw, []byte{0}) {
			if len(row) == 0 {
				continue
			}
			head, name, ok := strings.Cut(string(row), "\t")
			fields := strings.Fields(head)
			if !ok || len(fields) != 3 {
				return "", ErrIntegrity
			}
			items = append(items, treeItem{fields[0], fields[1], fields[2], name})
		}
	}
	name := parts[0]
	idx := -1
	for i, v := range items {
		if v.Name == name {
			idx = i
			break
		}
	}
	mode, kind := "100644", "blob"
	value := leaf
	if len(parts) == 1 && len(asTree) > 0 && asTree[0] {
		mode, kind = "040000", "tree"
	}
	if len(parts) > 1 {
		mode, kind = "040000", "tree"
		previous := ""
		if idx >= 0 {
			previous = items[idx].OID
		}
		var err error
		value, err = r.replace(ctx, previous, parts[1:], leaf, asTree...)
		if err != nil {
			return "", err
		}
	}
	item := treeItem{mode, kind, value, name}
	if idx < 0 {
		items = append(items, item)
	} else {
		items[idx] = item
	}
	return r.tree(ctx, items)
}
