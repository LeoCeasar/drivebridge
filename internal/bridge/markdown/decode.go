package markdown

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/yuin/goldmark/util"
)

type xnode struct {
	tag, text  string
	attrs      []xml.Attr
	kids       []*xnode
	start, end int
	raw        []byte
}

func (n *xnode) attr(k string) string {
	for _, a := range n.attrs {
		if a.Name.Local == k && a.Name.Space == "" {
			return a.Value
		}
	}
	return ""
}
func (n *xnode) allowed(names ...string) bool {
	for _, a := range n.attrs {
		if a.Name.Space != "" {
			return false
		}
		ok := a.Name.Local == "id"
		for _, s := range names {
			ok = ok || a.Name.Local == s
		}
		if !ok {
			return false
		}
	}
	return true
}
func parseXML(b []byte) ([]*xnode, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	var roots, stack []*xnode
	count := 0
	ids := map[string]bool{}
	for {
		start := int(d.InputOffset())
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		end := int(d.InputOffset())
		switch v := tok.(type) {
		case xml.StartElement:
			count++
			if count > 100000 || len(stack) > 100 {
				return nil, unsupported("XML complexity limit")
			}
			if v.Name.Space != "" {
				return nil, unsupported("XML namespace")
			}
			attrNames := map[xml.Name]bool{}
			for _, a := range v.Attr {
				if attrNames[a.Name] {
					return nil, unsupported("duplicate XML attribute")
				}
				attrNames[a.Name] = true
			}
			n := &xnode{tag: v.Name.Local, attrs: v.Attr, start: start}
			if id := n.attr("id"); id != "" {
				if ids[id] {
					return nil, unsupported("duplicate XML block ID")
				}
				ids[id] = true
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.kids = append(p.kids, n)
			} else {
				roots = append(roots, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, fmt.Errorf("unexpected XML end")
			}
			n := stack[len(stack)-1]
			n.end = end
			n.raw = bytes.Clone(b[n.start:end])
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(v)) != 0 {
					return nil, unsupported("text outside XML blocks")
				}
			} else {
				p := stack[len(stack)-1]
				p.kids = append(p.kids, &xnode{text: string(v), start: start, end: end})
			}
		default:
			return nil, unsupported("XML comments, directives and processing instructions")
		}
	}
	if len(stack) != 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return roots, nil
}
func Decode(b []byte, r string) (model.DocView, error) { return DecodeWithOptions(b, r, Options{}) }
func DecodeWithOptions(b []byte, r string, o Options) (model.DocView, error) {
	if o.ProjectionVersion == 0 && HasYAMLFrontMatterXML(b) {
		o.ProjectionVersion = 3
	}
	if o.ProjectionVersion != 1 {
		return decodeReadable(b, r, o)
	}
	return decodeLegacy(b, r, o)
}
func decodeLegacy(b []byte, r string, o Options) (model.DocView, error) {
	d := model.DocView{ProjectionVersion: 1, XML: bytes.Clone(b), Revision: r, SourceURL: o.SourceURL, ReferenceMap: append(json.RawMessage(nil), o.ReferenceMap...)}
	if len(b) > 8*MaxMarkdownBytes {
		return d, &ContentError{"body_too_large", "XML exceeds codec limit"}
	}
	nodes, e := parseXML(b)
	if e != nil {
		return d, e
	}
	title := false
	type entry struct {
		n       *xnode
		parent  string
		ordinal int
	}
	var entries []entry
	for _, n := range nodes {
		if n.tag == "title" {
			if title || len(entries) > 0 {
				return d, unsupported("misplaced or repeated title")
			}
			title = true
			continue
		}
		if n.tag == "fragment" || n.tag == "excerpt" {
			return d, unsupported("partial document response")
		}
		if (n.tag == "ul" || n.tag == "ol") && simpleList(n) && projectableList(n) {
			for i, c := range elements(n) {
				start := 1
				if n.tag == "ol" {
					if v, err := strconv.Atoi(elements(n)[0].attr("seq")); err == nil {
						start = v
					}
				}
				entries = append(entries, entry{c, n.tag, start + i})
			}
		} else {
			entries = append(entries, entry{n, "", 0})
		}
	}
	for i, en := range entries {
		n := en.n
		md, e := xmlMarkdown(n)
		kind := n.tag
		if en.parent != "" {
			kind = en.parent + "/li"
			if e == nil {
				marker := "- "
				if en.parent == "ol" {
					marker = n.attr("seq") + ". "
					if n.attr("seq") == "" || n.attr("seq") == "auto" {
						marker = strconv.Itoa(en.ordinal) + ". "
					}
				}
				md = indentFirst(md, marker, strings.Repeat(" ", len(marker)))
			}
		}
		if e == nil {
			aa, err := parseMarkdown([]byte(md), nil)
			if err != nil || len(aa) != 1 {
				e = unsupported("unrepresentable Markdown")
			} else {
				xx, err := parseXML([]byte(aa[0].xml))
				if err != nil || len(xx) != 1 || semanticXML(n) != semanticXML(unprojectCode(n, xx[0])) {
					e = unsupported("Markdown changes remote semantics")
				}
			}
		}
		opaque := e != nil
		if opaque {
			kind = "opaque"
			md = "Unsupported " + n.tag + " content"
			if safeURL(o.SourceURL) {
				md = "[" + md + " — open online](<" + strings.ReplaceAll(o.SourceURL, ">", "%3E") + ">)"
			} else {
				md += " (online source unavailable)"
			}
		}
		if i > 0 {
			sep := "\n\n"
			if en.parent != "" && entries[i-1].parent == en.parent {
				sep = "\n"
			}
			d.Markdown = append(d.Markdown, sep...)
		}
		start := len(d.Markdown)
		d.Markdown = append(d.Markdown, md...)
		d.Blocks = append(d.Blocks, model.Block{Kind: kind, RemoteID: n.attr("id"), Start: start, End: len(d.Markdown), Opaque: opaque, RawXML: bytes.Clone(n.raw)})
	}
	if len(d.Markdown) > 0 {
		d.Markdown = append(d.Markdown, '\n')
		d.Blocks[len(d.Blocks)-1].End++
	}
	if e = validate(d.Markdown); e != nil {
		return d, e
	}
	// Hash exactly the semantic XML generated by the same parser used for local
	// edits. Unrepresentable remote shapes become opaque before entering merge.
	for i := range d.Blocks {
		b := &d.Blocks[i]
		if b.Opaque {
			roots, err := parseXML(b.RawXML)
			if err != nil || len(roots) != 1 {
				return d, unsupported("opaque source")
			}
			b.Hash = digest("opaque\x00" + opaqueSemantic(roots[0]))
			continue
		}
		a, e := parseMarkdown(d.Markdown[b.Start:b.End], nil)
		if e != nil || len(a) != 1 {
			return d, unsupported("remote Markdown cannot be represented exactly")
		}
		b.Hash = a[0].Hash
		if roots, err := parseXML(b.RawXML); err == nil && len(roots) == 1 {
			fresh, _ := parseXML([]byte(a[0].xml))
			if len(fresh) == 1 {
				b.Hash = hashNode(a[0].Kind, unprojectCode(roots[0], fresh[0]))
			}
		}
		b.Kind = a[0].Kind
	}
	return d, nil
}
func elements(n *xnode) []*xnode {
	var a []*xnode
	for _, c := range n.kids {
		if c.tag != "" {
			a = append(a, c)
		}
	}
	return a
}
func whitespaceOnly(n *xnode) bool {
	for _, c := range n.kids {
		if c.tag == "" && strings.TrimSpace(c.text) != "" {
			return false
		}
	}
	return true
}
func simpleList(n *xnode) bool {
	if !n.allowed() || !whitespaceOnly(n) {
		return false
	}
	seq := 0
	for _, c := range elements(n) {
		if c.tag != "li" {
			return false
		}
		if n.tag == "ol" {
			s := c.attr("seq")
			if s != "" && s != "auto" {
				v, e := strconv.Atoi(s)
				if e != nil || v < 1 || (seq > 0 && v != seq+1) {
					return false
				}
				seq = v
			} else {
				seq++
			}
		}
	}
	return len(elements(n)) > 0
}
func literal(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && util.IsPunct(byte(r)) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
func inlineMD(n *xnode) (string, error) {
	var out strings.Builder
	for _, c := range n.kids {
		if c.tag == "" {
			out.WriteString(literal(c.text))
			continue
		}
		if !c.allowed("href") {
			return "", unsupported("inline attributes")
		}
		s, e := inlineMD(c)
		if e != nil {
			return "", e
		}
		switch c.tag {
		case "b":
			if !c.allowed() {
				return "", unsupported("bold attributes")
			}
			out.WriteString("**" + s + "**")
		case "em":
			if !c.allowed() {
				return "", unsupported("emphasis attributes")
			}
			out.WriteString("*" + s + "*")
		case "code":
			if !c.allowed() || len(elements(c)) > 0 {
				return "", unsupported("inline code shape")
			}
			v := plainText(c)
			if strings.ContainsAny(v, "\r\n") {
				return "", unsupported("multiline inline code")
			}
			f := strings.Repeat("`", longestRun(v, '`')+1)
			if strings.HasPrefix(v, "`") || strings.HasSuffix(v, "`") || (strings.HasPrefix(v, " ") && strings.HasSuffix(v, " ") && strings.TrimSpace(v) != "") {
				v = " " + v + " "
			}
			out.WriteString(f + v + f)
		case "a":
			u := c.attr("href")
			if !safeURL(u) {
				return "", unsupported("link URL")
			}
			out.WriteString("[" + s + "](<" + strings.NewReplacer("<", "%3C", ">", "%3E", "\n", "%0A", "\\", "%5C").Replace(u) + ">)")
		case "br":
			if len(c.kids) > 0 || !c.allowed() {
				return "", unsupported("break shape")
			}
			out.WriteString("  \n")
		default:
			return "", unsupported("inline " + c.tag)
		}
	}
	return out.String(), nil
}
func plainText(n *xnode) string {
	var s strings.Builder
	for _, c := range n.kids {
		if c.tag == "" {
			s.WriteString(c.text)
		} else if c.tag == "br" {
			s.WriteByte('\n')
		}
	}
	return s.String()
}
func longestRun(s string, c byte) int {
	max, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			if run > max {
				max = run
			}
		} else {
			run = 0
		}
	}
	return max
}
func indentFirst(s, first, next string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = first + lines[i]
		} else {
			lines[i] = next + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}
