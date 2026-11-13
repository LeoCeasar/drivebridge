package markdown

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/internal/bridge/platform"
)

// ErrConversionRulesChanged means another writer changed the file since reading.
var ErrConversionRulesChanged = errors.New("conversion rules changed; reload before saving")

// ConversionRulesRevision hashes the exact on-disk bytes, including formatting.
func ConversionRulesRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReadConversionRulesFile reads bounded bytes from a plain, non-symlink path.
// A missing file returns os.ErrNotExist; initialization belongs to the caller.
func ReadConversionRulesFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute conversion rules path required")
	}
	exists, err := existingConversionRulesFile(path)
	if err != nil {
		return nil, fmt.Errorf("conversion rules %s: %w", path, err)
	}
	if !exists {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConversionRulesBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConversionRulesBytes {
		return nil, fmt.Errorf("conversion rules %s: exceeds 8 MiB", path)
	}
	return data, nil
}

// SaveConversionRulesFile validates before acquiring a private file lock, checks
// the revision under the lock, then syncs and atomically publishes private bytes.
// It never modifies the active conversion snapshot or creates missing rules.
func SaveConversionRulesFile(ctx context.Context, path, expectedRevision string, document []byte) (*ConversionRules, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rules, err := PureParseConversionRules(document)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute conversion rules path required")
	}
	if _, err := ReadConversionRulesFile(path); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	if err := platform.CheckNoSymlinks(lockPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := platform.SecurePrivate(lockPath, false); err != nil {
		return nil, err
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, ctx.Err()
	}
	defer func() { _ = lock.Unlock() }()
	current, err := ReadConversionRulesFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrConversionRulesChanged
	}
	if err != nil {
		return nil, err
	}
	if ConversionRulesRevision(current) != expectedRevision {
		return nil, ErrConversionRulesChanged
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(dir, ".conversion-rules-*")
	if err != nil {
		return nil, err
	}
	name := temporary.Name()
	defer func() { _ = temporary.Close(); _ = os.Remove(name) }()
	if err := platform.SecurePrivate(name, false); err != nil {
		return nil, err
	}
	if _, err := temporary.Write(document); err != nil {
		return nil, err
	}
	if err := platform.SyncFile(temporary); err != nil {
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := existingConversionRulesFile(path); err != nil {
		return nil, err
	}
	if err := platform.ReplaceFile(name, path); err != nil {
		return nil, err
	}
	if err := platform.SyncDir(dir); err != nil {
		return nil, err
	}
	return rules, nil
}
