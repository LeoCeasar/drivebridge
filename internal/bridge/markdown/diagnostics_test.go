package markdown

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDiagnoseContentLocatesUnsupportedInline(t *testing.T) {
	body := []byte("# Heading\n\nfirst line\nwrong <span>value</span>\n")
	_, err := Encode(body)
	d := DiagnoseContent(body, err)
	if d == nil || d.Line != 4 || d.Kind != "unsupported_html" || d.Text != "wrong <span>value</span>" {
		t.Fatalf("diagnostic: %+v err=%v", d, err)
	}
}
func TestDiagnoseContentDoesNotInventSourceForTransport(t *testing.T) {
	if d := DiagnoseContent([]byte("ok\n"), errors.New("network")); d != nil {
		t.Fatalf("unexpected: %+v", d)
	}
}
func TestDiagnoseContentLocatesFrontMatter(t *testing.T) {
	body := []byte("---\nname: test\n")
	_, err := Encode(body)
	d := DiagnoseContent(body, err)
	if d == nil || d.Line != 1 || d.Kind != "front_matter" {
		t.Fatalf("diagnostic: %+v", d)
	}
}
func TestPlanFailureDoesNotInventLocalSourceLine(t *testing.T) {
	current := mustDecode(t, `<title id="doc">Test</title><p id="p">text</p>`)
	d := DiagnoseContent([]byte("replacement\n"), ExplainPlanFailure([]byte("replacement\n"), current, "ambiguous_mapping"))
	if d == nil || d.Kind != "ambiguous_mapping" || d.Line != 0 || d.Side != "" {
		t.Fatalf("diagnostic: %+v", d)
	}
}
func TestDiagnoseDoesNotReuseDifferentInputOffsets(t *testing.T) {
	_, err := Encode([]byte("first\n\nsecond\n\nthird <span>bad</span>\n"))
	d := DiagnoseContent([]byte("valid\n"), err)
	if d == nil || d.Line != 0 || d.Text != "" {
		t.Fatalf("attributed another source: %+v", d)
	}
}

func TestRemoteYAMLErrorCannotPointAtValidLocalBody(t *testing.T) {
	d := DiagnoseContent([]byte("# Valid local document\n"), unsupported("malformed YAML front matter code content"))
	if d == nil || d.Line != 0 || d.Side != "" || d.Text != "" {
		t.Fatalf("fabricated source: %+v", d)
	}
}

func TestAmbiguousMappingShowsRepeatedCloudLocations(t *testing.T) {
	current := mustDecode(t, `<title id="doc">Test</title><h2 id="h1">广州</h2><p id="a">状态：待补充</p><p id="middle">其他内容</p><h2 id="h2">银川</h2><p id="b">状态：待补充</p>`)
	desired := bytes.Replace(current.Markdown, []byte("其他内容"), []byte("已更新的其他内容"), 1)
	d := DiagnoseContent([]byte("newer unrelated local save"), ExplainPlanFailure(desired, current, "ambiguous_mapping"))
	if d == nil || d.Line != 0 || d.Side != "" || len(d.Locations) != 2 || d.LocationCount != 2 {
		t.Fatalf("diagnostic: %+v", d)
	}
	for i, want := range []string{"广州", "银川"} {
		loc := d.Locations[i]
		if loc.Side != "remote" || loc.Text != "状态：待补充" || loc.Section != want || loc.Kind != "p" || loc.Line <= 0 || loc.EndLine < loc.Line {
			t.Fatalf("location: %+v", loc)
		}
		lines := strings.Split(string(current.Markdown), "\n")
		if lines[loc.Line-1] != "状态：待补充" {
			t.Fatal("line points outside captured cloud text", loc)
		}
	}
}

func TestAmbiguousMappingLocationsAreBoundedAndUnicodeSafe(t *testing.T) {
	raw := `<title id="doc">Test</title>`
	for i := 0; i < 20; i++ {
		raw += fmt.Sprintf(`<p id="p%d">%s</p>`, i, strings.Repeat("中文字", 100))
	}
	current := mustDecode(t, raw)
	d := DiagnoseContent(nil, ExplainPlanFailure(append(bytes.Clone(current.Markdown), []byte("\nadded\n")...), current, "ambiguous_mapping"))
	if len(d.Locations) != 8 || d.LocationCount != 20 {
		t.Fatalf("unbounded: %+v", d)
	}
	for i, loc := range d.Locations {
		if !utf8.ValidString(loc.Text) || utf8.RuneCountInString(loc.Text) > 240 || i > 0 && loc.Line <= d.Locations[i-1].Line {
			t.Fatal(loc)
		}
	}
}

