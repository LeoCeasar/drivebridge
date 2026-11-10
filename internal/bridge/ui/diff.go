package ui

import (
	"github.com/pmezard/go-difflib/difflib"
	"sort"
	"strings"
)

func lines(s string) []string {
	if s == "" {
		return nil
	}
	v := strings.SplitAfter(s, "\n")
	if v[len(v)-1] == "" {
		v = v[:len(v)-1]
	}
	return v
}
func diffHunks(base, local, remote string) []DiffHunk {
	b, l, r := lines(base), lines(local), lines(remote)
	if len(b)+len(l)+len(r) > 2000 || len(base)+len(local)+len(remote) > 2<<20 {
		return []DiffHunk{{ID: 0, BaseStart: 0, BaseEnd: len(b), LocalStart: 0, LocalEnd: len(l), RemoteStart: 0, RemoteEnd: len(r), Base: base, Local: local, Remote: remote, WholeDocument: true, BaseChanges: []DiffChange{{Start: 0, End: len(b), Kind: "replace"}}, LocalChanges: []DiffChange{{Start: 0, End: len(l), Kind: "replace"}}, RemoteChanges: []DiffChange{{Start: 0, End: len(r), Kind: "replace"}}}}
	}
	lc := difflib.NewMatcher(b, l).GetOpCodes()
	rc := difflib.NewMatcher(b, r).GetOpCodes()
	type interval struct{ a, b int }
	var changed []interval
	for _, ops := range [][]difflib.OpCode{lc, rc} {
		for _, o := range ops {
			if o.Tag != 'e' {
				changed = append(changed, interval{o.I1, o.I2})
			}
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].a == changed[j].a {
			return changed[i].b < changed[j].b
		}
		return changed[i].a < changed[j].a
	})
	var merged []interval
	for _, x := range changed {
		if len(merged) > 0 && x.a <= merged[len(merged)-1].b {
			if x.b > merged[len(merged)-1].b {
				merged[len(merged)-1].b = x.b
			}
		} else {
			merged = append(merged, x)
		}
	}
	var result []DiffHunk
	for i, x := range merged {
		h := DiffHunk{ID: i, BaseStart: x.a, BaseEnd: x.b, Base: strings.Join(b[x.a:x.b], ""), Local: project(x.a, x.b, lc, l), Remote: project(x.a, x.b, rc, r)}
		h.LocalStart, h.LocalEnd = variantRange(x.a, x.b, lc)
		h.RemoteStart, h.RemoteEnd = variantRange(x.a, x.b, rc)
		h.LocalChanges, h.LocalAdded, h.LocalRemoved = changedSpans(x.a, x.b, lc, "local")
		h.RemoteChanges, h.RemoteAdded, h.RemoteRemoved = changedSpans(x.a, x.b, rc, "remote")
		h.BaseChanges = []DiffChange{}
		for _, changes := range [][]DiffChange{h.LocalChanges, h.RemoteChanges} {
			for _, change := range changes {
				base := change
				base.Start, base.End = change.BaseStart, change.BaseEnd
				h.BaseChanges = append(h.BaseChanges, base)
			}
		}
		result = append(result, h)
	}
	return result
}
func project(start, end int, ops []difflib.OpCode, variant []string) string {
	var out strings.Builder
	for _, o := range ops {
		switch o.Tag {
		case 'e':
			a, b := max(start, o.I1), min(end, o.I2)
			if a < b {
				out.WriteString(strings.Join(variant[o.J1+a-o.I1:o.J1+b-o.I1], ""))
			}
		case 'i':
			if o.I1 >= start && o.I1 <= end {
				out.WriteString(strings.Join(variant[o.J1:o.J2], ""))
			}
		default:
			if o.I1 >= start && o.I2 <= end {
				out.WriteString(strings.Join(variant[o.J1:o.J2], ""))
			}
		}
	}
	return out.String()
}

func changedSpans(start, end int, ops []difflib.OpCode, side string) (result []DiffChange, added, removed int) {
	result = []DiffChange{}
	for _, op := range ops {
		if op.Tag == 'e' || op.I1 < start || op.I2 > end {
			continue
		}
		kind := "replace"
		if op.Tag == 'i' {
			kind = "insert"
		} else if op.Tag == 'd' {
			kind = "delete"
		}
		result = append(result, DiffChange{Start: op.J1, End: op.J2, BaseStart: op.I1, BaseEnd: op.I2, Kind: kind, Side: side})
		added += op.J2 - op.J1
		removed += op.I2 - op.I1
	}
	return
}
func variantRange(start, end int, ops []difflib.OpCode) (int, int) {
	first, last := -1, 0
	for _, op := range ops {
		a, b := op.J1, op.J2
		include := false
		if op.Tag == 'e' {
			left, right := max(start, op.I1), min(end, op.I2)
			if left < right {
				a, b = op.J1+left-op.I1, op.J1+right-op.I1
				include = true
			}
		} else if op.I1 >= start && op.I2 <= end {
			include = true
		}
		if include {
			if first < 0 {
				first = a
			}
			last = b
		}
	}
	if first >= 0 {
		return first, last
	}
	for _, op := range ops {
		if start >= op.I1 && start <= op.I2 {
			if op.Tag == 'e' {
				p := op.J1 + start - op.I1
				return p, p
			}
			if start == op.I2 {
				return op.J2, op.J2
			}
			return op.J1, op.J1
		}
	}
	if len(ops) > 0 {
		p := ops[len(ops)-1].J2
		return p, p
	}
	return 0, 0
}
