package markdown

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"
)

const (
	plainMaxBytes    = 10 << 20
	plainMaxLines    = 100_000
	plainDiffWork    = 4_000_000
	plainRefineBytes = 1 << 20
)

// MergePlain merges an ordinary Markdown file as exact UTF-8 text. It does not
// interpret Markdown syntax or the online document's supported XML subset. On
// uncertainty it returns an independent copy of the local draft and a reason,
// never conflict markers that could accidentally be uploaded as real content.
func MergePlain(base, local, remote []byte) ([]byte, string) {
	fail := func(reason string) ([]byte, string) { return bytes.Clone(local), reason }
	for _, body := range [][]byte{base, local, remote} {
		if len(body) > plainMaxBytes {
			return fail("merge_complexity")
		}
		if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
			return fail("unsupported_content")
		}
	}
	if bytes.Equal(local, remote) || bytes.Equal(base, remote) {
		return bytes.Clone(local), ""
	}
	if bytes.Equal(base, local) {
		return bytes.Clone(remote), ""
	}
	for _, body := range [][]byte{base, local, remote} {
		if bytes.Count(body, []byte{'\n'}) > plainMaxLines {
			return fail("merge_complexity")
		}
	}
	budget := int64(plainDiffWork)
	merged, reason := plainMergeSequences(exactLines(base), exactLines(local), exactLines(remote), true, &budget)
	if reason != "" {
		return fail(reason)
	}
	if len(merged) > plainMaxBytes {
		return fail("merge_complexity")
	}
	return []byte(merged), ""
}

// Differences are measured in sequence indexes, with exact replacement bytes.
// Common boundaries are removed before the work check: a large document with a
// small edit does not need a whole-document rune-by-rune quadratic comparison.
func plainChanges(base, changed []string, budget *int64) ([]edit, bool) {
	prefix := 0
	for prefix < len(base) && prefix < len(changed) && base[prefix] == changed[prefix] {
		prefix++
	}
	endBase, endChanged := len(base), len(changed)
	for endBase > prefix && endChanged > prefix && base[endBase-1] == changed[endChanged-1] {
		endBase--
		endChanged--
	}
	if endBase == prefix || endChanged == prefix {
		if endBase == prefix && endChanged == prefix {
			return nil, true
		}
		return []edit{{prefix, endBase, strings.Join(changed[prefix:endChanged], "")}}, true
	}
	work := int64(endBase-prefix) * int64(endChanged-prefix)
	if work > *budget {
		return nil, false
	}
	*budget -= work
	matcher := difflib.NewMatcherWithJunk(base[prefix:endBase], changed[prefix:endChanged], false, nil)
	var result []edit
	for _, op := range matcher.GetOpCodes() {
		if op.Tag != 'e' {
			result = append(result, edit{prefix + op.I1, prefix + op.I2, strings.Join(changed[prefix+op.J1:prefix+op.J2], "")})
		}
	}
	return result, true
}

type plainEdit struct {
	edit
	local bool
}

func plainMergeSequences(base, local, remote []string, refine bool, budget *int64) (string, string) {
	left, ok := plainChanges(base, local, budget)
	if !ok {
		return "", "merge_complexity"
	}
	right, ok := plainChanges(base, remote, budget)
	if !ok {
		return "", "merge_complexity"
	}
	all := make([]plainEdit, 0, len(left)+len(right))
	for _, e := range left {
		all = append(all, plainEdit{e, true})
	}
	for _, e := range right {
		all = append(all, plainEdit{e, false})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].end < all[j].end
	})
	var out strings.Builder
	position := 0
	for i := 0; i < len(all); {
		start, end := all[i].start, all[i].end
		next := i + 1
		for next < len(all) && plainOverlap(start, end, all[next].edit) {
			if all[next].end > end {
				end = all[next].end
			}
			next++
		}
		b := strings.Join(base[start:end], "")
		l := plainApply(base, all[i:next], start, end, true)
		r := plainApply(base, all[i:next], start, end, false)
		var selected string
		switch {
		case l == r, b == r:
			selected = l
		case b == l:
			selected = r
		case !refine:
			return "", "overlapping_edits"
		default:
			// Only overlapping line regions need rune precision. Bound the
			// allocation too, even if their unchanged middle is extremely long.
			if len(b)+len(l)+len(r) > plainRefineBytes {
				return "", "merge_complexity"
			}
			var reason string
			selected, reason = plainMergeSequences(strings.Split(b, ""), strings.Split(l, ""), strings.Split(r, ""), false, budget)
			if reason != "" {
				return "", reason
			}
		}
		out.WriteString(strings.Join(base[position:start], ""))
		out.WriteString(selected)
		position = end
		i = next
	}
	out.WriteString(strings.Join(base[position:], ""))
	return out.String(), ""
}

// Insertions on a replacement boundary are grouped for finer inspection; two
// positive-width edits merely adjacent to each other remain independent.
func plainOverlap(start, end int, next edit) bool {
	if start == end {
		return next.start <= start && next.end >= start
	}
	if next.start == next.end {
		return next.start >= start && next.start <= end
	}
	return next.start < end && next.end > start
}

func plainApply(base []string, group []plainEdit, start, end int, local bool) string {
	var out strings.Builder
	position := start
	for _, e := range group {
		if e.local != local {
			continue
		}
		out.WriteString(strings.Join(base[position:e.start], ""))
		out.WriteString(e.value)
		position = e.end
	}
	out.WriteString(strings.Join(base[position:end], ""))
	return out.String()
}
