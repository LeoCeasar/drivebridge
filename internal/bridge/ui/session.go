package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

var ErrAlreadyRunning = errors.New("UI host already running")

type session struct {
	csrf    string
	expires time.Time
}
type descriptor struct {
	URL, Instance, Secret, ConfigKey string
	PID                              int
}
type notification struct {
	Revision string
	Count    int
	Opened   bool
}
type Host struct {
	prepared                                       preparedConflicts
	backend                                        Backend
	notifier                                       Notifier
	runtimeDir, configKey, base, instance, control string
	listener                                       net.Listener
	server                                         *http.Server
	lock                                           *flock.Flock
	mu                                             sync.Mutex
	bootstraps                                     map[string]time.Time
	sessions                                       map[string]session
	notices                                        map[model.ConflictID]notification
	closeOnce                                      sync.Once
	done                                           chan struct{}
	closeErr                                       error
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func tokenHash(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func same(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func Start(ctx context.Context, o Options) (*Host, error) {
	if o.Backend == nil || o.ConfigKey == "" {
		return nil, errors.New("UI backend and configuration identity required")
	}
	if err := platform.EnsurePrivateDir(o.RuntimeDir); err != nil {
		return nil, err
	}
	lp := filepath.Join(o.RuntimeDir, "host.lock")
	if err := platform.CheckNoSymlinks(lp); err != nil {
		return nil, err
	}
	lock := flock.New(lp)
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAlreadyRunning
	}
	if err = platform.SecurePrivate(lp, false); err != nil {
		lock.Unlock()
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		lock.Unlock()
		return nil, err
	}
	secret, err := randomToken()
	if err != nil {
		listener.Close()
		lock.Unlock()
		return nil, err
	}
	n := o.Notifier
	if n == nil {
		n = desktopNotifier{}
	}
	h := &Host{backend: o.Backend, notifier: n, runtimeDir: o.RuntimeDir, configKey: o.ConfigKey, base: "http://" + listener.Addr().String(), instance: uuid.NewString(), control: secret, listener: listener, lock: lock, bootstraps: map[string]time.Time{}, sessions: map[string]session{}, notices: map[model.ConflictID]notification{}, done: make(chan struct{})}
	noticesPath := filepath.Join(h.runtimeDir, "notifications.json")
	if err = platform.CheckNoSymlinks(noticesPath); err == nil {
		if b, e := os.ReadFile(noticesPath); e == nil {
			err = json.Unmarshal(b, &h.notices)
		} else if !os.IsNotExist(e) {
			err = e
		}
	}
	if err != nil {
		listener.Close()
		lock.Unlock()
		return nil, err
	}
	if err = h.writeDescriptor(); err != nil {
		listener.Close()
		lock.Unlock()
		return nil, err
	}
	h.server = &http.Server{Handler: http.HandlerFunc(h.serve), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = h.server.Serve(listener) }()
	go func() {
		select {
		case <-ctx.Done():
			c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = h.Close(c)
		case <-h.done:
		}
	}()
	return h, nil
}
func (h *Host) URL() string { return h.base }
func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		close(h.done)
		h.prepared.close()
		shutdown, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		h.closeErr = h.server.Shutdown(shutdown)
		if h.closeErr != nil {
			_ = h.server.Close()
		}
		h.closeErr = errors.Join(h.closeErr, os.Remove(filepath.Join(h.runtimeDir, "host.json")), h.lock.Unlock())
	})
	return h.closeErr
}
func atomicJSON(p string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = platform.CheckNoSymlinks(p); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".ui-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = platform.SecurePrivate(name, false); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = platform.ReplaceFile(name, p); err != nil {
		return err
	}
	return platform.SyncDir(filepath.Dir(p))
}
func (h *Host) writeDescriptor() error {
	return atomicJSON(filepath.Join(h.runtimeDir, "host.json"), descriptor{h.base, h.instance, h.control, h.configKey, os.Getpid()})
}
func validID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}
func (h *Host) BootstrapURL(conflict model.ConflictID) (string, error) {
	if conflict != "" && !validID(string(conflict)) {
		return "", errors.New("invalid conflict ID")
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for key, expires := range h.bootstraps {
		if expires.Before(now) {
			delete(h.bootstraps, key)
		}
	}
	if len(h.bootstraps) >= 64 {
		return "", errors.New("too many pending browser sessions")
	}
	h.bootstraps[tokenHash(token)] = now.Add(3 * time.Minute)
	if conflict != "" {
		notice := h.notices[conflict]
		notice.Opened = true
		h.notices[conflict] = notice
		if err = atomicJSON(filepath.Join(h.runtimeDir, "notifications.json"), h.notices); err != nil {
			return "", err
		}
	}
	query := ""
	if conflict != "" {
		query = "?conflict=" + url.QueryEscape(string(conflict))
	}
	return h.base + "/" + query + "#bootstrap=" + token, nil
}
func (h *Host) newSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Bootstrap string `json:"bootstrap"`
	}
	if err := decode(w, r, &input); err != nil {
		respondError(w, 400, "invalid_request")
		return
	}
	h.mu.Lock()
	expires, ok := h.bootstraps[tokenHash(input.Bootstrap)]
	delete(h.bootstraps, tokenHash(input.Bootstrap))
	h.mu.Unlock()
	if !ok || expires.Before(time.Now()) {
		respondError(w, 403, "unauthorized")
		return
	}
	token, err := randomToken()
	if err != nil {
		respondError(w, 500, "unavailable")
		return
	}
	csrf, err := randomToken()
	if err != nil {
		respondError(w, 500, "unavailable")
		return
	}
	h.mu.Lock()
	for k, v := range h.sessions {
		if v.expires.Before(time.Now()) {
			delete(h.sessions, k)
		}
	}
	if len(h.sessions) >= 64 {
		h.mu.Unlock()
		respondError(w, 429, "too_many_sessions")
		return
	}
	h.sessions[tokenHash(token)] = session{csrf: csrf, expires: time.Now().Add(8 * time.Hour)}
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "drivebridge_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
	respond(w, 200, map[string]string{"csrf": csrf})
}
func (h *Host) authorize(r *http.Request) (session, bool) {
	cookie, err := r.Cookie("drivebridge_session")
	if err != nil {
		return session{}, false
	}
	h.mu.Lock()
	v, ok := h.sessions[tokenHash(cookie.Value)]
	h.mu.Unlock()
	return v, ok && v.expires.After(time.Now())
}

