package markdown

import (
	"errors"
	"testing"
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