func xmlMarkdown(n *xnode) (string, error) {
	switch n.tag {
	case "p", "h1", "h2", "h3", "h4", "h5", "h6":
		if !n.allowed("align") || n.attr("align") != "" && n.attr("align") != "left" {
			return "", unsupported("text layout")
		}
		s, e := inlineMD(n)
		if n.tag != "p" {
			s = strings.Repeat("#", int(n.tag[1]-'0')) + " " + s
		}
		return s, e
	case "li":
		if !n.allowed("seq", "seq-marker") {
			return "", unsupported("list attributes")
		}
		var out []string
		var inline []*xnode
		flush := func() error {
			if len(inline) > 0 {
				s, e := inlineMD(&xnode{kids: inline})
				if e != nil {
					return e
				}
				out = append(out, s)
				inline = nil
			}
			return nil
		}
		for _, c := range n.kids {
			switch c.tag {
			case "p", "ul", "ol", "blockquote", "pre":
				if e := flush(); e != nil {
					return "", e
				}
				s, e := xmlMarkdown(c)
				if e != nil {
					return "", e
				}
				out = append(out, s)
			default:
				inline = append(inline, c)
			}
		}
		if e := flush(); e != nil {
			return "", e
		}
		return strings.Join(out, "\n\n"), nil
	case "ul", "ol":
		if !simpleList(n) {
			return "", unsupported("list sequence or wrapper")
		}
		var out []string
		for i, c := range elements(n) {
			s, e := xmlMarkdown(c)
			if e != nil {
				return "", e
			}
			m := "- "
			if n.tag == "ol" {
				v := c.attr("seq")
				if v == "" || v == "auto" {
					v = strconv.Itoa(i + 1)
				}
				m = v + ". "
			}
			out = append(out, indentFirst(s, m, strings.Repeat(" ", len(m))))
		}
		return strings.Join(out, "\n"), nil
	case "pre":
		if !n.allowed("lang", "caption", "wrap") || !whitespaceOnly(n) || len(elements(n)) != 1 || elements(n)[0].tag != "code" {
			return "", unsupported("code shape")
		}
		c := elements(n)[0]
		if !c.allowed() {
			return "", unsupported("code attributes")
		}
		for _, v := range elements(c) {
			if v.tag != "br" || !v.allowed() || len(v.kids) > 0 {
				return "", unsupported("code child")
			}
		}
		s := plainText(c)
		if strings.Contains(s, "\r") || strings.EqualFold(n.attr("lang"), "mermaid") || strings.ContainsAny(n.attr("lang"), " \r\n`") {
			return "", unsupported("code content")
		}
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		f := strings.Repeat("`", max(3, longestRun(s, '`')+1))
		return f + n.attr("lang") + "\n" + s + f, nil
	case "blockquote":
		if !n.allowed() || !whitespaceOnly(n) {
			return "", unsupported("quote shape")
		}
		var out []string
		for _, c := range elements(n) {
			s, e := xmlMarkdown(c)
			if e != nil {
				return "", e
			}
			out = append(out, s)
		}
		return indentFirst(strings.Join(out, "\n\n"), "> ", "> "), nil
	case "hr":
		if !n.allowed() || len(n.kids) > 0 {
			return "", unsupported("rule shape")
		}
		return "---", nil
	case "table":
		return tableMD(n)
	default:
		return "", unsupported("block " + n.tag)
	}
}
func tableMD(n *xnode) (string, error) {
	if !n.allowed() || !whitespaceOnly(n) {
		return "", unsupported("table attributes")
	}
	var rows [][]string
	var aligns []string
	header := false
	for _, section := range elements(n) {
		if section.tag == "colgroup" {
			if !section.allowed() || !whitespaceOnly(section) {
				return "", unsupported("columns")
			}
			for _, col := range elements(section) {
				if col.tag != "col" || !col.allowed("width") || len(col.kids) > 0 {
					return "", unsupported("column metadata")
				}
			}
			continue
		}
		if section.tag != "thead" && section.tag != "tbody" || !section.allowed() || !whitespaceOnly(section) {
			return "", unsupported("table section")
		}
		if section.tag == "thead" {
			if header || len(rows) > 0 {
				return "", unsupported("table headers")
			}
			header = true
		}
		for _, row := range elements(section) {
			if row.tag != "tr" || !row.allowed() || !whitespaceOnly(row) {
				return "", unsupported("table row")
			}
			var cells []string
			for i, cell := range elements(row) {
				want := "td"
				if section.tag == "thead" {
					want = "th"
				}
				if cell.tag != want || !cell.allowed("vertical-align") || !whitespaceOnly(cell) || (cell.attr("vertical-align") != "" && cell.attr("vertical-align") != "top") {
					return "", unsupported("table cell")
				}
				ps := elements(cell)
				if len(ps) != 1 || ps[0].tag != "p" || !ps[0].allowed("align") {
					return "", unsupported("table cell blocks")
				}
				align := ps[0].attr("align")
				if align != "" && align != "left" && align != "center" && align != "right" {
					return "", unsupported("table alignment")
				}
				if len(rows) == 0 {
					aligns = append(aligns, align)
				} else if i >= len(aligns) || align != aligns[i] {
					return "", unsupported("mixed table alignment")
				}
				s, e := inlineMD(ps[0])
				if e != nil || strings.ContainsAny(s, "\r\n") {
					return "", unsupported("table inline shape")
				}
				s = escapeTablePipes(s)
				cells = append(cells, s)
			}
			if len(cells) == 0 || len(rows) > 0 && len(cells) != len(rows[0]) {
				return "", unsupported("nonrectangular table")
			}
			rows = append(rows, cells)
		}
	}
	if !header || len(rows) == 0 {
		return "", unsupported("missing table header")
	}
	var out []string
	for i, row := range rows {
		out = append(out, "| "+strings.Join(row, " | ")+" |")
		if i == 0 {
			var sep []string
			for _, v := range aligns {
				s := "---"
				switch v {
				case "left":
					s = ":---"
				case "right":
					s = "---:"
				case "center":
					s = ":---:"
				}
				sep = append(sep, s)
			}
			out = append(out, "| "+strings.Join(sep, " | ")+" |")
		}
	}
	return strings.Join(out, "\n"), nil
}

