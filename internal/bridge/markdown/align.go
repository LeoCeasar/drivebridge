package markdown

import (
	"fmt"
	"slices"
)

type alignment struct {
	index   []int
	inserts map[int][]int
}

func hashes(a []atom) []string {
	r := make([]string, len(a))
	for i := range a {
		r[i] = a[i].Hash
	}
	return r
}

// Unique content anchors establish order. Only a single unmatched block between
// anchors can be inferred as an edit. Matching is never a positional identity guess
// across repeated or reordered content.
func align(base, other []atom) (alignment, error) {
	a := alignment{index: make([]int, len(base)), inserts: map[int][]int{}}
	for i := range a.index {
		a.index[i] = -1
	}
	bh, oh := hashes(base), hashes(other)
	if slices.Equal(bh, oh) {
		for i := range a.index {
			a.index[i] = i
		}
		return a, nil
	}
	bm, om := map[string]int{}, map[string]int{}
	for i, b := range base {
		if _, ok := bm[b.Hash]; ok {
			return a, fmt.Errorf("repeated base blocks")
		}
		bm[b.Hash] = i
	}
	for i, b := range other {
		if _, ok := om[b.Hash]; ok {
			return a, fmt.Errorf("repeated changed blocks")
		}
		om[b.Hash] = i
	}
	type anchor struct{ b, o int }
	anchors := []anchor{{-1, -1}}
	last := -1
	for i, b := range base {
		if j, ok := om[b.Hash]; ok {
			if j <= last {
				return a, fmt.Errorf("reordered blocks")
			}
			anchors = append(anchors, anchor{i, j})
			last = j
		}
	}
	anchors = append(anchors, anchor{len(base), len(other)})
	for k := 1; k < len(anchors); k++ {
		p, q := anchors[k-1], anchors[k]
		nb, no := q.b-p.b-1, q.o-p.o-1
		switch {
		case nb == 0:
			for j := p.o + 1; j < q.o; j++ {
				a.inserts[q.b] = append(a.inserts[q.b], j)
			}
		case no == 0:
		case nb == 1 && no == 1:
			if base[p.b+1].Kind != other[p.o+1].Kind && !base[p.b+1].Opaque {
				return a, fmt.Errorf("block shape changed")
			}
			a.index[p.b+1] = p.o + 1
		default:
			return a, fmt.Errorf("ambiguous block alignment")
		}
		if q.b < len(base) {
			a.index[q.b] = q.o
		}
	}
	return a, nil
}
