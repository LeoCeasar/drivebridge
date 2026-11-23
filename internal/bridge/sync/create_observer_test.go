package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/lib/larkcli"
)

// This fixture intentionally exposes no mutating cloud capability. Adding a
// mutation to the observer interface would prevent this test file compiling.
type creationReadSource struct {
	items   []larkcli.DriveItem
	listErr error
	doc     cloud.Document
	docErr  error
	file    ObservedFile
	fileErr error
}

var _ ReadOnlyCreationSource = (*creationReadSource)(nil)

func (s *creationReadSource) List(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parent != "parent" {
		panic("wrong parent")
	}
	return s.items, s.listErr
}
func (s *creationReadSource) FetchDoc(ctx context.Context, token, revision string) (cloud.Document, error) {
	if err := ctx.Err(); err != nil {
		return cloud.Document{}, err
	}
	if token != "candidate" || revision != "" {
		panic("wrong document request")
	}
	return s.doc, s.docErr
}
func (s *creationReadSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	if err := ctx.Err(); err != nil {
		return ObservedFile{}, err
	}
	if token != "candidate" {
		panic("wrong file request")
	}
	return s.file, s.fileErr
}
func observerOp() model.CreateOp {
	return model.CreateOp{ID: "op", Kind: model.KindDocx, ParentToken: "parent", TemporaryName: "临时文档", FinalName: "最终文档", State: "unknown"}
}
func observerSource() *creationReadSource {
	return &creationReadSource{
		items: []larkcli.DriveItem{{Token: "candidate", Name: "临时文档", Type: "docx", ParentToken: "parent"}},
		doc:   cloud.Document{Token: "candidate", Revision: "7", XML: []byte(`<title id="doc">临时文档</title><p id="one">你好世界</p>`)},
	}
}

// Fails if the observer returns only a token without checking full document
// content, or mutates its captured input while decoding.
func TestObserveCreationReadOnlyUnicodeDocument(t *testing.T) {
	op := observerOp()
	before := op
	input := []byte("你好世界\n")
	got, err := ObserveCreation(context.Background(), observerSource(), op, input)
	if err != nil || !got.Verified || got.Token != "candidate" || got.Revision != "7" || got.Name != "临时文档" {
		t.Fatalf("observation=%+v err=%v", got, err)
	}
	if !reflect.DeepEqual(op, before) || string(input) != "你好世界\n" {
		t.Fatal("captured intent changed")
	}
}

// Fails if exact temporary-name lookup adopts aliases, final names, or another
// same-name token after stable candidate evidence has already been recorded.
func TestObserveCreationCandidateSelection(t *testing.T) {
	for _, tc := range []struct {
		name             string
		token, candidate string
		items            []larkcli.DriveItem
		reason           string
		verified         bool
	}{
		{name: "absent", reason: "not_found"},
		{name: "final name alone", items: []larkcli.DriveItem{{Token: "candidate", Name: "最终文档", Type: "docx"}}, reason: "not_found"},
		{name: "unicode alias", items: []larkcli.DriveItem{{Token: "candidate", Name: "临时文档 ", Type: "docx"}}, reason: "not_found"},
		{name: "wrong kind", items: []larkcli.DriveItem{{Token: "candidate", Name: "临时文档", Type: "file"}}, reason: "not_found"},
		{name: "ambiguous", items: []larkcli.DriveItem{{Token: "candidate", Name: "临时文档", Type: "docx"}, {Token: "other", Name: "临时文档", Type: "docx"}}, reason: "multiple_matches"},
		{name: "stable moved", candidate: "candidate", items: []larkcli.DriveItem{{Token: "other", Name: "临时文档", Type: "docx"}}, reason: "location_changed"},
		{name: "stable renamed", token: "candidate", items: []larkcli.DriveItem{{Token: "candidate", Name: "somewhere else", Type: "docx"}}, reason: "location_changed"},
		{name: "stable final", candidate: "candidate", items: []larkcli.DriveItem{{Token: "candidate", Name: "最终文档", Type: "docx"}}, verified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := observerSource()
			s.items = tc.items
			op := observerOp()
			op.Token = tc.token
			op.CandidateToken = tc.candidate
			got, err := ObserveCreation(context.Background(), s, op, []byte("你好世界\n"))
			if got.Reason != tc.reason || got.Verified != tc.verified {
				t.Fatalf("observation=%+v err=%v", got, err)
			}
			if tc.verified && err != nil || !tc.verified && !errors.Is(err, model.ErrUnknownResult) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// Fails if a matching first entry hides a malformed later entry or a partial
// listing is accepted as complete namespace evidence.
func TestObserveCreationRejectsIncompleteOrMalformedParent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		item    larkcli.DriveItem
		listErr error
	}{
		{name: "missing token", item: larkcli.DriveItem{Name: "other", Type: "file"}},
		{name: "missing name", item: larkcli.DriveItem{Token: "other", Type: "file"}},
		{name: "missing type", item: larkcli.DriveItem{Token: "other", Name: "other"}},
		{name: "duplicate token", item: larkcli.DriveItem{Token: "candidate", Name: "other", Type: "docx"}},
		{name: "wrong parent", item: larkcli.DriveItem{Token: "other", Name: "other", Type: "file", ParentToken: "elsewhere"}},
		{name: "partial", listErr: larkcli.ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := observerSource()
			if tc.item.Token != "" || tc.item.Name != "" {
				s.items = append(s.items, tc.item)
			}
			s.listErr = tc.listErr
			got, err := ObserveCreation(context.Background(), s, observerOp(), []byte("你好世界\n"))
			if got.Verified || got.Token != "" || got.Phase != "list_parent" || !errors.Is(err, larkcli.ErrProtocol) {
				t.Fatalf("observation=%+v err=%v", got, err)
			}
		})
	}
}

