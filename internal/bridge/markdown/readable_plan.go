package markdown

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// PlanStepWithBase keeps the two checkpoints separate. Whole-document parsing
// precedes planning, and any unsupported edit discards every planned operation.
func PlanStepWithBase(baseLocal, local []byte, baseRemote, remote model.DocView) (model.MergeResult, error) {
	if baseRemote.ProjectionVersion < 2 && remote.ProjectionVersion < 2 {
		return planPlainWithBase(baseLocal, local, baseRemote, remote)
	}
	rich := false
	for _, d := range []model.DocView{baseRemote, remote} {
		for _, b := range d.Blocks {
			rich = rich || b.Readable
		}
	}
	if !rich {
		return planPlainWithBase(baseLocal, local, baseRemote, remote)
	}
	return planReadable(baseLocal, local, baseRemote, remote)
}
func PlanResolvedStepWithBase(baseLocal, local []byte, baseRemote, remote model.DocView) (model.MergeResult, error) {
	rich := false
	for _, d := range []model.DocView{baseRemote, remote} {
		for _, b := range d.Blocks {
			rich = rich || b.Readable
		}
	}
	if !rich {
		return PlanResolvedStep(baseLocal, local, remote)
	}
	if baseRemote.ProjectionVersion < 2 && remote.ProjectionVersion < 2 {
		return PlanResolvedStep(baseLocal, local, remote)
	}
	r, e := PlanStepWithBase(baseLocal, local, baseRemote, remote)
	if e == nil && r.ConflictReason == "" {
		for i := range r.Operations {
			var p model.OperationPayload
			_ = json.Unmarshal(r.Operations[i].Payload, &p)
			p.ExplicitResolution = true
			r.Operations[i].Payload, _ = json.Marshal(p)
		}
	}
	return r, e
}

func projectedAtoms(b []byte) ([]atom, error) {
	if e := validate(b); e != nil {
		return nil, e
	}
	root := parser().Parser().Parse(text.NewReader(b))
	var out []atom
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		if _, ok := n.(*ast.List); ok {
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				out = append(out, atom{node: c})
			}
		} else {
			out = append(out, atom{node: n})
		}
	}
	for i := range out {
		p := out[i].node.Pos()
		if p < 0 || p > len(b) {
			return nil, unsupported("source position")
		}
		for p > 0 && b[p-1] != '\n' {
			p--
		}
		out[i].Start = p
	}
	for i := range out {
		end := len(b)
		if i+1 < len(out) {
			end = out[i+1].Start
		}
		for end > out[i].Start && b[end-1] == '\n' {
			end--
		}
		out[i].End = end
		source := b[out[i].Start:end]
		if _, ok := out[i].node.(*ast.HTMLBlock); ok {
			nodes, e := parseXML(source)
			if e != nil || len(nodes) != 1 {
				return nil, unsupported("readable HTML shape")
			}
			out[i].Kind = nodes[0].tag
			out[i].xml = string(source)
			normalizeLocalAssetAliases(nodes[0])
			out[i].Hash = digest("readable\x00" + opaqueSemantic(nodes[0]))
			out[i].Readable = true
		} else {
			a, e := parseMarkdown(source, nil)
			if e != nil || len(a) != 1 {
				return nil, unsupported("whole document shape")
			}
			p, q := out[i].Start, out[i].End
			out[i] = a[0]
			out[i].Start = p
			out[i].End = q
		}
	}
	// CommonMark-consumed reference definitions and all other nonblank gaps fail.
	p := 0
	for _, a := range out {
		if len(bytes.TrimSpace(b[p:a.Start])) != 0 {
			return nil, unsupported("unmapped source")
		}
		p = a.End
	}
	if len(bytes.TrimSpace(b[p:])) != 0 {
		return nil, unsupported("unmapped source")
	}
	return out, nil
}

