package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	if !errors.Is(canonicalError(larkcli.ErrNotDispatched), ErrNotDispatched) {
		t.Fatal("pre-start transport result lost")
	}
}

func TestCreateDocPreStartFailureDoesNotBecomeUnknownWrite(t *testing.T) {
	c, err := New(managed(t, filepath.Join(t.TempDir(), "missing-cli")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateDoc(context.Background(), "parent", "title", []byte("<p>body</p>"))
	if !errors.Is(err, ErrNotDispatched) || errors.Is(err, model.ErrUnknownResult) {
		t.Fatalf("pre-start creation became an unknown write: %v", err)
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

func TestCreateDocPreservesPartialEvidence(t *testing.T) {
	for _, scenario := range []string{"success", "partial", "bad-warning", "identity", "bad-revision", "null-result", "failed-result", "unknown-result"} {
		t.Run(scenario, func(t *testing.T) {
			data := `{"document":{"document_id":"created","revision_id":7,"url":"https://example.invalid/created"}}`
			identity := "user"
			ok := true
			exit := 0
			switch scenario {
			case "partial":
				data = `{"result":"failed","warnings":["recover creation"],"document":{"document_id":"candidate","revision_id":3}}`
				ok = false
				exit = 1
			case "bad-warning":
				data = `{"document":{"document_id":"created","revision_id":7},"warnings":7}`
			case "identity":
				identity = "tenant"
			case "bad-revision":
				data = `{"document":{"document_id":"created","revision_id":null}}`
			case "null-result":
				data = `{"result":null,"document":{"document_id":"created","revision_id":7}}`
			case "failed-result":
				data = `{"result":"failed","document":{"document_id":"created","revision_id":7}}`
			case "unknown-result":
				data = `{"result":"pending","document":{"document_id":"created","revision_id":7}}`
			}
			envelope := fmt.Sprintf(`{"ok":%t,"identity":%q,"data":%s}`, ok, identity, data)
			script := filepath.Join(t.TempDir(), "cli")
			if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '"+envelope+"'\nexit "+strconv.Itoa(exit)+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			c, err := New(managed(t, script), Options{})
			if err != nil {
				t.Fatal(err)
			}
			doc, err := c.CreateDoc(context.Background(), "parent", "title", []byte("<p>input</p>"))
			if string(doc.Data) != data {
				t.Fatal("creation outcome data changed")
			}
			if scenario == "success" {
				if err != nil || doc.Token != "created" || doc.Revision != "7" || doc.SourceURL != "https://example.invalid/created" {
					t.Fatalf("success rejected: token=%q revision=%q err=%v", doc.Token, doc.Revision, err)
				}
				return
			}
			if !errors.Is(err, model.ErrUnknownResult) || len(doc.Data) == 0 {
				t.Fatal("unknown creation evidence discarded", err)
			}
			if scenario == "partial" {
				if doc.Result != "failed" || len(doc.Warnings) != 1 || doc.Warnings[0] != "recover creation" || doc.Token != "candidate" || doc.Revision != "3" {
					t.Fatal("partial outcome lost")
				}
			} else {
				if doc.Token != "created" {
					t.Fatal("candidate ID lost")
				}
				if scenario == "bad-revision" && doc.Revision != "" {
					t.Fatal("invalid revision manufactured")
				}
				if scenario == "identity" && !errors.Is(err, model.ErrIdentityChanged) {
					t.Fatal("identity failure lost", err)
				}
			}
			if strings.Contains(err.Error(), "recover creation") || strings.Contains(err.Error(), "candidate") {
				t.Fatal("raw evidence entered error log")
			}
		})
	}
}