// semanticXML omits only identity and explicitly retained display metadata.
// It catches Markdown delimiter edge cases that parse successfully but lose style.
func semanticXML(n *xnode) string {
	if n.tag == "" {
		return esc(n.text)
	}
	if n.tag == "code" {
		return "<code>" + esc(plainText(n)) + "</code>"
	}
	if n.tag == "colgroup" {
		return ""
	}
	if n.tag == "br" {
		return "<br/>"
	}
	var out strings.Builder
	out.WriteString("<" + n.tag)
	for _, key := range []string{"href", "lang", "align"} {
		v := n.attr(key)
		if v != "" && !(key == "align" && v == "left") {
			out.WriteString(" " + key + `="` + esc(v) + `"`)
		}
	}
	out.WriteByte('>')
	if n.tag == "li" {
		var inline strings.Builder
		flush := func() {
			if inline.Len() > 0 {
				out.WriteString("<p>" + inline.String() + "</p>")
				inline.Reset()
			}
		}
		for _, c := range n.kids {
			switch c.tag {
			case "p", "ul", "ol", "blockquote", "pre":
				flush()
				out.WriteString(semanticXML(c))
			default:
				inline.WriteString(semanticXML(c))
			}
		}
		flush()
		out.WriteString("</li>")
		return out.String()
	}
	for _, c := range n.kids {
		if c.tag == "" && strings.TrimSpace(c.text) == "" && (n.tag == "table" || n.tag == "thead" || n.tag == "tbody" || n.tag == "tr" || n.tag == "td" || n.tag == "th" || n.tag == "blockquote" || n.tag == "ul" || n.tag == "ol") {
			continue
		}
		out.WriteString(semanticXML(c))
	}
	out.WriteString("</" + n.tag + ">")
	return out.String()
}
func escapeTablePipes(s string) string {
	var out strings.Builder
	slashes := 0
	for _, r := range s {
		if r == '|' && slashes%2 == 0 {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
		if r == '\\' {
			slashes++
		} else {
			slashes = 0
		}
	}
	return out.String()
}

func semanticHash(kind, x string) string {
	n, e := parseXML([]byte(x))
	if e != nil || len(n) != 1 {
		return ""
	}
	return operationNodeHash(kind, n[0])
}

// A closing Markdown fence needs a separator newline. For a remote code block
// lacking a terminal LF it is projection syntax, removed exactly once on write.
func unprojectCode(old, fresh *xnode) *xnode {
	if old.tag != "pre" || fresh.tag != "pre" || isYAMLFrontMatterNode(fresh) {
		return fresh
	}
	oc, nc := elements(old), elements(fresh)
	if len(oc) != 1 || len(nc) != 1 {
		return fresh
	}
	s := plainText(oc[0])
	if s != "" && !strings.HasSuffix(s, "\n") {
		v := plainText(nc[0])
		v = strings.TrimSuffix(v, "\n")
		nc[0].kids = codeNodes(v)
	}
	return fresh
}
func codeNodes(s string) []*xnode {
	var out []*xnode
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		if i > 0 {
			out = append(out, &xnode{tag: "br"})
		}
		if p != "" {
			out = append(out, &xnode{text: p})
		}
	}
	return out
}

