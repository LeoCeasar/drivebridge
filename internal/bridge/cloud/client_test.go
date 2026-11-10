package cloud

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestApplyDocCapabilityAndBatchGate(t *testing.T) {
	c, err := New(managed(t, "must-not-run"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ops := range [][]model.Operation{nil, {{Kind: model.OperationBlockReplace}, {Kind: model.OperationBlockDelete}}, {{Kind: model.OperationBlockReplace, BlockID: "block", Route: "docs_shortcut_xml", CapabilityID: "unverified", Payload: []byte(`{"xml":"<p>new</p>"}`)}}} {
		_, err := c.ApplyDoc(context.Background(), "doc", "3", ops)
		if err == nil {
			t.Fatal("unverified mutation dispatched")
		}
	}
	if _, err := c.FileMeta(context.Background(), "file"); !errors.Is(err, larkcli.ErrMetadataUnknown) {
		t.Fatal(err)
	}
}
func TestCanonicalErrorMapping(t *testing.T) {
	if !errors.Is(canonicalError(larkcli.ErrUnknownResult), model.ErrUnknownResult) || !errors.Is(canonicalError(larkcli.ErrIdentity), model.ErrIdentityChanged) {
		t.Fatal("wire errors escaped canonical mapping")
	}
}

func TestEvidenceQualifiedCapabilitySubmitsOnlyOneMutation(t *testing.T) {
	script := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"ok\":true,\"identity\":\"user\",\"data\":{\"result\":\"success\",\"document\":{\"revision_id\":4}}}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	grant := CapabilityGrant{Descriptor: model.OperationCapability{ID: "test-only", Kind: model.OperationBlockReplace, Route: ShortcutXMLRoute}, CLISourceRevision: CLIRevision, Evidence: "offline: test-only authority supplied by fixture", Verified: true}
	c, err := New(managed(t, script), Options{Capabilities: []CapabilityGrant{grant}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.ApplyDoc(context.Background(), "doc", "3", []model.Operation{{ID: "local-journal", Kind: model.OperationBlockReplace, BlockID: "block", Route: ShortcutXMLRoute, CapabilityID: "test-only", Payload: []byte(`{"xml":"<p>new</p>","before_hash":"semantic","after_hash":"semantic"}`)}})
	if err != nil || result.Revision != "4" || len(result.Completed) != 1 {
		t.Fatal(result, err)
	}
	grant.CLISourceRevision = "other"
	c, err = New(managed(t, "must-not-run"), Options{Capabilities: []CapabilityGrant{grant}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ApplyDoc(context.Background(), "doc", "3", []model.Operation{{Kind: model.OperationBlockReplace, BlockID: "block", Route: ShortcutXMLRoute, CapabilityID: "test-only", Payload: []byte(`{"xml":"new"}`)}})
	if !errors.Is(err, larkcli.ErrUnsupportedCapability) {
		t.Fatal(err)
	}
}
func TestCloudObservationHasCanonicalIdentity(t *testing.T) {
	script := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'bytes' > download\nprintf '%s' '{\"ok\":true,\"identity\":\"user\",\"data\":{\"saved_path\":\"download\",\"size_bytes\":5}}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := New(managed(t, script), Options{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	o, err := c.ObserveFile(context.Background(), "file")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.Identity.Kind != model.FileIdentityObservation || o.Identity.Version != "" || o.Identity.Validate() != nil || o.Size != 5 {
		t.Fatalf("%+v", o)
	}
	rc, _ := o.Open()
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "bytes" {
		t.Fatal(string(b))
	}
}

func TestCloudRejectsDirectAndLifecycleRunners(t *testing.T) {
	if _, err := New(larkcli.NewDirect("unused"), Options{}); err == nil {
		t.Fatal("direct runner accepted")
	}
	lifecycle, err := larkcli.NewEnrollment(larkcli.EnrollmentOptions{Executable: "unused", AuthRef: "ref", AuthRoot: t.TempDir(), Coordinator: testCoordinator{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New(lifecycle, Options{}); err == nil {
		t.Fatal("lifecycle runner accepted")
	}
}

type testCoordinator struct{}

func (testCoordinator) WithCredentialLocks(ctx context.Context, a, u string, run func() error) error {
	return run()
}
func (testCoordinator) WithEnrollmentLock(ctx context.Context, run func() error) error { return run() }
func managed(t *testing.T, executable string) *larkcli.Runner {
	t.Helper()
	r, err := larkcli.NewManaged(larkcli.ManagedOptions{Executable: executable, AuthRef: "ref", AuthRoot: t.TempDir(), Identity: larkcli.Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: testCoordinator{}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUnknownMutationAlsoPreservesIdentityChanged(t *testing.T) {
	err := canonicalError(errors.Join(larkcli.ErrUnknownResult, larkcli.ErrIdentity))
	if !errors.Is(err, model.ErrUnknownResult) || !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("one canonical error masked another", err)
	}
}
