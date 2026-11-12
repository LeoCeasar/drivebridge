package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func (r *Repository) commitVersion(ctx context.Context, head string, l ledger, v Version) (string, error) {
	v.Commit = ""
	v.Pinned = false
	var items []treeItem
	for role, ref := range v.Bodies {
		if ref.OID != "" {
			h := sha256.Sum256([]byte(role))
			items = append(items, treeItem{"100644", "blob", ref.OID, "body-" + hex.EncodeToString(h[:])})
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	oid, err := r.blob(ctx, strings.NewReader(string(raw)))
	if err != nil {
		return "", err
	}
	items = append(items, treeItem{"100644", "blob", oid, "version.json"})
	leaf, err := r.tree(ctx, items)
	if err != nil {
		return "", err
	}
	root := ""
	if head != "" {
		b, err := r.git(ctx, nil, "rev-parse", head+"^{tree}")
		if err != nil {
			return "", err
		}
		root = strings.TrimSpace(string(b))
	}
	root, err = r.replace(ctx, root, []string{"entries", string(v.Event.Entry.ID), string(v.Event.MountID)}, leaf, true)
	if err != nil {
		return "", err
	}
	ledgerBytes, _ := json.Marshal(l)
	ledgerOID, err := r.blob(ctx, strings.NewReader(string(ledgerBytes)))
	if err != nil {
		return "", err
	}
	root, err = r.replace(ctx, root, []string{"source-watermarks.json"}, ledgerOID)
	if err != nil {
		return "", err
	}
	root, err = r.replace(ctx, root, []string{"last-event.json"}, oid)
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", root, "-m", "DriveBridge " + v.Event.Kind + "\n\nEvent-ID: " + v.ID}
	if head != "" {
		args = append(args, "-p", head)
	}
	b, err := r.git(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(b))
	if _, err = r.git(ctx, nil, "update-ref", "refs/heads/history", commit, head); err != nil {
		return "", err
	}

	return commit, nil
}