// Fails if acquisition errors are flattened into a generic reason or verified
// candidate identity is thrown away when later content acquisition fails.
func TestObserveCreationPreservesPhaseErrorsAndCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, phase                string
		listErr, docErr, errorWant error
		candidate                  string
	}{
		{name: "parent auth", phase: "list_parent", listErr: larkcli.ErrAuthRequired, errorWant: larkcli.ErrAuthRequired},
		{name: "document auth", phase: "document_read", docErr: larkcli.ErrAuthRequired, errorWant: larkcli.ErrAuthRequired, candidate: "candidate"},
		{name: "document canceled", phase: "document_read", docErr: context.Canceled, errorWant: context.Canceled, candidate: "candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := observerSource()
			s.listErr = tc.listErr
			s.docErr = tc.docErr
			got, err := ObserveCreation(context.Background(), s, observerOp(), []byte("你好世界\n"))
			if got.Verified || got.Token != tc.candidate || got.Phase != tc.phase || !errors.Is(err, tc.errorWant) {
				t.Fatalf("observation=%+v err=%v", got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := ObserveCreation(ctx, observerSource(), observerOp(), []byte("你好世界\n"))
	if got.Verified || !errors.Is(err, context.Canceled) {
		t.Fatalf("observation=%+v err=%v", got, err)
	}
}

// Fails if malformed metadata, warnings, or unreadable XML grants ownership,
// or mismatching content loses its candidate evidence.
func TestObserveCreationDocumentValidation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		change        func(*cloud.Document)
		phase, reason string
		want          error
	}{
		{name: "wrong token", change: func(d *cloud.Document) { d.Token = "other" }, phase: "document_read", want: larkcli.ErrProtocol},
		{name: "missing revision", change: func(d *cloud.Document) { d.Revision = "" }, phase: "document_read", want: larkcli.ErrProtocol},
		{name: "warnings", change: func(d *cloud.Document) { d.Warnings = []string{"unsupported"} }, phase: "conversion", want: model.ErrUnsupportedContent},
		{name: "invalid XML", change: func(d *cloud.Document) { d.XML = []byte("<p>") }, phase: "conversion", want: model.ErrUnsupportedContent},
		{name: "different body", change: func(d *cloud.Document) { d.XML = []byte(`<title id="doc">T</title><p id="one">changed</p>`) }, phase: "readback", reason: "content_mismatch", want: errCreationContentMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := observerSource()
			tc.change(&s.doc)
			got, err := ObserveCreation(context.Background(), s, observerOp(), []byte("你好世界\n"))
			if got.Verified || got.Token != "candidate" || got.Phase != tc.phase || got.Reason != tc.reason || !errors.Is(err, tc.want) {
				t.Fatalf("observation=%+v err=%v", got, err)
			}
		})
	}
}

type observerReader struct {
	io.Reader
	closeErr error
	closed   bool
}

func (r *observerReader) Close() error { r.closed = true; return r.closeErr }
func fileObserverSource(body string) (*creationReadSource, *observerReader, model.CreateOp) {
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	op := observerOp()
	op.Kind = model.KindFile
	op.Input = model.BlobID(digest)
	r := &observerReader{Reader: strings.NewReader(body)}
	s := observerSource()
	s.items[0].Type = "file"
	s.file = ObservedFile{Token: "candidate", Size: int64(len(body)), Reader: r, Identity: model.FileIdentity{Kind: model.FileIdentityServerVersion, Version: "v1", SHA256: digest, StartedAt: now, CompletedAt: now}}
	return s, r, op
}

