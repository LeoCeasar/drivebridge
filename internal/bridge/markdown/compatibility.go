package markdown

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
)

// Only the empty HTML break tag is normalized. Resource-bearing or attributed
// HTML stays outside this compatibility rule, and code literals never reach it.
var simpleHTMLBreak = regexp.MustCompile(`(?i)^<br\s*/?>$`)

func isSimpleHTMLBreak(raw []byte) bool {
	return simpleHTMLBreak.MatchString(strings.TrimSpace(string(raw)))
}

// Relative destinations have no stable cloud base. Preserve them as visible
// text; do not upload an invalid active link or silently discard the path.
func readableRelativeURL(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\\\r\n\x00") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "" && u.Host == ""
}

func spacingBreak(n ast.Node, source []byte) bool {
	h, ok := n.(*ast.HTMLBlock)
	return ok && !h.HasClosure() && isSimpleHTMLBreak(h.Lines().Value(source))
}
func compatibleSpacing(source []byte) bool {
	for _, line := range bytes.Split(source, []byte("\n")) {
		if len(bytes.TrimSpace(line)) != 0 && !isSimpleHTMLBreak(line) {
			return false
		}
	}
	return true
}
