// Package online captures safe, shared online diagnostics without dispatching
// requests or changing the business operation journal.
package online

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/onlinecooldown"
	"github.com/rclone/rclone/lib/larkcli"
)

type phaseError struct {
	err              error
	operation, phase string
}

func (e *phaseError) Error() string { return "online operation failed" }
func (e *phaseError) Unwrap() error { return e.err }

// AtPhase marks the boundary that failed while retaining all errors.Is facts.
// The outermost phase annotation takes precedence over Capture's fallbacks.
func AtPhase(err error, operation, phase string) error {
	if err == nil {
		return nil
	}
	return &phaseError{err: err, operation: operation, phase: phase}
}

// AtPhaseIfUnset preserves a more specific failed boundary supplied below.
func AtPhaseIfUnset(err error, operation, phase string) error {
	var prior *phaseError
	if errors.As(err, &prior) {
		return err
	}
	return AtPhase(err, operation, phase)
}

type cancelKey struct{}

// WithCancelOrigin is for trusted lifecycle boundaries, never client input.
// Unknown values are deliberately reduced to an unspecified cancellation.
func WithCancelOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, cancelKey{}, safeCancelOrigin(origin))
}

func safeCancelOrigin(origin string) string {
	switch origin {
	case "user_stop", "runtime_shutdown", "caller_gone", "phase_deadline", "parent_deadline", "unspecified_cancel":
	default:
		origin = "unspecified_cancel"
	}
	return origin
}

type cancellationCause struct{ origin string }

func (e cancellationCause) Error() string              { return "online lifecycle interrupted" }
func (e cancellationCause) CancellationOrigin() string { return e.origin }
func (e cancellationCause) Unwrap() error {
	if e.origin == "phase_deadline" || e.origin == "parent_deadline" {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

// CancellationCause is for trusted lifecycle owners invoking CancelCauseFunc.
// Its marker survives context.Cause without storing arbitrary caller text.
func CancellationCause(origin string) error { return cancellationCause{safeCancelOrigin(origin)} }

// CancellationOrigin resolves the boundary that actually ended a request.
// A live parent's intent tag cannot override a child transport marker.
func CancellationOrigin(ctx context.Context, err error) string {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	var marker interface{ CancellationOrigin() string }
	if ctx.Err() != nil {
		if errors.As(context.Cause(ctx), &marker) {
			return safeCancelOrigin(marker.CancellationOrigin())
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "parent_deadline"
		}
		if origin, ok := ctx.Value(cancelKey{}).(string); ok {
			return safeCancelOrigin(origin)
		}
	}
	if errors.As(err, &marker) {
		return safeCancelOrigin(marker.CancellationOrigin())
	}
	return "unspecified_cancel"
}

type retryAtError interface{ RetryAt() time.Time }

// Cause classifies only typed evidence and exact, verified protocol values.
// An unrecognized error never becomes a network error by default.
func Cause(err error) string {
	apis := cliErrors(err)
	switch {
	case errors.Is(err, model.ErrIdentityChanged), errors.Is(err, larkcli.ErrIdentity):
		return "identity_changed"
	case errors.Is(err, larkcli.ErrCredentialStorage):
		return "credential_unavailable"
	case errors.Is(err, larkcli.ErrAuthRequired):
		for _, api := range apis {
			if cliCause(api) == "scope_missing" {
				return "scope_missing"
			}
		}
		return "auth_required"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "insufficient_space"
	case errors.Is(err, syscall.EIO):
		return "io_error"
	case errors.Is(err, onlinecooldown.ErrState), errors.Is(err, onlinecooldown.ErrInvalid):
		return "local_state_corrupt"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, model.ErrUnsupportedContent), errors.Is(err, larkcli.ErrUnsupportedCapability):
		return "unsupported_content"
	case errors.Is(err, model.ErrGenerationChanged):
		return "generation_changed"
	case errors.Is(err, model.ErrVersionChanged):
		return "version_changed"
	}
	if cause := nativeCause(err); cause != "" {
		return cause
	}
	for _, api := range apis {
		if api.RetryAfter < 0 {
			return "protocol_error"
		}
	}
	var wait retryAtError
	if errors.As(err, &wait) {
		return "rate_limited"
	}
	// errors.Join can contain several structured failures. A generic API
	// envelope must not hide a specific authentication or credential failure.
	for _, cause := range []string{"credential_unavailable", "scope_missing", "auth_required", "permission_denied", "rate_limited", "quota_exceeded", "service_unavailable"} {
		for _, api := range apis {
			if cliCause(api) == cause {
				return cause
			}
		}
	}
	var cert *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	if errors.As(err, &cert) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &record) {
		return "tls_error"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "request_timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "connection_error"
	}
	if errors.Is(err, larkcli.ErrProtocol) {
		return "protocol_error"
	}
	for _, cause := range []string{"transport_error", "api_error_unknown"} {
		for _, api := range apis {
			if cliCause(api) == cause {
				return cause
			}
		}
	}
	if errors.Is(err, larkcli.ErrVerification) {
		return "verification_unavailable"
	}
	return "unclassified_error"
}