// Fails if a matching filename or metadata digest replaces actual stream
// verification, or if download resources leak on any validation branch.
func TestObserveCreationFileValidation(t *testing.T) {
	closeFailure := errors.New("close failed")
	for _, tc := range []struct {
		name     string
		change   func(*creationReadSource, *observerReader, *model.CreateOp)
		want     error
		verified bool
	}{
		{name: "unicode valid", verified: true},
		{name: "different captured content", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) {
			op.Input = model.BlobID(strings.Repeat("a", 64))
		}, want: errCreationContentMismatch},
		{name: "wrong token", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) { s.file.Token = "other" }, want: larkcli.ErrProtocol},
		{name: "negative size", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) { s.file.Size = -1 }, want: larkcli.ErrProtocol},
		{name: "invalid identity", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) { s.file.Identity.Version = "" }, want: larkcli.ErrProtocol},
		{name: "short download", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) { s.file.Size++ }, want: larkcli.ErrProtocol},
		{name: "long download", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) { s.file.Size-- }, want: larkcli.ErrProtocol},
		{name: "wrong digest", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) {
			s.file.Identity.SHA256 = strings.Repeat("a", 64)
		}, want: larkcli.ErrProtocol},
		{name: "close failure", change: func(s *creationReadSource, r *observerReader, op *model.CreateOp) { r.closeErr = closeFailure }, want: closeFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, op := fileObserverSource("文件内容\n")
			if tc.change != nil {
				tc.change(s, r, &op)
			}
			got, err := ObserveCreation(context.Background(), s, op, nil)
			if got.Verified != tc.verified || got.Token != "candidate" || (!tc.verified && !errors.Is(err, tc.want)) || (tc.verified && err != nil) {
				t.Fatalf("observation=%+v err=%v", got, err)
			}
			if !r.closed {
				t.Fatal("download lease was not closed")
			}
		})
	}
}

// The source returns completed fixtures even after its context expires. A
// missed child deadline must not become successful candidate evidence.
type creationTimedSource struct {
	*creationReadSource
	before func(context.Context, string)
	calls  []string
}

func (s *creationTimedSource) List(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	s.calls = append(s.calls, "list_parent")
	s.before(ctx, "list_parent")
	return s.items, s.listErr
}
func (s *creationTimedSource) FetchDoc(ctx context.Context, token, revision string) (cloud.Document, error) {
	s.calls = append(s.calls, "document_read")
	s.before(ctx, "document_read")
	return s.doc, s.docErr
}
func (s *creationTimedSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	s.calls = append(s.calls, "readback")
	s.before(ctx, "readback")
	return s.file, s.fileErr
}

// Fails if the parent budget is passed directly to a remote read, if child
// expiry loses its origin, or if expired read results are accepted as valid.
func TestObserveCreationReadPhaseBudgets(t *testing.T) {
	for _, phase := range []string{"list_parent", "document_read", "readback"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithTimeout(online.WithCancelOrigin(context.Background(), "user_stop"), 90*time.Second)
				defer cancel()
				fixture := observerSource()
				op := observerOp()
				if phase == "readback" {
					fixture, _, op = fileObserverSource("文件内容\n")
				}
				source := &creationTimedSource{creationReadSource: fixture}
				source.before = func(ctx context.Context, current string) {
					if current != phase {
						return
					}
					deadline, ok := ctx.Deadline()
					if !ok || deadline.Sub(time.Now()) != 30*time.Second {
						t.Errorf("%s read deadline=%v, want 30 seconds", current, deadline.Sub(time.Now()))
					}
					<-ctx.Done()
				}
				start := time.Now()
				got, err := ObserveCreation(parent, source, op, []byte("你好世界\n"))
				failure := online.Capture(parent, err, "create", "unknown", "dispatched_unconfirmed", time.Now())
				if got.Verified || got.Phase != phase || !errors.Is(err, context.DeadlineExceeded) || failure.Phase != phase || failure.CancelOrigin != "phase_deadline" || parent.Err() != nil || time.Since(start) != 30*time.Second {
					t.Fatalf("observation=%+v failure=%+v err=%v parent=%v elapsed=%v", got, failure, err, parent.Err(), time.Since(start))
				}
				wantCalls := []string{"list_parent"}
				if phase != "list_parent" {
					wantCalls = append(wantCalls, phase)
				}
				if !reflect.DeepEqual(source.calls, wantCalls) {
					t.Fatalf("reads=%v want=%v", source.calls, wantCalls)
				}
			})
		})
	}
}

