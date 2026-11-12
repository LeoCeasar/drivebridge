package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"
)

func contentDifference(a, b []byte) (d Difference) {
	ah, bh := sha256.Sum256(a), sha256.Sum256(b)
	d.Left = Ref{SHA256: hex.EncodeToString(ah[:]), Size: int64(len(a))}
	d.Right = Ref{SHA256: hex.EncodeToString(bh[:]), Size: int64(len(b))}
	d.Binary = !utf8.Valid(a) || !utf8.Valid(b) || bytes.IndexByte(a, 0) >= 0 || bytes.IndexByte(b, 0) >= 0
	return
}

// DiffBytes is a bounded fallback for durable pending content while Git is down.
func DiffBytes(a, b []byte) (Difference, error) {
	d := contentDifference(a, b)
	if d.Binary {
		return d, nil
	}
	if len(a) > 512<<10 || len(b) > 512<<10 || bytes.Count(a, []byte{'\n'}) > 1000 || bytes.Count(b, []byte{'\n'}) > 1000 {
		d.Limited = true
		return d, nil
	}
	patch, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: difflib.SplitLines(string(a)), B: difflib.SplitLines(string(b)), FromFile: "before", ToFile: "after", Context: 3})
	d.Patch = patch
	return d, err
}
func (r *Repository) DiffContent(ctx context.Context, a, b []byte) (d Difference, err error) {
	d = contentDifference(a, b)
	if d.Binary {
		return d, nil
	}
	if len(a) > 10<<20 || len(b) > 10<<20 {
		d.Limited = true
		return d, nil
	}
	limited, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = r.locked(limited, func() error {
		left, e := r.blob(limited, bytes.NewReader(a))
		if e != nil {
			return e
		}
		right, e := r.blob(limited, bytes.NewReader(b))
		if e != nil {
			return e
		}
		raw, e := r.git(limited, nil, "diff", "--no-ext-diff", "--no-textconv", left, right, "--")
		if e != nil {
			return e
		}
		rows := strings.Split(string(raw), "\n")
		if len(rows) > 10000 {
			d.Limited = true
			rows = rows[:10000]
		}
		d.Patch = strings.Join(rows, "\n")
		return nil
	})
	return
}
