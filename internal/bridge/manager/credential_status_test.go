package manager

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
	"testing"
)

func TestCredentialStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		code  string
		retry bool
	}{{larkcli.ErrAuthRequired, "unauthorized", false}, {larkcli.ErrCredentialStorage, "credential_unavailable", true}, {larkcli.ErrVerification, "network", true}, {model.ErrIdentityChanged, "identity_changed", false}, {context.DeadlineExceeded, "network", true}} {
		info := ErrorInfo(tc.err)
		if info.Code != tc.code || info.Retryable != tc.retry {
			t.Fatalf("%v: %+v", tc.err, info)
		}
	}
}
