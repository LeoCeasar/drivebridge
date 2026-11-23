package online_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/internal/bridge/onlinecooldown"
	"github.com/rclone/rclone/lib/larkcli"
)

// Removing specific typed classifications must not turn known causes into a
// generic network error, including when wrapped or joined with write evidence.
func TestCauseUsesEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"auth", larkcli.ErrAuthRequired, "auth_required"},
		{"credential", larkcli.ErrCredentialStorage, "credential_unavailable"},
		{"identity", model.ErrIdentityChanged, "identity_changed"},
		{"cli identity", larkcli.ErrIdentity, "identity_changed"},
		{"verification unavailable", larkcli.ErrVerification, "verification_unavailable"},
		{"permission", &os.PathError{Op: "open", Path: "SECRET", Err: os.ErrPermission}, "permission_denied"},
		{"space", syscall.ENOSPC, "insufficient_space"},
		{"quota", syscall.EDQUOT, "insufficient_space"},
		{"io", syscall.EIO, "io_error"},
		{"deadline", context.DeadlineExceeded, "request_timeout"},
		{"cancel", context.Canceled, "canceled"},
		{"dns", &net.DNSError{Err: "SECRET", Name: "SECRET"}, "dns_error"},
		{"connect", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "connection_error"},
		{"timeout", &net.DNSError{IsTimeout: true}, "request_timeout"},
		{"tls", &tls.CertificateVerificationError{Err: errors.New("SECRET")}, "tls_error"},
		{"protocol", larkcli.ErrProtocol, "protocol_error"},
		{"unknown result", model.ErrUnknownResult, "unclassified_error"},
		{"auth with unknown", errors.Join(model.ErrUnknownResult, larkcli.ErrAuthRequired), "auth_required"},
		{"unsupported", model.ErrUnsupportedContent, "unsupported_content"},
		{"unknown", errors.New("network SECRET"), "unclassified_error"},
		{"bare EOF", io.EOF, "unclassified_error"},
		{"nil", nil, "unclassified_error"},
		{"verified rate code", &larkcli.Error{Type: "api", Code: "99991400"}, "rate_limited"},
		{"verified scope code", &larkcli.Error{Type: "api", Code: "99991672"}, "scope_missing"},
		{"typed authorization retains scope", errors.Join(larkcli.ErrAuthRequired, &larkcli.Error{Type: "authorization", Code: "99991672"}), "scope_missing"},
		{"429 not HTTP", &larkcli.Error{Type: "api", Code: "429"}, "api_error_unknown"},
		{"unknown API", &larkcli.Error{Type: "api", Code: "SECRET"}, "api_error_unknown"},
		{"exact subtype", &larkcli.Error{Type: "api", Subtype: "rate_limit"}, "rate_limited"},
		{"no substring match", &larkcli.Error{Type: "SECRET_auth_permission_rate", Subtype: "SECRET_scope"}, "unclassified_error"},
		{"symbolic permission", &larkcli.Error{Code: "permission_denied"}, "permission_denied"},
		{"symbolic auth", &larkcli.Error{Code: "auth_required"}, "auth_required"},
		{"symbolic credential", &larkcli.Error{Code: "credential_unavailable"}, "credential_unavailable"},
		{"symbolic rate", &larkcli.Error{Code: "rate_limited"}, "rate_limited"},
		{"symbolic scope", &larkcli.Error{Code: "scope_missing"}, "scope_missing"},
		{"symbolic quota", &larkcli.Error{Code: "quota_exceeded"}, "quota_exceeded"},
		{"symbolic service", &larkcli.Error{Code: "service_unavailable"}, "service_unavailable"},
		{"authentication type", &larkcli.Error{Type: "authentication", Subtype: "user_auth_required"}, "auth_required"},
		{"joined typed authentication", errors.Join(&larkcli.Error{Type: "api", Code: "123"}, &larkcli.Error{Type: "authentication", Subtype: "user_auth_required"}), "auth_required"},
		{"credential subtype", &larkcli.Error{Type: "authentication", Subtype: "storage"}, "credential_unavailable"},
		{"network type", &larkcli.Error{Type: "network", Subtype: "transport"}, "transport_error"},
		{"typed verification retains network", errors.Join(larkcli.ErrVerification, &larkcli.Error{Type: "network", Subtype: "transport"}), "transport_error"},
		{"bad retry duration", &larkcli.Error{Type: "api", Subtype: "rate_limit", RetryAfter: -time.Second}, "protocol_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := online.Cause(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
				t.Fatalf("cause=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestCapturePreservesUnknownWriteAndSafeFacts(t *testing.T) {
	at := time.Date(2026, 11, 19, 1, 2, 3, 0, time.FixedZone("test", 8*3600))
	err := online.AtPhase(errors.Join(model.ErrUnknownResult, larkcli.ErrAuthRequired), "create", "list_parent")
	got := online.Capture(context.Background(), err, "update", "write_dispatch", "not_dispatched", at)
	if got.Cause != "auth_required" || got.Outcome != "dispatched_unconfirmed" || got.Action != "wait_auth" || got.Operation != "create" || got.Phase != "list_parent" {
		t.Fatalf("capture=%+v", got)
	}
	if got.ID == "" || got.SchemaVersion != 1 || !got.At.Equal(at) {
		t.Fatalf("identity/time=%+v", got)
	}
	if second := online.Capture(context.Background(), err, "create", "readback", "", at); second.ID == got.ID {
		t.Fatal("diagnostic IDs repeated")
	}
	if !errors.Is(err, model.ErrUnknownResult) || !errors.Is(err, larkcli.ErrAuthRequired) {
		t.Fatal("phase wrapper lost error chain")
	}
	if online.AtPhase(nil, "create", "readback") != nil {
		t.Fatal("nil phase error")
	}
	for _, e := range []error{model.ErrUnknownResult, larkcli.ErrUnknownResult, errors.Join(larkcli.ErrNotDispatched, model.ErrUnknownResult)} {
		if got := online.Capture(context.Background(), e, "create", "readback", "completed_verified", at); got.Outcome != "dispatched_unconfirmed" {
			t.Fatalf("unknown lost: %+v", got)
		}
	}
	got = online.Capture(context.Background(), errors.Join(larkcli.ErrNotDispatched, larkcli.ErrAuthRequired), "create", "identity_check", "dispatched_unconfirmed", at)
	if got.Outcome != "dispatched_unconfirmed" {
		t.Fatalf("supplied unknown lost: %+v", got)
	}
	got = online.Capture(context.Background(), larkcli.ErrNotDispatched, "create", "identity_check", "", at)
	if got.Outcome != "not_dispatched" {
		t.Fatalf("start proof not captured: %+v", got)
	}
}

func TestCaptureRedactsUntrustedMachineFields(t *testing.T) {
	for _, code := range []string{"SECRET", "123456789012345678901", "-429", "429\nSECRET", " 429", "1e3"} {
		err := &larkcli.Error{Type: "SECRET", Subtype: "SECRET", Code: code, Cause: errors.New("SECRET")}
		got := online.Capture(context.Background(), err, "SECRET", "SECRET", "SECRET", time.Now())
		if got.APICode != "" || got.Operation != "unknown" || got.Phase != "unknown" || got.Outcome != "unknown" {
			t.Fatalf("unsafe fields: %+v", got)
		}
		b, _ := json.Marshal(model.OnlineRecord{First: &got, Last: &got, History: []model.OnlineFailure{got}})
		if strings.Contains(string(b), "SECRET") {
			t.Fatalf("raw text leaked: %s", b)
		}
	}
	for _, code := range []string{"0", "429", "12345678901234567890"} {
		got := online.Capture(context.Background(), &larkcli.Error{Type: "api", Code: code}, "sync", "readback", "rejected_verified", time.Now())
		if got.APICode != code {
			t.Fatalf("numeric code lost: %+v", got)
		}
	}
}

type waitError struct{ at time.Time }

func (w waitError) Error() string      { return "SECRET" }
func (w waitError) RetryAt() time.Time { return w.at }

func TestCaptureRateDelayUsesOnlyRealBounds(t *testing.T) {
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		err     error
		cause   string
		seconds int64
	}{
		{&larkcli.Error{Type: "api", Subtype: "rate_limit", RetryAfter: 8 * 24 * time.Hour}, "rate_limited", 691200},
		{&larkcli.Error{Type: "api", Subtype: "rate_limit", RetryAfter: 1500 * time.Millisecond}, "rate_limited", 2},
		{&larkcli.Error{Type: "api", Subtype: "rate_limit", RetryAfter: time.Duration(math.MaxInt64)}, "rate_limited", 9223372037},
		{&larkcli.Error{Type: "api", Subtype: "rate_limit", RetryAfter: -time.Second}, "protocol_error", 0},
		{&larkcli.Error{Type: "api", Code: "429", RetryAfter: 3 * time.Second}, "api_error_unknown", 0},
		{errors.Join(&larkcli.Error{Type: "api", Code: "99991400", RetryAfter: 3 * time.Second}, &larkcli.Error{Type: "api", Code: "99991400", RetryAfter: 8 * time.Second}), "rate_limited", 8},
		{waitError{at.Add(1500 * time.Millisecond)}, "rate_limited", 2},
		{waitError{at.Add(-time.Second)}, "rate_limited", 0},
		{waitError{at}, "rate_limited", 0},
	} {
		got := online.Capture(context.Background(), tc.err, "sync", "readback", "", at)
		if got.Cause != tc.cause || got.RetryAfterSeconds != tc.seconds {
			t.Fatalf("capture=%+v want=%s/%d", got, tc.cause, tc.seconds)
		}
	}
}

