package markdown

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/model"
)

func mustDecode(t *testing.T, s string) model.DocView {
	t.Helper()
	d, e := DecodeWithOptions([]byte(s), "7", Options{ProjectionVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func merged(t *testing.T, b, l string, r model.DocView) model.MergeResult {
	t.Helper()
	m, e := Merge([]byte(b), []byte(l), r)
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func noConflict(t *testing.T, m model.MergeResult) {
	t.Helper()
	if m.ConflictReason != "" {
		t.Fatalf("conflict %s", m.ConflictReason)
	}
}
func conflict(t *testing.T, m model.MergeResult) {
	t.Helper()
	if m.ConflictReason == "" || len(m.Operations) != 0 {
		t.Fatalf("unsafe result %+v", m)
	}
}
func payload(t *testing.T, m model.MergeResult) model.OperationPayload {
	t.Helper()
	if len(m.Operations) != 1 {
		t.Fatalf("operations %+v", m)
	}
	var p model.OperationPayload
	if e := json.Unmarshal(m.Operations[0].Payload, &p); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestLiteralAngleTagNotLost(t *testing.T) {
	d := mustDecode(t, `<title id="doc">not body</title><p id="p">中文 *星号*、x_y、a[b]、&lt;tag&gt;、C:\Users\test。</p>`)
	if bytes.Contains(d.Markdown, []byte("not body")) {
		t.Fatal("title leaked")
	}
	m := merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "中文", "修改", 1), d)
	noConflict(t, m)
	p := payload(t, m)
	if !strings.Contains(p.XML, `&lt;tag&gt;`) || !strings.Contains(p.XML, `C:\Users\test。`) {
		t.Fatal(p.XML)
	}
}
func TestCodeAndBackslashRoundtrip(t *testing.T) {
	d := mustDecode(t, "<pre id=\"code\" lang=\"go\" caption=\"&#xA;\" wrap=\"false\"><code>\n\tC:\\tmp &lt;tag&gt;<br/>```<br/></code></pre>")
	x, e := Encode(d.Markdown)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(x, []byte("C:\\tmp &lt;tag&gt;")) || !bytes.Contains(x, []byte("```<br/>")) {
		t.Fatalf("%s", x)
	}
	m := merged(t, string(d.Markdown), string(d.Markdown), d)
	noConflict(t, m)
	if !bytes.Equal(m.Markdown, d.Markdown) || len(m.Operations) != 0 {
		t.Fatal(m)
	}
}
func TestUTF8SourceRanges(t *testing.T) {
	src := []byte("# 中文😀\r\n\r\n- 第一\r\n- 第二\r\n\r\n~~~go\r\n汉字\r\n~~~\r\n\r\n末尾")
	a, e := parseMarkdown(src, nil)
	if e != nil {
		t.Fatal(e)
	}
	want := []string{"# 中文😀\r\n", "- 第一\r\n", "- 第二\r\n", "~~~go\r\n汉字\r\n~~~\r\n", "末尾"}
	if len(a) != len(want) {
		t.Fatalf("%+v", a)
	}
	for i, v := range a {
		got := src[v.Start:v.End]
		if string(got) != want[i] || !utf8.Valid(got) {
			t.Fatalf("%d %q", i, got)
		}
	}
}
func TestDisjointEditsMerge(t *testing.T) {
	r := mustDecode(t, `<p id="p">alpha beta GAMMA</p>`)
	m := merged(t, "alpha beta gamma\n", "ALPHA beta gamma\n", r)
	noConflict(t, m)
	if string(m.Markdown) != "ALPHA beta GAMMA\n" {
		t.Fatalf("%q", m.Markdown)
	}
	if payload(t, m).XML != "<p>ALPHA beta GAMMA</p>" {
		t.Fatal(m)
	}
}
func TestEqualEditsDeduplicate(t *testing.T) {
	r := mustDecode(t, `<p id="p">new</p>`)
	m := merged(t, "old\n", "new\n", r)
	noConflict(t, m)
	if len(m.Operations) != 0 {
		t.Fatal(m)
	}
}
func TestSameInsertionConflict(t *testing.T) {
	conflict(t, merged(t, "ab\n", "aLb\n", mustDecode(t, `<p id="p">aRb</p>`)))
}
func TestDeleteVsEditConflict(t *testing.T) {
	conflict(t, merged(t, "old\n", "", mustDecode(t, `<p id="p">new</p>`)))
}
func TestRepeatedBlocksConflict(t *testing.T) {
	conflict(t, merged(t, "same\n\nsame\n", "edited\n\nsame\n", mustDecode(t, `<p id="a">same</p><p id="b">same</p>`)))
}
func TestTableBothChangedConflict(t *testing.T) {
	base := "| A | B |\n| --- | --- |\n| one | two |\n"
	r := mustDecode(t, `<table id="t"><thead><tr><th><p>A</p></th><th><p>B</p></th></tr></thead><tbody><tr><td><p>one</p></td><td><p>remote</p></td></tr></tbody></table>`)
	conflict(t, merged(t, base, strings.Replace(base, "one", "local", 1), r))
}
func TestOpaqueChangedBlocksUpload(t *testing.T) {
	raw := `<callout id="opaque" background-color="rgb(1,2,3)"><p id="child">hello &amp; world</p></callout>`
	d, e := DecodeWithOptions([]byte(`<p id="p">before</p>`+raw), "1", Options{ProjectionVersion: 1, SourceURL: "https://example.com/doc", ReferenceMap: json.RawMessage(`{"x":1}`)})
	if e != nil {
		t.Fatal(e)
	}
	if !d.Blocks[1].Opaque || string(d.Blocks[1].RawXML) != raw || !bytes.Contains(d.Markdown, []byte("https://example.com/doc")) {
		t.Fatal(d)
	}
	m := merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "before", "after", 1), d)
	noConflict(t, m)
	if m.Operations[0].BlockID != "p" {
		t.Fatal(m)
	}
	conflict(t, merged(t, string(d.Markdown), "before\n", d))
	conflict(t, merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "Unsupported", "Changed", 1), d))
}
func TestEncodeSubsetAndUnsafe(t *testing.T) {
	for _, s := range []string{"<script>bad</script>", "![image](https://x)", "- [x] task", "```mermaid\na-->b\n```", "$$x+y$$", "$x+y$"} {
		if x, e := Encode([]byte(s)); e == nil {
			t.Errorf("accepted %q as %s", s, x)
		}
	}
	x, e := Encode([]byte("# Head\n\n**bold** *em* `code` [label](https://example.com/?x=1&y=2)\n\n> quote\n\n- one\n  - nested\n\n1. first\n2. second\n\n---\n"))
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"<h1>", "<b>bold</b>", "<em>em</em>", "<blockquote>", "<ul>", "<ol>", "<hr/>"} {
		if !bytes.Contains(x, []byte(s)) {
			t.Errorf("missing %s in %s", s, x)
		}
	}
	x, e = Encode(nil)
	if e != nil || len(x) != 0 {
		t.Fatal(string(x), e)
	}
}
func TestSanitizedFixtureUntouchedRaw(t *testing.T) {
	b, e := os.ReadFile("testdata/subset-fixtures.json")
	if e != nil {
		t.Fatal(e)
	}
	var fixtures []struct{ Label, XML string }
	if e = json.Unmarshal(b, &fixtures); e != nil {
		t.Fatal(e)
	}
	d := mustDecode(t, fixtures[1].XML)
	m := merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "Heading", "Heading revised", 1), d)
	noConflict(t, m)
	p := payload(t, m)
	if m.Operations[0].BlockID != "synthetic_block_3" || p.XML != "<h1>Heading revised</h1>" {
		t.Fatal(m, string(p.XML))
	}
	for _, b := range d.Blocks {
		if !bytes.Contains(d.XML, b.RawXML) {
			t.Fatal("raw source lost")
		}
	}
	m = merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "first", "first revised", 1), d)
	noConflict(t, m)
	if m.Operations[0].BlockID != "synthetic_block_8" || payload(t, m).XML != "<li>first revised</li>" {
		t.Fatal(m)
	}
}