// Minimum-edit alignment accepts only a unique complete mapping. In particular,
// deleting one of repeated equal paragraphs has more than one valid identity.
func alignProjected(a, b []atom) (alignment, error) {
	n, m := len(a), len(b)
	// Repeated unchanged display blocks may anchor their complete sequence, but
	// changing their multiplicity cannot establish which remote identity was edited.
	ac, bc := map[string]int{}, map[string]int{}
	for _, v := range a {
		ac[v.Hash]++
	}
	for _, v := range b {
		bc[v.Hash]++
	}
	for h, c := range ac {
		if c > 1 && bc[h] != c {
			return alignment{}, unsupported("ambiguous_mapping")
		}
	}
	for h, c := range bc {
		if c > 1 && ac[h] != c {
			return alignment{}, unsupported("ambiguous_mapping")
		}
	}
	if (n+1)*(m+1) > 4_000_000 {
		return alignment{}, unsupported("mapping complexity")
	}
	width := m + 1
	cost := make([]int, (n+1)*width)
	ways := make([]uint8, len(cost))
	ways[0] = 1
	for i := 0; i <= n; i++ {
		for j := 0; j <= m; j++ {
			if i == 0 && j == 0 {
				continue
			}
			k := i*width + j
			best := n + m + 4
			var w uint8
			add := func(c int, v uint8) {
				if c < best {
					best = c
					w = v
				} else if c == best {
					w += v
				}
				if w > 2 {
					w = 2
				}
			}
			if i > 0 {
				add(cost[k-width]+1, ways[k-width])
			}
			if j > 0 {
				add(cost[k-1]+1, ways[k-1])
			}
			if i > 0 && j > 0 && a[i-1].Kind == b[j-1].Kind {
				v := 1
				if a[i-1].Hash == b[j-1].Hash {
					v = 0
				}
				add(cost[k-width-1]+v, ways[k-width-1])
			}
			cost[k] = best
			ways[k] = w
		}
	}
	if ways[n*width+m] != 1 {
		return alignment{}, unsupported("ambiguous_mapping")
	}
	r := alignment{index: make([]int, n), inserts: map[int][]int{}}
	for i := range r.index {
		r.index[i] = -1
	}
	for i, j := n, m; i > 0 || j > 0; {
		k := i*width + j
		if i > 0 && j > 0 && a[i-1].Kind == b[j-1].Kind {
			v := 1
			if a[i-1].Hash == b[j-1].Hash {
				v = 0
			}
			if cost[k] == cost[k-width-1]+v {
				r.index[i-1] = j - 1
				i--
				j--
				continue
			}
		}
		if j > 0 && cost[k] == cost[k-1]+1 {
			r.inserts[i] = append([]int{j - 1}, r.inserts[i]...)
			j--
		} else {
			i--
		}
	}
	return r, nil
}