func TestOuterPhaseRemainsTheFailedBoundary(t *testing.T) {
	err := online.AtPhase(online.AtPhase(larkcli.ErrAuthRequired, "sync", "identity_check"), "create", "readback")
	got := online.Capture(context.Background(), err, "update", "write_dispatch", "dispatched_unconfirmed", time.Now())
	if got.Operation != "create" || got.Phase != "readback" || !errors.Is(err, larkcli.ErrAuthRequired) {
		t.Fatalf("phase capture=%+v", got)
	}
}

func TestCancellationProvenanceMustComeFromTrustedContext(t *testing.T) {
	for _, origin := range []string{"user_stop", "runtime_shutdown", "caller_gone", "phase_deadline", "parent_deadline", "SECRET", ""} {
		ctx, cancel := context.WithCancel(online.WithCancelOrigin(context.Background(), origin))
		cancel()
		got := online.Capture(ctx, context.Canceled, "sync", "readback", "", time.Now())
		want := origin
		if origin == "SECRET" || origin == "" {
			want = "unspecified_cancel"
		}
		if got.CancelOrigin != want {
			t.Fatalf("origin=%q got=%+v", origin, got)
		}
		if got := online.Capture(ctx, errors.New("user_stop SECRET"), "sync", "readback", "", time.Now()); got.CancelOrigin != "" {
			t.Fatalf("non-cancel provenance invented: %+v", got)
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("user_stop"))
	if got := online.Capture(ctx, context.Canceled, "sync", "readback", "", time.Now()); got.CancelOrigin != "unspecified_cancel" {
		t.Fatalf("inferred untrusted cause: %+v", got)
	}
}

type cancellationError struct{ origin string }

func (e cancellationError) Error() string              { return "SECRET" }
func (e cancellationError) Unwrap() error              { return context.Canceled }
func (e cancellationError) CancellationOrigin() string { return e.origin }

func TestTrustedTransportCancellationMarker(t *testing.T) {
	canceledParent, cancel := context.WithCancel(online.WithCancelOrigin(context.Background(), "user_stop"))
	cancel()
	for _, tc := range []struct {
		ctx          context.Context
		origin, want string
	}{
		{context.Background(), "phase_deadline", "phase_deadline"},
		{context.Background(), "runtime_shutdown", "runtime_shutdown"},
		{context.Background(), "SECRET", "unspecified_cancel"},
		{canceledParent, "caller_gone", "user_stop"},
		{online.WithCancelOrigin(context.Background(), "user_stop"), "phase_deadline", "phase_deadline"},
	} {
		got := online.Capture(tc.ctx, fmt.Errorf("wrapped: %w", cancellationError{tc.origin}), "sync", "readback", "", time.Now())
		if got.CancelOrigin != tc.want {
			t.Fatalf("cancel=%+v want origin=%s", got, tc.want)
		}
	}
}

type deadlineError struct{}

func (deadlineError) Error() string              { return "SECRET" }
func (deadlineError) Unwrap() error              { return context.DeadlineExceeded }
func (deadlineError) CancellationOrigin() string { return "phase_deadline" }

func TestLiveParentTagDoesNotInventCancellationProvenance(t *testing.T) {
	ctx := online.WithCancelOrigin(context.Background(), "user_stop")
	got := online.Capture(ctx, context.Canceled, "sync", "readback", "", time.Now())
	if got.CancelOrigin != "unspecified_cancel" {
		t.Fatalf("live parent was reported canceled: %+v", got)
	}
}

func TestChildDeadlineMarkerPreservesTimeoutCause(t *testing.T) {
	ctx := online.WithCancelOrigin(context.Background(), "user_stop")
	got := online.Capture(ctx, fmt.Errorf("wrapped: %w", deadlineError{}), "sync", "readback", "", time.Now())
	if got.Cause != "request_timeout" || got.CancelOrigin != "phase_deadline" {
		t.Fatalf("child timeout provenance lost: %+v", got)
	}
}

func TestActualContextCancellationCauseProvidesLifecycleProvenance(t *testing.T) {
	ctx, cancel := context.WithCancelCause(online.WithCancelOrigin(context.Background(), "caller_gone"))
	cancel(cancellationError{"runtime_shutdown"})
	got := online.Capture(ctx, ctx.Err(), "sync", "readback", "", time.Now())
	if got.CancelOrigin != "runtime_shutdown" {
		t.Fatalf("actual cancellation cause lost: %+v", got)
	}
	ctxDeadline, cancelDeadline := context.WithDeadline(online.WithCancelOrigin(context.Background(), "user_stop"), time.Now().Add(-time.Second))
	defer cancelDeadline()
	got = online.Capture(ctxDeadline, ctxDeadline.Err(), "sync", "readback", "", time.Now())
	if got.Cause != "request_timeout" || got.CancelOrigin != "parent_deadline" {
		t.Fatalf("deadline inferred user stop: %+v", got)
	}
}

func TestBrokenCooldownStateRequiresLocalRepair(t *testing.T) {
	for _, err := range []error{onlinecooldown.ErrState, onlinecooldown.ErrInvalid} {
		f := online.Capture(context.Background(), fmt.Errorf("wrapped: %w", err), "sync", "identity_check", "not_dispatched", time.Now())
		if f.Cause != "local_state_corrupt" || f.Action != "repair_local" {
			t.Fatalf("cooldown failure=%+v", f)
		}
	}
}

func TestSafeLabelsAndUnknownOutcomeConstrainAction(t *testing.T) {
	for _, fn := range []func(string) string{online.Message, online.PhaseLabel, online.ActionLabel} {
		if s := fn("SECRET"); s == "" || strings.Contains(s, "SECRET") {
			t.Fatalf("unsafe unknown label %q", s)
		}
	}
	for _, tc := range []struct {
		err    error
		action string
	}{
		{&net.DNSError{}, "observe_only"}, {larkcli.ErrAuthRequired, "wait_auth"}, {model.ErrIdentityChanged, "review_conflict"}, {os.ErrPermission, "check_permissions"}, {syscall.ENOSPC, "repair_local"}, {larkcli.ErrProtocol, "inspect_diagnostic"},
	} {
		got := online.Capture(context.Background(), errors.Join(model.ErrUnknownResult, tc.err), "create", "readback", "", time.Now())
		if got.Action != tc.action {
			t.Fatalf("unsafe action: %+v", got)
		}
	}
}

func TestChangedGenerationAndVersionRequireReplanningWithoutLosingUnknownWrite(t *testing.T) {
	for _, tc := range []struct {
		err   error
		cause string
		label string
	}{
		{model.ErrGenerationChanged, "generation_changed", "本机"},
		{model.ErrVersionChanged, "version_changed", "版本"},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", tc.err)
			if cause := online.Cause(err); cause != tc.cause {
				t.Fatalf("cause=%s want=%s", cause, tc.cause)
			}
			got := online.Capture(context.Background(), err, "update", "merge_plan", "not_dispatched", time.Now())
			if got.Action != "replan" {
				t.Fatalf("changed input not replanned: %+v", got)
			}
			if label := online.Message(got.Cause); !strings.Contains(label, tc.label) {
				t.Fatalf("unhelpful changed-input label: %s", label)
			}
			got = online.Capture(context.Background(), errors.Join(model.ErrUnknownResult, err), "update", "readback", "not_dispatched", time.Now())
			if got.Cause != tc.cause || got.Outcome != "dispatched_unconfirmed" || got.Action != "observe_only" {
				t.Fatalf("changed input lost unknown-write constraint: %+v", got)
			}
		})
	}
}
