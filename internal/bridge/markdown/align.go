package markdown

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

// Equal-content objects are matched only inside uniquely anchored context.
func align(base, other []atom) (alignment, error) { return alignContext(base, other, false) }
