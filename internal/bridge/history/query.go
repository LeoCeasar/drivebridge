package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	bolt "go.etcd.io/bbolt"
)

type Query struct {
	Search  string        `json:"search,omitempty"`
	Before  uint64        `json:"before,omitempty"`
	Limit   int           `json:"limit,omitempty"`
	EntryID model.EntryID `json:"entry_id,omitempty"`
	MountID model.MountID `json:"mount_id,omitempty"`
	Kind    string        `json:"kind,omitempty"`
}
type Page struct {
	Upper    uint64    `json:"upper"`
	Versions []Version `json:"versions"`
	Cursor   uint64    `json:"cursor,omitempty"`
}
type Difference struct {
	Patch       string `json:"patch"`
	Limited     bool   `json:"limited"`
	Binary      bool   `json:"binary"`
	Left, Right Ref
}

func (r *Repository) List(ctx context.Context, q Query) (page Page, err error) {
	page.Versions = []Version{}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 100
	}
	err = r.locked(ctx, func() error {
		if err := r.syncIndex(ctx); err != nil {
			return err
		}
		pins, e := r.readPins()
		if e != nil {
			return e
		}
		return r.index(func(tx *bolt.Tx) error {
			c := tx.Bucket([]byte("order")).Cursor()
			if k, _ := c.Last(); k != nil {
				n, e := strconv.ParseUint(string(k), 10, 64)
				if e != nil {
					return e
				}
				page.Upper = n
			}
			k, id := c.Last()
			if q.Before > 0 {
				k, id = c.Seek([]byte(fmt.Sprintf("%020d", q.Before)))
				if k == nil {
					k, id = c.Last()
				} else {
					k, id = c.Prev()
				}
			}
			for ; k != nil; k, id = c.Prev() {
				if err := ctx.Err(); err != nil {
					return err
				}
				var v Version
				if err := json.Unmarshal(tx.Bucket([]byte("versions")).Get(id), &v); err != nil {
					return err
				}
				if q.Search != "" && !strings.Contains(strings.ToLower(v.Event.Entry.Remote), strings.ToLower(q.Search)) {
					continue
				}
				if q.EntryID != "" && v.Event.Entry.ID != q.EntryID || q.MountID != "" && v.Event.MountID != q.MountID || q.Kind != "" && v.Event.Kind != q.Kind {
					continue
				}
				v.Pinned = pins[v.ID]
				page.Versions = append(page.Versions, v)
				if len(page.Versions) == q.Limit {
					page.Cursor = v.Order
					break
				}
			}
			return nil
		})
	})
	return
}
func (r *Repository) version(ctx context.Context, id string) (v Version, err error) {
	if !validVersion(id) {
		return v, ErrBodyUnavailable
	}
	err = r.index(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("versions")).Get([]byte(id))
		if b == nil {
			return ErrBodyUnavailable
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		if v.Event.ProfileID != r.profile || v.Event.IdentityHash != r.identity {
			return model.ErrIdentityChanged
		}
		return nil
	})
	return
}
func (r *Repository) Version(ctx context.Context, id string) (v Version, err error) {
	err = r.locked(ctx, func() error {
		if e := r.syncIndex(ctx); e != nil {
			return e
		}
		v, err = r.version(ctx, id)
		if err == nil {
			pins, e := r.readPins()
			if e != nil {
				return e
			}
			v.Pinned = pins[id]
		}
		return err
	})
	return
}
func chooseRef(v Version, role string) (Ref, error) {
	if role == "" && v.Event.Kind == "draft_saved" {
		if _, ok := v.Bodies["draft"]; ok {
			role = "draft"
		}
	}
	if role == "" && v.Event.Kind == "remote_observed" {
		if _, ok := v.Bodies["remote"]; ok {
			role = "remote"
		}
	}
	if role == "" {
		role = "local"
		if _, ok := v.Bodies[role]; !ok {
			role = "remote"
		}
	}
	if ref, ok := v.Bodies[role]; ok {
		return ref, nil
	}
	if strings.HasPrefix(role, "media:") {
		if ref, ok := v.Media[strings.TrimPrefix(role, "media:")]; ok {
			return ref, nil
		}
	}
	return Ref{}, ErrBodyUnavailable
}
func (r *Repository) OpenBody(ctx context.Context, id, role string) (body io.ReadCloser, err error) {
	release, err := r.LeaseVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	err = r.locked(ctx, func() error {
		if err := r.syncIndex(ctx); err != nil {
			return err
		}
		v, err := r.version(ctx, id)
		if err != nil {
			return err
		}
		ref, err := chooseRef(v, role)
		if err != nil {
			return err
		}
		if !validHash(ref.SHA256) || ref.Size < 0 {
			return ErrIntegrity
		}
		if ref.OID != "" {
			if len(ref.OID) != 40 && len(ref.OID) != 64 {
				return ErrIntegrity
			}
			body, err = r.openGitBody(ctx, ref)
			return err
		}
		if !validHash(ref.Asset) || ref.Asset != ref.SHA256 {
			return ErrIntegrity
		}
		name := filepath.Join(r.dir, "objects", ref.Asset)
		if err := platform.CheckNoSymlinks(name); err != nil {
			return err
		}
		f, err := os.Open(name)
		if err != nil {
			return errors.Join(ErrBodyUnavailable, err)
		}
		stat, err := f.Stat()
		if err != nil || !stat.Mode().IsRegular() || stat.Size() != ref.Size {
			f.Close()
			return ErrIntegrity
		}
		body = &verifiedBody{ReadCloser: f, hash: sha256.New(), want: ref.SHA256, size: ref.Size, ctx: ctx}
		return nil
	})
	if err != nil {
		release()
	} else {
		body = &leasedBody{ReadCloser: body, release: release}
	}
	return
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}
type verifiedBody struct {
	ctx context.Context
	io.ReadCloser
	hash       hashWriter
	want       string
	checked    bool
	size, read int64
}