// OpenExisting talks only to the private descriptor's authenticated control
// endpoint; a browser session or reusable browser credential is never persisted.
func OpenExisting(ctx context.Context, dir, key string, conflict model.ConflictID) (string, error) {
	p := filepath.Join(dir, "host.json")
	if err := platform.CheckNoSymlinks(p); err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var d descriptor
	if err = json.Unmarshal(b, &d); err != nil {
		return "", err
	}
	u, err := url.Parse(d.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid UI host descriptor")
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil || !net.ParseIP(host).IsLoopback() || d.ConfigKey != key || !validID(d.Instance) || d.Secret == "" {
		return "", errors.New("UI descriptor identity mismatch")
	}
	body, _ := json.Marshal(map[string]string{"instance": d.Instance, "conflict_id": string(conflict)})
	req, err := http.NewRequestWithContext(ctx, "POST", d.URL+"/internal/open", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+d.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected UI redirect") }}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct{ Instance, URL string }
	if res.StatusCode != 200 {
		return "", fmt.Errorf("UI host control refused: %d", res.StatusCode)
	}
	if err = json.NewDecoder(ioLimit(res.Body, 16<<10)).Decode(&out); err != nil {
		return "", err
	}
	if out.Instance != d.Instance || !strings.HasPrefix(out.URL, d.URL+"/") {
		return "", errors.New("UI instance handshake mismatch")
	}
	return out.URL, nil
}
