package markdown

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	htmlrenderer "github.com/yuin/goldmark/renderer/html"
)

func esc(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
func nodeXML(n ast.Node, src []byte) (string, error) {
	children := func() (string, error) {
		var b strings.Builder
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			s, e := nodeXML(c, src)
			if e != nil {
				return "", e
			}
			b.WriteString(s)
		}
		return b.String(), nil
	}
	wrap := func(tag string) (string, error) {
		s, e := children()
		return "<" + tag + ">" + s + "</" + strings.Fields(tag)[0] + ">", e
	}
	switch v := n.(type) {
	case *ast.Text:
		var b bytes.Buffer
		raw := v.Segment.Value(src)
		// Formula delimiters outside code are outside the editable subset.
		for i, c := range raw {
			if c == '$' && (i == 0 || raw[i-1] != '\\') {
				return "", unsupported("formula delimiter")
			}
		}
		w := bufio.NewWriter(&b)
		htmlrenderer.DefaultWriter.Write(w, raw)
		_ = w.Flush()
		if v.HardLineBreak() {
			b.WriteString("<br/>")
		} else if v.SoftLineBreak() {
			// Feishu discards literal XML text newlines; CommonMark soft
			// breaks render as whitespace, so preserve the word boundary.
			b.WriteByte(' ')
		}
		return b.String(), nil
	case *ast.String:
		return esc(string(v.Value)), nil
	case *ast.Paragraph:
		return wrap("p")
	case *ast.TextBlock:
		return children()
	case *ast.Heading:
		return wrap(fmt.Sprintf("h%d", v.Level))
	case *ast.Emphasis:
		if v.Level == 2 {
			return wrap("b")
		}
		return wrap("em")
	case *ast.CodeSpan:
		var b strings.Builder
		for c := v.FirstChild(); c != nil; c = c.NextSibling() {
			s := string(c.(*ast.Text).Segment.Value(src))
			b.WriteString(strings.ReplaceAll(s, "\n", " "))
		}
		return "<code>" + esc(b.String()) + "</code>", nil
	case *ast.FencedCodeBlock:
		lang := string(v.Language(src))
		if strings.EqualFold(lang, "mermaid") {
			return "", unsupported("Mermaid")
		}
		if v.Info != nil && strings.TrimSpace(string(v.Info.Segment.Value(src))) != lang {
			return "", unsupported("code info attributes")
		}
		code := string(v.Lines().Value(src))
		code = strings.ReplaceAll(code, "\r\n", "\n")
		attr := ""
		if lang != "" {
			attr = ` lang="` + esc(lang) + `"`
		}
		return "<pre" + attr + "><code>" + strings.ReplaceAll(esc(code), "&#xA;", "<br/>") + "</code></pre>", nil
	case *ast.Blockquote:
		return wrap("blockquote")
	case *ast.List:
		tag := "ul"
		if v.IsOrdered() {
			tag = "ol"
		}
		return wrap(tag)
	case *ast.ListItem:
		s, e := children()
		seq := ""
		if l, ok := v.Parent().(*ast.List); ok && l.IsOrdered() {
			i := l.Start
			for c := l.FirstChild(); c != v; c = c.NextSibling() {
				i++
			}
			seq = fmt.Sprintf(` seq="%d"`, i)
		}
		return "<li" + seq + ">" + s + "</li>", e
	case *ast.Link:
		if len(v.Title) > 0 {
			return "", unsupported("link title")
		}
		url := string(v.Destination)
		if !safeURL(url) {
			return "", unsupported("unsafe link URL")
		}
		return wrap(`a href="` + esc(url) + `"`)
	case *ast.AutoLink:
		u := string(v.URL(src))
		if !safeURL(u) {
			return "", unsupported("unsafe link URL")
		}
		return `<a href="` + esc(u) + `">` + esc(string(v.Label(src))) + `</a>`, nil
	case *ast.ThematicBreak:
		return "<hr/>", nil
	case *east.Table:
		var b strings.Builder
		b.WriteString("<table><thead>")
		for c := v.FirstChild(); c != nil; c = c.NextSibling() {
			if _, ok := c.(*east.TableHeader); !ok && c == v.FirstChild().NextSibling() {
				b.WriteString("</thead><tbody>")
			}
			s, e := nodeXML(c, src)
			if e != nil {
				return "", e
			}
			b.WriteString(s)
		}
		if v.ChildCount() == 1 {
			b.WriteString("</thead><tbody>")
		}
		b.WriteString("</tbody></table>")
		return b.String(), nil
	case *east.TableHeader, *east.TableRow:
		return wrap("tr")
	case *east.TableCell:
		tag := "td"
		if _, ok := v.Parent().(*east.TableHeader); ok {
			tag = "th"
		}
		s, e := children()
		attr := ""
		if v.Alignment != east.AlignNone {
			attr = ` align="` + v.Alignment.String() + `"`
		}
		return "<" + tag + "><p" + attr + ">" + s + "</p></" + tag + ">", e
	default:
		return "", unsupported(n.Kind().String())
	}
}
func safeURL(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:") || strings.HasPrefix(l, "#")
}