func TestUnsafeShapesRemainOpaque(t *testing.T) {
	for _, x := range []string{`<p id="p"><b> bold </b></p>`, `<p id="p"><em></em></p>`, `<p id="p" color="red">red</p>`, `<table id="p"><thead><tr><th colspan="2"><p>x</p></th></tr></thead></table>`} {
		d := mustDecode(t, x)
		if len(d.Blocks) != 1 || !d.Blocks[0].Opaque || string(d.Blocks[0].RawXML) != x {
			t.Errorf("unsafe editable decode %s: %+v %q", x, d.Blocks, d.Markdown)
		}
	}
}
func TestTableLiteralPipeAndCode(t *testing.T) {
	x := `<table id="t"><thead><tr><th><p>A</p></th></tr></thead><tbody><tr><td><p>a|b <code>c|d</code> C:\tmp</p></td></tr></tbody></table>`
	d := mustDecode(t, x)
	if d.Blocks[0].Opaque {
		t.Fatal("simple table was opaque")
	}
	out, e := Encode(d.Markdown)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(out, []byte(`a|b <code>c|d</code> C:\tmp`)) {
		t.Fatalf("%s => %s", d.Markdown, out)
	}
}
func TestEntityAndEscapedSyntax(t *testing.T) {
	d := mustDecode(t, `<p id="p">&amp;lt;tag&amp;gt; &amp;amp; &lt;tag&gt; $5</p>`)
	x, e := Encode(d.Markdown)
	if e != nil {
		t.Fatal(e)
	}
	if string(x) != `<p>&amp;lt;tag&amp;gt; &amp;amp; &lt;tag&gt; $5</p>` {
		t.Fatalf("%q => %s", d.Markdown, x)
	}
}
func TestListAutoAndNested(t *testing.T) {
	for _, x := range []string{`<ol><li id="a" seq="auto">one</li><li id="b" seq="auto">two</li></ol>`, `<ul><li id="a">one<ul><li id="nested">child</li></ul></li><li id="b">two</li></ul>`} {
		d := mustDecode(t, x)
		m := merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "two", "second", 1), d)
		noConflict(t, m)
		if m.Operations[0].BlockID != "b" {
			t.Fatal(m)
		}
	}
}
func TestKnownMetadataRetained(t *testing.T) {
	d := mustDecode(t, `<pre id="p" caption="&#xA;" wrap="false" lang="go"><code>old<br/></code></pre>`)
	m := merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "old", "new", 1), d)
	noConflict(t, m)
	p := payload(t, m)
	if !strings.Contains(p.XML, `caption="&#xA;"`) || !strings.Contains(p.XML, `wrap="false"`) || strings.Contains(p.XML, `id=`) {
		t.Fatal(p.XML)
	}
	d = mustDecode(t, `<table id="t"><colgroup><col width="215"/></colgroup><thead><tr><th vertical-align="top"><p id="h">A</p></th></tr></thead><tbody><tr><td vertical-align="top"><p id="v">old</p></td></tr></tbody></table>`)
	m = merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "old", "new", 1), d)
	noConflict(t, m)
	p = payload(t, m)
	if !strings.Contains(p.XML, `width="215"`) || !strings.Contains(p.XML, `vertical-align="top"`) || strings.Contains(p.XML, `id=`) {
		t.Fatal(p.XML)
	}
}
func TestMinimalInsertionDeletionAndSourceStyle(t *testing.T) {
	d := mustDecode(t, `<p id="a">alpha</p><p id="b">beta</p>`)
	m := merged(t, "alpha\r\n\r\nbeta", "ALPHA\r\n\r\nbeta", d)
	noConflict(t, m)
	if string(m.Markdown) != "ALPHA\r\n\r\nbeta" {
		t.Fatalf("%q", m.Markdown)
	}
	p := payload(t, m)
	if m.Operations[0].BlockID != "a" || p.BeforeHash == "" || p.AfterHash == "" {
		t.Fatal(m)
	}
	m = merged(t, string(d.Markdown), string(d.Markdown)+"\nnew\n", d)
	noConflict(t, m)
	if m.Operations[0].Kind != model.OperationBlockInsertAfter || m.Operations[0].AfterBlockID != "b" {
		t.Fatal(m)
	}
	m = merged(t, string(d.Markdown), "alpha\n", d)
	noConflict(t, m)
	if m.Operations[0].Kind != model.OperationBlockDelete || m.Operations[0].BlockID != "b" {
		t.Fatal(m)
	}
	m = merged(t, string(d.Markdown), "new\n\n"+string(d.Markdown), d)
	noConflict(t, m)
	if m.Operations[0].AfterBlockID != "0" || m.Operations[0].Kind != model.OperationBlockInsertAfter {
		t.Fatal(m)
	}
	conflict(t, merged(t, string(d.Markdown), "beta\n\nalpha\n", d))
}
func TestMalformedAndLimit(t *testing.T) {
	for _, x := range []string{`<p>x`, `<fragment><p>x</p></fragment>`, `<!DOCTYPE x><p>x</p>`, `<p>x</p>outside`} {
		if _, e := Decode([]byte(x), ""); e == nil {
			t.Errorf("accepted %s", x)
		}
	}
	for _, b := range [][]byte{{0xff}, bytes.Repeat([]byte("x"), MaxMarkdownBytes+1), []byte("a\x00b")} {
		if _, e := Encode(b); e == nil {
			t.Fatal("invalid content accepted")
		}
		m, e := Merge(nil, b, model.DocView{})
		if e != nil || m.ConflictReason == "" || len(m.Operations) != 0 {
			t.Fatal(m, e)
		}
	}
}

