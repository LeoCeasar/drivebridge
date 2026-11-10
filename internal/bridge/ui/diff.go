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
		return []DiffHunk{{ID: 0, BaseStart: 0, BaseEnd: len(b), Base: base, Local: local, Remote: remote}}
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
		result = append(result, DiffHunk{ID: i, BaseStart: x.a, BaseEnd: x.b, Base: strings.Join(b[x.a:x.b], ""), Local: project(x.a, x.b, lc, l), Remote: project(x.a, x.b, rc, r)})
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