func TestAmbiguousMappingNewDuplicatesUseMergedSource(t *testing.T) {
	current := mustDecode(t, `<title id="doc">Test</title><p id="a">original</p>`)
	desired := []byte("# Test\n\nnew repeated\n\nnew repeated\n")
	d := DiagnoseContent([]byte("different editor contents"), ExplainPlanFailure(desired, current, "ambiguous_mapping"))
	if len(d.Locations) != 2 {
		t.Fatal(d)
	}
	for _, loc := range d.Locations {
		if loc.Side != "merged" || loc.Text != "new repeated" {
			t.Fatal("invented local attribution", loc)
		}
	}
}

func TestAmbiguousMappingTableSnippetIncludesRows(t *testing.T) {
	current := mustDecode(t, `<title id="doc">Test</title><table id="t1"><thead><tr><th><p>A</p></th><th><p>B</p></th></tr></thead><tbody><tr><td><p>same</p></td><td><p>value</p></td></tr></tbody></table><p id="p">middle</p><table id="t2"><thead><tr><th><p>A</p></th><th><p>B</p></th></tr></thead><tbody><tr><td><p>same</p></td><td><p>value</p></td></tr></tbody></table>`)
	desired := bytes.Replace(current.Markdown, []byte("middle"), []byte("updated"), 1)
	d := DiagnoseContent(nil, ExplainPlanFailure(desired, current, "ambiguous_mapping"))
	if len(d.Locations) != 2 {
		t.Fatal(d)
	}
	for _, loc := range d.Locations {
		if loc.Kind != "table" || !strings.Contains(loc.Text, "same") || loc.EndLine <= loc.Line {
			t.Fatal(loc)
		}
	}
}

func TestMappingDiagnosisPreservesUnchangedRichTable(t *testing.T) {
	current := readable(t, `<p id="a">same</p><p id="b">same</p><table id="t"><tbody><tr><td><p id="x">first</p><p id="y">second</p></td></tr></tbody></table>`)
	desired := bytes.Replace(current.Markdown, []byte("same\n\n"), nil, 1)
	plan, err := PlanMergedStep(desired, current)
	if err != nil || plan.ConflictReason != "ambiguous_mapping" {
		t.Fatal("fixture", plan.ConflictReason, err)
	}
	d := DiagnoseContent(desired, ExplainPlanFailure(desired, current, plan.ConflictReason))
	if d.Kind != "ambiguous_mapping" || len(d.Locations) != 2 || d.Line != 0 {
		t.Fatal("untouched rich content blamed", d)
	}
	for _, loc := range d.Locations {
		if loc.Side != "remote" || loc.Text != "same" {
			t.Fatal(loc)
		}
	}
}

func TestMergedCandidatesExcludeCloudPlaceholders(t *testing.T) {
	for _, raw := range []string{`<p id="a"></p><p id="m">middle</p><p id="b"></p>`, `<callout id="a" color="red"><p>same</p></callout><p id="m">middle</p><callout id="b" color="blue"><p>same</p></callout>`} {
		current := readable(t, raw)
		desired := bytes.Replace(current.Markdown, []byte("middle"), []byte("new\n\nnew"), 1)
		plan, err := PlanMergedStep(desired, current)
		if err != nil || plan.ConflictReason != "ambiguous_mapping" {
			t.Fatal("fixture", plan.ConflictReason, err)
		}
		d := DiagnoseContent(desired, ExplainPlanFailure(desired, current, plan.ConflictReason))
		if d.Kind != "ambiguous_mapping" || len(d.Locations) != 2 || d.LocationCount != 2 {
			t.Fatal("placeholder reported as repeated user text", d)
		}
		for _, loc := range d.Locations {
			if loc.Side != "merged" || loc.Text != "new" {
				t.Fatal(loc)
			}
		}
	}
}

func TestMappingDiagnosisWithoutEvidenceDoesNotAssertRepeatedWords(t *testing.T) {
	current := mustDecode(t, `<ul><li id="a">aaa</li><li id="b">bbb</li><li id="c">ccc</li></ul>`)
	desired := []byte("- bbb\n- aaa\n- ccc\n")
	plan, err := PlanMergedStep(desired, current)
	if err != nil || plan.ConflictReason != "ambiguous_mapping" {
		t.Fatal("fixture", plan.ConflictReason, err)
	}
	d := DiagnoseContent(desired, ExplainPlanFailure(desired, current, plan.ConflictReason))
	if d.Kind != "ambiguous_mapping" || len(d.Locations) != 0 || strings.Contains(d.Message, "重复") {
		t.Fatal("invented repeated-text cause", d)
	}
}

func TestActualUnsupportedHTMLStillHasCapturedLocation(t *testing.T) {
	current := mustDecode(t, `<p id="a">supported</p>`)
	desired := []byte("# Title\n\nwrong <span>new unsupported</span>\n")
	d := DiagnoseContent(desired, ExplainPlanFailure(desired, current, "unsupported_content"))
	if d.Kind != "unsupported_html" || d.Line != 3 || !strings.Contains(d.Text, "new unsupported") {
		t.Fatal(d)
	}
}
