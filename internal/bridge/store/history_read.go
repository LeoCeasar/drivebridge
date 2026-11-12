package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func historyEventTx(tx *bolt.Tx, id string) (e model.HistoryEvent, err error) {
	source, number, ok := strings.Cut(id, ":")
	seq, parseErr := strconv.ParseUint(number, 10, 64)
	if !ok || parseErr != nil || seq == 0 || strconv.FormatUint(seq, 10) != number || source != string(tx.Bucket([]byte("meta")).Get([]byte("history_source"))) {
		return e, ErrNotFound
	}
	raw := tx.Bucket([]byte("history_events")).Get(historyKey(seq))
	if raw == nil {
		return e, ErrNotFound
	}
	if err = json.Unmarshal(raw, &e); err != nil {
		return
	}
	var sc Scope
	if err = get(tx, "meta", "scope", &sc); err != nil {
		return
	}
	if e.ID != id || e.Source != source || e.ProfileID != sc.ProfileID || e.MountID != sc.MountID || e.IdentityHash != sc.IdentityHash {
		return e, model.ErrIdentityChanged
	}
	e.Archived = tx.Bucket([]byte("history_receipts")).Get(historyKey(seq)) != nil
	return
}
func (s *Store) HistoryEvent(ctx context.Context, id string) (out model.HistoryEvent, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error { var err error; out, err = historyEventTx(tx, id); return err })
	return
}
func (s *Store) HistoryRecent(ctx context.Context, entry model.EntryID, search string, limit int) (out []model.HistoryEvent, err error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	search = strings.ToLower(search)
	err = s.view(ctx, func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("history_events")).Cursor()
		for k, raw := c.Last(); k != nil && len(out) < limit; k, raw = c.Prev() {
			if e := ctx.Err(); e != nil {
				return e
			}
			if tx.Bucket([]byte("history_receipts")).Get(k) != nil {
				break
			}
			var ev model.HistoryEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				return err
			}
			if entry != "" && ev.Entry.ID != entry || search != "" && !strings.Contains(strings.ToLower(ev.Entry.Remote), search) {
				continue
			}
			if ev.Bodies["local"] == "" && ev.Bodies["draft"] == "" {
				continue
			}
			out = append(out, ev)
		}
		return nil
	})
	return
}
func (s *Store) LeaseHistoryEvent(ctx context.Context, id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed || !s.bound {
		return nil, ErrUnbound
	}
	var ev model.HistoryEvent
	err := s.db.View(func(tx *bolt.Tx) error { var err error; ev, err = historyEventTx(tx, id); return err })
	if err != nil {
		return nil, err
	}
	for _, b := range ev.Blobs() {
		if err = s.checkBlob(b); err != nil {
			return nil, err
		}
	}
	for _, b := range ev.Blobs() {
		s.leases[b]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, b := range ev.Blobs() {
				s.leases[b]--
				if s.leases[b] == 0 {
					delete(s.leases, b)
				}
			}
		})
	}, nil
}

type verifiedHistoryBody struct {
	io.ReadCloser
	h    hash.Hash
	want string
	ctx  context.Context
}

func (v *verifiedHistoryBody) Read(p []byte) (int, error) {
	if err := v.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := v.ReadCloser.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
	}
	if err == io.EOF && hex.EncodeToString(v.h.Sum(nil)) != v.want {
		return n, ErrIntegrity
	}
	return n, err
}
func (s *Store) ReadHistoryBody(ctx context.Context, id, role string) (io.ReadCloser, error) {
	ev, err := s.HistoryEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	if role == "" {
		role = "local"
		if ev.Kind == "draft_saved" && ev.Bodies["draft"] != "" {
			role = "draft"
		} else if ev.Kind == "remote_observed" && ev.Bodies["remote"] != "" {
			role = "remote"
		} else if ev.Bodies[role] == "" {
			role = "remote"
		}
	}
	b := ev.Bodies[role]
	if strings.HasPrefix(role, "media:") {
		b = ev.Media[strings.TrimPrefix(role, "media:")]
	}
	if b == "" {
		return nil, ErrNotFound
	}
	f, err := s.ReadBlob(ctx, b)
	if err != nil {
		return nil, err
	}
	return &verifiedHistoryBody{ReadCloser: f, h: sha256.New(), want: string(b), ctx: ctx}, nil
}
