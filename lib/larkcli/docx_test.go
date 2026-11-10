package larkcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func handleDocxProcess(s string, args []string, emit func(string)) bool {
	switch s {
	case "doc-fetch":
		if arg(args, "--doc-format") != "xml" || arg(args, "--detail") != "full" || arg(args, "--scope") != "full" {
			os.Exit(2)
		}
		emit(`{"document":{"document_id":"doc","revision_id":3,"content":"<p>nonempty</p>","url":"https://example.invalid/doc","reference_map":{"r":1}},"warnings":[]}`)
	case "doc-create":
		b, _ := io.ReadAll(os.Stdin)
		if string(b) != "<p>create</p>" || arg(args, "--content") != "-" {
			os.Exit(2)
		}
		emit(`{"document":{"document_id":"newdoc","revision_id":1,"url":"https://example.invalid/newdoc"}}`)

	case "doc-create-partial":
		fmt.Print(`{"ok":false,"identity":"user","data":{"result":"failed","warnings":["partial"]}}`)
		os.Exit(1)
	case "doc-create-identity":
		fmt.Print(`{"ok":true,"identity":"tenant","data":{"document":{"document_id":"candidate","revision_id":3,"url":"https://example.invalid/candidate"},"warnings":["untrusted"]}}`)
	case "doc-create-bad-warning":
		emit(`{"document":{"document_id":"candidate","revision_id":3,"url":"https://example.invalid/candidate"},"warnings":7}`)
	case "doc-create-bad-revision":
		emit(`{"document":{"document_id":"candidate","revision_id":"bad"}}`)
	case "doc-flat":
		emit(`{"document_id":"doc","revision_id":3,"content":"bad"}`)
	case "doc-missing":
		emit(`{"document":{"document_id":"doc","revision_id":3}}`)
	case "doc-empty":
		emit(`{"document":{"document_id":"doc","revision_id":3,"content":""}}`)
	case "doc-wrong":
		emit(`{"document":{"document_id":"other","revision_id":3,"content":"x"}}`)
	case "doc-apply":
		b, _ := io.ReadAll(os.Stdin)
		if string(b) != "<p>replacement</p>" || arg(args, "--content") != "-" || arg(args, "--command") != "block_replace" || arg(args, "--block-id") != "block" || arg(args, "--revision-id") != "3" || strings.Contains(strings.Join(args, " "), "client-token") {
			os.Exit(2)
		}
		emit(`{"result":"success","document":{"document_id":"doc","revision_id":4}}`)
	default:
		return false
	}
	return true
}
func TestDocxNestedContentAndID(t *testing.T) {
	d := NewDocx(NewDirect(testExecutable(t, "doc-fetch")))
	doc, err := d.Fetch(context.Background(), "doc", "")
	if err != nil || string(doc.XML) != "<p>nonempty</p>" || doc.Token != "doc" || doc.Revision != "3" || len(doc.ReferenceMap) == 0 {
		t.Fatalf("%+v %v", doc, err)
	}
	created, err := NewDocx(NewDirect(testExecutable(t, "doc-create"))).Create(context.Background(), "folder", "title", []byte("<p>create</p>"))
	if err != nil || created.Token != "newdoc" || created.Revision != "1" || created.SourceURL != "https://example.invalid/newdoc" {
		t.Fatalf("%+v %v", created, err)
	}
}
func TestDocxMissingRequiredFieldsRejected(t *testing.T) {
	for _, s := range []string{"doc-flat", "doc-missing", "doc-wrong"} {
		_, err := NewDocx(NewDirect(testExecutable(t, s))).Fetch(context.Background(), "doc", "")
		if err == nil {
			t.Fatal(s)
		}
	}
	doc, err := NewDocx(NewDirect(testExecutable(t, "doc-empty"))).Fetch(context.Background(), "doc", "")
	if err != nil || len(doc.XML) != 0 {
		t.Fatalf("%+v %v", doc, err)
	}
}
func TestDocxOneMutationAndUnknownNotReplayed(t *testing.T) {
	d := NewDocx(NewDirect(testExecutable(t, "doc-apply")))
	result, err := d.Apply(context.Background(), "doc", "3", DocMutation{Kind: "block_replace", BlockID: "block", XML: "<p>replacement</p>"})
	if err != nil || result.Result != "success" {
		t.Fatalf("%+v %v", result, err)
	}
	_, err = NewDocx(NewDirect(testExecutable(t, "partial"))).Apply(context.Background(), "doc", "3", DocMutation{Kind: "block_replace", BlockID: "block", XML: "x"})
	if !errors.Is(err, ErrUnknownResult) {
		t.Fatalf("%v", err)
	}
	_, err = d.Apply(context.Background(), "doc", "0", DocMutation{Kind: "block_replace", BlockID: "block", XML: "x"})
	if err == nil {
		t.Fatal("revision zero accepted")
	}
}

func TestDocxRequiredFieldTypes(t *testing.T) {
	for _, body := range []string{`{"document":{"document_id":"doc","content":"x"}}`, `{"document":{"revision_id":1,"content":"x"}}`, `{"document":{"document_id":"doc","revision_id":0,"content":"x"}}`, `{"document":{"document_id":"doc","revision_id":"1","content":"x"}}`, `{"document":{"document_id":"doc","revision_id":1,"content":null}}`, `{"document":{"document_id":"doc","revision_id":1,"content":7}}`} {
		if _, err := decodeDocument([]byte(body), true); err == nil {
			t.Fatal("required field accepted", body)
		}
	}
}

func TestDocxCreatePreservesEvidenceOnUnknown(t *testing.T) {
	for _, scenario := range []string{"doc-create-partial", "doc-create-identity", "doc-create-bad-warning", "doc-create-bad-revision"} {
		t.Run(scenario, func(t *testing.T) {
			doc, err := NewDocx(NewDirect(testExecutable(t, scenario))).Create(context.Background(), "folder", "title", []byte("x"))
			if !errors.Is(err, ErrUnknownResult) {
				t.Fatal("unknown lost", err)
			}
			if len(doc.Data) == 0 {
				t.Fatal("creation evidence lost")
			}
			if scenario == "doc-create-partial" {
				if doc.Result != "failed" || len(doc.Warnings) != 1 || doc.Warnings[0] != "partial" || doc.Token != "" || doc.Revision != "" {
					t.Fatal("partial evidence lost or fabricated", doc)
				}
			} else {
				if doc.Token != "candidate" {
					t.Fatal("valid identity discarded", doc)
				}
				if scenario == "doc-create-bad-revision" {
					if doc.Revision != "" {
						t.Fatal("invalid revision accepted")
					}
				} else if doc.Revision != "3" {
					t.Fatal("valid revision discarded", doc)
				}
				if scenario != "doc-create-bad-revision" && doc.SourceURL != "https://example.invalid/candidate" {
					t.Fatal("valid URL discarded")
				}
				if scenario == "doc-create-identity" && !errors.Is(err, ErrIdentity) {
					t.Fatal("identity error discarded", err)
				}
			}
		})
	}
}
