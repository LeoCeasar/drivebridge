// Package larkcli provides a typed, non-shell transport for the pinned official CLI.
package larkcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

type Request struct {
	Args  []string
	Stdin []byte
	Dir   string
	// Timeout includes credential-lock waiting and execution. Non-positive values
	// select the finite business default; an earlier context deadline wins.
	Timeout time.Duration
}

const businessTimeout = 2 * time.Minute
const transferTimeout = 15 * time.Minute

type Response struct {
	Data     json.RawMessage
	Warnings []string
	Result   string
}
type Identity struct{ Brand, AppID, UserOpenID string }

// Coordinator owns OS-user gate -> AppID -> optional OpenID lock ordering.
// Its OS-user namespace must be independent of StateDir/configuration roots.
// The callback covers start, streams, cancellation and reap of each child.
type Coordinator interface {
	WithCredentialLocks(context.Context, string, string, func() error) error
	WithEnrollmentLock(context.Context, func() error) error
}
type ManagedOptions struct {
	Executable, AuthRef, AuthRoot string
	Identity                      Identity
	Coordinator                   Coordinator
}
type Runner struct {
	executable, authRef, authRoot string
	identity                      Identity
	coordinator                   Coordinator
	managed                       bool
}

func validCoordinator(c Coordinator) bool {
	if c == nil {
		return false
	}
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !v.IsNil()
	}
	return true
}

// NewDirect preserves native CLI configuration and official locking.
func NewDirect(executable string) *Runner { return &Runner{executable: executable} }
func NewManaged(o ManagedOptions) (*Runner, error) {
	if o.Executable == "" || o.AuthRef == "" || !filepath.IsAbs(o.AuthRoot) || !validCoordinator(o.Coordinator) || o.Identity.AppID == "" || o.Identity.UserOpenID == "" || (o.Identity.Brand != "feishu" && o.Identity.Brand != "lark") {
		return nil, ErrIdentity
	}
	return &Runner{executable: o.Executable, authRef: o.AuthRef, authRoot: o.AuthRoot, identity: o.Identity, coordinator: o.Coordinator, managed: true}, nil
}

// ManagedIdentity returns a verified business binding, never an enrollment's
// partial identity. Direct and lifecycle-only runners cannot cross product APIs.
func (r *Runner) ManagedIdentity() (Identity, bool) {
	if r == nil || !r.managed || r.identity.AppID == "" || r.identity.UserOpenID == "" {
		return Identity{}, false
	}
	return r.identity, true
}

// Explicit whitelist: provider overrides, workspace switches, profile selectors,
// shell startup hooks and arbitrary credentials are never inherited in managed mode.
var managedEnvKeys = map[string]bool{"PATH": true, "HOME": true, "USERPROFILE": true, "LOCALAPPDATA": true, "APPDATA": true, "SYSTEMROOT": true, "WINDIR": true, "TMPDIR": true, "TMP": true, "TEMP": true, "LANG": true, "LC_ALL": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "SSL_CERT_BUNDLE": true, "CURL_CA_BUNDLE": true}

func (r *Runner) environment() []string {
	if !r.managed {
		return os.Environ()
	}
	return managedEnvironment(os.Environ(), r.authRoot, runtime.GOOS == "windows")
}
func managedEnvironment(base []string, authRoot string, caseInsensitive bool) []string {
	var env []string
	for _, v := range base {
		k, _, _ := strings.Cut(v, "=")
		if caseInsensitive {
			k = strings.ToUpper(k)
		}
		if managedEnvKeys[k] {
			env = append(env, v)
		}
	}
	return append(env, "LARKSUITE_CLI_CONFIG_DIR="+authRoot)
}

