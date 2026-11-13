package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
)

// Git content is streamed separately from bounded metadata command output.
// The version lease held by OpenBody also keeps the selected Git epoch alive.
func (r *Repository) openGitBody(ctx context.Context, ref Ref) (io.ReadCloser, error) {
	b, err := hex.DecodeString(ref.OID)
	if err != nil || (len(b) != 20 && len(b) != 32) || hex.EncodeToString(b) != ref.OID || !validHash(ref.SHA256) || ref.Size < 0 {
		return nil, ErrIntegrity
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := r.gitCommand(ctx, nil, "cat-file", "blob", ref.OID)
	cmd.Stderr = &boundedOutput{limit: 16 << 10}
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		if out != nil {
			out.Close()
		}
		return nil, errors.Join(ErrIntegrity, err)
	}
	stream := &gitBody{ReadCloser: out, cmd: cmd, cancel: cancel}
	return &verifiedBody{ReadCloser: stream, hash: sha256.New(), want: ref.SHA256, size: ref.Size, ctx: ctx}, nil
}

type gitBody struct {
	io.ReadCloser
	cmd    *exec.Cmd
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (b *gitBody) wait() error {
	b.once.Do(func() { b.err = b.cmd.Wait() })
	return b.err
}
func (b *gitBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		if e := b.wait(); e != nil {
			return n, errors.Join(ErrIntegrity, e)
		}
	}
	return n, err
}
func (b *gitBody) Close() error {
	b.cancel()
	err := b.ReadCloser.Close()
	_ = b.wait() // An early close intentionally cancels the rest of the stream.
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}
