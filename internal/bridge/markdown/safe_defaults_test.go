package markdown

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestSafeDefaultConversionsEncode(t *testing.T) {
	cases := []struct{ source, xml string }{
		{"one <strong>bold <i>and italic</i></strong> end", "<p>one <b>bold <em>and italic</em></b> end</p>"},
		{"<B>bold</B> <EM>italic</EM>", "<p><b>bold</b> <em>italic</em></p>"},
		{"<strong>\nblock text\n</strong>\n", "<p><b> block text </b></p>"},
		{"# <strong>Heading</strong>\n", "<h1><b>Heading</b></h1>"},
		{"| H |\n| - |\n| <strong>bold</strong> |", "<table><thead><tr><th><p>H</p></th></tr></thead><tbody><tr><td><p><b>bold</b></p></td></tr></tbody></table>"},
		{"<p>one &amp; <strong>two</strong></p>\n", "<p>one &amp; <b>two</b></p>"},
		{"<div>one\ntwo</div>\n\nafter\n", "<p>one two</p><p>after</p>"},
		{"<p>one *literal*</p>\n\n<div>two</div>\n", "<p>one *literal*</p><p>two</p>"},
		{"    echo $HOME\n    <strong>literal</strong>\n", "<pre><code>echo $HOME<br/>&lt;strong&gt;literal&lt;/strong&gt;<br/></code></pre>"},
		{"Cost $100, $20.50; $HOME and $PATH.", "<p>Cost $100, $20.50; $HOME and $PATH.</p>"},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			got, err := Encode([]byte(c.source))
			if err != nil || string(got) != c.xml {
				t.Fatalf("got %q %v, want %q", got, err, c.xml)
			}
			view, err := Decode(got, "1")
			if err != nil {
				t.Fatal(err)
			}
			again, err := Encode(view.Markdown)
			if err != nil || !bytes.Equal(got, again) {
				t.Fatalf("round trip %q => %q (%v)", view.Markdown, again, err)
			}
		})
	}
}

func TestSafeDefaultsRejectUnsafeOrAmbiguousContentBeforePlanning(t *testing.T) {
	before := mustDecode(t, `<title id="doc">Synthetic</title><p id="p">before</p><p id="tail">unchanged</p>`)
	for _, source := range []string{
		"<strong>missing", "<b>one</strong>", "<b><i>one</b></i>", "one</em>", "<b/>one", "<b class=x>one</b>",
		"<strong>one\n\ntwo</strong>", "<p class=x>one</p>", "<p>one</div>", "<div><p>one</p></div>",
		"<p>one</p><p>two</p>", "<p>one<img src=x></p>", "<div><!-- preserved -->one</div>", "<p><a href='https://example.com'>one</a></p>",
		"<strong>![image](https://example.com/i.png)</strong>", "[link](https://example.com \"title\")",
		"<p>$x$</p>", "<div>&#36;x&#36;</div>", "$x$", "$$x$$", "$100$", "$HOME$", "$x", "$x **bold**$", "$100 + x$", "$", "$variable", "$HOME/path$",
	} {
		t.Run(source, func(t *testing.T) {
			if got, err := Encode([]byte(source)); err == nil {
				t.Fatalf("unsafe input accepted: %q", got)
			}
			result, err := PlanStep(before.Markdown, []byte("first safe change\n\n"+source+"\n\nunchanged\n"), before)
			if err != nil {
				t.Fatal(err)
			}
			if result.ConflictReason == "" || len(result.Operations) != 0 {
				t.Fatalf("partially planned invalid document: %+v", result)
			}
		})
	}
}

func TestSafeDefaultsWholeDocumentPlanAndVerify(t *testing.T) {
	for _, replacement := range []string{"<strong>bold</strong> and $100", "<p><em>paragraph</em></p>", "    echo $HOME\n    <b>literal</b>\n"} {
		before := mustDecode(t, `<title id="doc">Synthetic</title><p id="p">before</p><p id="tail">unchanged</p>`)
		if strings.HasPrefix(replacement, "    ") {
			before = mustDecode(t, `<title id="doc">Synthetic</title><pre id="p"><code>before<br/></code></pre><p id="tail">unchanged</p>`)
		}
		result, err := PlanStep(before.Markdown, []byte(replacement+"\n\nunchanged\n"), before)
		if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
			t.Fatalf("plan %q: %+v %v", replacement, result, err)
		}
		var p model.OperationPayload
		if err = json.Unmarshal(result.Operations[0].Payload, &p); err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(string(before.XML), string(before.Blocks[0].RawXML), strings.Replace(p.XML, ">", ` id="new">`, 1), 1)
		after, err := Decode([]byte(changed), "2")
		if err != nil {
			t.Fatal(err)
		}
		if err = VerifyOperation(before, after, result.Operations[0]); err != nil {
			t.Fatalf("verify %q: %v", replacement, err)
		}
		tampered := strings.Replace(changed, "unchanged", "altered", 1)
		bad, err := Decode([]byte(tampered), "3")
		if err != nil {
			t.Fatal(err)
		}
		if err = VerifyOperation(before, bad, result.Operations[0]); err == nil {
			t.Fatal("accepted changed whole-document context")
		}
	}
}

