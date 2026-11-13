package markdown

import (
	"bytes"
	"html"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
)

// These are fixed policies, recorded in the same effective document as editable
// rules. They never act as arbitrary source or target substitutions.
func safeDefaultRule(id string) (ConversionRule, bool) {
	var source, target, match string
	switch id {
	case "basic-inline-html":
		source, target, match = "strong|b|em|i", "native_emphasis", "basic_inline_html"
	case "simple-html-paragraph":
		source, target, match = "p|div", "native_paragraph", "simple_html_paragraph"
	case "indented-code":
		source, target, match = "indented_code", "native_code_block", "indented_code"
	case "ordinary-dollar-text":
		source, target, match = "$", "preserve_text", "ordinary_dollar_text"
	default:
		return ConversionRule{}, false
	}
	return ConversionRule{ID: id, Source: source, Target: target, Context: "document", Match: match}, true
}

var basicHTMLPattern = regexp.MustCompile(`(?i)^<(/?)(strong|b|em|i)[ \t]*>$`)
var paragraphHTMLPattern = regexp.MustCompile(`(?is)^<(p|div)[ \t]*>(.*)</(p|div)[ \t]*>$`)

func basicHTMLTag(raw []byte) (name string, close bool, native string, ok bool) {
	m := basicHTMLPattern.FindSubmatch(raw)
	if m == nil {
		return
	}
	name = strings.ToLower(string(m[2]))
	close = len(m[1]) > 0
	tag := "em"
	if name == "strong" || name == "b" {
		tag = "b"
	}
	prefix := ""
	if close {
		prefix = "/"
	}
	return name, close, "<" + prefix + tag + ">", true
}

// Each inline AST container owns its pairs. No tag may leak across a paragraph,
// table cell, Markdown emphasis or link boundary, even if its XML would balance.
func validateBasicHTMLChildren(n ast.Node, src []byte, rules *ConversionRules) error {
	if rules == nil || !rules.safeDefaults["basic-inline-html"] {
		return nil
	}
	var stack []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		h, ok := c.(*ast.RawHTML)
		if !ok {
			continue
		}
		name, closing, _, ok := basicHTMLTag(h.Segments.Value(src))
		if !ok {
			continue
		}
		if !closing {
			stack = append(stack, name)
			continue
		}
		if len(stack) == 0 || stack[len(stack)-1] != name {
			return locateContent(unsupported("RawHTML"), c, src)
		}
		stack = stack[:len(stack)-1]
	}
	if len(stack) > 0 {
		return unsupported("RawHTML")
	}
	return nil
}

// HTML blocks accept one attribute-free p/div root or a complete basic inline
// formatting block. Markdown syntax inside HTML remains literal. All HTML tokens
// must be known, balanced and attribute-free; resources never become plain text.
func safeHTMLBlock(n *ast.HTMLBlock, src []byte, rules *ConversionRules) (string, bool) {
	raw := bytes.TrimSpace(n.Lines().Value(src))
	if n.HasClosure() {
		raw = bytes.TrimSpace(append(bytes.Clone(raw), n.ClosureLine.Value(src)...))
	}
	m := paragraphHTMLPattern.FindSubmatch(raw)
	body := raw
	if m != nil && bytes.EqualFold(m[1], m[3]) && rules.safeDefaults["simple-html-paragraph"] {
		body = m[2]
	} else {
		// An attribute-free basic format tag on its own line becomes a
		// CommonMark HTML block too. Its complete balanced inline content is
		// still one paragraph; blank lines split the AST and fail pairing.
		end := bytes.IndexByte(raw, '>')
		if end < 0 || !rules.safeDefaults["basic-inline-html"] {
			return "", false
		}
		_, closing, _, ok := basicHTMLTag(raw[:end+1])
		if !ok || closing {
			return "", false
		}
	}
	var out strings.Builder
	var stack []string
	for len(body) > 0 {
		i := bytes.IndexByte(body, '<')
		if i < 0 {
			i = len(body)
		}
		// HTML line breaks separate words; code literals never enter this function.
		text := strings.ReplaceAll(strings.ReplaceAll(string(body[:i]), "\r\n", "\n"), "\n", " ")
		if strings.Contains(text, "\r") {
			return "", false
		}
		decoded := html.UnescapeString(text)
		decodedBytes := []byte(decoded)
		for i, c := range decodedBytes {
			if c == '$' && (!rules.safeDefaults["ordinary-dollar-text"] || !ordinaryDollar(decodedBytes, i)) {
				return "", false
			}
		}
		out.WriteString(esc(decoded))
		body = body[i:]
		if len(body) == 0 {
			break
		}
		j := bytes.IndexByte(body, '>')
		if j < 0 {
			return "", false
		}
		token := body[:j+1]
		body = body[j+1:]
		name, closing, native, ok := basicHTMLTag(token)
		if ok && rules.safeDefaults["basic-inline-html"] {
			if !closing {
				stack = append(stack, name)
			} else {
				if len(stack) == 0 || stack[len(stack)-1] != name {
					return "", false
				}
				stack = stack[:len(stack)-1]
			}
			out.WriteString(native)
			continue
		}
		if converted, ok := rules.lookup(token, "paragraph"); ok && isSimpleHTMLBreak(token) {
			out.WriteString(converted.xml)
			continue
		}
		return "", false
	}
	if len(stack) != 0 {
		return "", false
	}
	return "<p>" + out.String() + "</p>", true
}

// A missing closing delimiter is ambiguous, so only visibly monetary amounts and
// conventional uppercase shell variables are accepted. Lowercase variables and
// all paired/display delimiters remain protected. Operators or another dollar
// later in the same non-whitespace token also keep the token protected.
func ordinaryDollar(raw []byte, start int) bool {
	end := start + 1
	for end < len(raw) && raw[end] != ' ' && raw[end] != '\t' && raw[end] != '\n' && raw[end] != '\r' {
		end++
	}
	token := string(raw[start+1 : end])
	if strings.ContainsAny(token, "$+*=\\^{}") {
		return false
	}
	if len(token) == 0 {
		return false
	}
	// Punctuation is permitted only at the end, preserving prose boundaries.
	token = strings.TrimRight(token, ",.;:!?)]")
	if token == "" {
		return false
	}
	if token[0] >= '0' && token[0] <= '9' {
		digits := true
		dot := false
		for _, c := range token {
			if c == '.' && !dot {
				dot = true
				continue
			}
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		return digits
	}
	for i, c := range token {
		if (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}
