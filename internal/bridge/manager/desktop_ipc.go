package manager

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/platform"
)

var ErrDesktopRunning = errors.New("desktop instance already running")

func (m *Manager) desktopRoot() string    { return filepath.Join(m.domain, "desktop") }
func (m *Manager) desktopRunLock() string { return filepath.Join(m.desktopRoot(), "run.lock") }

// readDesktopEndpoint accepts another configuration only for installation,
// which must retire the OS-user desktop regardless of the invoking config.
func (m *Manager) readDesktopEndpoint(sameConfig bool) (endpoint, string, error) {
	var ep endpoint
	path := filepath.Join(m.desktopRoot(), "ipc.json")
	if err := platform.CheckNoSymlinks(path); err != nil {
		return ep, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ep, "", err
	}
	if len(raw) > 64<<10 || json.Unmarshal(raw, &ep) != nil || ep.SchemaVersion != 1 || ep.MountID != "" || !filepath.IsAbs(ep.ConfigFile) || filepath.Clean(ep.ConfigFile) != ep.ConfigFile || !validID(ep.InstanceID) || ep.PID <= 0 || ep.KeyRef != "ipc.key" || (sameConfig && ep.ConfigFile != m.ConfigFile()) {
		return ep, "", ErrInstanceChanged
	}
	u, err := url.Parse(ep.Address)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() != "127.0.0.1" {
		return ep, "", ErrInstanceChanged
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return ep, "", ErrInstanceChanged
	}
	path = filepath.Join(m.desktopRoot(), ep.KeyRef)
	if err = platform.CheckNoSymlinks(path); err != nil {
		return ep, "", err
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		return ep, "", err
	}
	if len(raw) != 64 {
		return ep, "", ErrInstanceChanged
	}
	if _, err = hex.DecodeString(string(raw)); err != nil {
		return ep, "", ErrInstanceChanged
	}
	return ep, string(raw), nil
}

// DesktopRegister retains singleton ownership until the caller has completely
// stopped the UI and tray and invokes release. A quit request only asks the
// caller to begin shutdown; it never releases ownership or stops workers.
func (m *Manager) DesktopRegister(ctx context.Context, open func(context.Context) error, shutdown func()) (release func(), err error) {
	admission, err := m.startLease(ctx)
	if err != nil {
		return nil, err
	}
	defer admission()
	lock, err := privateLock(m.desktopRunLock())
	if err != nil {
		return nil, err
	}
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		if _, _, err = m.readDesktopEndpoint(true); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return nil, ErrDesktopRunning
	}
	owned := true
	defer func() {
		if owned {
			_ = lock.Unlock()
		}
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	serving := false
	defer func() {
		if !serving {
			_ = listener.Close()
		}
	}()
	keyBytes := make([]byte, 32)
	if _, err = rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := hex.EncodeToString(keyBytes)
	keyPath := filepath.Join(m.desktopRoot(), "ipc.key")
	if err = platform.CheckNoSymlinks(keyPath); err != nil {
		return nil, err
	}
	if err = os.WriteFile(keyPath, []byte(key), 0600); err != nil {
		return nil, err
	}
	if err = platform.SecurePrivate(keyPath, false); err != nil {
		return nil, err
	}
	ep := endpoint{SchemaVersion: 1, ConfigFile: m.ConfigFile(), InstanceID: uuid.NewString(), PID: os.Getpid(), Address: "http://" + listener.Addr().String(), KeyRef: "ipc.key"}
	if err = atomicJSON(filepath.Join(m.desktopRoot(), "ipc.json"), ep); err != nil {
		return nil, err
	}
	var quitOnce sync.Once
	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(key) || subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Host != listener.Addr().String() || r.Header.Get("Origin") != "" || r.Header.Get("X-DriveBridge-Instance") != ep.InstanceID {
			rw.WriteHeader(http.StatusForbidden)
			return
		}
		out := envelope{InstanceID: ep.InstanceID, PID: ep.PID, ConfigFile: ep.ConfigFile, Ready: true}
		var callErr error
		if r.URL.RawQuery != "" || r.URL.RawPath != "" {
			callErr = ErrConfig
		} else {
			switch {
			case r.URL.Path == "/status" && r.Method == "GET":
			case r.URL.Path == "/open" && r.Method == "POST":
				if open != nil {
					callErr = open(r.Context())
				}
			case r.URL.Path == "/quit" && r.Method == "POST":
				if shutdown != nil {
					go quitOnce.Do(shutdown)
				}
			default:
				callErr = ErrConfig
			}
		}
		if callErr != nil {
			code := errorCode(callErr)
			out.Error = &Error{Code: code, Message: code}
		}
		_ = json.NewEncoder(rw).Encode(out)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = server.Serve(listener) }()
	serving, owned = true, false
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = server.Close()
			_ = os.Remove(filepath.Join(m.desktopRoot(), "ipc.json"))
			_ = os.Remove(keyPath)
			_ = lock.Unlock()
		})
	}, nil
}

func (m *Manager) desktopLocked() (bool, error) {
	lock, err := privateLock(m.desktopRunLock())
	if err != nil {
		return false, err
	}
	ok, err := lock.TryLock()
	if err != nil {
		return false, err
	}
	if ok {
		return false, lock.Unlock()
	}
	return true, nil
}
func (m *Manager) DesktopRunning(ctx context.Context) (bool, error) {
	locked, err := m.desktopLocked()
	if err != nil || !locked {
		return false, err
	}
	ep, key, err := m.readDesktopEndpoint(true)
	if err != nil {
		return false, err
	}
	err = callEndpoint(ctx, ep, key, "/status", nil, nil)
	return err == nil, err
}
func (m *Manager) desktopCall(ctx context.Context, route string) error {
	locked, err := m.desktopLocked()
	if err != nil {
		return err
	}
	if !locked {
		if route == "/quit" {
			return nil
		}
		return ErrUnavailable
	}
	ep, key, err := m.readDesktopEndpoint(true)
	if err != nil {
		return err
	}
	return callEndpoint(ctx, ep, key, route, struct{}{}, nil)
}
func (m *Manager) DesktopOpen(ctx context.Context) error { return m.desktopCall(ctx, "/open") }
func (m *Manager) DesktopQuit(ctx context.Context) error { return m.desktopCall(ctx, "/quit") }

// installationDesktopLease runs under exclusive lifecycle admission. Quit IPC
// deliberately acquires no lifecycle lease: the desktop must be able to answer
// even while installation has frozen all new workers and config mutations.
func (m *Manager) installationDesktopLease(ctx context.Context) (func(), error) {
	lock, err := privateLock(m.desktopRunLock())
	if err != nil {
		return nil, err
	}
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		ep, key, err := m.readDesktopEndpoint(false)
		if err != nil {
			return nil, errors.Join(ErrPending, err)
		}
		if err = callEndpoint(ctx, ep, key, "/quit", struct{}{}, nil); err != nil && !errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		waitCtx, cancel := context.WithTimeout(ctx, 61*time.Second)
		ok, err = lock.TryLockContext(waitCtx, 5*time.Millisecond)
		cancel()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrPending
		}
	}
	return func() { _ = lock.Unlock() }, nil
}
