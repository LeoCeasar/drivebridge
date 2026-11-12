package markdown

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

const readableFixture = `<title id="doc">Guide</title><p id="intro">before</p><p id="empty"></p><table id="table"><colgroup><col width="400"/></colgroup><tbody><tr><td><p id="one">cat app/.env</p><p id="two">git log</p><p id="styled"><b><span text-color="red">docker</span></b> build</p><img id="image" src="media-token" name="photo.png" mime="image/png" alt="Screenshot"/></td></tr></tbody></table><pre id="code" lang="sh"><code>echo hi<br/>pwd</code></pre><p id="tail">after</p>`

func readable(t *testing.T, s string) model.DocView {
	t.Helper()
	d, e := Decode([]byte(s), "1")
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestReadableProjection(t *testing.T) {
	d := readable(t, readableFixture)
	if d.ProjectionVersion != 2 || len(d.Assets) != 1 {
		t.Fatalf("version/assets: %#v", d)
	}
	for _, s := range []string{"before", "<p></p>", "<table>", "cat app/.env", "git log", "<b><span>docker</span></b> build", "<img src=\".drivebridge-assets/", "```sh\necho hi\npwd\n```", "after"} {
		if !strings.Contains(string(d.Markdown), s) {
			t.Errorf("missing %q in %s", s, d.Markdown)
		}
	}
	for _, s := range []string{"Unsupported", `id="`, "media-token", "text-color"} {
		if strings.Contains(string(d.Markdown), s) {
			t.Errorf("leaked projection context %q", s)
		}
	}
	if len(d.Blocks[2].Children) != 2 {
		t.Fatalf("nested writable paragraphs: %#v", d.Blocks[2].Children)
	}
	legacy, e := DecodeWithOptions([]byte(readableFixture), "1", Options{ProjectionVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(legacy.Markdown), "Unsupported table content") || len(legacy.Assets) != 0 {
		t.Fatal("legacy projection changed")
	}
	for i, b := range d.Blocks {
		if b.Start < 0 || b.End > len(d.Markdown) || b.Start > b.End {
			t.Fatalf("invalid block offsets %d %#v", i, b)
		}
	}
}
func TestReadableNestedPlanAndVerify(t *testing.T) {
	base := readable(t, readableFixture)
	local := bytes.Replace(base.Markdown, []byte("cat app/.env"), []byte("cat app/.env.local"), 1)
	result, e := PlanStepWithBase(base.Markdown, local, base, base)
	if e != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatalf("plan: %#v %v", result, e)
	}
	op := result.Operations[0]
	if op.BlockID != "one" || op.Kind != model.OperationBlockReplace {
		t.Fatalf("wrong target %#v", op)
	}
	var p model.OperationPayload
	if e = json.Unmarshal(op.Payload, &p); e != nil {
		t.Fatal(e)
	}
	if p.XML != "<p>cat app/.env.local</p>" {
		t.Fatalf("rewrote enclosing resource: %s", p.XML)
	}
	after := readable(t, strings.Replace(readableFixture, `<p id="one">cat app/.env</p>`, `<p id="new-one">cat app/.env.local</p>`, 1))
	if e = VerifyOperation(base, after, op); e != nil {
		t.Fatalf("verify nested: %v", e)
	}
	bad := readable(t, strings.Replace(string(after.XML), "git log", "changed sibling", 1))
	if VerifyOperation(base, bad, op) == nil {
		t.Fatal("accepted sibling effect")
	}
	bad = readable(t, strings.Replace(string(after.XML), `width="400"`, `width="401"`, 1))
	if VerifyOperation(base, bad, op) == nil {
		t.Fatal("accepted changed parent metadata")
	}
}
func TestReadableConcurrentAndWholeDocumentProtection(t *testing.T) {
	base := readable(t, readableFixture)
	local := bytes.Replace(base.Markdown, []byte("cat app/.env"), []byte("cat app/.env.local"), 1)
	remote := readable(t, strings.Replace(readableFixture, "git log", "git log --oneline", 1))
	result, e := PlanStepWithBase(base.Markdown, local, base, remote)
	if e != nil || result.ConflictReason != "" || len(result.Operations) != 1 || !bytes.Contains(result.Markdown, []byte("git log --oneline")) {
		t.Fatalf("different nodes: %#v %v", result, e)
	}
	remote = readable(t, strings.Replace(readableFixture, "cat app/.env", "cat app/.env.remote", 1))
	result, e = PlanStepWithBase(base.Markdown, local, base, remote)
	if e != nil || result.ConflictReason != "overlapping_edits" || len(result.Operations) != 0 {
		t.Fatalf("same-node: %#v %v", result, e)
	}
	for _, change := range []struct{ old, new string }{{"<tbody>", "<thead>"}, {"git log</p>", "git log</p><p>new</p>"}, {".png\" alt=", ".jpg\" alt="}, {"<span>docker</span>", "<em>docker</em>"}, {"<p></p>", "<p>new empty</p>"}, {"cat app/.env", "<b>rich</b>"}, {"after", "after\n\n<script>evil</script>"}} {
		edited := bytes.Replace(local, []byte(change.old), []byte(change.new), 1)
		edited = bytes.Replace(edited, []byte("before"), []byte("ordinary changed"), 1)
		result, e = PlanStepWithBase(base.Markdown, edited, base, base)
		if e != nil || result.ConflictReason == "" || len(result.Operations) != 0 || !bytes.Equal(result.Markdown, edited) {
			t.Errorf("unsupported %q: %#v %v", change.old, result, e)
		}
	}
}
func TestReadableNestedSpecialCharactersAndAmbiguity(t *testing.T) {
	raw := `<p id="before">anchor</p><table id="t"><tbody><tr><td><p id="a">one &amp; two</p><p id="b">tail</p></td></tr></tbody></table><p id="after">end</p>`
	base := readable(t, raw)
	local := bytes.Replace(base.Markdown, []byte("one &amp; two"), []byte("中文 &lt;tag&gt; &amp; 'quote'"), 1)
	result, e := PlanStepWithBase(base.Markdown, local, base, base)
	if e != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatalf("special characters: %#v %v", result, e)
	}
	// Removing one of repeated empty paragraphs has ambiguous identity and no ops.
	base = readable(t, `<p id="a"></p><p id="b"></p><p id="c">end</p>`)
	local = bytes.Replace(base.Markdown, []byte("<p></p>\n\n"), nil, 1)
	result, e = PlanStepWithBase(base.Markdown, local, base, base)
	if e != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatalf("ambiguous: %#v %v", result, e)
	}
}
func TestReadableAssetPaths(t *testing.T) {
	for _, name := range []string{"../../escape.png", "x.SVG", "https://evil/a.jpeg", "x.PNG"} {
		p := AssetPath("token", name, "image/png")
		if !strings.HasPrefix(p, ".drivebridge-assets/") || strings.Contains(strings.TrimPrefix(p, ".drivebridge-assets/"), "/") || strings.Contains(p, "token") {
			t.Fatal(p)
		}
	}
}

