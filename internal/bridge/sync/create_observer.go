package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/lib/larkcli"
)

// ReadOnlyCreationSource deliberately grants no capability to create, adopt,
// rename, or otherwise mutate a cloud resource. List must return a complete
// parent listing; a failed or partial listing is never absence evidence.
type ReadOnlyCreationSource interface {
	FetchDoc(context.Context, string, string) (cloud.Document, error)
	ObserveFile(context.Context, string) (ObservedFile, error)
	List(context.Context, string) ([]larkcli.DriveItem, error)
}

// CreationObservation is evidence for the journal's normal executor. Verified
// proves captured content, and does not authorize any remaining cloud write.
type CreationObservation struct {
	Token, Revision, Name, Reason, Phase string
	Verified                             bool
}

// ObserveCreation only observes the original operation. It neither binds a
// candidate nor finishes the journal, even when its content and name match.
func ObserveCreation(ctx context.Context, source ReadOnlyCreationSource, op model.CreateOp, input []byte) (CreationObservation, error) {
	out := CreationObservation{Phase: "list_parent"}
	fail := func(phase string, err error) (CreationObservation, error) {
		out.Phase = phase
		return out, online.AtPhase(creationReadResult(ctx, err), "create", phase)
	}
	if err := ctx.Err(); err != nil {
		return fail("list_parent", err)
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return fail("list_parent", &creationReadCancellation{error: context.DeadlineExceeded, origin: "parent_deadline"})
	}
	if op.ParentToken == "" || op.TemporaryName == "" || (op.Kind != model.KindDocx && op.Kind != model.KindFile) || op.Token != "" && op.CandidateToken != "" && op.Token != op.CandidateToken {
		return fail("list_parent", larkcli.ErrProtocol)
	}
	stable := op.Token
	out.Revision = op.Revision
	if stable == "" {
		stable = op.CandidateToken
		out.Revision = op.CandidateRevision
	}
	out.Token = stable
	readCtx, cancelRead := context.WithTimeoutCause(ctx, 30*time.Second, errCreationReadBudget)
	items, err := source.List(readCtx, op.ParentToken)
	err = creationReadResult(readCtx, err)
	cancelRead()
	if err != nil {
		return fail("list_parent", err)
	}
	if err = ctx.Err(); err != nil {
		return fail("list_parent", err)
	}
	seen := make(map[string]bool, len(items))
	// Validate the entire listing before deriving any candidate or absence fact.
	for _, item := range items {
		if item.Token == "" || item.Name == "" || item.Type == "" || seen[item.Token] || item.ParentToken != "" && item.ParentToken != op.ParentToken {
			return fail("list_parent", larkcli.ErrProtocol)
		}
		seen[item.Token] = true
	}
	count := 0
	for _, item := range items {
		if stable != "" {
			if item.Token != stable {
				continue
			}
			out.Name = item.Name
			if item.Type != string(op.Kind) || item.Name != op.TemporaryName && item.Name != op.FinalName {
				out.Reason = "location_changed"
				return fail("namespace_verify", model.ErrUnknownResult)
			}
		} else if item.Name != op.TemporaryName || item.Type != string(op.Kind) {
			continue
		}
		count++
		out.Token = item.Token
		out.Name = item.Name
	}
	if count != 1 {
		out.Reason = "not_found"
		if stable != "" {
			out.Reason = "location_changed"
		}
		if count > 1 {
			out.Reason = "multiple_matches"
			out.Token = ""
			out.Name = ""
		}
		return fail("namespace_verify", model.ErrUnknownResult)
	}
	if err = ctx.Err(); err != nil {
		phase := "document_read"
		if op.Kind == model.KindFile {
			phase = "readback"
		}
		return fail(phase, err)
	}
	if op.Kind == model.KindFile {
		readCtx, cancelRead = context.WithTimeoutCause(ctx, 30*time.Second, errCreationReadBudget)
		err = observeCreationFile(readCtx, source, op, out.Token)
		err = creationReadResult(readCtx, err)
		cancelRead()
		if err != nil {
			if errors.Is(err, errCreationContentMismatch) {
				out.Reason = "content_mismatch"
			}
			return fail("readback", err)
		}
	} else {
		readCtx, cancelRead = context.WithTimeoutCause(ctx, 30*time.Second, errCreationReadBudget)
		d, readErr := source.FetchDoc(readCtx, out.Token, "")
		readErr = creationReadResult(readCtx, readErr)
		cancelRead()
		if d.Token == out.Token && d.Revision != "" {
			out.Revision = d.Revision
		}
		if readErr != nil {
			return fail("document_read", readErr)
		}
		if err = ctx.Err(); err != nil {
			return fail("document_read", err)
		}
		if d.Token != out.Token || d.Revision == "" {
			return fail("document_read", larkcli.ErrProtocol)
		}
		if len(d.Warnings) > 0 {
			return fail("conversion", model.ErrUnsupportedContent)
		}
		view, decodeErr := markdown.DecodeWithOptions(d.XML, d.Revision, markdown.Options{SourceURL: d.SourceURL, ReferenceMap: d.ReferenceMap})
		if decodeErr != nil {
			return fail("conversion", errors.Join(model.ErrUnsupportedContent, decodeErr))
		}
		if !markdown.MatchesEncodedIntent(input, view) {
			wanted, encodeErr := markdown.Encode(input)
			if encodeErr != nil {
				return fail("conversion", encodeErr)
			}
			actual, encodeErr := markdown.Encode(view.Markdown)
			if encodeErr != nil {
				return fail("conversion", encodeErr)
			}
			if !bytes.Equal(wanted, actual) {
				out.Reason = "content_mismatch"
				return fail("readback", errors.Join(model.ErrUnknownResult, errCreationContentMismatch))
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return fail("readback", err)
	}
	out.Phase = "readback"
	out.Verified = true
	return out, nil
}

// The store's blob IDs are lowercase SHA-256 digests. Compare a bounded stream
// against that immutable original, without materializing a large file in RAM.
func observeCreationFile(ctx context.Context, source ReadOnlyCreationSource, op model.CreateOp, token string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	o, err := source.ObserveFile(ctx, token)
	// A source may return an acquired reader with an acquisition error; its lease
	// still belongs to this call and must be released.
	if o.Reader != nil {
		defer func() { err = errors.Join(err, o.Reader.Close()) }()
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if o.Reader == nil || o.Token != token || o.Size < 0 || o.Identity.Validate() != nil {
		return larkcli.ErrProtocol
	}
	original, decodeErr := hex.DecodeString(string(op.Input))
	if decodeErr != nil || len(original) != sha256.Size || hex.EncodeToString(original) != string(op.Input) {
		return larkcli.ErrProtocol
	}
	reader := &creationContextReader{ctx: ctx, reader: o.Reader}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(reader, o.Size))
	if err != nil {
		return err
	}
	if n != o.Size {
		return larkcli.ErrProtocol
	}
	// Probe one extra byte rather than overflowing Size+1 at MaxInt64.
	var extra [1]byte
	nExtra, err := io.ReadFull(reader, extra[:])
	if err != nil && err != io.EOF {
		return err
	}
	if nExtra != 0 {
		return larkcli.ErrProtocol
	}
	digest := hash.Sum(nil)
	if hex.EncodeToString(digest) != o.Identity.SHA256 {
		return larkcli.ErrProtocol
	}
	if !bytes.Equal(digest, original) {
		return errors.Join(model.ErrUnknownResult, errCreationContentMismatch)
	}
	return ctx.Err()
}

type creationContextReader struct {
	ctx        context.Context
	reader     io.Reader
	emptyReads int
}

func (r *creationContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if n == 0 && err == nil {
		r.emptyReads++
		if r.emptyReads >= 100 {
			return 0, io.ErrNoProgress
		}
	} else {
		r.emptyReads = 0
	}
	return n, err
}

var errCreationReadBudget = errors.New("creation observation read budget elapsed")

type creationReadCancellation struct {
	error
	origin string
}

func (e *creationReadCancellation) Unwrap() error              { return e.error }
func (e *creationReadCancellation) CancellationOrigin() string { return e.origin }

// Only our own child timer proves phase expiry. An earlier parent cancellation
// retains its original context/error provenance; transport-owned deadlines also
// keep their own markers when the observer's child context remains live.
func creationReadResult(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	err = errors.Join(err, ctx.Err())
	if context.Cause(ctx) == errCreationReadBudget {
		return &creationReadCancellation{error: err, origin: "phase_deadline"}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &creationReadCancellation{error: err, origin: "parent_deadline"}
	}
	return err
}
