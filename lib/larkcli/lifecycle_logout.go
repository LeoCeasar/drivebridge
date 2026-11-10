package larkcli

import (
	"context"
	"errors"
)

// Logout clears the CLI root's selected user configuration and asks the CLI to
// revoke/remove its credentials. The CLI's remote revocation is best effort;
// successful local logout is not proof that every server credential was revoked.
// The product service must guard every affected AppID reference before calling.
func (r *Runner) Logout(ctx context.Context) error {
	if r == nil || !r.managed || r.identity.AppID == "" || r.identity.UserOpenID == "" {
		return ErrIdentity
	}
	var out struct {
		OK        *bool  `json:"ok"`
		LoggedOut *bool  `json:"loggedOut"`
		Reason    string `json:"reason"`
	}
	if err := r.lifecycleJSON(ctx, []string{"auth", "logout", "--json"}, &out); err != nil {
		return errors.Join(ErrUnknownResult, err)
	}
	if out.OK == nil || !*out.OK || out.LoggedOut == nil {
		return ErrUnknownResult
	}
	if !*out.LoggedOut && out.Reason != "not_logged_in" && out.Reason != "not_configured" {
		return ErrUnknownResult
	}
	return nil
}
