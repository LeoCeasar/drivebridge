package markdown

import (
	"bytes"
	"sort"
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type normalizedPart struct {
	start, end int
	value      string
}

// NormalizeForMerge edits background copies only. Rules cannot reach code,
// links, images, escaped syntax or the contents of protected raw HTML blocks.
// All matching uses original source positions; inserted targets are not replayed.
func NormalizeForMerge(source []byte) ([]byte, error) {
	first, err := normalizeForMergePass(source)
	if err != nil || bytes.Equal(first, source) {
		return first, err
	}
	second, err := normalizeForMergePass(first)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(first, second) {
		return nil, unsupported("conversion rules are not stable for this input; targets must not create further matches")
	}
	return first, nil
}
func normalizeForMergePass(source []byte) ([]byte, error) {
	html, err := normalizeForMergeOnce(source, true, false)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(html, source) && !EquivalentForSync(source, html) {
		html = source
	}
	return normalizeForMergeOnce(html, false, true)
}

// EquivalentForSync ignores only codec-equivalent spelling, never unsupported
// structures or protected rich resources. It does not rewrite either input.
func EquivalentForSync(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	left, err := Encode(a)
	if err != nil {
		return false
	}
	right, err := Encode(b)
	return err == nil && bytes.Equal(left, right)
}
func normalizeForMergeOnce(source []byte, htmlRules, literalRules bool) ([]byte, error) {
	if err := validate(source); err != nil {
		return nil, err
	}
	if bytes.Count(source, []byte{'\n'}) > plainMaxLines {
		return nil, unsupported("normalization line limit")
	}
	rules := currentConversionRules()
	root := parser().Parser().Parse(text.NewReader(source))
	var parts []normalizedPart
	projectedSize := len(source)
	addPart := func(p normalizedPart) error {
		if p.start < 0 || p.end < p.start || p.end > len(source) {
			return unsupported("invalid normalization span")
		}
		projectedSize += len(p.value) - (p.end - p.start)
		if projectedSize > MaxMarkdownBytes {
			return &ContentError{"body_too_large", "converted Markdown exceeds 10 MiB"}
		}
		parts = append(parts, p)
		return nil
	}
	contextOf := func(n ast.Node) string {
		for p := n.Parent(); p != nil; p = p.Parent() {
			if _, ok := p.(*east.TableCell); ok {
				return "table"
			}
		}
		return "paragraph"
	}
	err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.CodeSpan, *ast.FencedCodeBlock, *ast.CodeBlock, *ast.Link, *ast.AutoLink, *ast.Image:
			return ast.WalkSkipChildren, nil
		case *ast.HTMLBlock:
			if !htmlRules {
				return ast.WalkSkipChildren, nil
			}
			raw := v.Lines().Value(source)
			c, ok := rules.lookup(bytes.TrimSpace(raw), "paragraph")
			if ok && !v.HasClosure() && v.Lines().Len() > 0 {
				replacement := c.markdown
				if c.spacing {
					replacement = "\n"
				}
				if err := addPart(normalizedPart{v.Lines().At(0).Start, v.Lines().At(v.Lines().Len() - 1).Stop, replacement}); err != nil {
					return ast.WalkStop, err
				}
			}
			return ast.WalkSkipChildren, nil
		case *ast.RawHTML:
			if !htmlRules {
				return ast.WalkContinue, nil
			}
			if v.Segments.Len() != 1 {
				return ast.WalkContinue, nil
			}
			c, ok := rules.lookup(v.Segments.Value(source), contextOf(n))
			if !ok {
				return ast.WalkContinue, nil
			}
			span := v.Segments.At(0)
			end := span.Stop
			if strings.HasSuffix(c.markdown, "\n") {
				if bytes.HasPrefix(source[end:], []byte("\r\n")) {
					end += 2
				} else if end < len(source) && source[end] == '\n' {
					end++
				}
			}
			if err := addPart(normalizedPart{span.Start, end, c.markdown}); err != nil {
				return ast.WalkStop, err
			}
		case *ast.Text:
			if !literalRules {
				return ast.WalkContinue, nil
			}
			replacer := rules.literals[contextOf(n)]
			if replacer == nil {
				return ast.WalkContinue, nil
			}
			raw := v.Segment.Value(source)
			start := 0
			apply := func(end int) error {
				if end <= start {
					return nil
				}
				chunk := string(raw[start:end])
				w := &normalizationWriter{}
				if _, err := replacer.WriteString(w, chunk); err != nil {
					return err
				}
				if w.String() != chunk {
					if err := addPart(normalizedPart{v.Segment.Start + start, v.Segment.Start + end, w.String()}); err != nil {
						return err
					}
				}
				return nil
			}
			for i := 0; i+1 < len(raw); i++ {
				if raw[i] == '\\' && util.IsPunct(raw[i+1]) {
					if err := apply(i); err != nil {
						return ast.WalkStop, err
					}
					i++
					start = i + 1
				}
			}
			if err := apply(len(raw)); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].start < parts[j].start })
	out := &normalizationWriter{}
	position := 0
	for _, p := range parts {
		if p.start < position || p.end > len(source) {
			return nil, unsupported("overlapping normalization")
		}
		if _, err = out.Write(source[position:p.start]); err != nil {
			return nil, err
		}
		if _, err = out.Write([]byte(p.value)); err != nil {
			return nil, err
		}
		position = p.end
	}
	if _, err = out.Write(source[position:]); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type normalizationWriter struct{ bytes.Buffer }

func (w *normalizationWriter) Write(b []byte) (int, error) {
	if len(b) > MaxMarkdownBytes-w.Len() {
		return 0, &ContentError{"body_too_large", "converted Markdown exceeds 10 MiB"}
	}
	return w.Buffer.Write(b)
}
func (w *normalizationWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
