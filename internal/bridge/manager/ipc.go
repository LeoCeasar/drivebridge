package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

const maxIPCBody = 128 << 20

type endpoint struct {
	SchemaVersion int           `json:"schema_version"`
	MountID       model.MountID `json:"mount_id"`
	ConfigFile    string        `json:"config_file"`
	InstanceID    string        `json:"instance_id"`
	PID           int           `json:"pid"`
	Address       string        `json:"address"`
	KeyRef        string        `json:"key_ref"`
}
type envelope struct {
	InstanceID string          `json:"instance_id"`
	PID        int             `json:"pid"`
	MountID    model.MountID   `json:"mount_id"`
	ConfigFile string          `json:"config_file"`
	Ready      bool            `json:"ready"`
	Data       json.RawMessage `json:"data,omitempty"`
	Error      *Error          `json:"error,omitempty"`
}
type Error struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

func (e *Error) Error() string     { return e.Code }
func (e *Error) Is(err error) bool { return errors.Is(errorFromCode(e.Code), err) }
func (m *Manager) writeEndpoint(id model.MountID, e endpoint) error {
	s, err := m.spec(context.Background(), id)
	if err != nil {
		return err
	}
	return atomicJSON(filepath.Join(s.Paths.Root, "ipc.json"), e)
}
func (m *Manager) readEndpoint(id model.MountID) (endpoint, string, error) {
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		return endpoint{}, "", err
	}
	return readEndpointAt(spec.Paths.Root, id, m.options.ConfigFile)
}

