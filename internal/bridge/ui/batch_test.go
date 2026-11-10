package ui

import (
	"io"
	"strings"
	"testing"
)

func TestConflictQueueEntryAndScriptAreServed(t *testing.T) {
	h, _, c, _, _ := fixture(t)
	for _, tc := range []struct {
		path, contentType, marker string
	}{
		{"/?view=conflicts", "text/html", `src="/batch-view.js"`},
		{"/batch-view.js", "text/javascript", "DriveBridgeBatch"},
	} {
		res := request(t, c, h, "GET", tc.path, nil, "", "")
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), tc.contentType) || !strings.Contains(string(body), tc.marker) {
			t.Fatalf("%s: status=%d type=%s body=%s err=%v", tc.path, res.StatusCode, res.Header.Get("Content-Type"), body, err)
		}
	}
}
