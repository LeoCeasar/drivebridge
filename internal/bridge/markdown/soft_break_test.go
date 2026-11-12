package markdown

import (
	"testing"
)

// Feishu's XML transport removes literal newlines in text nodes. CommonMark
// soft breaks must become a space, rather than concatenate the adjacent words.
func TestEncodeSoftBreakSurvivesFeishuWhitespaceNormalization(t *testing.T) {
	for _, input := range []string{"first\nsecond", "first\r\nsecond"} {
		got, err := Encode([]byte(input))
		if err != nil || string(got) != "<p>first second</p>" {
			t.Fatalf("soft break %q encoded as %q: %v", input, got, err)
		}
	}
	got, err := Encode([]byte("first  \nsecond"))
	if err != nil || string(got) != "<p>first<br/>second</p>" {
		t.Fatalf("hard break changed: %q %v", got, err)
	}
}
