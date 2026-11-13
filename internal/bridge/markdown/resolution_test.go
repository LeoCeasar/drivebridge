package markdown

import (
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestExplicitResolutionRetainsOpaqueAndChangedRemote(t *testing.T) {
	remote := mustDecode(t, `<title id="doc">Title</title><p id="a">old</p><widget id="rich" type="unknown">keep</widget>`)
	result, err := PlanResolvedStep(remote.Markdown, []byte("replacement\n"), remote)
	if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatal(result, err)
	}
	base := []byte("alpha\n\nbeta\n")
	changed := mustDecode(t, `<title id="doc">Title</title><p id="a">remote alpha</p><p id="b">remote beta</p>`)
	result, err = PlanResolvedStep(base, []byte("replacement\n"), changed)
	if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatal(result, err)
	}
}
func TestExplicitResolutionDuplicateTextUsesExactIDsAndContext(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><p id="a">same</p><p id="b">same</p>`)
	after := mustDecode(t, `<title id="doc">Title</title><p id="b">same</p>`)
	op := operation(model.OperationBlockDelete, "a", "", model.OperationPayload{BeforeHash: before.Blocks[0].Hash, ExplicitResolution: true})
	if err := VerifyOperation(before, after, op); err != nil {
		t.Fatal(err)
	}
	wrong := mustDecode(t, `<title id="doc">Title</title><p id="a">same</p>`)
	if err := VerifyOperation(before, wrong, op); err == nil {
		t.Fatal("different surviving ID accepted")
	}
}

func TestPlainStructuralRewriteKeepsUniqueInteriorAnchor(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><p id="first">alpha beta gamma</p><p id="anchor">stable anchor 中文😀</p><p id="tail">tail</p>`)
	desired := []byte("# newest local version 99\n\nstable anchor 中文😀\n")
	result, err := PlanStepWithBase(before.Markdown, desired, before, before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatalf("anchored structural edit: %+v %v", result, err)
	}
	op := result.Operations[0]
	if op.Kind != model.OperationBlockDelete || op.BlockID != "first" {
		t.Fatalf("must preserve surviving anchor ID: %+v", op)
	}
	var payload model.OperationPayload
	if err = json.Unmarshal(op.Payload, &payload); err != nil || payload.ExplicitResolution {
		t.Fatalf("automatic edit borrowed explicit resolution: %+v %v", payload, err)
	}
}

func TestPlainInteriorAnchorFallbackRetainsConflictBoundaries(t *testing.T) {
	cases := []struct{ name, xml, local string }{
		{"no surviving anchor", `<title id="doc">Title</title><p id="a">old one</p><p id="b">old two</p>`, "# new heading\n\nall new text\n"},
		{"repeated anchor", `<title id="doc">Title</title><p id="a">old one</p><p id="b">same</p><p id="c">same</p><p id="d">tail</p>`, "# new heading\n\nsame\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := mustDecode(t, tc.xml)
			result, err := PlanStepWithBase(before.Markdown, []byte(tc.local), before, before)
			if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
				t.Fatalf("unsafe structural rewrite: %+v %v", result, err)
			}
		})
	}
	before := mustDecode(t, `<title id="doc">Title</title><p id="a">old one</p><p id="b">stable anchor</p><p id="c">tail</p>`)
	changed := mustDecode(t, `<title id="doc">Title</title><p id="a">cloud edited first paragraph</p><p id="b">stable anchor</p><p id="c">tail</p>`)
	result, err := PlanStepWithBase(before.Markdown, []byte("# new heading\n\nstable anchor\n"), before, changed)
	if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatalf("concurrent cloud edit lost conflict: %+v %v", result, err)
	}
}