func TestCodeTerminatorProjection(t *testing.T) {
	for _, code := range []string{"one", "one<br/>two", "one<br/>", "one<br/>two<br/>"} {
		d := mustDecode(t, `<pre id="p" lang="go"><code>`+code+`</code></pre>`)
		if d.Blocks[0].Opaque {
			t.Fatalf("ordinary code became opaque: %s", code)
		}
		local := strings.Replace(string(d.Markdown), "one", "ONE", 1)
		m := merged(t, string(d.Markdown), local, d)
		noConflict(t, m)
		p := payload(t, m)
		want := strings.Replace(code, "one", "ONE", 1)
		if !strings.Contains(p.XML, "<code>"+want+"</code>") {
			t.Fatalf("%s => %s", code, p.XML)
		}
		after := mustDecode(t, strings.Replace(p.XML, "<pre ", `<pre id="new" `, 1))
		if p.BeforeHash != d.Blocks[0].Hash || p.AfterHash != after.Blocks[0].Hash {
			t.Fatal("hash ignores actual terminator", p, d.Blocks, after.Blocks)
		}
		m = merged(t, string(d.Markdown), string(d.Markdown), d)
		noConflict(t, m)
		if len(m.Operations) != 0 || string(d.Blocks[0].RawXML) != `<pre id="p" lang="go"><code>`+code+`</code></pre>` {
			t.Fatal("unchanged code touched")
		}
	}
	base := mustDecode(t, `<pre id="p"><code>alpha beta gamma</code></pre>`)
	remote := mustDecode(t, `<pre id="p"><code>alpha beta GAMMA</code></pre>`)
	m := merged(t, string(base.Markdown), strings.Replace(string(base.Markdown), "alpha", "ALPHA", 1), remote)
	noConflict(t, m)
	if !strings.Contains(payload(t, m).XML, "<code>ALPHA beta GAMMA</code>") {
		t.Fatal(m)
	}
}
func TestRemoteRulesAndReferenceDefinitions(t *testing.T) {
	d := mustDecode(t, `<hr id="r"/>`)
	if d.Blocks[0].Opaque {
		t.Fatal("rule opaque")
	}
	if _, e := Encode([]byte("---\n")); e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"text\n\n[id]: https://example.com\n", "[label][id]\n\n[id]: https://example.com\n"} {
		if _, e := Encode([]byte(s)); e == nil {
			t.Fatal("reference definition silently consumed")
		}
	}
}