// readEndpointAt is bound to a validated historical ownership record, allowing
// installation recovery without trusting the current config's mutable paths.
func readEndpointAt(root string, id model.MountID, configFile string) (endpoint, string, error) {
	var e endpoint
	if !filepath.IsAbs(root) || !filepath.IsAbs(configFile) || !validID(string(id)) {
		return e, "", ErrInstanceChanged
	}
	path := filepath.Join(root, "ipc.json")
	if err := platform.CheckNoSymlinks(path); err != nil {
		return e, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return e, "", err
	}
	if len(b) > 64<<10 || json.Unmarshal(b, &e) != nil || e.SchemaVersion != 1 || e.MountID != id || e.ConfigFile != configFile || !validID(e.InstanceID) || e.PID <= 0 || e.KeyRef != "ipc.key" {
		return e, "", ErrInstanceChanged
	}
	u, err := url.Parse(e.Address)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() != "127.0.0.1" {
		return e, "", ErrInstanceChanged
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return e, "", ErrInstanceChanged
	}
	keyPath := filepath.Join(root, e.KeyRef)
	if err = platform.CheckNoSymlinks(keyPath); err != nil {
		return e, "", err
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return e, "", err
	}
	if len(raw) != 64 {
		return e, "", ErrInstanceChanged
	}
	if _, err = hex.DecodeString(string(raw)); err != nil {
		return e, "", ErrInstanceChanged
	}
	return e, string(raw), nil
}
func (m *Manager) call(ctx context.Context, id model.MountID, route string, input, output any) error {
	ep, key, err := m.readEndpoint(id)
	if err != nil {
		return err
	}
	return callEndpoint(ctx, ep, key, route, input, output)
}
func callEndpoint(ctx context.Context, ep endpoint, key, route string, input, output any) error {
	var body io.Reader
	method := "GET"
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return ErrConfig
		}
		if len(data) > maxIPCBody {
			return ErrConfig
		}
		body = bytes.NewReader(data)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, ep.Address+route, body)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-DriveBridge-Instance", ep.InstanceID)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 0}
	res, err := client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxIPCBody+1))
	if err != nil || len(raw) > maxIPCBody {
		return ErrUnavailable
	}
	var out envelope
	if json.Unmarshal(raw, &out) != nil {
		return ErrInstanceChanged
	}
	if out.InstanceID != ep.InstanceID || out.PID != ep.PID || out.MountID != ep.MountID || out.ConfigFile != ep.ConfigFile {
		return ErrInstanceChanged
	}
	if out.Error != nil {
		return out.Error
	}
	if res.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	if output != nil && len(out.Data) > 0 {
		if err = json.Unmarshal(out.Data, output); err != nil {
			return ErrUnavailable
		}
	}
	return nil
}
func (m *Manager) serveWorker(w *worker) (func(), error) {
	if err := platform.EnsurePrivateDir(w.spec.Paths.Root); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 32)
	if _, err = rand.Read(keyBytes); err != nil {
		listener.Close()
		return nil, err
	}
	key := hex.EncodeToString(keyBytes)
	keyPath := filepath.Join(w.spec.Paths.Root, "ipc.key")
	if err = platform.CheckNoSymlinks(keyPath); err != nil {
		listener.Close()
		return nil, err
	}
	if err = os.WriteFile(keyPath, []byte(key), 0600); err != nil {
		listener.Close()
		return nil, err
	}
	if err = platform.SecurePrivate(keyPath, false); err != nil {
		listener.Close()
		return nil, err
	}
	ep := endpoint{SchemaVersion: 1, MountID: w.spec.Mount.ID, ConfigFile: m.options.ConfigFile, InstanceID: w.instance, PID: os.Getpid(), Address: "http://" + listener.Addr().String(), KeyRef: "ipc.key"}
	if err = m.writeEndpoint(w.spec.Mount.ID, ep); err != nil {
		listener.Close()
		return nil, err
	}
	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(key) || subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Host != listener.Addr().String() || r.Header.Get("Origin") != "" || r.Header.Get("X-DriveBridge-Instance") != w.instance {
			rw.WriteHeader(http.StatusForbidden)
			return
		}
		w.mu.Lock()
		ready := w.ready
		w.mu.Unlock()
		out := envelope{InstanceID: w.instance, PID: os.Getpid(), MountID: w.spec.Mount.ID, ConfigFile: m.options.ConfigFile, Ready: ready}
		var value any
		var err error
		if r.URL.RawQuery != "" || r.URL.RawPath != "" {
			err = ErrConfig
		} else {
			value, err = w.dispatch(r)
		}
		if err != nil {
			code := errorCode(err)
			out.Error = &Error{Code: code, Retryable: code == "network" || code == "worker_unavailable", Message: code}
		} else {
			out.Data, _ = json.Marshal(value)
		}
		_ = json.NewEncoder(rw).Encode(out)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = server.Serve(listener) }()
	return func() {
		_ = server.Close()
		current, _, err := m.readEndpoint(w.spec.Mount.ID)
		if err == nil && current.InstanceID == w.instance {
			_ = os.Remove(filepath.Join(w.spec.Paths.Root, "ipc.json"))
			_ = os.Remove(keyPath)
		}
	}, nil
}
func decodeInput(r *http.Request, v any) error {
	if r.Method != "POST" {
		return ErrConfig
	}
	d := json.NewDecoder(io.LimitReader(r.Body, maxIPCBody+1))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return ErrConfig
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrConfig
	}
	return nil
}
func (w *worker) dispatch(r *http.Request) (any, error) {
	ctx := r.Context()
	if r.URL.Path == "/status" && r.Method == "GET" {
		return w.status(ctx)
	}
	w.mu.Lock()
	s := w.services
	ready := w.ready
	w.mu.Unlock()
	if s == nil || !ready {
		return nil, ErrReadyTimeout
	}
	switch r.URL.Path {
	case "/retry", "/draft", "/resolve", "/cache/clean", "/conflict/claim", "/conflict/export", "/conflict", "/refresh-directories":
		controlCtx, release, err := w.beginControl(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		ctx = controlCtx
	}
	switch r.URL.Path {
	case "/refresh-directories":
		var in struct{}
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		if s.refreshDirectories == nil {
			return nil, ErrDriver
		}
		return struct{}{}, s.refreshDirectories(ctx)
	case "/stop", "/recovery/stop":
		var input struct {
			Wait int64 `json:"wait_ns"`
		}
		if err := decodeInput(r, &input); err != nil {
			return nil, err
		}
		if input.Wait < 0 || time.Duration(input.Wait) > 24*time.Hour {
			return nil, ErrConfig
		}
		return struct{}{}, w.stopPolicy(ctx, time.Duration(input.Wait), r.URL.Path == "/recovery/stop")
	case "/uploads":
		if r.Method != "GET" {
			return nil, ErrConfig
		}
		return s.store.Pending(ctx)
	case "/retry":
		var in struct{}
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		if s.retry == nil {
			return nil, ErrDriver
		}
		return struct{}{}, s.retry(ctx)
	case "/conflicts":
		if r.Method != "GET" {
			return nil, ErrConfig
		}
		return w.conflicts(ctx)
	case "/conflict":
		var in struct {
			ID        model.ConflictID `json:"id"`
			NoRefresh bool             `json:"no_refresh"`
		}
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		return w.conflictDetail(ctx, in.ID, !in.NoRefresh)
	case "/conflict/export":
		var in struct {
			ID model.ConflictID `json:"id"`
		}
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		return s.store.ExportConflictCopies(ctx, in.ID)
	case "/conflict/claim":
		var in struct {
			ID model.ConflictID `json:"id"`
		}
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		return s.store.ClaimConflictNotification(ctx, in.ID)
	case "/draft":
		var in struct {
			ID      model.ConflictID `json:"id"`
			Content []byte           `json:"content"`
		}
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		return struct{}{}, s.store.SaveDraft(ctx, in.ID, bytes.NewReader(in.Content))
	case "/resolve":
		var in model.ResolveRequest
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		if s.resolve == nil {
			return nil, ErrDriver
		}
		return struct{}{}, s.resolve(ctx, in)
	case "/cache":
		if r.Method != "GET" {
			return nil, ErrConfig
		}
		return cacheStatus(w.spec.Paths.Root)
	case "/cache/clean":
		var in struct{}
		if err := decodeInput(r, &in); err != nil {
			return nil, err
		}
		return s.store.GC(ctx, storeGCOptions(w.spec.Mount.CacheMaxBytes))
	case "/logs":
		if r.Method != "GET" {
			return nil, ErrConfig
		}
		return readLogs(filepath.Join(w.spec.Paths.Logs, "events.jsonl"))
	default:
		return nil, ErrConfig
	}
}
