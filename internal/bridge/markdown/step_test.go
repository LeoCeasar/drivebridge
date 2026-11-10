package markdown

import (
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestStepPlansConsecutiveInsertionsWithoutGuessingIDs(t *testing.T) {
	remote := mustDecode(t, `<title id="doc">Title</title>`)
	result, err := PlanStep(nil, []byte("first\n\nsecond\n\nthird\n"), remote)
	if err != nil || result.ConflictReason != "" || !result.Deferred || len(result.Operations) != 1 || result.Operations[0].AfterBlockID != "0" {
		t.Fatalf("%+v %v", result, err)
	}
	remote = mustDecode(t, `<title id="doc">Title</title><p id="first">first</p>`)
	result, err = PlanStep(remote.Markdown, result.Markdown, remote)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 || result.Operations[0].Kind != model.OperationBlockInsertAfter || result.Operations[0].AfterBlockID != "first" {
		t.Fatalf("%+v %v", result, err)
	}
}
