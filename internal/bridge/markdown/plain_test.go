package markdown

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestMergePlainThreeWay(t *testing.T) {
	for _, tc := range []struct {
		name, base, local, remote, want, reason string
	}{
		{"unchanged", "a\nb\n", "a\nb\n", "a\nb\n", "a\nb\n", ""},
		{"local only", "a\nb\n", "A\nb\n", "a\nb\n", "A\nb\n", ""},
		{"remote only", "a\nb\n", "a\nb\n", "a\nB\n", "a\nB\n", ""},
		{"same edit", "a\nb\n", "A\nb\n", "A\nb\n", "A\nb\n", ""},
		{"different adjacent lines", "a\nb\nc\n", "a\nB\nc\n", "a\nb\nC\n", "a\nB\nC\n", ""},
		{"same line separate words", "red fish blue bird\n", "RED fish blue bird\n", "red fish BLUE bird\n", "RED fish BLUE bird\n", ""},
		{"same line incompatible", "a\nold\nb\n", "a\nleft\nb\n", "a\nright\nb\n", "a\nleft\nb\n", "overlapping_edits"},
		{"delete versus edit", "alpha\nbeta\ngamma\n", "alpha\ngamma\n", "alpha\nBETA\ngamma\n", "alpha\ngamma\n", "overlapping_edits"},
		{"both delete", "alpha\nbeta\ngamma\n", "alpha\ngamma\n", "alpha\ngamma\n", "alpha\ngamma\n", ""},
		{"same insertion differs", "a\nb\n", "a\nleft\nb\n", "a\nright\nb\n", "a\nleft\nb\n", "overlapping_edits"},
		{"same insertion equal", "a\nb\n", "a\nsame\nb\n", "a\nsame\nb\n", "a\nsame\nb\n", ""},
		{"independent insertions", "a\nb\nc\n", "left\na\nb\nc\n", "a\nb\nc\nright\n", "left\na\nb\nc\nright\n", ""},
		{"independent deletion", "a\nb\nc\nd\n", "a\nc\nd\n", "a\nb\nc\nD\n", "a\nc\nD\n", ""},
		{"unicode crlf no final newline", "标题\r\n猫咪🐱和小狗🐶\r\n结尾", "新标题\r\n猫咪🐱和小狗🐶\r\n结尾", "标题\r\n猫咪🐯和小狗🐶\r\n结尾", "新标题\r\n猫咪🐯和小狗🐶\r\n结尾", ""},
		{"arbitrary markdown syntax", "---\ncustom: value\n---\n<widget/>\n| a | b |\n", "---\ncustom: local\n---\n<widget/>\n| a | b |\n", "---\ncustom: value\n---\n<widget remote/>\n| a | b |\n", "---\ncustom: local\n---\n<widget remote/>\n| a | b |\n", ""},
		{"empty baseline differing creates", "", "left", "right", "left", "overlapping_edits"},
		{"empty baseline same creates", "", "same", "same", "same", ""},
		{"local adds final newline remote edits earlier", "a\nb", "a\nb\n", "A\nb", "A\nb\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := MergePlain([]byte(tc.base), []byte(tc.local), []byte(tc.remote))
			if string(got) != tc.want || reason != tc.reason {
				t.Fatalf("got %q (%q), want %q (%q)", got, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestMergePlainRejectsUnsafeContentAndReturnsIndependentDraft(t *testing.T) {
	for _, tc := range []struct {
		name                string
		base, local, remote []byte
		reason              string
	}{
		{"invalid local UTF8", []byte("base"), []byte{0xff}, []byte("base"), "unsupported_content"},
		{"invalid remote UTF8", []byte("base"), []byte("local"), []byte{0xff}, "unsupported_content"},
		{"nul even unilateral", []byte("base"), []byte("local\x00"), []byte("base"), "unsupported_content"},
		{"oversized", []byte("base"), bytes.Repeat([]byte{'a'}, (10<<20)+1), []byte("base"), "merge_complexity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := MergePlain(tc.base, tc.local, tc.remote)
			if reason != tc.reason || !bytes.Equal(got, tc.local) {
				t.Fatal(reason, len(got))
			}
			if len(got) > 0 {
				got[0] ^= 1
				if bytes.Equal(got, tc.local) {
					t.Fatal("draft aliases input")
				}
			}
		})
	}
}

func TestMergePlainLargeDocumentUsesLineAnchors(t *testing.T) {
	var lines []string
	for i := range 6000 {
		lines = append(lines, fmt.Sprintf("paragraph %05d with ordinary markdown words\r\n", i))
	}
	base := strings.Join(lines, "")
	local := strings.Replace(base, lines[10], "local paragraph\r\n", 1)
	remote := strings.Replace(base, lines[5990], "remote paragraph\r\n", 1)
	want := strings.Replace(local, lines[5990], "remote paragraph\r\n", 1)
	got, reason := MergePlain([]byte(base), []byte(local), []byte(remote))
	if reason != "" || string(got) != want {
		t.Fatal("sparse edits on large document", reason)
	}
}

func TestMergePlainBoundsAdversarialDiffWork(t *testing.T) {
	var b, l, r strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&b, "base %d\n", i)
		fmt.Fprintf(&l, "left %d\n", i)
		fmt.Fprintf(&r, "right %d\n", i)
	}
	got, reason := MergePlain([]byte(b.String()), []byte(l.String()), []byte(r.String()))
	if reason != "merge_complexity" || string(got) != l.String() {
		t.Fatal(reason)
	}
}

func TestMergePlainResultDoesNotAliasSuccessInputs(t *testing.T) {
	base, local := []byte("old"), []byte("new")
	got, reason := MergePlain(base, local, base)
	if reason != "" || string(got) != "new" {
		t.Fatal(reason, string(got))
	}
	got[0] = 'X'
	if string(local) != "new" {
		t.Fatal("success aliases input")
	}
}

func TestMergePlainBoundsCombinedOutput(t *testing.T) {
	base := []byte("a\nz\n")
	local := append([]byte("a\n"), bytes.Repeat([]byte{'l'}, 6<<20)...)
	local = append(local, []byte("\nz\n")...)
	remote := append([]byte("a\nz\n"), bytes.Repeat([]byte{'r'}, 6<<20)...)
	remote = append(remote, '\n')
	got, reason := MergePlain(base, local, remote)
	if reason != "merge_complexity" || !bytes.Equal(got, local) {
		t.Fatal(reason, len(got))
	}
}

func FuzzMergePlain(f *testing.F) {
	f.Add("a\nb\nc\n", "a\nB\nc\n", "a\nb\nC\n")
	f.Add("猫咪🐱和小狗🐶\r\n", "猫咪🐯和小狗🐶\r\n", "猫咪🐱和小狗🐕\r\n")
	f.Add("", "left", "right")
	f.Add("\x00", "\xff", "")
	f.Fuzz(func(t *testing.T, base, local, remote string) {
		if len(base)+len(local)+len(remote) > 8000 {
			return
		}
		b, l, r := []byte(base), []byte(local), []byte(remote)
		got, reason := MergePlain(b, l, r)
		if reason != "" && !bytes.Equal(got, l) {
			t.Fatal("conflict lost local draft")
		}
		if reason == "" {
			reversed, reverseReason := MergePlain(b, r, l)
			if reverseReason != "" || !bytes.Equal(got, reversed) {
				t.Fatal("successful merge asymmetric")
			}
		}
	})
}
