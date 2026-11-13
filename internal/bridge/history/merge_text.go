package history

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/platform"
)

// MergeText uses the packaged, verified Git runtime. Only a clean merge exposes
// output; conflict markers and command failures never become upload content.
// Private scratch inputs are temporary and do not create comparison files in
// the mounted directory or alter the account history repository.
func MergeText(ctx context.Context, base, local, remote []byte) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	for _, b := range [][]byte{base, local, remote} {
		if len(b) > 10<<20 {
			return nil, false, ErrLimit
		}
		if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			return nil, false, ErrIntegrity
		}
	}
	if bytes.Equal(local, remote) || bytes.Equal(base, remote) {
		return bytes.Clone(local), true, nil
	}
	if bytes.Equal(base, local) {
		return bytes.Clone(remote), true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	executable, err := resolveGit(ctx, "")
	if err != nil {
		return nil, false, err
	}
	dir, err := os.MkdirTemp("", "drivebridge-merge-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, false, err
	}
	if err = platform.EnsurePrivateDir(dir); err != nil {
		return nil, false, err
	}
	names := []string{"local", "base", "remote"}
	for i, b := range [][]byte{local, base, remote} {
		if err = os.WriteFile(filepath.Join(dir, names[i]), b, 0600); err != nil {
			return nil, false, err
		}
	}
	repo := Repository{executable: executable, gitDir: filepath.Join(dir, "unused.git")}
	cmd := repo.gitCommand(ctx, nil, "merge-file", "--stdout", "--", filepath.Join(dir, "local"), filepath.Join(dir, "base"), filepath.Join(dir, "remote"))
	cmd.Dir = dir
	out := &boundedOutput{limit: 10 << 20}
	stderr := &boundedOutput{limit: 16 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		var exit *exec.ExitError
		// merge-file returns the conflict count capped at 127. Fatal errors are
		// negative statuses (255 on Unix) or an unsuccessful process invocation.
		if errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() <= 127 {
			return nil, false, nil
		}
		return nil, false, errors.Join(ErrGitUnavailable, err)
	}
	return bytes.Clone(out.Bytes()), true, nil
}
