package markdown

import "testing"

func TestSimpleHTMLBreakCompatibility(t *testing.T) {
	cases := []struct{ name, source, want string }{
		{"paragraph variants", "one<br>two<BR/>three<br />four", "<p>one<br/>two<br/>three<br/>four</p>"},
		{"table cell", "| Item |\n| --- |\n| one<br>two<br/>three |\n", "<table><thead><tr><th><p>Item</p></th></tr></thead><tbody><tr><td><p>one; two; three</p></td></tr></tbody></table>"},
		{"standalone break", "first\n\n<br />\n\nlast\n", "<p>first</p><p>last</p>"},
		{"inline literal", "`<br>` and `x<br/>y`", "<p><code>&lt;br&gt;</code> and <code>x&lt;br/&gt;y</code></p>"},
		{"fenced literal", "```html\n<br>\n```\n", "<pre lang=\"html\"><code>&lt;br&gt;<br/></code></pre>"},
		{"escaped literal", `one \<br> two`, "<p>one &lt;br&gt; two</p>"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Encode([]byte(tt.source))
			if err != nil || string(got) != tt.want {
				t.Fatalf("Encode = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
func TestRelativeLinkKeepsReadableDestination(t *testing.T) {
	cases := []struct{ source, want string }{
		{"[guide](skills/example/SKILL.md)", "<p>guide (skills/example/SKILL.md)</p>"},
		{"[**guide**](../notes.md#part)", "<p><b>guide</b> (../notes.md#part)</p>"},
		{"[guide](https://example.com/notes)", "<p><a href=\"https://example.com/notes\">guide</a></p>"},
	}
	for _, tt := range cases {
		got, err := Encode([]byte(tt.source))
		if err != nil || string(got) != tt.want {
			t.Fatalf("%q => %q, %v; want %q", tt.source, got, err, tt.want)
		}
	}
}
func TestCompatibilityKeepsUnknownHTMLAndURLBlocked(t *testing.T) {
	for _, source := range []string{"one<br onclick=\"evil()\">two", "<br data-x=\"1\">", "one<span>two</span>", "[x](javascript:alert(1))", "[x](data:text/plain,hello)", "[x](file:///private/path)", "[x](//outside.example/path)"} {
		if _, err := Encode([]byte(source)); err == nil {
			t.Fatalf("unsafe or unsupported input accepted: %q", source)
		}
	}
}