func TestReadableSeparateCheckpoints(t *testing.T) {
	base := readable(t, readableFixture)
	baseline := readable(t, strings.Replace(readableFixture, "cat app/.env", "cat remote/.env", 1))
	local := bytes.Replace(base.Markdown, []byte("cat app/.env"), []byte("cat app/.env.local"), 1)
	r, e := PlanStepWithBase(base.Markdown, local, baseline, baseline)
	if e != nil || r.ConflictReason != "overlapping_edits" || len(r.Operations) != 0 {
		t.Fatalf("discarded previously merged content: %#v %v", r, e)
	}
	r, e = PlanStepWithBase(base.Markdown, base.Markdown, baseline, baseline)
	if e != nil || r.ConflictReason != "" || len(r.Operations) != 0 || !bytes.Equal(r.Markdown, baseline.Markdown) {
		t.Fatalf("unchanged local lost remote checkpoint: %#v %v", r, e)
	}
}

func TestReadableOrdinaryInsertDelete(t *testing.T) {
	base := readable(t, readableFixture)
	cases := []struct {
		name                 string
		local                []byte
		kind, anchor, target string
	}{
		{"before document", append([]byte("new introduction\n\n"), base.Markdown...), model.OperationBlockInsertAfter, "0", ""},
		{"before table", bytes.Replace(base.Markdown, []byte("<table>"), []byte("new before table\n\n<table>"), 1), model.OperationBlockInsertAfter, "empty", ""},
		{"after table", bytes.Replace(base.Markdown, []byte("</table>"), []byte("</table>\n\nnew after table"), 1), model.OperationBlockInsertAfter, "table", ""},
		{"heading after table", bytes.Replace(base.Markdown, []byte("</table>"), []byte("</table>\n\n# New section"), 1), model.OperationBlockInsertAfter, "table", ""},
		{"code after table", bytes.Replace(base.Markdown, []byte("</table>"), []byte("</table>\n\n```python\nprint(1)\n```"), 1), model.OperationBlockInsertAfter, "table", ""},
		{"delete ordinary", bytes.Replace(base.Markdown, []byte("before\n\n"), nil, 1), model.OperationBlockDelete, "", "intro"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r, e := PlanStepWithBase(base.Markdown, tt.local, base, base)
			if e != nil || r.ConflictReason != "" || len(r.Operations) != 1 || !bytes.Equal(r.Markdown, tt.local) {
				t.Fatalf("plan %#v %v", r, e)
			}
			op := r.Operations[0]
			if op.Kind != tt.kind || op.AfterBlockID != tt.anchor || op.BlockID != tt.target {
				t.Fatalf("wrong target %#v", op)
			}
		})
	}
	// Insertions do not authorize deleting a protected table or image.
	start := bytes.Index(base.Markdown, []byte("<table>"))
	end := bytes.Index(base.Markdown, []byte("</table>")) + len("</table>")
	local := append(append([]byte("new paragraph\n\n"), base.Markdown[:start]...), base.Markdown[end:]...)
	r, e := PlanStepWithBase(base.Markdown, local, base, base)
	if e != nil || r.ConflictReason == "" || len(r.Operations) != 0 || !bytes.Equal(r.Markdown, local) {
		t.Fatalf("protected deletion %#v %v", r, e)
	}
}
func TestReadableInsertionConcurrencyAndDeferred(t *testing.T) {
	base := readable(t, readableFixture)
	local := append([]byte("new local\n\n"), base.Markdown...)
	remote := readable(t, strings.Replace(readableFixture, `<p id="tail">after</p>`, `<p id="remote">remote addition</p><p id="tail">after</p>`, 1))
	r, e := PlanStepWithBase(base.Markdown, local, base, remote)
	if e != nil || r.ConflictReason != "" || len(r.Operations) != 1 || !bytes.Contains(r.Markdown, []byte("new local")) || !bytes.Contains(r.Markdown, []byte("remote addition")) {
		t.Fatalf("independent inserts %#v %v", r, e)
	}
	local = append([]byte("first new\n\nsecond new\n\n"), base.Markdown...)
	r, e = PlanStepWithBase(base.Markdown, local, base, base)
	if e != nil || r.ConflictReason != "" || len(r.Operations) != 1 || !r.Deferred || r.Operations[0].AfterBlockID != "0" || !bytes.Equal(r.Markdown, local) {
		t.Fatalf("deferred chain %#v %v", r, e)
	}
	// Apply the first insertion with a server-generated identity, then replan.
	after := readable(t, strings.Replace(readableFixture, `<p id="intro">before</p>`, `<p id="generated">first new</p><p id="intro">before</p>`, 1))
	next, e := PlanStepWithBase(after.Markdown, local, after, after)
	if e != nil || next.ConflictReason != "" || len(next.Operations) != 1 || next.Operations[0].AfterBlockID != "generated" {
		t.Fatalf("replan current anchor %#v %v", next, e)
	}
}

