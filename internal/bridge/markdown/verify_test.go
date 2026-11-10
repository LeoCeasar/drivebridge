package markdown

import (
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestOperationReadbackChecksWholeContext(t *testing.T) {
	b := mustDecode(t, `<title id="doc">Title</title><p id="a">alpha</p><p id="b">beta</p>`)
	m := merged(t, string(b.Markdown), strings.Replace(string(b.Markdown), "alpha", "changed", 1), b)
	noConflict(t, m)
	a := mustDecode(t, `<title id="doc">Title</title><p id="new">changed</p><p id="b">beta</p>`)
	if err := VerifyOperation(b, a, m.Operations[0]); err != nil {
		t.Fatal(err)
	}
	bad := mustDecode(t, `<title id="doc">Changed title</title><p id="new">changed</p><p id="b">beta</p>`)
	if err := VerifyOperation(b, bad, m.Operations[0]); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	bad = mustDecode(t, `<title id="doc">Title</title><p id="new">changed</p><p id="b">other</p>`)
	if err := VerifyOperation(b, bad, m.Operations[0]); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
}

func TestOperationReadbackInsertDeleteAndAmbiguity(t *testing.T) {
	b := mustDecode(t, `<title id="doc">Title</title><p id="a">alpha</p><p id="b">beta</p>`)
	for _, local := range []string{"first\n\n" + string(b.Markdown), "alpha\n\n"} {
		m := merged(t, string(b.Markdown), local, b)
		noConflict(t, m)
		var a model.DocView
		if m.Operations[0].Kind == model.OperationBlockDelete {
			a = mustDecode(t, `<title id="doc">Title</title><p id="a">alpha</p>`)
		} else {
			a = mustDecode(t, `<title id="doc">Title</title><p id="n">first</p><p id="a">alpha</p><p id="b">beta</p>`)
		}
		if err := VerifyOperation(b, a, m.Operations[0]); err != nil {
			t.Fatal(err)
		}
	}
	m := merged(t, string(b.Markdown), strings.Replace(string(b.Markdown), "alpha", "changed", 1), b)
	dup := mustDecode(t, `<title id="doc">Title</title><p id="new">changed</p><p id="b">beta</p><p id="x">changed</p>`)
	if err := VerifyOperation(b, dup, m.Operations[0]); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
}