func cliCause(api *larkcli.Error) string {
	// These exact symbolic values are bridge/CLI compatibility enums, never
	// arbitrary upstream messages. APICode persistence remains numeric only.
	switch api.Code {
	case "permission_denied", "auth_required", "credential_unavailable", "rate_limited", "scope_missing", "quota_exceeded", "service_unavailable":
		return api.Code
	}
	switch {
	case api.Subtype == "storage":
		return "credential_unavailable"
	case api.Code == "99991672":
		return "scope_missing"
	case api.Type == "authentication" || api.Type == "authorization":
		return "auth_required"
	case api.Code == "99991400" || api.Subtype == "rate_limit":
		return "rate_limited"
	case api.Type == "network" || api.Type == "transport":
		return "transport_error"
	case api.Type == "api":
		return "api_error_unknown"
	}
	return "unclassified_error"
}

func cliErrors(err error) []*larkcli.Error {
	var found []*larkcli.Error
	walkErrors(err, func(err error) {
		if api, ok := err.(*larkcli.Error); ok && api != nil {
			found = append(found, api)
		}
	})
	return found
}

func walkErrors(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			walkErrors(child, visit)
		}
	case interface{ Unwrap() error }:
		walkErrors(e.Unwrap(), visit)
	}
}

// Capture records independent cause and outcome facts, applying only stricter
// action suggestions when an unknown write is present. It never grants replay.
func Capture(ctx context.Context, err error, operation, phase, outcome string, at time.Time) model.OnlineFailure {
	var marked *phaseError
	if errors.As(err, &marked) {
		if marked.operation != "" {
			operation = marked.operation
		}
		phase = marked.phase
	}
	if _, ok := operations[operation]; !ok {
		operation = "unknown"
	}
	if _, ok := phases[phase]; !ok {
		phase = "unknown"
	}
	switch outcome {
	case "not_dispatched", "dispatched_unconfirmed", "rejected_verified", "completed_verified":
	default:
		outcome = "unknown"
	}
	// Unknown write evidence is monotonic. Even a later preflight failure does
	// not prove an earlier write was never dispatched.
	if errors.Is(err, model.ErrUnknownResult) || errors.Is(err, larkcli.ErrUnknownResult) {
		outcome = "dispatched_unconfirmed"
	}
	if outcome == "unknown" && errors.Is(err, larkcli.ErrNotDispatched) {
		outcome = "not_dispatched"
	}
	cause := Cause(err)
	f := model.OnlineFailure{SchemaVersion: 1, ID: uuid.NewString(), Operation: operation, Phase: phase, Cause: cause, Outcome: outcome, Action: action(cause, outcome), At: at.UTC()}
	apis := cliErrors(err)
	// Prefer the envelope explaining the cause over an unrelated joined error.
	for _, api := range apis {
		if numericCode(api.Code) && (f.APICode == "" || cliCause(api) == cause) {
			f.APICode = api.Code
			if cliCause(api) == cause {
				break
			}
		}
	}
	if cause == "rate_limited" {
		for _, api := range apis {
			if cliCause(api) == "rate_limited" {
				f.RetryAfterSeconds = max(f.RetryAfterSeconds, ceilSeconds(api.RetryAfter))
			}
		}
		walkErrors(err, func(err error) {
			if wait, ok := err.(retryAtError); ok {
				f.RetryAfterSeconds = max(f.RetryAfterSeconds, ceilSeconds(wait.RetryAt().Sub(at)))
			}
		})
	}
	if cause == "canceled" || cause == "request_timeout" && errors.Is(err, context.DeadlineExceeded) {
		f.CancelOrigin = CancellationOrigin(ctx, err)
		if cause == "canceled" && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			f.Cause = "request_timeout"
			f.Action = action(f.Cause, outcome)
		}
	}
	return f
}

func numericCode(code string) bool {
	if len(code) == 0 || len(code) > 20 {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	seconds := int64(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	return seconds
}
func action(cause, outcome string) string {
	switch cause {
	case "auth_required", "credential_unavailable":
		return "wait_auth"
	case "identity_changed":
		return "review_conflict"
	case "permission_denied", "scope_missing", "quota_exceeded":
		return "check_permissions"
	case "insufficient_space", "io_error", "local_state_corrupt":
		return "repair_local"
	case "protocol_error", "api_error_unknown", "unclassified_error", "tls_error", "unsupported_content":
		return "inspect_diagnostic"
	case "canceled":
		return "observe_only"
	}
	if outcome == "dispatched_unconfirmed" {
		return "observe_only"
	}
	if cause == "generation_changed" || cause == "version_changed" {
		return "replan"
	}
	return "wait_backoff"
}
