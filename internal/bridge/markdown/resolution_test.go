package markdown

import (
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
