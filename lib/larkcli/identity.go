package larkcli

import (
	"bytes"
	"context"
	"errors"
	"time"
)

// ConfigMetadata deliberately cannot retain masked/unmasked appSecret or users prose.
type ConfigMetadata struct{ AppID, Brand string }
type IdentityMetadata struct {
	Identity
	Verified  bool
	Scope     string
	ExpiresAt time.Time
}

func (r *Runner) lifecycleJSON(ctx context.Context, args []string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, businessTimeout)
	defer cancel()
	started := false
	err := r.locked(ctx, r.managed && r.identity.AppID == "", func() error {
		cmd := r.command(ctx, args, "")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		var err error
		started, err = runBusinessCommand(cmd)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			e, found, _ := stderrResponse(stderr.Bytes())
			if found && e.Error != nil {
				return e.Error.safe()
			}
			return &Error{Type: "lifecycle", Subtype: "exit"}
		}
		return singleJSON(stdout.Bytes(), out)
	})
	if err != nil && !started {
		return errors.Join(ErrNotDispatched, err)
	}
	return err
}
func (r *Runner) InspectConfig(ctx context.Context) (ConfigMetadata, error) {
	var raw struct {
		AppID string `json:"appId"`
		Brand string `json:"brand"`
	}
	if err := r.lifecycleJSON(ctx, []string{"config", "show"}, &raw); err != nil {
		return ConfigMetadata{}, err
	}
	if raw.AppID == "" || (raw.Brand != "feishu" && raw.Brand != "lark") {
		return ConfigMetadata{}, ErrProtocol
	}
	if r.identity.AppID != "" && r.identity.AppID != raw.AppID {
		return ConfigMetadata{}, ErrIdentity
	}
	return ConfigMetadata{raw.AppID, raw.Brand}, nil
}
func (r *Runner) VerifyIdentity(ctx context.Context) (IdentityMetadata, error) {
	if r.managed && r.identity.AppID == "" {
		return IdentityMetadata{}, ErrIdentity
	}
	var raw struct {
		AppID      string `json:"appId"`
		Brand      string `json:"brand"`
		Identities *struct {
			User *struct {
				OpenID    string `json:"openId"`
				Verified  *bool  `json:"verified"`
				Available bool   `json:"available"`
				Scope     string `json:"scope"`
				ExpiresAt string `json:"expiresAt"`
			} `json:"user"`
		} `json:"identities"`
	}
	if err := r.lifecycleJSON(ctx, []string{"auth", "status", "--verify", "--json"}, &raw); err != nil {
		return IdentityMetadata{}, err
	}
	if raw.Identities == nil || raw.Identities.User == nil {
		return IdentityMetadata{}, ErrProtocol
	}
	u := raw.Identities.User
	if raw.AppID == "" || (raw.Brand != "feishu" && raw.Brand != "lark") || u.OpenID == "" || u.Verified == nil || !*u.Verified || !u.Available {
		return IdentityMetadata{}, ErrIdentity
	}
	if (r.identity.AppID != "" && r.identity.AppID != raw.AppID) || (r.identity.UserOpenID != "" && r.identity.UserOpenID != u.OpenID) || (r.identity.Brand != "" && r.identity.Brand != raw.Brand) {
		return IdentityMetadata{}, ErrIdentity
	}
	out := IdentityMetadata{Identity: Identity{AppID: raw.AppID, Brand: raw.Brand, UserOpenID: u.OpenID}, Verified: true, Scope: u.Scope}
	if u.ExpiresAt != "" {
		var err error
		out.ExpiresAt, err = time.Parse(time.RFC3339, u.ExpiresAt)
		if err != nil {
			return IdentityMetadata{}, ErrProtocol
		}
	}
	return out, nil
}