func TestReadableOrdinaryStructuralReadback(t *testing.T) {
	base := readable(t, readableFixture)
	local := bytes.Replace(base.Markdown, []byte("</table>"), []byte("</table>\n\nnew paragraph"), 1)
	r, e := PlanStepWithBase(base.Markdown, local, base, base)
	if e != nil || r.ConflictReason != "" || len(r.Operations) != 1 {
		t.Fatalf("plan %#v %v", r, e)
	}
	after := readable(t, strings.Replace(readableFixture, "</table>", `</table><p id="new">new paragraph</p>`, 1))
	if err := VerifyOperation(base, after, r.Operations[0]); err != nil {
		t.Fatalf("insert readback %v", err)
	}
	local = bytes.Replace(base.Markdown, []byte("before\n\n"), nil, 1)
	r, e = PlanStepWithBase(base.Markdown, local, base, base)
	if e != nil || r.ConflictReason != "" || len(r.Operations) != 1 {
		t.Fatalf("plan %#v %v", r, e)
	}
	after = readable(t, strings.Replace(readableFixture, `<p id="intro">before</p>`, "", 1))
	if err := VerifyOperation(base, after, r.Operations[0]); err != nil {
		t.Fatalf("delete readback %v", err)
	}
}

func TestReadableRepeatedMediaTokenDifferentExtensions(t *testing.T) {
	d := readable(t, `<img id="a" src="same-token" name="a.png" mime="image/png" alt="first"/><table id="t"><tbody><tr><td><img id="b" src="same-token" name="b.jpg" mime="image/jpeg" alt="second"/><img id="c" src="same-token" name="copy.png" mime="image/png" alt="repeat"/></td></tr></tbody></table>`)
	if len(d.Assets) != 2 {
		t.Fatalf("every distinct emitted asset path must be granted: %#v", d.Assets)
	}
	for _, a := range d.Assets {
		if a.Token != "same-token" || !bytes.Contains(d.Markdown, []byte(`src="`+a.Path+`"`)) {
			t.Fatalf("asset does not match emitted image: %#v", a)
		}
	}
	if d.Assets[0].Name != "a.png" || d.Assets[0].MIME != "image/png" || d.Assets[1].Name != "b.jpg" || d.Assets[1].MIME != "image/jpeg" {
		t.Fatalf("first metadata per path was not retained: %#v", d.Assets)
	}
}

func TestManagedImageDotPrefixIsEquivalentButReplacementIsRejected(t *testing.T) {
	for _, xml := range []string{readableFixture, `<p id="p">before</p><img id="image" src="media-token" mime="image/png"/>`} {
		base := readable(t, xml)
		local := bytes.ReplaceAll(base.Markdown, []byte(`src=".drivebridge-assets/`), []byte(`src="./.drivebridge-assets/`))
		result, err := PlanStepWithBase(base.Markdown, local, base, base)
		if err != nil || result.ConflictReason != "" || len(result.Operations) != 0 {
			t.Fatal("equivalent local image path blocked safe save", result.ConflictReason, err)
		}
		changed := bytes.ReplaceAll(local, []byte(`./.drivebridge-assets/`), []byte(`../.drivebridge-assets/`))
		result, err = PlanStepWithBase(base.Markdown, changed, base, base)
		if err == nil && result.ConflictReason == "" {
			t.Fatal("external image path mutation accepted")
		}
	}
}
