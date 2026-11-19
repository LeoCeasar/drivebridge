package markdown

import (
	"errors"
	"fmt"
	"slices"
)

var (
	errAlignmentIdentity = errors.New("content block identity is ambiguous")
	errAlignmentOrder    = errors.New("content blocks moved across context")
	errAlignmentBudget   = errors.New("content alignment budget exceeded")
)

const contextAlignmentBudget = 4_000_000

type alignmentKey struct{ kind, hash string }

func blockAlignmentKey(a atom) alignmentKey { return alignmentKey{a.Kind, a.Hash} }
func hardAlignmentFailure(err error) bool {
	return errors.Is(err, errAlignmentIdentity) || errors.Is(err, errAlignmentOrder) || errors.Is(err, errAlignmentBudget)
}

// alignContext partitions by ordered, unchanged blocks that are unique within
// their current region. Equal content in other regions never aliases an ID.
// Work is iterative and charged to one shared budget, including DP leaf grids.
func alignContext(base, other []atom, projected bool) (alignment, error) {
	budget := contextAlignmentBudget
	return alignContextBudgeted(base, other, projected, &budget)
}
func alignContextBudgeted(base, other []atom, projected bool, budget *int) (alignment, error) {
	result := alignment{index: make([]int, len(base)), inserts: map[int][]int{}}
	for i := range result.index {
		result.index[i] = -1
	}
	type region struct{ a, b, n, m int }
	pending := []region{{n: len(base), m: len(other)}}
	var softFailure error
	changedOld, changedNew := map[alignmentKey]bool{}, map[alignmentKey]bool{}
	for len(pending) > 0 {
		part := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		*budget -= part.n + part.m + 1
		if *budget < 0 {
			return alignment{}, errAlignmentBudget
		}
		a, b := base[part.a:part.a+part.n], other[part.b:part.b+part.m]
		if slices.Equal(hashes(a), hashes(b)) {
			for i := range a {
				if a[i].Kind != b[i].Kind {
					return alignment{}, errAlignmentIdentity
				}
				result.index[part.a+i] = part.b + i
			}
			continue
		}
		ac, bc := map[alignmentKey]int{}, map[alignmentKey]int{}
		positions := map[alignmentKey]int{}
		for _, v := range a {
			ac[blockAlignmentKey(v)]++
		}
		for j, v := range b {
			k := blockAlignmentKey(v)
			bc[k]++
			positions[k] = j
		}
		type anchor struct{ i, j int }
		anchors := []anchor{}
		last := -1
		for i, v := range a {
			k := blockAlignmentKey(v)
			if ac[k] == 1 && bc[k] == 1 {
				j := positions[k]
				if j <= last {
					return alignment{}, errAlignmentOrder
				}
				anchors = append(anchors, anchor{i, j})
				last = j
			}
		}
		if len(anchors) > 0 {
			previous := anchor{-1, -1}
			for _, next := range append(anchors, anchor{len(a), len(b)}) {
				n, m := next.i-previous.i-1, next.j-previous.j-1
				if n > 0 || m > 0 {
					pending = append(pending, region{part.a + previous.i + 1, part.b + previous.j + 1, n, m})
				}
				if next.i < len(a) {
					result.index[part.a+next.i] = part.b + next.j
				}
				previous = next
			}
			continue
		}
		// Removing an entire bounded region does not select between survivors.
		if len(b) == 0 {
			continue
		}
		var local alignment
		var err error
		if projected {
			cells := (int64(len(a)) + 1) * (int64(len(b)) + 1)
			if cells > int64(*budget) {
				return alignment{}, errAlignmentBudget
			}
			*budget -= int(cells)
			local, err = alignProjectedGap(a, b)
		} else {
			local, err = alignPlainGap(a, b)
		}
		if err != nil {
			for k, n := range ac {
				if n > 1 || bc[k] > 1 {
					return alignment{}, fmt.Errorf("%w: %v", errAlignmentIdentity, err)
				}
			}
			for _, n := range bc {
				if n > 1 {
					return alignment{}, fmt.Errorf("%w: %v", errAlignmentIdentity, err)
				}
			}
			if softFailure == nil {
				softFailure = err
			}
			for _, v := range a {
				changedOld[blockAlignmentKey(v)] = true
			}
			for _, v := range b {
				changedNew[blockAlignmentKey(v)] = true
			}
			continue
		}
		for i, j := range local.index {
			if j >= 0 {
				result.index[part.a+i] = part.b + j
			}
		}
		for gap, indices := range local.inserts {
			for _, j := range indices {
				result.inserts[part.a+gap] = append(result.inserts[part.a+gap], part.b+j)
			}
		}
	}
	// A deletion and equal-content insertion elsewhere may be a move. Context
	// proves the regions but cannot authorize creating a replacement identity.
	for i, j := range result.index {
		old := blockAlignmentKey(base[i])
		if j < 0 {
			changedOld[old] = true
		} else if next := blockAlignmentKey(other[j]); old != next {
			changedOld[old] = true
			changedNew[next] = true
		}
	}
	for _, indices := range result.inserts {
		for _, j := range indices {
			changedNew[blockAlignmentKey(other[j])] = true
		}
	}
	for key := range changedOld {
		if changedNew[key] {
			return alignment{}, errAlignmentOrder
		}
	}
	if softFailure != nil {
		return alignment{}, softFailure
	}
	return result, nil
}

func alignPlainGap(base, other []atom) (alignment, error) {
	result := alignment{index: make([]int, len(base)), inserts: map[int][]int{}}
	for i := range result.index {
		result.index[i] = -1
	}
	for _, atoms := range [][]atom{base, other} {
		seen := map[alignmentKey]bool{}
		for _, a := range atoms {
			k := blockAlignmentKey(a)
			if seen[k] {
				return alignment{}, errAlignmentIdentity
			}
			seen[k] = true
		}
	}
	switch {
	case len(base) == 0:
		for j := range other {
			result.inserts[0] = append(result.inserts[0], j)
		}
	case len(other) == 0:
	case len(base) == 1 && len(other) == 1:
		if base[0].Kind != other[0].Kind && !base[0].Opaque {
			return alignment{}, fmt.Errorf("block shape changed")
		}
		result.index[0] = 0
	default:
		return alignment{}, fmt.Errorf("ambiguous block alignment")
	}
	return result, nil
}
