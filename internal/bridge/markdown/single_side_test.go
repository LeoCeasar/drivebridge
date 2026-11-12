package markdown

import (
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestSingleSideEditAndInsertionUsesKnownChangedBlock(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><h1 id="heading">Heading</h1><p id="p">first</p>`)
	local := []byte("# Heading\n\nsecond\n\nappended\n")
	got, err := PlanStepWithBase(before.Markdown, local, before, before)
	if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 || got.Operations[0].Kind != model.OperationBlockDelete || got.Operations[0].BlockID != "p" {
		t.Fatalf("single-side edit requires intervention: %+v %v", got, err)
	}
	if payload(t, got).ExplicitResolution {
		t.Fatal("automatic plan claimed human authorization")
	}
	after := mustDecode(t, `<title id="doc">Title</title><h1 id="heading">Heading</h1>`)
	if err := VerifyOperation(before, after, got.Operations[0]); err != nil {
		t.Fatal(err)
	}
	changed := mustDecode(t, `<title id="doc">Title</title><h1 id="heading">Heading</h1><p id="p">cloud change</p>`)
	got, err = PlanStepWithBase(before.Markdown, local, before, changed)
	if err != nil || got.ConflictReason == "" || len(got.Operations) != 0 {
		t.Fatalf("dual-sided ambiguity bypassed: %+v %v", got, err)
	}
}

func TestSingleSideFallbackKeepsOpaqueAndDuplicateIdentityProtection(t *testing.T) {
	for _, xml := range []string{
		`<title id="doc">Title</title><widget id="rich">keep</widget><p id="p">first</p>`,
		`<title id="doc">Title</title><p id="a">same</p><p id="b">same</p>`,
	} {
		before := mustDecode(t, xml)
		got, err := PlanStepWithBase(before.Markdown, []byte("replacement\n\nappended\n"), before, before)
		if err != nil || got.ConflictReason == "" || len(got.Operations) != 0 {
			t.Fatalf("unsafe fallback: %+v %v", got, err)
		}
	}
}
