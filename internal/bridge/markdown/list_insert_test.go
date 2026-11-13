package markdown

import (
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestAppendSectionAfterListUsesLastItemIdentity(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><h1 id="h">Heading</h1><ul><li id="a">first</li><li id="b">second</li></ul>`)
	local := append(append([]byte{}, before.Markdown...), []byte("\n## Added\n\n[guide](skills/example.md)\n")...)
	got, err := PlanStepWithBase(before.Markdown, local, before, before)
	if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 || !got.Deferred {
		t.Fatalf("append section: %+v %v", got, err)
	}
	op := got.Operations[0]
	if op.Kind != model.OperationBlockInsertAfter || op.AfterBlockID != "b" {
		t.Fatalf("wrong list anchor: %+v", op)
	}
	after := mustDecode(t, string(before.XML)+`<h2 id="added">Added</h2>`)
	if err = VerifyOperation(before, after, op); err != nil {
		t.Fatalf("append readback: %v", err)
	}
}
func TestInsertListItemUsesValidListXML(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><h1 id="h">Heading</h1>`)
	got, err := PlanStepWithBase(before.Markdown, []byte("# Heading\n\n- first\n- second\n"), before, before)
	if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 || !got.Deferred {
		t.Fatalf("insert list: %+v %v", got, err)
	}
	var p model.OperationPayload
	if err = json.Unmarshal(got.Operations[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.XML != "<ul><li>first</li></ul>" {
		t.Fatalf("invalid list XML: %q", p.XML)
	}
	after := mustDecode(t, string(before.XML)+`<ul><li id="first">first</li></ul>`)
	if err = VerifyOperation(before, after, got.Operations[0]); err != nil {
		t.Fatalf("list insertion readback: %v", err)
	}
	got, err = PlanStepWithBase(after.Markdown, []byte("# Heading\n\n- first\n- second\n"), after, after)
	if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 {
		t.Fatalf("continue list: %+v %v", got, err)
	}
	final := mustDecode(t, string(before.XML)+`<ul><li id="first">first</li><li id="second">second</li></ul>`)
	if err = VerifyOperation(after, final, got.Operations[0]); err != nil {
		t.Fatalf("continued list readback: %v", err)
	}
}
func TestUnilateralRewriteWithListKeepsSharedHeading(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><h1 id="h">Heading</h1><p id="a">old first</p><p id="b">old second</p>`)
	local := []byte("# Heading\n\nnew first\n\nnew second\n\n- entry one\n- entry two\n")
	got, err := PlanStepWithBase(before.Markdown, local, before, before)
	if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 {
		t.Fatalf("supported rewrite: %+v %v", got, err)
	}
	op := got.Operations[0]
	var p model.OperationPayload
	_ = json.Unmarshal(op.Payload, &p)
	if op.Kind != model.OperationBlockDelete || op.BlockID != "a" || p.ExplicitResolution {
		t.Fatalf("unsafe fallback: %+v %+v", op, p)
	}
}

func TestUnilateralInsertionsPreserveReusedSectionIdentity(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><h1 id="h">Heading</h1><h2 id="section">Existing section</h2><p id="a">old first</p><p id="b">old second</p>`)
	local := []byte("# Heading\n\nnew introduction\n\nextra context\n\n## Existing section\n\nnew first\n\nnew second\n\n- entry one\n")
	got, err := PlanStepWithBase(before.Markdown, local, before, before)
	if err != nil || got.ConflictReason != "" || len(got.Operations) == 0 {
		t.Fatalf("insertion before retained section: %+v %v", got, err)
	}
	op := got.Operations[0]
	if op.Kind != model.OperationBlockInsertAfter || op.AfterBlockID != "h" {
		t.Fatalf("retained section must not be deleted: %+v", op)
	}
}

func TestUnilateralAppendKeepsRepeatedTextAndVerifiesPosition(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><h1 id="h">Heading</h1><p id="a">same</p>`)
	local := []byte("# Heading\n\nsame\n\nsame\n\nlast\n")
	got, err := PlanStepWithBase(before.Markdown, local, before, before)
	if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 {
		t.Fatalf("append repeated content: %+v %v", got, err)
	}
	op := got.Operations[0]
	if op.Kind != model.OperationBlockInsertAfter || op.AfterBlockID != "a" {
		t.Fatal(op)
	}
	after := mustDecode(t, string(before.XML)+`<p id="added">same</p>`)
	if err = VerifyOperation(before, after, op); err != nil {
		t.Fatalf("exact-position repeated insertion: %v", err)
	}
	wrong := mustDecode(t, `<title id="doc">Title</title><h1 id="h">Heading</h1><p id="added">same</p><p id="a">same</p>`)
	if err = VerifyOperation(before, wrong, op); err == nil {
		t.Fatal("wrong-position insertion accepted")
	}
}
