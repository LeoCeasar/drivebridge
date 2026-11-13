package markdown

import (
	"bytes"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/rclone/rclone/internal/bridge/model"
)

// Merge plans mutations against the current remote IDs only. After each mutation
// the caller must fetch/decode and replan; replacement may invalidate old IDs.
// Conflicts always return zero operations and retain the local bytes as the draft.
func Merge(baseLocal, local []byte, remote model.DocView) (model.MergeResult, error) {
	return merge(baseLocal, local, remote, false, false)
}

// PlanStep permits a supported insertion chain to defer operations whose new
// anchors do not exist yet. The caller must read back and rebase after each step.
// Markdown remains the complete merged intent; no future block ID is guessed.
func PlanStep(baseLocal, local []byte, remote model.DocView) (model.MergeResult, error) {
	return merge(baseLocal, local, remote, true, false)
}
func merge(baseLocal, local []byte, remote model.DocView, deferAnchors, allowTableResize bool) (model.MergeResult, error) {
	fail := func(reason string) (model.MergeResult, error) {
		return model.MergeResult{Markdown: bytes.Clone(local), ConflictReason: reason, Hunks: []model.Hunk{{BaseStart: 0, BaseEnd: len(exactLines(baseLocal)), Base: exactLines(baseLocal), Local: exactLines(local), Remote: exactLines(remote.Markdown)}}}, nil
	}
	for _, v := range remote.Blocks {
		if v.Start < 0 || v.End < v.Start || v.End > len(remote.Markdown) {
			return fail("unsupported_content")
		}
	}
	parse := func(b []byte) ([]atom, error) { return parseMarkdown(b, &remote) }
	b, e := parse(baseLocal)
	if e != nil {
		return contentConflict(local, e)
	}
	l, e := parse(local)
	if e != nil {
		return contentConflict(local, e)
	}
	r, e := parse(remote.Markdown)
	if e != nil {
		return contentConflict(local, e)
	}
	if len(r) != len(remote.Blocks) {
		return fail("unsupported_content")
	}
	for i := range r {
		rb := remote.Blocks[i]
		actual := r[i].Hash
		if !rb.Opaque {
			old, err := parseXML(rb.RawXML)
			fresh, err2 := parseXML([]byte(r[i].xml))
			if err != nil || err2 != nil || len(old) != 1 || len(fresh) != 1 {
				return fail("unsupported_content")
			}
			if _, err := xmlMarkdown(old[0]); err != nil {
				return fail("unsupported_content")
			}
			projected := unprojectCode(old[0], fresh[0])
			if semanticXML(old[0]) != semanticXML(projected) {
				return fail("unsupported_content")
			}
			actual = hashNode(r[i].Kind, projected)
			if remote.ProjectionVersion >= 3 {
				actual = operationNodeHash(r[i].Kind, projected)
			}
		}
		if rb.Hash != actual || rb.Start < 0 || rb.End > len(remote.Markdown) {
			return fail("unsupported_content")
		}
		projected := r[i].Hash
		r[i].Block = rb
		r[i].Hash = projected
		r[i].actualHash = actual
	}
	if len(b) > 10000 || len(l) > 10000 || len(r) > 10000 {
		return fail("merge_complexity")
	}
	alignmentReason := "ambiguous_mapping"
	for _, v := range b {
		if v.Opaque {
			alignmentReason = "unsupported_content"
			break
		}
	}
	al, e := align(b, l)
	if e != nil {
		return fail(alignmentReason)
	}
	ar, e := align(b, r)
	if e != nil {
		return fail(alignmentReason)
	}
	type chosen struct {
		a             atom
		text          string
		local, remote int
	}
	var out []chosen
	source := func(a atom, s []byte) string { return string(s[a.Start:a.End]) }
	for i := 0; i <= len(b); i++ {
		li, ri := al.inserts[i], ar.inserts[i]
		if len(li) > 0 && len(ri) > 0 {
			lh, rh := []string{}, []string{}
			for _, v := range li {
				lh = append(lh, l[v].Hash)
			}
			for _, v := range ri {
				rh = append(rh, r[v].Hash)
			}
			if !slices.Equal(lh, rh) {
				return fail("same_insertion")
			}
			for k, v := range li {
				out = append(out, chosen{l[v], source(l[v], local), v, ri[k]})
			}
		} else {
			for _, v := range li {
				if l[v].Opaque {
					return fail("unsupported_content")
				}
				out = append(out, chosen{l[v], source(l[v], local), v, -1})
			}
			for _, v := range ri {
				out = append(out, chosen{r[v], source(r[v], remote.Markdown), -1, v})
			}
		}
		if i == len(b) {
			break
		}
		lii, rii := al.index[i], ar.index[i]
		if b[i].Opaque {
			if lii < 0 || !l[lii].Opaque || l[lii].Hash != b[i].Hash {
				return fail("unsupported_content")
			}
			if rii < 0 {
				continue
			}
			out = append(out, chosen{r[rii], source(l[lii], local), lii, rii})
			continue
		}
		if lii < 0 && rii < 0 {
			continue
		}
		if lii < 0 {
			if r[rii].Hash != b[i].Hash {
				return fail("delete_vs_edit")
			}
			continue
		}
		if rii < 0 {
			if l[lii].Hash != b[i].Hash {
				return fail("delete_vs_edit")
			}
			continue
		}
		lv, rv := l[lii], r[rii]
		if lv.Opaque || rv.Opaque {
			return fail("unsupported_content")
		}
		c := chosen{lv, source(lv, local), lii, rii}
		switch {
		case lv.Hash == rv.Hash:
		case rv.Hash == b[i].Hash:
		case lv.Hash == b[i].Hash:
			c.a = rv
			c.text = withEnding(source(rv, remote.Markdown), source(lv, local))
		default:
			if b[i].Kind == "table" {
				return fail("table_both_changed")
			}
			merged, ok := mergeText(trimEnding(source(b[i], baseLocal)), trimEnding(source(lv, local)), trimEnding(source(rv, remote.Markdown)))
			if !ok {
				return fail("overlapping_edits")
			}
			c.text = withEnding(merged, source(lv, local))
			aa, err := parseMarkdown([]byte(c.text), nil)
			if err != nil || len(aa) != 1 || aa[0].Kind != lv.Kind {
				return fail("unsupported_content")
			}
			c.a = aa[0]
		}
		out = append(out, c)
	}
	// Keep all original local trivia whenever selected neighboring atoms remain
	// neighbors. New adjacency gets a minimal blank-line separator.
	var md strings.Builder
	previousLocal := -1
	for i, c := range out {
		if i == 0 {
			if c.local == 0 {
				md.Write(local[:l[0].Start])
			}
		} else {
			if c.local >= 0 && previousLocal >= 0 && c.local == previousLocal+1 {
				md.Write(local[l[previousLocal].End:l[c.local].Start])
			} else {
				existing := md.String()
				if !strings.HasSuffix(existing, "\n") {
					md.WriteByte('\n')
				}
				if c.a.parent == "" || out[i-1].a.parent != c.a.parent {
					md.WriteByte('\n')
				}
			}
		}
		md.WriteString(c.text)
		previousLocal = c.local
	}
	if len(out) > 0 && out[len(out)-1].local == len(l)-1 && len(l) > 0 {
		md.Write(local[l[len(l)-1].End:])
	} else if len(out) == 0 && len(l) == 0 {
		md.Write(local)
	}
	result := model.MergeResult{Markdown: []byte(md.String())}
	// Reparse output: a delimiter changed by merge must never silently change shape.
	final, e := parseMarkdown(result.Markdown, &remote)
	if e != nil || len(final) != len(out) {
		return fail("unsupported_content")
	}
	for i := range final {
		if final[i].Hash != out[i].a.Hash {
			return fail("unsupported_content")
		}
	}
	kept := map[int]bool{}
	for _, c := range out {
		if c.remote >= 0 {
			kept[c.remote] = true
		}
	}
	for i, rv := range r {
		if kept[i] {
			continue
		}
		if rv.Opaque || rv.RemoteID == "" {
			return fail("unsupported_content")
		}
		result.Operations = append(result.Operations, operation(model.OperationBlockDelete, rv.RemoteID, "", model.OperationPayload{BeforeHash: rv.actualHash}))
	}
	for i, c := range out {
		if c.remote >= 0 {
			rv := r[c.remote]
			if c.a.Hash == rv.Hash {
				continue
			}
			if rv.Opaque || rv.RemoteID == "" {
				return fail("unsupported_content")
			}
			x, err := replacementXMLMode(rv, c.a, allowTableResize)
			if err != nil {
				return fail("unsupported_content")
			}
			result.Operations = append(result.Operations, operation(model.OperationBlockReplace, rv.RemoteID, "", model.OperationPayload{XML: x, BeforeHash: rv.actualHash, AfterHash: semanticHash(c.a.Kind, x)}))
			continue
		}
		// Sentinel 0 is the experimentally confirmed document-start anchor. It
		// addresses the first body position, never the title or an invented ID.
		if i == 0 && !c.a.Opaque {
			result.Operations = append(result.Operations, operation(model.OperationBlockInsertAfter, "", "0", model.OperationPayload{XML: insertionXML(c.a), AfterHash: c.a.Hash}))
			continue
		}
		// A newly inserted block has no current ID: do not chain guesses.
		if deferAnchors && i > 0 && out[i-1].remote < 0 && !c.a.Opaque {
			result.Deferred = true
			continue
		}
		if i == 0 || out[i-1].remote < 0 || c.a.Opaque {
			return fail("unsupported_content")
		}
		anchor := r[out[i-1].remote]
		if anchor.RemoteID == "" || anchor.Opaque || !rootInsertionAnchor(remote, anchor, c.a.parent) {
			return fail("unsupported_content")
		}
		result.Operations = append(result.Operations, operation(model.OperationBlockInsertAfter, "", anchor.RemoteID, model.OperationPayload{XML: insertionXML(c.a), AfterHash: c.a.Hash}))
	}
	return result, nil
}
func contentConflict(local []byte, e error) (model.MergeResult, error) {
	var ce *ContentError
	if errors.As(e, &ce) {
		return model.MergeResult{Markdown: bytes.Clone(local), ConflictReason: ce.Reason}, nil
	}
	return model.MergeResult{}, e
}
func trimEnding(s string) string        { return strings.TrimRight(s, "\r\n") }
func withEnding(s, style string) string { return trimEnding(s) + style[len(trimEnding(style)):] }

