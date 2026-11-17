package syncer

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
	"testing"
)

func TestCredentialSyncClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{{larkcli.ErrAuthRequired, "auth_required"}, {larkcli.ErrCredentialStorage, "credential_unavailable"}, {larkcli.ErrVerification, "network_or_protocol"}, {model.ErrIdentityChanged, "identity_changed"}, {errors.Join(model.ErrUnknownResult, larkcli.ErrAuthRequired), "unknown_result"}} {
		if code := classify(tc.err); code != tc.code {
			t.Fatalf("%v: got %s want %s", tc.err, code, tc.code)
		}
	}
}
