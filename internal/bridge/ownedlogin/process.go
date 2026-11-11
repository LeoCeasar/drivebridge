package ownedlogin

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/discovery"
)

const maxBody = 16 << 20
const BundleID = "org.drivebridge.sessionhost"
const ProtocolVersion = 1

type command struct {
	ID    string `json:"id"`
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Query string `json:"query,omitempty"`
}
type helperResult struct {
	Kind        string `json:"kind"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
	Body        string `json:"body"`
}
type response struct {
	ID     string        `json:"id"`
	OK     *bool         `json:"ok"`
	Result *helperResult `json:"result,omitempty"`
	Error  string        `json:"error,omitempty"`
}
type delivery struct {
	response response
	err      error
}
type process struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	mu        sync.Mutex
	writeMu   sync.Mutex
	pending   map[string]chan delivery
	closed    bool
	done      chan struct{}
	ready     chan error
	readyOnce sync.Once
}

func helperExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrUnavailable
	}
	path = filepath.Clean(path)
	if strings.HasSuffix(path, ".app") {
		path = filepath.Join(path, "Contents", "MacOS", "drivebridge-session-host")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode()&0111 == 0) {
		return "", ErrUnavailable
	}
	return path, nil
}
func startHelper(path, profile, origin string) (*process, error) {
	exe, err := helperExecutable(path)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "--origin", origin, "--profile-id", profile)
	configureHelperProcess(cmd)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, ErrUnavailable
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, ErrUnavailable
	}
	p := &process{cmd: cmd, stdin: stdin, pending: map[string]chan delivery{}, done: make(chan struct{}), ready: make(chan error, 1)}
	if cmd.Start() != nil {
		return nil, ErrUnavailable
	}
	go func() {
		// Drain JSONL before Wait closes StdoutPipe, including the final reply.
		p.read(stdout)
		_ = cmd.Wait()
		p.disconnected()
		close(p.done)
	}()
	return p, nil
}
func (p *process) disconnected() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.readyOnce.Do(func() { p.ready <- ErrDisconnected })
	for id, ch := range p.pending {
		ch <- delivery{err: ErrDisconnected}
		delete(p.pending, id)
	}
}
func (p *process) read(in io.Reader) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), (32<<20)+4096)
	for scanner.Scan() {
		p.dispatch(scanner.Bytes())
	}
	p.disconnected()
}
func (p *process) dispatch(data []byte) {
	var event struct {
		Event      string `json:"event"`
		Protocol   int    `json:"protocol"`
		BundleID   string `json:"bundle_id"`
		Persistent bool   `json:"persistent"`
		Error      string `json:"error"`
	}
	if json.Unmarshal(data, &event) == nil && event.Event == "ready" {
		err := error(nil)
		if event.Protocol != ProtocolVersion || event.BundleID != BundleID {
			err = ErrUnavailable
		} else if event.Error == "missing_runtime" && !event.Persistent {
			err = ErrRuntimeUnavailable
		} else if !event.Persistent || event.Error != "" {
			err = ErrUnavailable
		}
		p.readyOnce.Do(func() { p.ready <- err })
		return
	}
	var reply response
	if !utf8.Valid(data) || json.Unmarshal(data, &reply) != nil || !canonicalID(reply.ID) {
		return
	}
	p.mu.Lock()
	ch := p.pending[reply.ID]
	delete(p.pending, reply.ID)
	p.mu.Unlock()
	if ch != nil {
		if reply.OK == nil {
			ch <- delivery{err: discovery.ErrProtocol}
		} else {
			ch <- delivery{response: reply}
		}
	}
}
func (p *process) call(ctx context.Context, op, path, query string) (response, error) {
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	id := uuid.NewString()
	ch := make(chan delivery, 1)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return response{}, ErrDisconnected
	}
	if len(p.pending) >= 8 {
		p.mu.Unlock()
		return response{}, ErrProtocol
	}
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	p.writeMu.Lock()
	err := json.NewEncoder(p.stdin).Encode(command{ID: id, Op: op, Path: path, Query: query})
	p.writeMu.Unlock()
	if err != nil {
		return response{}, ErrDisconnected
	}
	select {
	case item := <-ch:
		if item.err != nil {
			return response{}, item.err
		}
		if !*item.response.OK {
			switch item.response.Error {
			case "login", "needs_login":
				return response{}, discovery.ErrNeedsLogin
			case "disconnected":
				return response{}, ErrDisconnected
			default:
				return response{}, discovery.ErrProtocol
			}
		}
		return item.response, nil
	case <-ctx.Done():
		return response{}, ctx.Err()
	}
}
func (p *process) stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, _ = p.call(shutdownCtx, "shutdown", "", "")
	cancel()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
	case <-timer.C:
	}
	_ = p.cmd.Process.Kill()
	_ = p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
	return ErrProtocol
}
