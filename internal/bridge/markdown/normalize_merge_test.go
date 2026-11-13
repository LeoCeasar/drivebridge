package markdown

import (
	"bytes"
	"strings"
	"testing"
)

func TestNormalizeForMergeBreaksAndProtectedLiterals(t *testing.T) {
	cases := []struct{ in, want string }{
		{"one<br>two<BR/>three<br />four\n", "one  \ntwo  \nthree  \nfour\n"},
		{"| H |\n| - |\n| one<br>two |\n", "| H |\n| - |\n| one\\; two |\n"},
		{"`<br>` and \\<br>\n\n```html\n<br>\n```\n", "`<br>` and \\<br>\n\n```html\n<br>\n```\n"},
		{"[link](https://example.com/<br>)\n", "[link](https://example.com/<br>)\n"},
	}
	for _, tc := range cases {
		src := []byte(tc.in)
		got, err := NormalizeForMerge(src)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%q => %q %v want %q", tc.in, got, err, tc.want)
		}
		if !bytes.Equal(src, []byte(tc.in)) {
			t.Fatal("input changed")
		}
		again, err := NormalizeForMerge(got)
		if err != nil || !bytes.Equal(again, got) {
			t.Fatalf("not idempotent %q %v", again, err)
		}
	}
}
func TestNormalizeForMergeLiteralRulesProtectCodeAndLinks(t *testing.T) {
	installTestRules(t, ConversionRule{ID: "a", Source: "PLACEHOLDER", Target: "DONE", Context: "paragraph", Match: "literal"})
	input := "PLACEHOLDER and NEXT `PLACEHOLDER` [PLACEHOLDER](https://example.com/PLACEHOLDER)\n"
	got, err := NormalizeForMerge([]byte(input))
	if err != nil || string(got) != "DONE and NEXT `PLACEHOLDER` [PLACEHOLDER](https://example.com/PLACEHOLDER)\n" {
		t.Fatalf("literal %q %v", got, err)
	}
}
func TestNormalizeForMergeKeepsRichHTML(t *testing.T) {
	src := []byte("<table>\n<tr><td>one<br>two</td></tr>\n</table>\n")
	got, err := NormalizeForMerge(src)
	if err != nil || !bytes.Equal(got, src) {
		t.Fatalf("rich HTML mutated %q %v", got, err)
	}
	_, err = NormalizeForMerge([]byte(strings.Repeat("x", MaxMarkdownBytes+1)))
	if err == nil {
		t.Fatal("unbounded body accepted")
	}
}

func TestNormalizeForMergeRejectsBoundaryCascadeAndGrowth(t *testing.T) {
	t.Run("boundary", func(t *testing.T) {
		installTestRules(t, ConversionRule{ID: "short", Source: "a", Target: "b", Context: "paragraph", Match: "literal"}, ConversionRule{ID: "long", Source: "bc", Target: "d", Context: "paragraph", Match: "literal"})
		if _, err := NormalizeForMerge([]byte("ac\n")); err == nil {
			t.Fatal("unstable boundary accepted")
		}
	})
	t.Run("growth", func(t *testing.T) {
		installTestRules(t, ConversionRule{ID: "expand", Source: "a", Target: strings.Repeat("x", 65536), Context: "paragraph", Match: "literal"})
		if _, err := NormalizeForMerge([]byte(strings.Repeat("a", 1000))); err == nil {
			t.Fatal("unbounded expansion accepted")
		}
	})
}
func TestNormalizeForMergeLiteralEscapesAndContext(t *testing.T) {
	installTestRules(t, ConversionRule{ID: "escaped", Source: "!OLD!", Target: "NEW", Context: "paragraph", Match: "literal"}, ConversionRule{ID: "table", Source: "PLACEHOLDER", Target: "DONE", Context: "table", Match: "literal"})
	in := "!OLD! \\!OLD!\n\nPLACEHOLDER\n\n| H |\n| - |\n| PLACEHOLDER |\n"
	got, err := NormalizeForMerge([]byte(in))
	want := "NEW \\!OLD!\n\nPLACEHOLDER\n\n| H |\n| - |\n| DONE |\n"
	if err != nil || string(got) != want {
		t.Fatalf("context %q %v want %q", got, err, want)
	}
}
func TestNormalizeForMergeLineEndBreakDoesNotAddParagraph(t *testing.T) {
	for _, in := range []string{"one<br>\ntwo\n", "one<br/>\r\ntwo\n"} {
		got, err := NormalizeForMerge([]byte(in))
		before, beforeErr := Encode([]byte(in))
		after, afterErr := Encode(got)
		if err != nil || beforeErr != nil || afterErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("line end changed %q => %q before=%s after=%s %v", in, got, before, after, err)
		}
	}
}

func TestNormalizeForMergePreservesContainerAndTrailingBreakSemantics(t *testing.T) {
	for _, in := range []string{"one<br>\n", "one<br><br>two\n", "- one<br><br>two\n", "> one<br><br>two\n", "- one<br>two\n", "> one<br>two\n"} {
		before, err := Encode([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := NormalizeForMerge([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		after, err := Encode(normalized)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("structure changed input=%q normalized=%q before=%s after=%s err=%v", in, normalized, before, after, err)
		}
	}
}