type edit struct {
	start, end int
	value      string
}

// Exact rune strings avoid UTF-8 splits; difflib auto-junk is explicitly disabled.
// A strict work budget bounds repeated-text quadratic behavior.
func edits(a, b string) ([]edit, bool) {
	ar, br := strings.Split(a, ""), strings.Split(b, "")
	if int64(len(ar))*int64(len(br)) > 4_000_000 {
		return nil, false
	}
	m := difflib.NewMatcherWithJunk(ar, br, false, nil)
	var out []edit
	for _, o := range m.GetOpCodes() {
		if o.Tag != 'e' {
			out = append(out, edit{o.I1, o.I2, strings.Join(br[o.J1:o.J2], "")})
		}
	}
	return out, true
}
func mergeText(base, local, remote string) (string, bool) {
	if local == remote {
		return local, true
	}
	if base == remote {
		return local, true
	}
	if base == local {
		return remote, true
	}
	l, ok := edits(base, local)
	if !ok {
		return "", false
	}
	r, ok := edits(base, remote)
	if !ok {
		return "", false
	}
	all := append([]edit(nil), l...)
	for _, b := range r {
		equal := false
		for _, a := range l {
			if a == b {
				equal = true
				break
			}
			overlap := a.start < b.end && b.start < a.end
			if a.start == a.end {
				overlap = overlap || a.start >= b.start && a.start <= b.end
			}
			if b.start == b.end {
				overlap = overlap || b.start >= a.start && b.start <= a.end
			}
			if overlap {
				return "", false
			}
		}
		if !equal {
			all = append(all, b)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })
	rr := []rune(base)
	var out strings.Builder
	p := 0
	for _, e := range all {
		out.WriteString(string(rr[p:e.start]))
		out.WriteString(e.value)
		p = e.end
	}
	out.WriteString(string(rr[p:]))
	return out.String(), true
}

// Hunk indexes are line indexes; SplitAfter retains exact terminators and never
// invents a final newline (unlike difflib.SplitLines).
func exactLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	a := strings.SplitAfter(string(b), "\n")
	if a[len(a)-1] == "" {
		a = a[:len(a)-1]
	}
	return a
}

// List wrappers in the XML projection group directly addressable sibling items.
// Wrap new items explicitly; a naked <li> is not a document fragment.
func insertionXML(a atom) string {
	if a.parent == "ul" || a.parent == "ol" {
		return "<" + a.parent + ">" + a.xml + "</" + a.parent + ">"
	}
	return a.xml
}
func rootInsertionAnchor(d model.DocView, anchor atom, insertedParent string) bool {
	if anchor.parent == "" {
		return true
	}
	nodes, err := parseXML(d.XML)
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if n.tag != "ul" && n.tag != "ol" {
			continue
		}
		if !simpleList(n) || !projectableList(n) {
			continue
		}
		items := elements(n)
		for i, item := range items {
			if item.attr("id") == anchor.RemoteID {
				return insertedParent == n.tag || i == len(items)-1
			}
		}
	}
	return false
}
