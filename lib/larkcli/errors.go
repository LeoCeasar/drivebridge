package larkcli

import (
	"errors"
	"time"
)

var (
	ErrProtocol              = errors.New("CLI protocol error")
	ErrMetadataUnknown       = errors.New("metadata_unknown")
	ErrUnsupportedCapability = errors.New("unsupported_capability")
	ErrUnknownResult         = errors.New("unknown_result")
	ErrIdentity              = errors.New("identity mismatch")
	// ErrNotDispatched proves that no CLI child was started for this request.
	ErrNotDispatched = errors.New("CLI request rejected before dispatch")
)

// Error contains machine-readable fields only. Upstream messages and command
// arguments may contain credentials or document content and are never retained.
type Error struct {
	Type, Subtype, Code string
	Retryable           bool
	RetryAfter          time.Duration
	Cause               error
}

func (e *Error) Error() string {
	return "official CLI command failed"
}
func (e *Error) Unwrap() error { return e.Cause }
func protocol() error          { return ErrProtocol }

func mutationError(err error) error {
	if errors.Is(err, ErrNotDispatched) {
		return err
	}
	return errors.Join(ErrUnknownResult, err)
}
