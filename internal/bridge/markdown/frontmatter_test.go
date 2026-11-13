package markdown

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestYAMLFrontMatterRoundTripAndProjectionVersions(t *testing.T) {
	for _, metadata := range []string{"---\nname: synthetic\nextra: '<br> PLACEHOLDER & <x>'\n---\n", "---\r\nname: synthetic\r\n# preserved comment\r\n---\r\n", "---\nname: synthetic\n---"} {
		body := ""
		if strings.HasSuffix(metadata, "\n") {
			body = "\n# Body\n\ntext\n"
		}
		input := []byte(metadata + body)
		encoded, err := Encode(input)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `caption="DriveBridge YAML front matter v1"`) {
			t.Fatalf("missing marker: %s", encoded)
		}
		// Cloud services may omit the display caption; code content is authoritative.
		readback := bytes.ReplaceAll(encoded, []byte(` caption="DriveBridge YAML front matter v1"`), nil)
		view, err := Decode(readback, "1")
		if err != nil || view.ProjectionVersion != 3 || !bytes.HasPrefix(view.Markdown, []byte(strings.TrimSuffix(metadata, "\n"))) {
			t.Fatalf("decode metadata=%q got=%q version=%d err=%v", metadata, view.Markdown, view.ProjectionVersion, err)
		}
		again, err := Encode(view.Markdown)
		if err != nil || !bytes.Equal(encoded, again) {
			t.Fatalf("round trip changed metadata: %s => %s (%v)", encoded, again, err)
		}
		old, err := DecodeWithOptions(encoded, "1", Options{ProjectionVersion: 2})
		if err != nil || old.ProjectionVersion != 2 || bytes.HasPrefix(old.Markdown, []byte("---")) {
			t.Fatalf("projection2 changed: %+v %v", old, err)
		}
		repeated, err := DecodeWithOptions(encoded, "1", Options{ProjectionVersion: 2})
		if err != nil || !bytes.Equal(old.Markdown, repeated.Markdown) {
			t.Fatal("projection2 is unstable")
		}
	}
}

func TestYAMLFrontMatterProtectsMetadataAndKeepsSourceOffsets(t *testing.T) {
	installTestRules(t, ConversionRule{ID: "synthetic", Source: "PLACEHOLDER", Target: "DONE", Context: "paragraph", Match: "literal"})
	metadata := "---\nname: PLACEHOLDER<br>\n---\n"
	input := []byte(metadata + "\nPLACEHOLDER\n")
	normalized, err := NormalizeForMerge(input)
	if err != nil || string(normalized) != metadata+"\nDONE\n" {
		t.Fatalf("metadata modified: %q %v", normalized, err)
	}
	atoms, err := parseMarkdown(input, nil)
	if err != nil || len(atoms) != 2 {
		t.Fatalf("atoms: %+v %v", atoms, err)
	}
	if atoms[0].Start != 0 || atoms[0].End != len(metadata) || atoms[0].Kind != "pre" || atoms[1].Start != len(metadata)+1 {
		t.Fatalf("wrong spans: %+v", atoms)
	}
}

func TestYAMLFrontMatterBoundariesAndMalformedInput(t *testing.T) {
	for _, input := range []string{"---\n", "---\n\nparagraph\n", "---\nparagraph\n---\n", "+++\nname: synthetic\n+++\n", "```yaml\n---\nname: synthetic\n---\n```\n"} {
		encoded, err := Encode([]byte(input))
		if err != nil || strings.Contains(string(encoded), "DriveBridge YAML front matter v1") {
			t.Fatalf("not front matter %q: %s %v", input, encoded, err)
		}
	}
	for _, input := range []string{"---\nname: synthetic\n", "---\nname: [broken\n---\n"} {
		_, err := Encode([]byte(input))
		var content *ContentError
		if !errors.As(err, &content) || !strings.Contains(content.Detail, "YAML front matter") {
			t.Fatalf("missing useful error: %q %v", input, err)
		}
	}
}

