package markdown

import (
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	"strings"
	"testing"
)

const resizeTablePrefix = `<title id="doc">Title</title><h1 id="heading">Heading</h1>`
const resizeTableSuffix = `<p id="tail">Tail</p>`
const resizeTableBefore = `<table id="table"><colgroup><col width="120"/></colgroup><thead><tr><th><p>Name</p></th></tr></thead><tbody><tr><td><p>old</p></td></tr></tbody></table>`

func TestUnchangedRemoteTableResizeKeepsSurroundingBlocks(t *testing.T) {
	before := mustDecode(t, resizeTablePrefix+resizeTableBefore+resizeTableSuffix)
	for _, local := range []string{
		"# Heading\n\n| Name |\n| --- |\n| old |\n| added |\n\nTail\n",
		"# Heading\n\n| Name | State |\n| --- | --- |\n| old | ready |\n\nTail\n",
	} {
		got, err := PlanStepWithBase(before.Markdown, []byte(local), before, before)
		if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 {
			t.Fatalf("unilateral resize: %+v %v", got, err)
		}
		op := got.Operations[0]
		var p model.OperationPayload
		if err = json.Unmarshal(op.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if op.Kind != model.OperationBlockReplace || op.BlockID != "table" || p.ExplicitResolution {
			t.Fatalf("unsafe resize: %+v %+v", op, p)
		}
		xml := strings.Replace(p.XML, "<table>", `<table id="resized">`, 1)
		after := mustDecode(t, resizeTablePrefix+xml+resizeTableSuffix)
		if err = VerifyOperation(before, after, op); err != nil {
			t.Fatalf("resize readback: %v", err)
		}
		if !strings.Contains(local, "State") && !strings.Contains(p.XML, `width="120"`) {
			t.Fatal("unchanged column width lost")
		}
	}
}
func TestTableResizeStillProtectsConcurrentChanges(t *testing.T) {
	before := mustDecode(t, resizeTablePrefix+resizeTableBefore+resizeTableSuffix)
	remote := mustDecode(t, resizeTablePrefix+strings.Replace(resizeTableBefore, ">old<", ">cloud<", 1)+resizeTableSuffix)
	local := []byte("# Heading\n\n| Name |\n| --- |\n| local |\n| added |\n\nTail\n")
	got, err := PlanStepWithBase(before.Markdown, local, before, remote)
	if err != nil || got.ConflictReason == "" || len(got.Operations) != 0 {
		t.Fatalf("concurrent table bypassed: %+v %v", got, err)
	}
}
func TestTableResizeNeverReplacesProtectedRichResources(t *testing.T) {
	xml := resizeTablePrefix + strings.Replace(resizeTableBefore, "<td><p>old</p></td>", `<td colspan="2"><p>old</p><img src="media"/></td>`, 1) + resizeTableSuffix
	before := mustDecode(t, xml)
	local := []byte("# Heading\n\n| Name |\n| --- |\n| local |\n| added |\n\nTail\n")
	got, err := PlanStepWithBase(before.Markdown, local, before, before)
	if err != nil || got.ConflictReason == "" || len(got.Operations) != 0 {
		t.Fatalf("protected table bypassed: %+v %v", got, err)
	}
}