func (v *verifiedBody) Read(p []byte) (int, error) {
	if err := v.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := v.ReadCloser.Read(p)
	if n > 0 {
		v.hash.Write(p[:n])
		v.read += int64(n)
		if v.read > v.size {
			return n, ErrIntegrity
		}
	}
	if err == io.EOF && !v.checked {
		v.checked = true
		if v.read != v.size || hex.EncodeToString(v.hash.Sum(nil)) != v.want {
			return n, ErrIntegrity
		}
	}
	return n, err
}
func (r *Repository) Read(ctx context.Context, id, role string) ([]byte, error) {
	f, err := r.OpenBody(ctx, id, role)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (10<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 10<<20 {
		return nil, ErrLimit
	}
	return b, nil
}
func (r *Repository) Diff(ctx context.Context, left, right string) (d Difference, err error) {
	for _, id := range []string{left, right} {
		release, e := r.LeaseVersion(ctx, id)
		if e != nil {
			return d, e
		}
		defer release()
	}
	a, err := r.Version(ctx, left)
	if err != nil {
		return d, err
	}
	b, err := r.Version(ctx, right)
	if err != nil {
		return d, err
	}
	d.Left, err = chooseRef(a, "")
	if err != nil {
		return d, err
	}
	d.Right, err = chooseRef(b, "")
	if err != nil {
		return d, err
	}
	if d.Left.Size > 10<<20 || d.Right.Size > 10<<20 {
		d.Limited = true
		return d, nil
	}
	ab, err := r.Read(ctx, left, "")
	if err != nil {
		return d, err
	}
	bb, err := r.Read(ctx, right, "")
	if err != nil {
		return d, err
	}
	if !utf8.Valid(ab) || !utf8.Valid(bb) || bytes.IndexByte(ab, 0) >= 0 || bytes.IndexByte(bb, 0) >= 0 || d.Left.OID == "" || d.Right.OID == "" {
		d.Binary = true
		return d, nil
	}
	limited, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = r.locked(limited, func() error {
		raw, e := r.git(limited, nil, "diff", "--no-ext-diff", "--no-textconv", d.Left.OID, d.Right.OID, "--")
		if e != nil {
			return e
		}
		lines := strings.Split(string(raw), "\n")
		if len(lines) > 10000 {
			lines = lines[:10000]
			d.Limited = true
		}
		d.Patch = strings.Join(lines, "\n")
		return nil
	})
	if errors.Is(err, ErrLimit) || errors.Is(err, context.DeadlineExceeded) {
		d.Limited = true
		return d, nil
	}
	return
}

type leasedBody struct {
	io.ReadCloser
	release func()
}

func (b *leasedBody) Close() error { err := b.ReadCloser.Close(); b.release(); return err }