func planReadable(base, local []byte, baseline, current model.DocView) (model.MergeResult, error) {
	fail := func(reason string) (model.MergeResult, error) {
		return model.MergeResult{Markdown: bytes.Clone(local), ConflictReason: reason}, nil
	}
	// Re-derive all trusted sidecar fields before using any nested remote ID.
	for di, d := range []model.DocView{baseline, current} {
		fresh, e := DecodeWithOptions(d.XML, d.Revision, Options{ProjectionVersion: 2, SourceURL: d.SourceURL, ReferenceMap: d.ReferenceMap})
		if e != nil || !bytes.Equal(fresh.Markdown, d.Markdown) || len(fresh.Blocks) != len(d.Blocks) {
			return fail("unsupported_content")
		}
		for i, b := range fresh.Blocks {
			if b.Hash != d.Blocks[i].Hash || b.RemoteID != d.Blocks[i].RemoteID || !bytes.Equal(b.RawXML, d.Blocks[i].RawXML) {
				return fail("unsupported_content")
			}
		}
		if di == 0 {
			baseline = fresh
		} else {
			current = fresh
		}
	}
	b, e := projectedAtoms(base)
	if e != nil {
		return fail("unsupported_content")
	}
	l, e := projectedAtoms(local)
	if e != nil {
		return fail("unsupported_content")
	}
	br, e := projectedAtoms(baseline.Markdown)
	if e != nil {
		return fail("unsupported_content")
	}
	r, e := projectedAtoms(current.Markdown)
	if e != nil || len(br) != len(baseline.Blocks) || len(r) != len(current.Blocks) {
		return fail("unsupported_content")
	}
	al, e := alignProjected(b, l)
	if e != nil {
		return fail("ambiguous_mapping")
	}
	ab, e := alignProjected(b, br)
	if e != nil {
		return fail("ambiguous_mapping")
	}
	ar, e := alignProjected(br, r)
	if e != nil {
		return fail("ambiguous_mapping")
	}
	// Structural drift between the two saved checkpoints needs a separate identity
	// mapping. Preserve that conservative boundary while merging current additions.
	if len(b) != len(br) {
		return fail("ambiguous_mapping")
	}
	for i := range b {
		if ab.index[i] != i {
			return fail("ambiguous_mapping")
		}
	}
	type chosen struct {
		a      atom
		text   string
		li, ri int
	}
	var out []chosen
	var ops []model.Operation
	source := func(a atom, src []byte) string { return string(src[a.Start:a.End]) }
	for i := 0; i <= len(b); i++ {
		ls, rs := al.inserts[i], ar.inserts[i]
		if len(ls) > 0 && len(rs) > 0 {
			if len(ls) != len(rs) {
				return fail("same_insertion")
			}
			for j, li := range ls {
				ri := rs[j]
				if l[li].Hash != r[ri].Hash {
					return fail("same_insertion")
				}
				if !ordinaryTopLevel(l[li]) {
					return fail("unsupported_content")
				}
				out = append(out, chosen{l[li], source(l[li], local), li, ri})
			}
		} else {
			for _, li := range ls {
				if !ordinaryTopLevel(l[li]) {
					return fail("unsupported_content")
				}
				out = append(out, chosen{l[li], source(l[li], local), li, -1})
			}
			for _, ri := range rs {
				out = append(out, chosen{r[ri], source(r[ri], current.Markdown), -1, ri})
			}
		}
		if i == len(b) {
			break
		}
		ba := b[i]
		li, ri := al.index[i], ar.index[i]
		old := baseline.Blocks[i]
		if li < 0 {
			// Local deletion must have an ordinary, unchanged current target.
			if !ordinaryTopLevel(br[i]) || old.Opaque || old.Readable {
				return fail("unsupported_content")
			}
			if ri >= 0 {
				now := current.Blocks[ri]
				if ba.Hash != br[i].Hash || r[ri].Hash != br[i].Hash {
					return fail("delete_vs_edit")
				}
				if now.RemoteID == "" || now.Opaque || now.Readable {
					return fail("unsupported_content")
				}
				ops = append(ops, operation(model.OperationBlockDelete, now.RemoteID, "", model.OperationPayload{BeforeHash: now.Hash}))
			}
			continue
		}
		la := l[li]
		if ri < 0 {
			if la.Hash != ba.Hash {
				return fail("delete_vs_edit")
			}
			continue
		}
		ra, now := r[ri], current.Blocks[ri]
		text := source(la, local)
		switch {
		case la.Hash == ba.Hash:
			text = source(ra, current.Markdown)
		case la.Hash == ra.Hash:
		case old.Readable || now.Readable:
			if !old.Readable || !now.Readable || old.Kind != "table" || now.Kind != "table" {
				return fail("unsupported_content")
			}
			x, planned, err := planTable(ba, la, br[i], ra, old, now)
			if err != nil {
				return contentConflict(local, err)
			}
			text = x
			ops = append(ops, planned...)
		default:
			if old.Opaque || now.Opaque || now.RemoteID == "" {
				return fail("unsupported_content")
			}
			if ba.Hash != br[i].Hash || br[i].Hash != ra.Hash {
				return fail("overlapping_edits")
			}
			ra.Block = now
			x, err := replacementXML(ra, la)
			if err != nil {
				return fail("unsupported_content")
			}
			ops = append(ops, operation(model.OperationBlockReplace, now.RemoteID, "", model.OperationPayload{XML: x, BeforeHash: now.Hash, AfterHash: semanticHash(la.Kind, x)}))
		}
		out = append(out, chosen{la, text, li, ri})
	}
	result := model.MergeResult{Operations: ops}
	var md strings.Builder
	previousLocal := -1
	for i, c := range out {
		if i == 0 {
			if c.li == 0 {
				md.Write(local[:l[0].Start])
			}
		} else if c.li >= 0 && previousLocal >= 0 && c.li == previousLocal+1 {
			md.Write(local[l[previousLocal].End:l[c.li].Start])
		} else {
			if !strings.HasSuffix(md.String(), "\n") {
				md.WriteByte('\n')
			}
			if c.a.parent == "" || out[i-1].a.parent != c.a.parent {
				md.WriteByte('\n')
			}
		}
		md.WriteString(c.text)
		previousLocal = c.li
		if c.ri >= 0 {
			continue
		}
		if !ordinaryTopLevel(c.a) {
			return fail("unsupported_content")
		}
		if i == 0 {
			result.Operations = append(result.Operations, operation(model.OperationBlockInsertAfter, "", "0", model.OperationPayload{XML: c.a.xml, AfterHash: c.a.Hash}))
			continue
		}
		if out[i-1].ri < 0 {
			result.Deferred = true
			continue
		}
		anchor := current.Blocks[out[i-1].ri]
		if anchor.RemoteID == "" || strings.Contains(anchor.Kind, "/li") {
			return fail("unsupported_content")
		}
		result.Operations = append(result.Operations, operation(model.OperationBlockInsertAfter, "", anchor.RemoteID, model.OperationPayload{XML: c.a.xml, AfterHash: c.a.Hash}))
	}
	if len(out) > 0 && out[len(out)-1].li == len(l)-1 && len(l) > 0 {
		md.Write(local[l[len(l)-1].End:])
	} else if len(out) > 0 {
		md.WriteByte('\n')
	} else if len(l) == 0 {
		md.Write(local)
	}
	result.Markdown = []byte(md.String())
	final, err := projectedAtoms(result.Markdown)
	if err != nil || len(final) != len(out) {
		return fail("unsupported_content")
	}
	for i, c := range out {
		expected, err := projectedAtoms([]byte(c.text))
		if err != nil || len(expected) != 1 || expected[0].Hash != final[i].Hash {
			return fail("unsupported_content")
		}
	}
	return result, nil
}