func hashNode(kind string, n *xnode) string {
	var seq strings.Builder
	var walk func(*xnode)
	walk = func(v *xnode) {
		if v.tag == "li" && v.attr("seq") != "" {
			seq.WriteString("\x00seq=" + v.attr("seq"))
		}
		for _, c := range v.kids {
			walk(c)
		}
	}
	walk(n)
	return digest(kind + "\x00" + semanticXML(n) + seq.String())
}

func opaqueSemantic(n *xnode) string {
	if n.tag == "" {
		return esc(n.text)
	}
	var out strings.Builder
	out.WriteString("<" + n.tag)
	attrs := append([]xml.Attr(nil), n.attrs...)
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].Name.Local < attrs[j].Name.Local })
	for _, a := range attrs {
		if a.Name.Local != "id" {
			out.WriteString(" " + a.Name.Local + `="` + esc(a.Value) + `"`)
		}
	}
	out.WriteByte('>')
	for _, c := range n.kids {
		out.WriteString(opaqueSemantic(c))
	}
	out.WriteString("</" + n.tag + ">")
	return out.String()
}

// A list with an unsupported child is one opaque subtree. Splitting it would
// expose an editable sibling whose parent contains unmapped resource semantics.
func projectableList(n *xnode) bool {
	for _, c := range elements(n) {
		md, e := xmlMarkdown(c)
		if e != nil {
			return false
		}
		marker := "- "
		if n.tag == "ol" {
			marker = "1. "
		}
		a, e := parseMarkdown([]byte(indentFirst(md, marker, strings.Repeat(" ", len(marker)))), nil)
		if e != nil || len(a) != 1 {
			return false
		}
		x, e := parseXML([]byte(a[0].xml))
		if e != nil || len(x) != 1 || semanticXML(c) != semanticXML(x[0]) {
			return false
		}
	}
	return true
}
