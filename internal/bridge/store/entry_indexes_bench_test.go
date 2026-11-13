package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// Vary the unrelated catalog size while keeping the result set constant.
func BenchmarkCatalogLookups(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			dir, err := filepath.EvalSymlinks(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			s, err := Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			scope := testScope()
			if err = s.BindScope(ctx, scope); err != nil {
				b.Fatal(err)
			}
			if err = s.update(ctx, func(tx *bolt.Tx) error {
				now := time.Now().UTC()
				for i := range count {
					parent := "unrelated"
					if i < 3 {
						parent = "target"
					}
					e := model.Entry{ID: model.EntryID(uuid.NewString()), MountID: scope.MountID, ProfileID: scope.ProfileID, Remote: fmt.Sprintf("%s/%d", parent, i), Kind: model.KindFile, Token: fmt.Sprintf("token-%d", i)}
					if err := put(tx, "metadata", string(e.ID), Metadata{FirstObserved: now, ModTime: now}); err != nil {
						return err
					}
					if err := saveEntry(tx, e); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.Run("Token", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := s.EntryByToken(ctx, "token-0"); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Parent", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					children, err := s.Entries(ctx, "target")
					if err != nil || len(children) != 3 {
						b.Fatalf("children=%d: %v", len(children), err)
					}
				}
			})
		})
	}
}
