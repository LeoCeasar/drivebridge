package markdown

import (
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	"strings"
)

func operation(kind, id, after string, p model.OperationPayload) model.Operation {
	b, _ := json.Marshal(p)
	return model.Operation{Kind: kind, BlockID: id, AfterBlockID: after, Payload: b}
}

// Retain known current metadata without copying unknown structures or IDs. Table
// layout is grafted only when its shape still matches, otherwise block the edit.
func replacementXML(old, new atom) (string, error) {
	return replacementXMLMode(old, new, false)
}
func replacementXMLMode(old, new atom, allowTableResize bool) (string, error) {
	if old.Kind != new.Kind {
		return "", unsupported("structural replacement")
	}
	roots, e := parseXML(old.RawXML)
	if e != nil || len(roots) != 1 {
		return "", unsupported("missing current XML")
	}
	fresh, e := parseXML([]byte(new.xml))
	if e != nil || len(fresh) != 1 {
		return "", unsupported("replacement XML")
	}
	a, b := roots[0], fresh[0]
	if a.tag == "pre" && b.tag == "pre" {
		oc, nc := elements(a), elements(b)
		if len(oc) == 1 && len(nc) == 1 {
			oldCode, newCode := plainText(oc[0]), plainText(nc[0])
			if oldCode != "" && !strings.HasSuffix(oldCode, "\n") {
				count := len(newCode) - len(strings.TrimRight(newCode, "\n"))
				if count != 1 || newCode == "\n" {
					return "", unsupported("ambiguous terminal newline edit to projected code")
				}
			}
		}
	}
	b = unprojectCode(a, b)
	if a.tag != b.tag {
		return "", unsupported("parent shape")
	}
	if allowTableResize && a.tag == "table" && b.tag == "table" {
		// Decode approval is deliberately rechecked before replacing a whole
		// table. Rich/merged/resource tables must never take this route.
		if _, err := xmlMarkdown(a); err != nil {
			return "", err
		}
		if _, err := xmlMarkdown(b); err != nil {
			return "", err
		}
		for _, c := range elements(a) {
			if c.tag == "colgroup" && len(elements(c)) == tableColumns(b) {
				b.kids = append([]*xnode{c}, b.kids...)
				break
			}
		}
	} else if e = graft(a, b); e != nil {
		return "", e
	}
	return serialize(b), nil
}
func graft(a, b *xnode) error {
	// Only Decode-approved shapes reach here. Keep pre display settings and table
	// layout. List sequence stays current; a Markdown numbering change is structural.
	switch a.tag {
	case "pre":
		for _, x := range a.attrs {
			if x.Name.Local == "caption" || x.Name.Local == "wrap" {
				b.attrs = append(b.attrs, x)
			}
		}
	case "li":
		if a.attr("seq") != "" && a.attr("seq") != "auto" && a.attr("seq") != b.attr("seq") {
			return unsupported("list renumbering")
		}
		for _, x := range a.attrs {
			if x.Name.Local == "seq-marker" {
				b.attrs = append(b.attrs, x)
			}
		}
	case "p":
		if a.attr("align") != "" && b.attr("align") == "" {
			for _, x := range a.attrs {
				if x.Name.Local == "align" {
					b.attrs = append(b.attrs, x)
				}
			}
		}
	case "table":
		ae, be := elements(a), elements(b)
		if len(ae) > 0 && ae[0].tag == "colgroup" {
			b.kids = append([]*xnode{ae[0]}, b.kids...)
			ae = ae[1:]
		}
		if len(ae) != len(be) {
			return unsupported("table section change")
		}
		for i := range ae {
			if e := graftTable(ae[i], be[i]); e != nil {
				return e
			}
		}
	}
	return nil
}
func graftTable(a, b *xnode) error {
	if a.tag != b.tag {
		return unsupported("table shape changed")
	}
	ae, be := elements(a), elements(b)
	if a.tag == "p" {
		return nil
	}
	if len(ae) != len(be) {
		return unsupported("table dimensions changed")
	}
	for _, x := range a.attrs {
		if x.Name.Local != "id" {
			b.attrs = append(b.attrs, x)
		}
	}
	for i := range ae {
		if e := graftTable(ae[i], be[i]); e != nil {
			return e
		}
	}
	return nil
}
func serialize(n *xnode) string {
	if n.tag == "" {
		return esc(n.text)
	}
	var b strings.Builder
	b.WriteString("<" + n.tag)
	for _, a := range n.attrs {
		if a.Name.Local != "id" {
			b.WriteString(" " + a.Name.Local + `="` + esc(a.Value) + `"`)
		}
	}
	if len(n.kids) == 0 && (n.tag == "br" || n.tag == "hr" || n.tag == "col") {
		b.WriteString("/>")
		return b.String()
	}
	b.WriteByte('>')
	for _, c := range n.kids {
		b.WriteString(serialize(c))
	}
	b.WriteString("</" + n.tag + ">")
	return b.String()
}

func tableColumns(n *xnode) int {
	for _, section := range elements(n) {
		if section.tag != "thead" && section.tag != "tbody" {
			continue
		}
		for _, row := range elements(section) {
			if row.tag == "tr" {
				return len(elements(row))
			}
		}
	}
	return 0
}