func ordinaryTopLevel(a atom) bool {
	if a.Readable || a.Opaque || a.parent != "" {
		return false
	}
	switch a.Kind {
	case "p", "h1", "h2", "h3", "h4", "h5", "h6", "pre":
		return true
	}
	return false
}

func paragraphNodes(n *xnode) []*xnode {
	var out []*xnode
	var walk func(*xnode)
	walk = func(v *xnode) {
		if v.tag == "p" {
			out = append(out, v)
		}
		for _, c := range v.kids {
			walk(c)
		}
	}
	walk(n)
	return out
}
func tableMask(n *xnode, allowed map[int]bool) string {
	p := 0
	var walk func(*xnode)
	walk = func(v *xnode) {
		if v.tag == "p" {
			if allowed[p] {
				v.kids = []*xnode{{text: "__drivebridge_paragraph__"}}
			}
			p++
		}
		for _, c := range v.kids {
			walk(c)
		}
	}
	walk(n)
	return opaqueSemantic(n)
}
func oneXML(s string) (*xnode, error) {
	n, e := parseXML([]byte(s))
	if e != nil {
		return nil, e
	}
	if len(n) != 1 {
		return nil, unsupported("table source")
	}
	normalizeLocalAssetAliases(n[0])
	return n[0], nil
}
func planTable(base, local, baseRemote, remote atom, old, now model.Block) (string, []model.Operation, error) {
	parse := func(s string) *xnode { n, _ := oneXML(s); return n }
	bn, ln, brn, rn := parse(base.xml), parse(local.xml), parse(baseRemote.xml), parse(remote.xml)
	on, nn := parse(string(old.RawXML)), parse(string(now.RawXML))
	if bn == nil || ln == nil || brn == nil || rn == nil || on == nil || nn == nil {
		return "", nil, unsupported("table XML")
	}
	bp, lp, brp, rp, op, np := paragraphNodes(bn), paragraphNodes(ln), paragraphNodes(brn), paragraphNodes(rn), paragraphNodes(on), paragraphNodes(nn)
	if len(bp) != len(lp) || len(bp) != len(brp) || len(bp) != len(rp) || len(bp) != len(op) || len(bp) != len(np) {
		return "", nil, unsupported("table paragraphs")
	}
	allowed := map[int]bool{}
	for i, p := range op {
		allowed[i] = len(elements(p)) == 0 && p.allowed("align") && p.attr("id") != "" && len(elements(np[i])) == 0 && np[i].allowed("align") && np[i].attr("id") != ""
	}
	// Validate every structural/style/resource byte semantically before planning.
	mask := tableMask(parse(base.xml), allowed)
	for _, a := range []atom{local, baseRemote, remote} {
		if tableMask(parse(a.xml), allowed) != mask {
			return "", nil, unsupported("table structure or style")
		}
	}
	if tableMask(parse(string(old.RawXML)), allowed) != tableMask(parse(string(now.RawXML)), allowed) {
		return "", nil, unsupported("remote table structure or style")
	}
	var ops []model.Operation
	for i := range bp {
		if !allowed[i] {
			continue
		}
		if len(elements(bp[i])) != 0 || len(elements(lp[i])) != 0 || len(elements(brp[i])) != 0 || len(elements(rp[i])) != 0 {
			return "", nil, unsupported("rich paragraph replacement")
		}
		bv, lv, brv, rv := plainText(bp[i]), plainText(lp[i]), plainText(brp[i]), plainText(rp[i])
		if lv == bv {
			continue
		}
		if lv == rv {
			continue
		}
		if bv != brv || rv != brv {
			return "", nil, &ContentError{Reason: "overlapping_edits", Detail: "same paragraph changed"}
		}
		fresh := &xnode{tag: "p", attrs: append([]xml.Attr(nil), np[i].attrs...), kids: []*xnode{{text: lv}}}
		x := serialize(fresh)
		ops = append(ops, operation(model.OperationBlockReplace, np[i].attr("id"), "", model.OperationPayload{XML: x, BeforeHash: hashNode("p", np[i]), AfterHash: hashNode("p", fresh)}))
		rp[i].kids = []*xnode{{text: lv}}
	}
	// Keep remote-only paragraph changes in the complete desired projection.
	return serialize(rn), ops, nil
}

// Editors may spell the same relative managed image with a leading "./".
// Normalize only that alias for comparison; other resource/path edits still
// differ from the accepted projection and remain unsupported.
func normalizeLocalAssetAliases(n *xnode) {
	if n.tag == "img" {
		for i, a := range n.attrs {
			if a.Name.Space == "" && a.Name.Local == "src" && strings.HasPrefix(a.Value, "./.drivebridge-assets/") {
				n.attrs[i].Value = strings.TrimPrefix(a.Value, "./")
			}
		}
	}
	for _, c := range n.kids {
		normalizeLocalAssetAliases(c)
	}
}