func TestNoLFTerminalBlankLineEditConflicts(t *testing.T) {
	d := mustDecode(t, `<pre id="p"><code>one</code></pre>`)
	for _, s := range []string{"```\none\n\n```\n", "```\n\n```\n", "```\n```\n"} {
		conflict(t, merged(t, string(d.Markdown), s, d))
	}
	d = mustDecode(t, `<pre id="p"><code>one<br/></code></pre>`)
	m := merged(t, string(d.Markdown), "```\none\n\n```\n", d)
	noConflict(t, m)
	if !strings.Contains(payload(t, m).XML, "one<br/><br/>") {
		t.Fatal(m)
	}
}
func TestOrderedRenumberingNotIgnored(t *testing.T) {
	d := mustDecode(t, `<ol><li id="a" seq="1">one</li><li id="b" seq="2">two</li></ol>`)
	conflict(t, merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "1.", "3.", 1), d))
}
func TestInvalidSidecarAndDuplicateIDs(t *testing.T) {
	d := mustDecode(t, `<callout id="p">opaque</callout>`)
	d.Blocks[0].End = -1
	conflict(t, merged(t, "text\n", "text\n", d))
	if _, e := Decode([]byte(`<p id="same">one</p><p id="same">two</p>`), ""); e == nil {
		t.Fatal("duplicate current IDs accepted")
	}
}

