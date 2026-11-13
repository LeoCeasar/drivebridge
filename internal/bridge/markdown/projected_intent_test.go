package markdown

import (
	"bytes"
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestProjectedHashVerifiesConsecutiveBreakWithoutProjectionMigration(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><p id="a">old</p>`)
	got, err := PlanMergedStep([]byte("one<br><br>two\n"), before)
	if err != nil || len(got.Operations) != 1 {
		t.Fatal(got, err)
	}
	op, err := ProjectOperationHash(got.Operations[0], before)
	if err != nil {
		t.Fatal(err)
	}
	after := mustDecode(t, `<title id="doc">Title</title><p id="new">one<br/><br/>two</p>`)
	if err = VerifyOperation(before, after, op); err != nil {
		t.Fatal(err)
	}
	var p model.OperationPayload
	_ = json.Unmarshal(op.Payload, &p)
	if p.ExplicitResolution || !MatchesEncodedIntent([]byte("one<BR><br />two\n"), after) {
		t.Fatal("safe break confirmation failed", p)
	}
	if bytes.Equal(after.Markdown, []byte("one  \n  \ntwo\n")) {
		t.Fatal("existing readable projection unexpectedly changed")
	}
}
func TestEncodedIntentDoesNotIgnoreRichResources(t *testing.T) {
	current := mustDecode(t, `<title id="doc">Title</title><p id="a">one</p><img id="img" src="private-token"/>`)
	if MatchesEncodedIntent([]byte("one\n"), current) {
		t.Fatal("resource omitted by semantic confirmation")
	}
	current = mustDecode(t, `<title id="doc">Title</title><p id="a" unknown="important">one</p>`)
	if MatchesEncodedIntent([]byte("one\n"), current) {
		t.Fatal("unknown attributes omitted by semantic confirmation")
	}
}

func TestProjectedHashRejectsCollisionBeforeCloudWrite(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Title</title><p id="a">old</p><p id="duplicate">one<br/><br/>two</p>`)
	original := mustDecode(t, `<title id="doc">Title</title><p id="a">old</p>`)
	result, err := PlanMergedStep([]byte("one<br><br>two\n"), original)
	if err != nil || len(result.Operations) != 1 {
		t.Fatal(result, err)
	}
	if _, err = ProjectOperationHash(result.Operations[0], before); err == nil {
		t.Fatal("ambiguous projected readback authorized")
	}
}