func TestYAMLFrontMatterRuleIsFixedAndAutomaticallyAdded(t *testing.T) {
	rules, err := PureParseConversionRules([]byte(`{"version":1,"rules":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var doc conversionRuleDocument
	if err = json.Unmarshal(rules.EffectiveDocumentJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	var structural ConversionRule
	for _, rule := range doc.Rules {
		if rule.ID == "yaml-front-matter" {
			structural = rule
		}
	}
	if structural.Target != "yaml_code_block" || structural.Match != "front_matter" || structural.Context != "document" {
		t.Fatalf("missing structural default: %+v", structural)
	}
	for _, rule := range []ConversionRule{{ID: "plain-table-resize", Source: "table", Target: "delete", Context: "document", Match: "table_resize"}, {ID: "yaml-front-matter", Source: "---", Target: "", Context: "document", Match: "front_matter"}, {ID: "custom-yaml", Source: "---", Target: "yaml_code_block", Context: "document", Match: "front_matter"}} {
		b, _ := json.Marshal(conversionRuleDocument{Version: 1, Rules: []ConversionRule{rule}})
		if _, err := PureParseConversionRules(b); err == nil {
			t.Fatalf("accepted unsafe structural rule %+v", rule)
		}
	}
}

func TestYAMLFrontMatterMetadataEditAndMarkerRemoval(t *testing.T) {
	xml, err := Encode([]byte("---\nname: synthetic\n---\n\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	xml = bytes.Replace(xml, []byte("<pre "), []byte(`<pre id="metadata" `), 1)
	xml = bytes.Replace(xml, []byte("<p>"), []byte(`<p id="body">`), 1)
	before, err := Decode(xml, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range []string{strings.Replace(string(before.Markdown), "synthetic", "changed", 1), "```yaml\n---\nname: synthetic\n---\n```\n\nbody\n"} {
		result, err := PlanStepWithBase(before.Markdown, []byte(local), before, before)
		if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
			t.Fatalf("edit failed: %+v %v", result, err)
		}
		p := payload(t, result)
		if strings.Count(p.XML, `caption=`) > 1 {
			t.Fatalf("duplicate marker: %s", p.XML)
		}
		removing := strings.HasPrefix(local, "```")
		if strings.Contains(p.XML, yamlFrontMatterCaption) == removing {
			t.Fatalf("wrong marker on replacement: %s", p.XML)
		}
		observedXML := strings.Replace(string(before.XML), string(before.Blocks[0].RawXML), strings.Replace(p.XML, "<pre", `<pre id="new-metadata"`, 1), 1)
		after, err := DecodeWithOptions([]byte(observedXML), "2", Options{ProjectionVersion: 3})
		if err != nil {
			t.Fatal(err)
		}
		if err = VerifyOperation(before, after, result.Operations[0]); err != nil {
			t.Fatalf("readback verification failed: %v", err)
		}
	}
}

func TestYAMLFrontMatterRequiresCodeContentMarker(t *testing.T) {
	plain := []byte(`<pre lang="yaml" caption="DriveBridge YAML front matter v1"><code>---<br/>name: synthetic<br/>---<br/></code></pre>`)
	view, err := Decode(plain, "1")
	if err != nil || view.ProjectionVersion != 2 || !bytes.HasPrefix(view.Markdown, []byte("```yaml\n")) {
		t.Fatalf("caption changed ordinary code meaning: %+v %v", view, err)
	}
	invalid := []byte(`<pre lang="yaml"><code># DriveBridge YAML front matter v1<br/>name: synthetic<br/></code></pre>`)
	_, err = Decode(invalid, "1")
	var content *ContentError
	if !errors.As(err, &content) || !strings.Contains(content.Detail, "YAML front matter") {
		t.Fatalf("invalid reserved content marker accepted: %v", err)
	}
}