func TestSafeDefaultRuleBoundariesAreFixed(t *testing.T) {
	rules, err := PureParseConversionRules([]byte(`{"version":1,"rules":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var doc conversionRuleDocument
	if err = json.Unmarshal(rules.EffectiveDocumentJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"basic-inline-html", "simple-html-paragraph", "indented-code", "ordinary-dollar-text"} {
		var found bool
		for _, r := range doc.Rules {
			if r.ID == id {
				found = true
				r.Target = ""
				b, _ := json.Marshal(conversionRuleDocument{Version: 1, Rules: []ConversionRule{r}})
				if _, err = PureParseConversionRules(b); err == nil {
					t.Fatalf("mutable structural rule %s", id)
				}
			}
		}
		if !found {
			t.Errorf("missing explicit default %s", id)
		}
	}
}

func TestSafeDefaultsNeverRewriteCodeOrMetadata(t *testing.T) {
	installTestRules(t, ConversionRule{ID: "word", Source: "KEEP", Target: "CHANGED", Context: "paragraph", Match: "literal"})
	input := []byte("---\nname: KEEP <b> $HOME\n---\n\n    KEEP <b> $HOME\n\n```html\nKEEP <b> $HOME\n```\n\n`KEEP <b> $HOME`\n")
	normalized, err := NormalizeForMerge(input)
	if err != nil || !bytes.Equal(input, normalized) {
		t.Fatalf("rewrote protected content: %q %v", normalized, err)
	}
	if _, err := Encode(input); err != nil {
		t.Fatal(err)
	}
}

func TestSafeDefaultsKeepOldProjectionInterpretation(t *testing.T) {
	previous := activeConversionRules.Load()
	t.Cleanup(func() { activeConversionRules.Store(previous) })
	doc, err := readConversionRuleDocument(defaultConversionRulesJSON)
	if err != nil {
		t.Fatal(err)
	}
	oldRules, err := compileConversionRules(conversionRuleDocument{Version: 1, Rules: doc.Rules[:4]})
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []string{readableFixture, `<p id="p">Cost $100 and $HOME</p><pre id="c" lang="sh"><code>echo $HOME<br/>&lt;b&gt;literal&lt;/b&gt;<br/></code></pre>`,
		`<p id="p"><strong>unknown HTML</strong></p>`, `<p id="p"><b></b></p>`, `<p id="p">&lt;p&gt;literal&lt;/p&gt;</p>`,
		`<pre id="c" lang="yaml"><code># DriveBridge YAML front matter v1<br/>---<br/>name: synthetic<br/>---<br/></code></pre><p id="p">body</p>`}
	for _, version := range []int{1, 2, 3} {
		for _, fixture := range fixtures {
			opts := Options{ProjectionVersion: version}
			activeConversionRules.Store(oldRules)
			old, oldErr := DecodeWithOptions([]byte(fixture), "1", opts)
			activeConversionRules.Store(defaultConversionRules)
			current, currentErr := DecodeWithOptions([]byte(fixture), "1", opts)
			if (oldErr == nil) != (currentErr == nil) || !reflect.DeepEqual(old, current) {
				t.Fatalf("projection %d changed for %s: old=%+v (%v), current=%+v (%v)", version, fixture, old, oldErr, current, currentErr)
			}
		}
	}
}

func TestIndentedCodePlanRetainsExistingNativeMetadataAndChangesLanguage(t *testing.T) {
	before := mustDecode(t, `<pre id="p" lang="sh" caption="synthetic caption" wrap="true"><code>before<br/></code></pre><p id="tail">unchanged</p>`)
	result, err := PlanStep(before.Markdown, []byte("    echo $HOME\n\nunchanged\n"), before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatalf("plan: %+v %v", result, err)
	}
	p := payload(t, result)
	if !strings.Contains(p.XML, `caption="synthetic caption"`) || !strings.Contains(p.XML, `wrap="true"`) || strings.Contains(p.XML, `lang="sh"`) {
		t.Fatalf("code metadata/language: %s", p.XML)
	}
	afterXML := strings.Replace(string(before.XML), string(before.Blocks[0].RawXML), strings.Replace(p.XML, "<pre", `<pre id="new"`, 1), 1)
	after, err := Decode([]byte(afterXML), "2")
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyOperation(before, after, result.Operations[0]); err != nil {
		t.Fatal(err)
	}
}
