package ui

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// serverTiming reports how long each stage of the accept-merge request took, as
// a standard Server-Timing header (visible in the browser's network panel). It
// carries only stage names and durations, never paths or document content.
type serverTiming struct {
	last  time.Time
	parts []string
}

func newServerTiming() *serverTiming { return &serverTiming{last: time.Now()} }

func (t *serverTiming) mark(stage string) {
	now := time.Now()
	t.parts = append(t.parts, fmt.Sprintf("%s;dur=%.1f", stage, float64(now.Sub(t.last).Microseconds())/1000))
	t.last = now
}

// write must be called before the response status is written.
func (t *serverTiming) write(w http.ResponseWriter) {
	w.Header().Set("Server-Timing", strings.Join(t.parts, ", "))
}
