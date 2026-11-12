package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rclone/rclone/internal/bridge/platform"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

var ErrIntegrity = errors.New("store integrity check failed")

// Validate checks schema-owned references and hashes without changing job state.
// Lifecycle callers must hold a quiescence lease for a stable complete inventory.
func (s *Store) Validate(ctx context.Context) error {
	err := s.validate(ctx)
	if err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	return nil
}
func (s *Store) validate(ctx context.Context) error {
	return s.validateSupported(ctx, false)
}

func (s *Store) validateSupported(ctx context.Context, allowLegacy bool) error {
	refs := make(map[model.BlobID]bool)
	snapRefs := make(map[model.SnapshotID]bool)
	entries := make(map[model.EntryID]model.Entry)
	snapshots := make(map[model.SnapshotID]model.Snapshot)
	jobs := make(map[model.JobID]jobRecord)
	ns := make(map[string]namespaceRecord)
	metadata := make(map[model.EntryID]Metadata)
	mark := func(b model.BlobID) {
		if b != "" {
			refs[b] = true
		}
	}
	markEntry := func(e model.Entry) {
		mark(e.Local)
		mark(e.Visible)
		mark(e.LocalCheckpoint)
		mark(e.Desired)
		if e.RemoteCheckpoint != "" {
			snapRefs[e.RemoteCheckpoint] = true
		}
	}
	err := s.view(ctx, func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if meta == nil {
			return errors.New("missing bucket meta")
		}
		version := string(meta.Get([]byte("schema_version")))
		legacy := allowLegacy && version == "1"
		if version != "2" && !legacy {
			return errors.New("unknown store schema")
		}
		for _, b := range buckets {
			if legacy && strings.HasPrefix(b, "history_") {
				continue
			}
			if tx.Bucket([]byte(b)) == nil {
				return fmt.Errorf("missing bucket %s", b)
			}
		}
		if !legacy {
			if err := historyProtectedTx(tx, mark); err != nil {
				return err
			}
		}
		var bound Scope
		if er := get(tx, "meta", "scope", &bound); er != nil {
			return er
		}
		if bound != s.scope {
			return model.ErrIdentityChanged
		}
		if er := each[model.Entry](tx, "entries", func(e model.Entry) error {
			entries[e.ID] = e
			markEntry(e)
			var m Metadata
			if er := get(tx, "metadata", string(e.ID), &m); er != nil {
				return er
			}
			metadata[e.ID] = m
			if m.HistorySnapshot != "" && m.HistoryMediaBody == e.Local {
				snapRefs[m.HistorySnapshot] = true
			}
			if !canonicalUUID(string(e.ID)) || e.MountID != bound.MountID || e.ProfileID != bound.ProfileID || !validKind(e.Kind) || m.FirstObserved.IsZero() || m.ModTime.IsZero() {
				return errors.New("invalid entry identity or metadata")
			}
			if er := validRemote(e.Remote, e.Kind == model.KindFolder); er != nil {
				return er
			}
			if !m.Tombstone {
				if model.EntryID(tx.Bucket([]byte("paths")).Get(pathKey(e.Remote))) != e.ID {
					return errors.New("entry path index mismatch")
				}
			}
			return nil
		}); er != nil {
			return er
		}
		if er := tx.Bucket([]byte("paths")).ForEach(func(k, v []byte) error {
			e, ok := entries[model.EntryID(v)]
			if !ok || metadata[e.ID].Tombstone || string(pathKey(e.Remote)) != string(k) {
				return errors.New("dangling path index")
			}
			return nil
		}); er != nil {
			return er
		}
		if er := each[snapshotRecord](tx, "snapshots", func(r snapshotRecord) error {
			sn := r.Snapshot
			if !canonicalUUID(string(sn.ID)) || sn.Token == "" || r.SavedAt.IsZero() {
				return errors.New("invalid immutable snapshot")
			}
			snapshots[sn.ID] = sn
			for _, blob := range sn.Blobs() {
				mark(blob)
			}
			return nil
		}); er != nil {
			return er
		}
		if er := each[jobRecord](tx, "jobs", func(r jobRecord) error {
			j := r.Job
			if _, ok := entries[j.EntryID]; !ok {
				return errors.New("job references absent entry")
			}
			if !canonicalUUID(string(j.ID)) || j.Local == "" || j.Generation > entries[j.EntryID].Generation {
				return errors.New("invalid captured job")
			}
			jobs[j.ID] = r
			mark(j.Local)
			mark(j.BaseLocal)
			mark(j.Plan)
			mark(r.Desired)
			if j.BaseRemote != "" {
				snapRefs[j.BaseRemote] = true
			}
			if len(r.Operations) != len(j.OperationIDs) || (len(r.Operations) > 0 && j.Plan == "") {
				return errors.New("plan operation journal mismatch")
			}
			for i, op := range r.Operations {
				if op.ID == "" || op.ID != j.OperationIDs[i] {
					return errors.New("operation identity mismatch")
				}
			}
			for index, result := range r.Results {
				if index < 0 || index >= len(r.Operations) || !json.Valid(result) {
					return errors.New("invalid operation result journal")
				}
			}
			return nil
		}); er != nil {
			return er
		}
		if er := each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if _, ok := entries[c.EntryID]; !ok {
				return errors.New("conflict references absent entry")
			}
			mark(c.Base)
			mark(c.Local)
			mark(c.Remote)
			mark(c.Draft)
			return nil
		}); er != nil {
			return er
		}
		if er := each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
			ns[r.Op.ID] = r
			for _, e := range r.Before {
				if _, ok := entries[e.ID]; !ok {
					return errors.New("namespace references absent entry")
				}
				markEntry(e)
			}
			for id := range r.Sealed {
				if _, ok := entries[id]; !ok {
					return errors.New("sealed namespace references absent entry")
				}
			}
			for _, id := range []model.EntryID{r.Op.SrcID, r.Op.DstID, r.Op.ResultID} {
				if id != "" {
					if _, ok := entries[id]; !ok {
						return errors.New("namespace result references absent entry")
					}
				}
			}
			mark(r.Op.Input)
			mark(r.Op.Previous)
			return nil
		}); er != nil {
			return er
		}
		for _, e := range entries {
			if e.FrozenBy != "" {
				r, ok := ns[e.FrozenBy]
				if !ok || (r.Op.State != "prepared" && r.Op.State != "sealed" && r.Op.State != "committed") {
					return errors.New("entry frozen by absent or terminal namespace")
				}
			}
		}
		if er := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
			if _, ok := entries[op.EntryID]; !ok {
				return errors.New("create references absent entry")
			}
			if op.Input == "" || op.TemporaryName == "" {
				return errors.New("create missing durable input or unique name")
			}
			if _, ok := jobs[op.JobID]; !ok && op.State != "finished" {
				return errors.New("pending create references absent job")
			}
			mark(op.Input)
			return nil
		}); er != nil {
			return er
		}
		if er := each[recoveryRecord](tx, "recovery", func(r recoveryRecord) error {
			if r.SnapshotID != "" {
				snapRefs[r.SnapshotID] = true
			}
			for _, e := range r.Entries {
				if _, ok := entries[e.ID]; !ok {
					return errors.New("recovery references absent entry")
				}
				markEntry(e)
			}
			if r.NamespaceID != "" {
				if _, ok := ns[r.NamespaceID]; !ok {
					return errors.New("recovery references absent namespace")
				}
			}
			return nil
		}); er != nil {
			return er
		}
		if er := each[publication](tx, "publications", func(p publication) error {
			if _, ok := entries[p.Candidate.EntryID]; !ok {
				return errors.New("publication references absent entry")
			}
			snapRefs[p.Candidate.NewRemote] = true
			if p.Candidate.ExpectedRemote != "" {
				snapRefs[p.Candidate.ExpectedRemote] = true
			}
			mark(p.Candidate.Visible)
			mark(p.AcceptedLocal)
			return nil
		}); er != nil {
			return er
		}
		for id := range snapRefs {
			if _, ok := snapshots[id]; !ok {
				return fmt.Errorf("missing snapshot %s", id)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for b := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := s.ReadBlob(ctx, b)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, contextReader{ctx: ctx, r: r})
		closeErr := r.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if hex.EncodeToString(h.Sum(nil)) != string(b) {
			return fmt.Errorf("blob hash mismatch: %s", b)
		}
	}
	for _, sn := range snapshots {
		if _, err := s.prepareSnapshot(sn); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// ValidateDirectory audits a closed backup without recovery writes or rebinding.
// It deliberately opens bbolt read-only; a running writer must first quiesce.
func ValidateDirectory(ctx context.Context, dir string, expected Scope) error {
	if err := platform.CheckNoSymlinks(dir); err != nil {
		return err
	}
	dbPath := filepath.Join(dir, "store.db")
	if err := platform.CheckNoSymlinks(dbPath); err != nil {
		return err
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	s := &Store{db: db, dir: dir, bound: true, scope: expected, leases: make(map[model.BlobID]int)}
	// An installer must be able to audit a supported old store before its first
	// writer performs migration. Never migrate data during this read-only gate.
	if err := s.validateSupported(ctx, true); err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	return nil
}
