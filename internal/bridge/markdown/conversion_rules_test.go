package markdown

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func installTestRules(t *testing.T, extra ...ConversionRule) {
	t.Helper()
	rules := conversionRuleDocument{Version: 1, Rules: extra}
	data, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConversionRules(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := activeConversionRules.Load()
	activeConversionRules.Store(loaded)
	t.Cleanup(func() { activeConversionRules.Store(previous) })
}

func TestConversionRulesASTAndSafeTargets(t *testing.T) {
	installTestRules(t,
		ConversionRule{ID: "word", Source: "<wbr>", Target: "**and** & more", Context: "paragraph", Match: "exact"},
		ConversionRule{ID: "table-word", Source: "<wbr>", Target: " / ", Context: "table", Match: "exact"},
	)
	cases := []struct{ source, want string }{
		{"one<wbr>two", "<p>one<b>and</b> &amp; moretwo</p>"},
		{"<wbr>\n", "<p><b>and</b> &amp; more</p>"},
		{"`<wbr>` and \\<wbr>", "<p><code>&lt;wbr&gt;</code> and &lt;wbr&gt;</p>"},
		{"```html\n<wbr>\n```", "<pre lang=\"html\"><code>&lt;wbr&gt;<br/></code></pre>"},
		{"[x](https://example.com/%3Cwbr%3E)", "<p><a href=\"https://example.com/%3Cwbr%3E\">x</a></p>"},
		{"| H |\n| - |\n| one<wbr>two |", "<table><thead><tr><th><p>H</p></th></tr></thead><tbody><tr><td><p>one / two</p></td></tr></tbody></table>"},
		{"one<br>two", "<p>one<br/>two</p>"},
	}
	for _, tt := range cases {
		got, err := Encode([]byte(tt.source))
		if err != nil || string(got) != tt.want {
			t.Fatalf("%q => %q, %v; want %q", tt.source, got, err, tt.want)
		}
	}
}

func TestConversionRulesPlanAndVerify(t *testing.T) {
	installTestRules(t, ConversionRule{ID: "word", Source: "<wbr>", Target: " / ", Context: "paragraph", Match: "exact"})
	before, err := Decode([]byte(`<p id="p1">original</p>`), "1")
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(before.Markdown)
	result, err := PlanStep(original, []byte("one<wbr>two\n"), before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatalf("plan: %#v, %v", result, err)
	}
	var p model.OperationPayload
	if err := json.Unmarshal(result.Operations[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.XML, "one / two") {
		t.Fatalf("payload: %s", p.XML)
	}
	after, err := Decode([]byte(`<p id="new">one / two</p>`), "2")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyOperation(before, after, result.Operations[0]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Markdown, original) {
		t.Fatal("decoder checkpoint changed")
	}
}

func TestConversionRulesValidationAndMissingFile(t *testing.T) {
	if _, err := LoadConversionRules(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		`{"version":2,"rules":[]}`,
		`{"version":1,"rules":[{"id":"x","source":"<wbr>","target":"<script>alert(1)</script>","context":"paragraph","match":"exact"}]}`,
		`{"version":1,"rules":[{"id":"x","source":"hello","target":"x","context":"paragraph","match":"exact"}]}`,
		`{"version":1,"rules":[{"id":"x","source":"<wbr>","target":"[x](javascript:alert(1))","context":"paragraph","match":"exact"}]}`,
		`{"version":1,"rules":[{"id":"x","source":"<wbr>","target":"x","context":"anywhere","match":"regex"}]}`,
		`{"version":1,"rules":[],"typo":true}`,
		`{"version":1,"rules":[]} {}`,
	} {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConversionRules(path); err == nil {
			t.Fatalf("accepted invalid configuration: %s", data)
		}
	}
}

func TestConversionRulesManyAppendAndImmutableConfiguration(t *testing.T) {
	rules := make([]ConversionRule, 5000)
	for i := range rules {
		rules[i] = ConversionRule{ID: fmt.Sprintf("rule-%d", i), Source: fmt.Sprintf("<token%d>", i), Target: "okay", Context: "paragraph", Match: "exact"}
	}
	installTestRules(t, rules...)
	got, err := Encode([]byte("one<token4999>two"))
	if err != nil || string(got) != "<p>oneokaytwo</p>" {
		t.Fatalf("many rules: %q, %v", got, err)
	}
	current := activeConversionRules.Load()
	if err := ConfigureConversionRules(current); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureConversionRules(defaultConversionRules); err == nil {
		t.Fatal("active rules changed without restart")
	}
}

func TestConversionRulesBreakOverrideAndNewline(t *testing.T) {
	installTestRules(t,
		ConversionRule{ID: "simple-break-table", Source: "<br>", Target: " | ", Context: "table", Match: "simple_html_break"},
		ConversionRule{ID: "line", Source: "<wbr>", Target: "\n", Context: "paragraph", Match: "exact"},
		ConversionRule{ID: "emphasis", Source: "<mark>", Target: " _word_ ", Context: "paragraph", Match: "exact"},
	)
	cases := []struct{ source, want string }{
		{"one<wbr>two", "<p>one<br/>two</p>"},
		{"one\n\n<wbr>\n\ntwo", "<p>one</p><p>two</p>"},
		{"one<mark>two", "<p>one <em>word</em> two</p>"},
		{"| H |\n| - |\n| one<BR />two |", "<table><thead><tr><th><p>H</p></th></tr></thead><tbody><tr><td><p>one | two</p></td></tr></tbody></table>"},
	}
	for _, tt := range cases {
		got, err := Encode([]byte(tt.source))
		if err != nil || string(got) != tt.want {
			t.Fatalf("%q => %q, %v; want %q", tt.source, got, err, tt.want)
		}
	}
	if _, err := Encode([]byte("one<WBR>two")); err == nil {
		t.Fatal("exact matcher ignored case")
	}
}

func TestConversionRulesDefaultSnapshotIsFrozen(t *testing.T) {
	previous := activeConversionRules.Load()
	activeConversionRules.Store(nil)
	t.Cleanup(func() { activeConversionRules.Store(previous) })
	if err := ConfigureConversionRules(defaultConversionRules); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureConversionRules(&ConversionRules{}); err == nil {
		t.Fatal("accepted unvalidated snapshot")
	}
	data := DefaultConversionRulesJSON()
	data[0] = 'x'
	if DefaultConversionRulesJSON()[0] != '{' {
		t.Fatal("default template mutated")
	}
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"rules":[{"id":"x","source":"<wbr>","target":"okay","context":"paragraph","match":"exact"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	rules, err := LoadConversionRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfigureConversionRules(rules); !errors.Is(err, ErrConversionRulesRestart) {
		t.Fatalf("expected restart error: %v", err)
	}
	if activeConversionRules.Load() != defaultConversionRules {
		t.Fatal("failed configure changed active snapshot")
	}
}

func TestConversionRulesReadablePlannerKeepsProtectedResources(t *testing.T) {
	installTestRules(t, ConversionRule{ID: "word", Source: "<wbr>", Target: " / ", Context: "paragraph", Match: "exact"})
	before := readable(t, readableFixture)
	checkpoint := bytes.Clone(before.Markdown)
	local := bytes.Replace(checkpoint, []byte("before"), []byte("one<wbr>two"), 1)
	result, err := PlanStepWithBase(checkpoint, local, before, before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatalf("readable plan: %#v, %v", result, err)
	}
	var payload model.OperationPayload
	if err := json.Unmarshal(result.Operations[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.XML != "<p>one / two</p>" {
		t.Fatalf("payload %s", payload.XML)
	}
	after := readable(t, strings.Replace(readableFixture, `<p id="intro">before</p>`, `<p id="new">one / two</p>`, 1))
	if err := VerifyOperation(before, after, result.Operations[0]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Markdown, checkpoint) {
		t.Fatal("readable checkpoint changed")
	}
	// An HTML table projection remains protected as a whole; source rules never
	// rewrite its raw XML or turn a resource replacement into a safe paragraph.
	edited := bytes.Replace(local, []byte(`<img src=`), []byte(`<wbr><img src=`), 1)
	blocked, err := PlanStepWithBase(checkpoint, edited, before, before)
	if err != nil || blocked.ConflictReason == "" || len(blocked.Operations) != 0 {
		t.Fatalf("protected resource accepted: %#v, %v", blocked, err)
	}
}

func TestConversionRulesInvalidRecords(t *testing.T) {
	base := ConversionRule{ID: "word", Source: "<wbr>", Target: "okay", Context: "paragraph", Match: "exact"}
	for _, mutate := range []func(*ConversionRule){
		func(r *ConversionRule) { r.ID = "" },
		func(r *ConversionRule) { r.Match = "regex" },
		func(r *ConversionRule) { r.Context = "code" },
		func(r *ConversionRule) { r.Source = "<wbr><br>" },
		func(r *ConversionRule) { r.Source = "<wbr>\n" },
		func(r *ConversionRule) { r.Target = "![x](https://example.com/x)" },
		func(r *ConversionRule) { r.Target = "<img src=x>" },
		func(r *ConversionRule) { r.Target = "first\nsecond" },
		func(r *ConversionRule) { r.Target = "\x00" },
		func(r *ConversionRule) { r.Target = "\uFFFE" },
		func(r *ConversionRule) { r.Target = "# heading" },
		func(r *ConversionRule) { r.Target = strings.Repeat("x", 65537) },
	} {
		rule := base
		mutate(&rule)
		if _, err := compileConversionRules(conversionRuleDocument{Version: 1, Rules: []ConversionRule{rule}}); err == nil {
			t.Fatalf("accepted invalid rule %#v", rule)
		}
	}
	if _, err := compileConversionRules(conversionRuleDocument{Version: 1, Rules: []ConversionRule{base, base}}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	other := base
	other.ID = "other"
	if _, err := compileConversionRules(conversionRuleDocument{Version: 1, Rules: []ConversionRule{base, other}}); err == nil {
		t.Fatal("ambiguous matching source accepted")
	}
	path := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte(" "), maxConversionRulesBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConversionRules(path); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestConversionRulesRequireEveryFieldButAllowExplicitEmptyTarget(t *testing.T) {
	const record = `{"id":"simple-break-paragraph","source":"<br>","target":"\n","context":"paragraph","match":"simple_html_break"}`
	for _, field := range []string{"id", "source", "target", "context", "match"} {
		for _, null := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/null=%v", field, null), func(t *testing.T) {
				var r map[string]json.RawMessage
				if err := json.Unmarshal([]byte(record), &r); err != nil {
					t.Fatal(err)
				}
				if null {
					r[field] = json.RawMessage(`null`)
				} else {
					delete(r, field)
				}
				data, err := json.Marshal(map[string]any{"version": 1, "rules": []any{r}})
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "rules.json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := LoadConversionRules(path); err == nil || !strings.Contains(err.Error(), field) {
					t.Fatalf("expected required field %s error, got %v", field, err)
				}
			})
		}
	}
	path := filepath.Join(t.TempDir(), "rules.json")
	data := `{"version":1,"rules":[{"id":"simple-break-paragraph","source":"<br>","target":"","context":"paragraph","match":"simple_html_break"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	rules, err := LoadConversionRules(path)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := rules.lookup([]byte("<br>"), "paragraph")
	if !ok || target.xml != "" || target.spacing {
		t.Fatalf("explicit empty target: %#v, matched=%v", target, ok)
	}
}
