package markdown

import (
	"bytes"
	"strings"
	"testing"
)

func TestPunctuationProjectionDoesNotAccumulateEscapes(t *testing.T) {
	xml := `<p id="p">version 3.1, /opt/server/nginx.conf: x = 1; (ok)! C:\tmp &amp; literal *stars*</p>`
	first := mustDecode(t, xml).Markdown
	for i := 0; i < 10; i++ {
		encoded, err := Encode(mustDecode(t, xml).Markdown)
		if err != nil {
			t.Fatal(err)
		}
		xml = strings.Replace(string(encoded), "<p>", `<p id="p">`, 1)
		got := mustDecode(t, xml).Markdown
		if !bytes.Equal(got, first) {
			t.Fatalf("round trip %d accumulated escapes: %q -> %q", i, first, got)
		}
	}
}