func TestOpaqueHashDescribesContentNotRemoteID(t *testing.T) {
	a := mustDecode(t, `<callout id="a" color="red"><p id="c">first</p></callout>`)
	b := mustDecode(t, `<callout id="b" color="red"><p id="d">first</p></callout>`)
	c := mustDecode(t, `<callout id="b" color="red"><p id="d">second</p></callout>`)
	if a.Blocks[0].Hash != b.Blocks[0].Hash || b.Blocks[0].Hash == c.Blocks[0].Hash {
		t.Fatal("opaque hash ignores contents or includes IDs")
	}
}
func TestRepeatedCodeLinesDisjointAndLocalStyle(t *testing.T) {
	code := "start\n" + strings.Repeat("same\n", 205) + "end\n"
	base := mustDecode(t, `<pre id="p"><code>`+strings.ReplaceAll(code, "\n", "<br/>")+`</code></pre>`)
	remote := mustDecode(t, `<pre id="p"><code>`+strings.ReplaceAll(strings.Replace(code, "end", "END", 1), "\n", "<br/>")+`</code></pre>`)
	local := strings.Replace(string(base.Markdown), "start", "START", 1)
	m := merged(t, string(base.Markdown), local, remote)
	noConflict(t, m)
	p := payload(t, m)
	if !strings.Contains(p.XML, "START<br/>") || !strings.Contains(p.XML, "END<br/>") {
		t.Fatal(p.XML)
	}
	d := mustDecode(t, `<p id="p">hello</p>`)
	m = merged(t, "hello\r\n", "hello\r\n", d)
	noConflict(t, m)
	if string(m.Markdown) != "hello\r\n" {
		t.Fatalf("%q", m.Markdown)
	}
}

func TestCorruptCurrentShapeCannotAuthorizePatch(t *testing.T) {
	d := mustDecode(t, `<p id="p">old</p>`)
	d.Blocks[0].RawXML = []byte(`<p id="p" color="red">old</p>`)
	conflict(t, merged(t, string(d.Markdown), "new\n", d))
	if _, e := Decode([]byte(`<p id="a" id="b">text</p>`), ""); e == nil {
		t.Fatal("duplicate attributes accepted")
	}
}
func TestConflictHunkPreservesMissingFinalNewline(t *testing.T) {
	m := merged(t, "a\nb", "a\nLOCAL", mustDecode(t, "<p id=\"p\">a\nREMOTE</p>"))
	conflict(t, m)
	if len(m.Hunks) != 1 || strings.Join(m.Hunks[0].Base, "") != "a\nb" || strings.Join(m.Hunks[0].Local, "") != "a\nLOCAL" {
		t.Fatalf("missing exact conflict context: %+v", m.Hunks)
	}
}

func TestUnsupportedListChildKeepsContainerOpaque(t *testing.T) {
	x := `<ul><li id="a">ordinary</li><li id="b"><image token="resource"/></li></ul>`
	d := mustDecode(t, `<p id="p">before</p>`+x)
	if len(d.Blocks) != 2 || !d.Blocks[1].Opaque || string(d.Blocks[1].RawXML) != x {
		t.Fatal(d.Blocks)
	}
	m := merged(t, string(d.Markdown), strings.Replace(string(d.Markdown), "before", "after", 1), d)
	noConflict(t, m)
	if payload(t, m).XML != "<p>after</p>" {
		t.Fatal(m)
	}
}

func TestDocumentStartSentinelDoesNotTargetTitle(t *testing.T) {
	d := mustDecode(t, `<title id="document">Title</title><p id="p">body</p>`)
	m := merged(t, string(d.Markdown), "first\n\n"+string(d.Markdown), d)
	noConflict(t, m)
	p := payload(t, m)
	if m.Operations[0].AfterBlockID != "0" || m.Operations[0].BlockID != "" || p.XML != "<p>first</p>" {
		t.Fatal(m)
	}
	d = mustDecode(t, `<title id="document">Title</title>`)
	m = merged(t, "", "first", d)
	noConflict(t, m)
	if m.Operations[0].AfterBlockID != "0" {
		t.Fatal(m)
	}
}

func TestOpaqueAmbiguityReportsUnsupported(t *testing.T) {
	d := mustDecode(t, `<p id="p">ordinary</p><callout id="c">unknown</callout>`)
	local := string(d.Markdown[d.Blocks[1].Start:d.Blocks[1].End]) + "\nordinary\n"
	m := merged(t, string(d.Markdown), local, d)
	conflict(t, m)
	if m.ConflictReason != "unsupported_content" {
		t.Fatalf("opaque relocation reason %s", m.ConflictReason)
	}
}