// Fails if all reads share the first 30-second deadline, rather than giving
// each stage its own sub-budget within the unchanged overall parent budget.
func TestObserveCreationReadBudgetRestartsForNextPhase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		source := &creationTimedSource{creationReadSource: observerSource()}
		source.before = func(ctx context.Context, phase string) {
			if phase == "list_parent" {
				time.Sleep(25 * time.Second)
				return
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 30*time.Second {
				t.Errorf("remaining document budget=%v", time.Until(deadline))
			}
			time.Sleep(20 * time.Second)
		}
		got, err := ObserveCreation(parent, source, observerOp(), []byte("你好世界\n"))
		if err != nil || !got.Verified {
			t.Fatalf("observation=%+v err=%v", got, err)
		}
	})
}

// Fails if a phase budget extends an earlier caller deadline or overrides its
// trusted cancellation origin, or starts a body read after list cancellation.
func TestObserveCreationCallerCancellationStopsFurtherReads(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "user stop"
		if deadline {
			name = "parent deadline"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				origin := "user_stop"
				wantErr := context.Canceled
				base := online.WithCancelOrigin(context.Background(), origin)
				var parent context.Context
				var cancel context.CancelFunc
				if deadline {
					origin = "parent_deadline"
					wantErr = context.DeadlineExceeded
					parent, cancel = context.WithTimeout(online.WithCancelOrigin(context.Background(), origin), 5*time.Second)
				} else {
					parent, cancel = context.WithCancel(base)
				}
				defer cancel()
				source := &creationTimedSource{creationReadSource: observerSource()}
				source.before = func(ctx context.Context, phase string) {
					if phase != "list_parent" {
						t.Fatalf("read %s after caller ended", phase)
					}
					if deadline {
						limit, ok := ctx.Deadline()
						if !ok || time.Until(limit) != 5*time.Second {
							t.Errorf("read extended parent budget: %v", time.Until(limit))
						}
						<-ctx.Done()
					} else {
						cancel()
					}
				}
				got, err := ObserveCreation(parent, source, observerOp(), []byte("你好世界\n"))
				failure := online.Capture(parent, err, "create", "unknown", "dispatched_unconfirmed", time.Now())
				if got.Verified || got.Phase != "list_parent" || !errors.Is(err, wantErr) || failure.CancelOrigin != origin || !reflect.DeepEqual(source.calls, []string{"list_parent"}) {
					t.Fatalf("observation=%+v failure=%+v reads=%v err=%v", got, failure, source.calls, err)
				}
			})
		})
	}
}

// A caller deadline is observable even without an optional lifecycle tag. It
// must not be confused with the observer's own child phase timer.
func TestObserveCreationUntaggedParentDeadlineProvenance(t *testing.T) {
	for _, expiredBeforeRead := range []bool{false, true} {
		name := "during read"
		if expiredBeforeRead {
			name = "before read"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				source := &creationTimedSource{creationReadSource: observerSource(), before: func(ctx context.Context, phase string) { <-ctx.Done() }}
				if expiredBeforeRead {
					time.Sleep(5 * time.Second)
				}
				got, err := ObserveCreation(parent, source, observerOp(), []byte("你好世界\n"))
				failure := online.Capture(parent, err, "create", "unknown", "dispatched_unconfirmed", time.Now())
				if got.Verified || !errors.Is(err, context.DeadlineExceeded) || failure.Phase != "list_parent" || failure.CancelOrigin != "parent_deadline" {
					t.Fatalf("observation=%+v failure=%+v err=%v", got, failure, err)
				}
				if expiredBeforeRead && len(source.calls) != 0 {
					t.Fatalf("reads after deadline=%v", source.calls)
				}
			})
		})
	}
}

type creationWaitingReader struct {
	ctx    context.Context
	closed bool
}

func (r *creationWaitingReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }
func (r *creationWaitingReader) Close() error             { r.closed = true; return nil }

// Fails if acquisition succeeds and drops the readback timer before consuming
// the body, or if a timed-out stream leaves its acquired reader unclosed.
func TestObserveCreationReadbackBudgetCoversStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		fixture, _, op := fileObserverSource("文件内容\n")
		reader := &creationWaitingReader{}
		source := &creationTimedSource{creationReadSource: fixture}
		source.before = func(ctx context.Context, phase string) {
			if phase == "readback" {
				reader.ctx = ctx
				source.file.Reader = reader
			}
		}
		start := time.Now()
		got, err := ObserveCreation(parent, source, op, nil)
		failure := online.Capture(parent, err, "create", "unknown", "dispatched_unconfirmed", time.Now())
		if got.Verified || got.Token != "candidate" || got.Phase != "readback" || !errors.Is(err, context.DeadlineExceeded) || failure.CancelOrigin != "phase_deadline" || time.Since(start) != 30*time.Second || !reader.closed {
			t.Fatalf("observation=%+v failure=%+v elapsed=%v closed=%v err=%v", got, failure, time.Since(start), reader.closed, err)
		}
	})
}
