package ui

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOnlineDiagnosticScriptIsServed(t *testing.T) {
	h, _, c, _, _ := fixture(t)
	r, err := c.Get(h.URL() + "/online-view.js")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") || len(b) < 100 {
		t.Fatal(r.Status, r.Header.Get("Content-Type"))
	}
}
