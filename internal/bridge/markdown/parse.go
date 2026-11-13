// Package markdown implements a deliberately bounded Markdown/XML bridge. It never
// writes remotely; operation targets are usable only until the next remote write.
package markdown

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

const MaxMarkdownBytes = 10 * 1024 * 1024

// ContentError is a non-uploadable input; Merge translates it into a conflict.
type ContentError struct{ Reason, Detail string }

func (e *ContentError) Error() string { return e.Reason + ": " + e.Detail }
func unsupported(s string) error      { return &ContentError{"unsupported_content", s} }

// Options supplies transport-owned context. ReferenceMap is retained, never replayed.
type Options struct {
	ProjectionVersion int
	SourceURL         string
	ReferenceMap      json.RawMessage
}
type atom struct {
	model.Block
	xml        string
	node       ast.Node
	parent     string
	actualHash string
}

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func validate(b []byte) error {
	if len(b) > MaxMarkdownBytes {
		return &ContentError{"body_too_large", "Markdown exceeds 10 MiB"}
	}
	if !utf8.Valid(b) {
		return unsupported("invalid UTF-8")
	}
	for _, r := range string(b) {
		if r < 32 && r != '\t' && r != '\n' && r != '\r' {
			return unsupported("XML-incompatible control character")
		}
	}
	return nil
}
func parser() goldmark.Markdown {
	return goldmark.New(goldmark.WithExtensions(extension.Table, extension.TaskList))
}

// Encode returns an XML body fragment, with no title, IDs, or resource references.
func Encode(b []byte) ([]byte, error) {
	a, e := parseMarkdown(b, nil)
	if e != nil {
		return nil, e
	}
	var out strings.Builder
	for i := 0; i < len(a); i++ {
		if a[i].parent != "" {
			p := a[i].parent
			out.WriteString("<" + p + ">")
			for ; i < len(a) && a[i].parent == p; i++ {
				out.WriteString(a[i].xml)
			}
			i--
			out.WriteString("</" + p + ">")
		} else {
			out.WriteString(a[i].xml)
		}
	}
	return []byte(out.String()), nil
}

// Each span owns its last content-line terminator, but not following blank lines.
// Positions come from goldmark's source positions, not rendered text/rune indices.
func parseMarkdown(b []byte, d *model.DocView) ([]atom, error) {
	if e := validate(b); e != nil {
		return nil, e
	}
	trimmed := bytes.TrimSpace(b)
	if bytes.HasPrefix(trimmed, []byte("---\n")) || bytes.HasPrefix(trimmed, []byte("---\r\n")) || bytes.HasPrefix(trimmed, []byte("+++\n")) {
		return nil, unsupported("front matter")
	}
	root := parser().Parser().Parse(text.NewReader(b))
	var a []atom
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		if spacingBreak(n, b) {
			continue
		}
		if list, ok := n.(*ast.List); ok {
			p := "ul"
			if list.IsOrdered() {
				p = "ol"
			}
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				a = append(a, atom{node: c, parent: p})
			}
		} else {
			a = append(a, atom{node: n})
		}
	}
	if len(a) > 10000 {
		return nil, &ContentError{"merge_complexity", "too many blocks"}
	}
	for i := range a {
		pos := a[i].node.Pos()
		if pos < 0 || pos > len(b) {
			return nil, unsupported("missing source position")
		}
		for pos > 0 && b[pos-1] != '\n' {
			pos--
		}
		a[i].Start = pos
	}
	for i := range a {
		end := len(b)
		if i+1 < len(a) {
			end = a[i+1].Start
		}
		for end > a[i].Start {
			lineEnd := end
			lineStart := end
			if lineStart > 0 && b[lineStart-1] == '\n' {
				lineStart--
			}
			for lineStart > a[i].Start && b[lineStart-1] != '\n' {
				lineStart--
			}
			if len(bytes.TrimSpace(b[lineStart:lineEnd])) != 0 {
				break
			}
			end = lineStart
		}
		a[i].End = end
		source := string(b[a[i].Start:end])
		matched := false
		if d != nil {
			for _, rb := range d.Blocks {
				if rb.Opaque && rb.Start >= 0 && rb.End >= rb.Start && rb.End <= len(d.Markdown) && strings.TrimSpace(source) == strings.TrimSpace(string(d.Markdown[rb.Start:rb.End])) {
					a[i].Block.Kind = "opaque"
					a[i].Opaque = true
					a[i].Hash = rb.Hash
					matched = true
					break
				}
			}
		}
		if matched {
			continue
		}
		x, e := nodeXML(a[i].node, b)
		if e != nil {
			return nil, e
		}
		a[i].xml = x
		a[i].Kind = nodeKind(a[i].node)
		if a[i].parent != "" {
			a[i].Kind = a[i].parent + "/li"
		}
		a[i].Hash = semanticHash(a[i].Kind, x)
	}
	// Reference definitions are consumed by CommonMark; unsupported references must
	// not disappear into an empty parse or be mistaken for an unmodified source gap.
	for i := 0; i < len(b); {
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			j = len(b) - i
		} else {
			j++
		}
		line := bytes.TrimSpace(b[i : i+j])
		covered := false
		for _, v := range a {
			if i >= v.Start && i < v.End {
				covered = true
				break
			}
		}
		if !covered && len(line) > 0 && !isSimpleHTMLBreak(line) {
			return nil, unsupported("unmapped source (reference definition or unsupported syntax)")
		}
		i += j
	}
	return a, nil
}
func nodeKind(n ast.Node) string {
	switch v := n.(type) {
	case *ast.Paragraph, *ast.TextBlock, *ast.HTMLBlock:
		return "p"
	case *ast.Heading:
		return fmt.Sprintf("h%d", v.Level)
	case *ast.FencedCodeBlock:
		return "pre"
	case *ast.Blockquote:
		return "blockquote"
	case *ast.ThematicBreak:
		return "hr"
	case *ast.ListItem:
		return "li"
	default:
		return strings.ToLower(n.Kind().String())
	}
}
