package markdown

import (
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestResolvedUnchangedTableCanResizeBesideList(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Test</title><ul><li id="item">keep</li></ul><table id="t"><thead><tr><th><p>A</p></th></tr></thead><tbody><tr><td><p>old</p></td></tr></tbody></table>`)
	desired := []byte("- keep\n\n| A | B |\n| --- | --- |\n| old | new |\n")
	result, err := PlanResolvedStepWithBase(before.Markdown, desired, before, before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatalf("safe table rejected: %+v %v", result, err)
	}
	if result.Operations[0].Kind != model.OperationBlockReplace || result.Operations[0].BlockID != "t" {
		t.Fatalf("wrong op %+v", result.Operations)
	}
	var p model.OperationPayload
	json.Unmarshal(result.Operations[0].Payload, &p)
	if !p.ExplicitResolution {
		t.Fatal("human intent missing")
	}
}
