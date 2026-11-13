package markdown

import (
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestGitMergedIntentCanReplaceUnanchoredPlainBody(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><p id="a">first</p><p id="b">last</p>`)
	got, err := PlanMergedStep([]byte("replacement\n"), before)
	if err != nil || got.ConflictReason != "" || len(got.Operations) != 1 || got.Operations[0].Kind != model.OperationBlockDelete || got.Operations[0].BlockID != "a" {
		t.Fatalf("plain rewrite %+v %v", got, err)
	}
	after := mustDecode(t, `<title id="doc">Title</title><p id="b">last</p>`)
	if err = VerifyOperation(before, after, got.Operations[0]); err != nil {
		t.Fatal(err)
	}
}
func TestGitMergedIntentNeverFlattensProtectedContent(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><p id="a">first</p><img id="media" src="token"/>`)
	got, err := PlanMergedStep([]byte("replacement\n"), before)
	if err == nil && got.ConflictReason == "" && len(got.Operations) > 0 {
		t.Fatal("protected resource removed", got)
	}
}