func (r *Runner) command(ctx context.Context, args []string, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.executable, args...)
	configureCommand(cmd)
	cmd.Env = r.environment()
	cmd.Dir = dir
	return cmd
}
func (r *Runner) locked(ctx context.Context, enrollment bool, run func() error) error {
	if !r.managed {
		return run()
	}
	if r.coordinator == nil {
		return ErrIdentity
	}
	if enrollment {
		return r.coordinator.WithEnrollmentLock(ctx, run)
	}
	return r.coordinator.WithCredentialLocks(ctx, r.identity.AppID, r.identity.UserOpenID, run)
}
func businessArgs(args []string) ([]string, error) {
	out := append([]string(nil), args...)
	hasAs, hasJSON := false, false
	for i, a := range args {
		if a == "--as" {
			if i+1 >= len(args) || args[i+1] != "user" {
				return nil, ErrIdentity
			}
			hasAs = true
		}
		if a == "--json" {
			hasJSON = true
		}
		if strings.HasPrefix(a, "--as=") || a == "--workspace" || strings.HasPrefix(a, "--workspace=") || a == "--profile" || strings.HasPrefix(a, "--profile=") || a == "--config-dir" || strings.HasPrefix(a, "--config-dir=") {
			return nil, ErrIdentity
		}
	}
	if !hasAs {
		out = append(out, "--as", "user")
	}
	if !hasJSON {
		out = append(out, "--json")
	}
	return out, nil
}
func (r *Runner) Run(ctx context.Context, req Request) (resp Response, err error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = businessTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if r.managed && (r.identity.AppID == "" || r.identity.UserOpenID == "") {
		return resp, errors.Join(ErrNotDispatched, ErrIdentity)
	}
	args, err := businessArgs(req.Args)
	if err != nil {
		return resp, errors.Join(ErrNotDispatched, err)
	}
	started := false
	err = r.locked(ctx, false, func() error {
		cmd := r.command(ctx, args, req.Dir)
		cmd.Stdin = bytes.NewReader(req.Stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		var runErr error
		started, runErr = runBusinessCommand(cmd)
		resp, err = decodeResponse(stdout.Bytes(), stderr.Bytes(), runErr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	})
	if err != nil && !started {
		err = errors.Join(ErrNotDispatched, err)
	}
	return resp, err
}

type wireEnvelope struct {
	OK       *bool           `json:"ok"`
	Identity string          `json:"identity"`
	Data     json.RawMessage `json:"data"`
	Error    *wireError      `json:"error"`
}
type wireError struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	Code       json.RawMessage `json:"code"`
	Retryable  bool            `json:"retryable"`
	RetryAfter int64           `json:"retry_after_seconds"`
}

func singleJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if d.Decode(out) != nil {
		return protocol()
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return protocol()
	}
	return nil
}
func (e *wireError) safe() *Error {
	code := string(e.Code)
	if len(code) > 64 {
		code = ""
	}
	if len(code) > 0 && code[0] == '"' {
		var s string
		if json.Unmarshal(e.Code, &s) == nil {
			code = s
		}
	}
	return &Error{Type: e.Type, Subtype: e.Subtype, Code: code, Retryable: e.Retryable, RetryAfter: time.Duration(e.RetryAfter) * time.Second}
}
func decodeResponse(stdout, stderr []byte, runErr error) (Response, error) {
	var resp Response
	var out wireEnvelope
	outErr := singleJSON(stdout, &out)
	se, stderrEnvelope, stderrErr := stderrResponse(stderr)
	if stderrErr != nil {
		return resp, stderrErr
	}

	var dataErr error
	if outErr == nil {
		resp.Data = out.Data
		var info struct {
			Warnings json.RawMessage `json:"warnings"`
			Result   json.RawMessage `json:"result"`
		}
		if len(out.Data) > 0 && json.Unmarshal(out.Data, &info) == nil {
			if len(info.Warnings) > 0 {
				if bytes.Equal(info.Warnings, []byte("null")) || json.Unmarshal(info.Warnings, &resp.Warnings) != nil {
					dataErr = ErrProtocol
				}
			}
			if len(info.Result) > 0 {
				if json.Unmarshal(info.Result, &resp.Result) != nil || resp.Result == "" {
					dataErr = ErrProtocol
				}
			}
		}
	}

	if stderrEnvelope {
		if se.Identity != "" && se.Identity != "user" {
			return resp, ErrIdentity
		}
		if *se.OK || se.Error == nil {
			return resp, protocol()
		}
		return resp, se.Error.safe()
	}
	if outErr != nil {
		return resp, &Error{Type: "transport", Cause: errors.Join(ErrProtocol, runErr)}
	}
	if out.OK == nil {
		return resp, protocol()
	}
	if out.Identity != "user" {
		return resp, ErrIdentity
	}
	if out.Error != nil {
		return resp, out.Error.safe()
	}
	if runErr != nil {
		return resp, &Error{Type: "process", Cause: runErr}
	}
	if !*out.OK {
		return resp, &Error{Type: "operation", Subtype: "failed"}
	}
	if len(out.Data) == 0 || bytes.Equal(bytes.TrimSpace(out.Data), []byte("null")) || bytes.TrimSpace(out.Data)[0] != '{' {
		return resp, protocol()
	}
	if dataErr != nil {
		return resp, dataErr
	}
	if resp.Result != "" && resp.Result != "success" {
		return resp, &Error{Type: "operation", Subtype: "unconfirmed"}
	}
	return resp, nil
}

// CLI diagnostics may precede its pretty-printed terminal JSON envelope.
// Inspect only JSON starting at a line boundary, preserve exactly one envelope,
// and never put the surrounding prose into an error.
func stderrResponse(data []byte) (wireEnvelope, bool, error) {
	var result wireEnvelope
	found := false
	for offset := 0; offset < len(data); {
		lineEnd := bytes.IndexByte(data[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(data) - offset
		}
		line := bytes.TrimSpace(data[offset : offset+lineEnd])
		if len(line) > 0 && line[0] == '{' {
			start := offset + bytes.IndexByte(data[offset:offset+lineEnd], '{')
			d := json.NewDecoder(bytes.NewReader(data[start:]))
			var env wireEnvelope
			if d.Decode(&env) == nil && env.OK != nil {
				if found {
					return wireEnvelope{}, false, ErrProtocol
				}
				result = env
				found = true
				offset = start + int(d.InputOffset())
				continue
			}
		}
		offset += lineEnd + 1
	}
	return result, found, nil
}
