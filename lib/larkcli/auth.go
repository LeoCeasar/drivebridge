package larkcli

import (
	"bufio"
	"context"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type EnrollmentOptions struct {
	Executable, AuthRef, AuthRoot, AppID string
	Coordinator                          Coordinator
}

// NewEnrollment creates a lifecycle-only runner; Run refuses business requests.
func NewEnrollment(o EnrollmentOptions) (*Runner, error) {
	if o.Executable == "" || o.AuthRef == "" || !filepath.IsAbs(o.AuthRoot) || !validCoordinator(o.Coordinator) {
		return nil, ErrIdentity
	}
	return &Runner{executable: o.Executable, authRef: o.AuthRef, authRoot: o.AuthRoot, coordinator: o.Coordinator, identity: Identity{AppID: o.AppID}, managed: true}, nil
}

// AuthEvent is a whitelist. Device codes, tokens, secrets and free text are discarded.
type AuthEvent struct {
	Kind, URL, UserCode string
	ExpiresIn           int64
}
type AuthResult struct {
	AppID, Brand, UserOpenID, Scope string
	Granted, Requested, Missing     []string
	ExpiresAt                       time.Time
}

// AuthStream retains the same child and its credential locks through Wait/reap.
// Initial is available only after a validated official URL. ExpiresAt in app
// registration is unknown; the local wait limit is independent of server expiry.
type AuthStream struct {
	Initial AuthEvent
	cancel  context.CancelFunc
	done    chan struct{}
	result  AuthResult
	err     error
}

func (s *AuthStream) Wait() (AuthResult, error)          { <-s.done; return s.result, s.err }
func (s *AuthStream) CancelAndWait() (AuthResult, error) { s.cancel(); return s.Wait() }
func (r *Runner) StartEnrollment(ctx context.Context, brand string) (*AuthStream, error) {
	if brand != "feishu" && brand != "lark" {
		return nil, ErrProtocol
	}
	return r.startStream(ctx, []string{"config", "init", "--new", "--brand", brand}, true)
}
func (r *Runner) StartAuth(ctx context.Context, scopes []string) (*AuthStream, error) {
	if r.identity.AppID == "" || len(scopes) == 0 {
		return nil, ErrIdentity
	}
	for _, scope := range scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			return nil, ErrProtocol
		}
	}
	return r.startStream(ctx, []string{"auth", "login", "--scope", strings.Join(scopes, " "), "--json"}, false)
}
func officialURL(raw string, enrollment bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return false
	}
	switch u.Host {
	case "open.feishu.cn", "open.larksuite.com":
		return !enrollment || (u.Path == "/page/cli" && u.Query().Get("user_code") != "")
	case "accounts.feishu.cn", "accounts.larksuite.com":
		return !enrollment
	}
	return false
}
func (r *Runner) startStream(parent context.Context, args []string, enrollment bool) (*AuthStream, error) {
	if !r.managed || r.coordinator == nil {
		return nil, ErrIdentity
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	s := &AuthStream{cancel: cancel, done: make(chan struct{})}
	ready := make(chan AuthEvent, 1)
	go func() {
		defer close(s.done)
		defer cancel()
		s.err = r.locked(ctx, enrollment, func() error {
			cmd := r.command(ctx, args, "")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return ErrProtocol
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				return ErrProtocol
			}
			if cmd.Start() != nil {
				return &Error{Type: "auth", Subtype: "start"}
			}
			var wg sync.WaitGroup
			var outErr, errErr error
			var once sync.Once
			publish := func(event AuthEvent) { once.Do(func() { ready <- event }) }
			wg.Add(2)
			go func() {
				defer wg.Done()
				scanner := bufio.NewScanner(stderr)
				scanner.Buffer(make([]byte, 4096), 1<<20)
				for scanner.Scan() {
					if enrollment {
						for _, field := range strings.Fields(scanner.Text()) {
							if officialURL(field, true) {
								publish(AuthEvent{Kind: "app_config", URL: field})
							}
						}
					}
				}
				if scanner.Err() != nil {
					errErr = ErrProtocol
					cancel()
				}
			}()
			go func() {
				defer wg.Done()
				if enrollment {
					data, readErr := io.ReadAll(io.LimitReader(stdout, (1<<20)+1))
					var out struct {
						AppID string `json:"appId"`
						Brand string `json:"brand"`
					}
					if readErr != nil || len(data) > 1<<20 || singleJSON(data, &out) != nil || out.AppID == "" || (out.Brand != "feishu" && out.Brand != "lark") {
						outErr = ErrProtocol
						cancel()
						return
					}
					s.result.AppID = out.AppID
					s.result.Brand = out.Brand
					return
				}
				scanner := bufio.NewScanner(stdout)
				scanner.Buffer(make([]byte, 4096), 1<<20)
				for scanner.Scan() {
					line := scanner.Bytes()
					var event struct {
						Event                       string  `json:"event"`
						URI                         string  `json:"verification_uri"`
						CompleteURI                 string  `json:"verification_uri_complete"`
						UserCode                    string  `json:"user_code"`
						ExpiresIn                   int64   `json:"expires_in"`
						OpenID                      string  `json:"user_open_id"`
						Scope                       *string `json:"scope"`
						Granted, Requested, Missing *[]string
						ExpiresAt                   string `json:"expires_at"`
					}
					if singleJSON(line, &event) != nil {
						outErr = ErrProtocol
						cancel()
						continue
					}
					switch event.Event {
					case "device_authorization":
						u := event.CompleteURI
						if u == "" {
							u = event.URI
						}
						if !officialURL(u, false) || event.ExpiresIn <= 0 || event.UserCode == "" {
							outErr = ErrProtocol
							cancel()
							continue
						}
						publish(AuthEvent{Kind: "user_auth", URL: u, UserCode: event.UserCode, ExpiresIn: event.ExpiresIn})
					case "authorization_complete":
						if event.OpenID == "" || event.Scope == nil || event.Granted == nil || event.Requested == nil || event.Missing == nil {
							outErr = ErrProtocol
							cancel()
							continue
						}
						s.result.UserOpenID = event.OpenID
						s.result.Scope = *event.Scope
						s.result.Granted = *event.Granted
						s.result.Requested = *event.Requested
						s.result.Missing = *event.Missing
						if event.ExpiresAt != "" {
							s.result.ExpiresAt, err = time.Parse(time.RFC3339, event.ExpiresAt)
							if err != nil {
								outErr = ErrProtocol
								cancel()
							}
						}
					default:
						outErr = ErrProtocol
						cancel()
					}
				}
				if scanner.Err() != nil {
					outErr = ErrProtocol
					cancel()
				}
			}()
			wg.Wait()
			waitErr := cmd.Wait()
			if outErr != nil || errErr != nil {
				return ErrProtocol
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if waitErr != nil {
				return &Error{Type: "auth", Subtype: "exit"}
			}
			if enrollment {
				if s.result.AppID == "" {
					return ErrProtocol
				}
			} else if s.result.UserOpenID == "" || len(s.result.Missing) != 0 {
				return ErrIdentity
			}
			return nil
		})
	}()
	select {
	case event := <-ready:
		s.Initial = event
		return s, nil
	case <-s.done:
		select {
		case event := <-ready:
			s.Initial = event
			return s, nil
		default:
		}
		if s.err == nil {
			return nil, ErrProtocol
		}
		return nil, s.err
	case <-ctx.Done():
		cancel()
		<-s.done
		return nil, ctx.Err()
	}
}
