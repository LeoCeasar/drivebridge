package markdown

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

//go:embed conversion-rules.default.json
var defaultConversionRulesJSON []byte

// ConversionRule converts a complete HTML AST token, never a source substring.
// Target is safe inline Markdown, or a single newline for a break.
type ConversionRule struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	Context string `json:"context"`
	Match   string `json:"match"`
}
type conversionRuleDocument struct {
	Version int              `json:"version"`
	Rules   []ConversionRule `json:"rules"`
}
type compiledConversion struct {
	xml     string
	spacing bool
}

// ConversionRules is an immutable, validated runtime snapshot.
// Its contents are deliberately private, including all lookup maps.
type ConversionRules struct {
	exact       map[string]compiledConversion
	breaks      map[string]compiledConversion
	fingerprint string
}

var defaultConversionRules = mustDefaultConversionRules()
var activeConversionRules atomic.Pointer[ConversionRules]

func currentConversionRules() *ConversionRules {
	if rules := activeConversionRules.Load(); rules != nil {
		return rules
	}
	activeConversionRules.CompareAndSwap(nil, defaultConversionRules)
	return activeConversionRules.Load()
}

// ErrConversionRulesRestart means this process already uses another snapshot.
var ErrConversionRulesRestart = errors.New("conversion_rules_restart_required")

// ConversionRulesPath places the optional user file beside the application config.
func ConversionRulesPath(configFile string) string {
	return filepath.Join(filepath.Dir(configFile), "conversion-rules.json")
}

// DefaultConversionRulesJSON returns a fresh copy suitable for a user-editable file.
func DefaultConversionRulesJSON() []byte { return bytes.Clone(defaultConversionRulesJSON) }

// ConfigureConversionRules installs a startup snapshot once. An identical snapshot
// is idempotent; a different snapshot requires restarting so planned writes and
// their verification cannot silently use different conversion semantics.
func ConfigureConversionRules(rules *ConversionRules) error {
	if rules == nil || rules.fingerprint == "" || rules.exact == nil || rules.breaks == nil {
		return errors.New("conversion rules: validated snapshot required")
	}
	for {
		old := activeConversionRules.Load()
		if old != nil && old.fingerprint == rules.fingerprint {
			return nil
		}
		if old != nil {
			return fmt.Errorf("%w: restart DriveBridge to load changed conversion-rules.json", ErrConversionRulesRestart)
		}
		if activeConversionRules.CompareAndSwap(old, rules) {
			return nil
		}
	}
}

const maxConversionRulesBytes = 8 * 1024 * 1024

// LoadConversionRules appends a user JSON document to the embedded defaults.
// A missing file uses the defaults; malformed or unreadable files return errors.
// Rules with default IDs may update their targets but not their match boundary.
func LoadConversionRules(path string) (*ConversionRules, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConversionRules, nil
	}
	if err != nil {
		return nil, fmt.Errorf("conversion rules %s: %w", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConversionRulesBytes+1))
	if err != nil {
		return nil, fmt.Errorf("conversion rules %s: %w", path, err)
	}
	if len(b) > maxConversionRulesBytes {
		return nil, fmt.Errorf("conversion rules %s: exceeds 8 MiB", path)
	}
	doc, err := readConversionRuleDocument(b)
	if err != nil {
		return nil, fmt.Errorf("conversion rules %s: %w", path, err)
	}
	defaults, _ := readConversionRuleDocument(defaultConversionRulesJSON)
	positions := map[string]int{}
	for i, r := range defaults.Rules {
		positions[r.ID] = i
	}
	seen := map[string]bool{}
	for _, r := range doc.Rules {
		if seen[r.ID] {
			return nil, fmt.Errorf("conversion rules %s: duplicate id %q", path, r.ID)
		}
		seen[r.ID] = true
		if i, ok := positions[r.ID]; ok {
			old := defaults.Rules[i]
			if r.Source != old.Source || r.Context != old.Context || r.Match != old.Match {
				return nil, fmt.Errorf("conversion rules %s: rule %q changes a default match boundary", path, r.ID)
			}
			defaults.Rules[i] = r
		} else {
			defaults.Rules = append(defaults.Rules, r)
		}
	}
	result, err := compileConversionRules(defaults)
	if err != nil {
		return nil, fmt.Errorf("conversion rules %s: %w", path, err)
	}
	return result, nil
}
func readConversionRuleDocument(b []byte) (conversionRuleDocument, error) {
	var d conversionRuleDocument
	if !utf8.Valid(b) || !json.Valid(b) {
		return d, errors.New("invalid JSON document")
	}
	// Pointers distinguish a deliberately empty target from an omitted or null
	// field. A missing default override must never silently erase a line break.
	var input struct {
		Version int `json:"version"`
		Rules   []struct {
			ID      *string `json:"id"`
			Source  *string `json:"source"`
			Target  *string `json:"target"`
			Context *string `json:"context"`
			Match   *string `json:"match"`
		} `json:"rules"`
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return d, err
	}
	if input.Version != 1 || input.Rules == nil {
		return d, errors.New("expected version 1 and a rules array")
	}
	d.Version = input.Version
	d.Rules = make([]ConversionRule, 0, len(input.Rules))
	for i, r := range input.Rules {
		for _, field := range []struct {
			name  string
			value *string
		}{
			{"id", r.ID}, {"source", r.Source}, {"target", r.Target},
			{"context", r.Context}, {"match", r.Match},
		} {
			if field.value == nil {
				return d, fmt.Errorf("rule %d: required field %q must be a non-null string", i+1, field.name)
			}
		}
		d.Rules = append(d.Rules, ConversionRule{ID: *r.ID, Source: *r.Source, Target: *r.Target, Context: *r.Context, Match: *r.Match})
	}
	return d, nil
}

func mustDefaultConversionRules() *ConversionRules {
	d, err := readConversionRuleDocument(defaultConversionRulesJSON)
	if err != nil {
		panic(err)
	}
	rules, err := compileConversionRules(d)
	if err != nil {
		panic(err)
	}
	return rules
}
func compileConversionRules(d conversionRuleDocument) (*ConversionRules, error) {
	out := &ConversionRules{exact: map[string]compiledConversion{}, breaks: map[string]compiledConversion{}}
	ids := map[string]bool{}
	for _, r := range d.Rules {
		fail := func(s string) (*ConversionRules, error) { return nil, fmt.Errorf("rule %q: %s", r.ID, s) }
		if strings.TrimSpace(r.ID) == "" || ids[r.ID] {
			return fail("nonempty unique id required")
		}
		ids[r.ID] = true
		if r.Context != "paragraph" && r.Context != "table" {
			return fail("context must be paragraph or table")
		}
		if len(r.Source) > 65536 || len(r.Target) > 65536 {
			return fail("source/target exceeds 64 KiB")
		}
		if r.Match != "exact" && r.Match != "simple_html_break" {
			return fail("match must be exact or simple_html_break")
		}
		if r.Match == "simple_html_break" && r.Source != "<br>" {
			return fail("simple_html_break source must be <br>")
		}
		if !rawHTMLToken(r.Source) {
			return fail("source must be one complete inline HTML token")
		}
		target, err := compileConversionTarget(r.Target)
		if err != nil {
			return fail("unsafe or unsupported target: " + err.Error())
		}
		key := r.Context + "\x00" + r.Source
		if r.Match == "simple_html_break" {
			if _, ok := out.breaks[r.Context]; ok {
				return fail("duplicate break matcher for context")
			}
			out.breaks[r.Context] = target
		} else {
			if _, ok := out.exact[key]; ok {
				return fail("duplicate source in context")
			}
			out.exact[key] = target
		}
	}
	b, _ := json.Marshal(d)
	out.fingerprint = digest(string(b))
	return out, nil
}
func rawHTMLToken(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || strings.ContainsAny(s, "\r\n") {
		return false
	}
	source := []byte("x" + s + "y")
	root := parser().Parser().Parse(text.NewReader(source))
	p := root.FirstChild()
	if p == nil || p.NextSibling() != nil {
		return false
	}
	count := 0
	for n := p.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.RawHTML); ok {
			if string(h.Segments.Value(source)) != s {
				return false
			}
			count++
		}
	}
	return count == 1
}
func compileConversionTarget(s string) (compiledConversion, error) {
	for _, r := range s {
		if r == 0xFFFE || r == 0xFFFF {
			return compiledConversion{}, errors.New("XML-incompatible character")
		}
	}
	if err := validate([]byte(s)); err != nil {
		return compiledConversion{}, err
	}
	if s == "\n" {
		return compiledConversion{xml: "<br/>", spacing: true}, nil
	}
	if strings.ContainsAny(s, "\r\n") {
		return compiledConversion{}, errors.New("only the standalone newline target may contain line breaks")
	}
	if strings.TrimSpace(s) == "" {
		return compiledConversion{xml: esc(s)}, nil
	}
	// Keep surrounding whitespace literal while parsing the actual target as
	// Markdown. Sentinels would change underscore emphasis and other boundaries.
	core := strings.Trim(s, " \t")
	leading := s[:len(s)-len(strings.TrimLeft(s, " \t"))]
	trailing := s[len(strings.TrimRight(s, " \t")):]
	source := []byte(core)
	root := parser().Parser().Parse(text.NewReader(source))
	p := root.FirstChild()
	if _, ok := p.(*ast.Paragraph); !ok || p.NextSibling() != nil {
		return compiledConversion{}, errors.New("inline Markdown required")
	}
	err := ast.Walk(p, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.(type) {
		case *ast.Paragraph, *ast.Text, *ast.String, *ast.Emphasis, *ast.CodeSpan:
			return ast.WalkContinue, nil
		}
		return ast.WalkStop, errors.New("links, images and HTML are not allowed")
	})
	if err != nil {
		return compiledConversion{}, err
	}
	x, err := nodeXMLWithRules(p, source, nil)
	if err != nil {
		return compiledConversion{}, err
	}
	return compiledConversion{xml: esc(leading) + strings.TrimSuffix(strings.TrimPrefix(x, "<p>"), "</p>") + esc(trailing)}, nil
}

func (r *ConversionRules) lookup(raw []byte, context string) (compiledConversion, bool) {
	if r == nil {
		return compiledConversion{}, false
	}
	if v, ok := r.exact[context+"\x00"+string(raw)]; ok {
		return v, true
	}
	if isSimpleHTMLBreak(raw) {
		v, ok := r.breaks[context]
		return v, ok
	}
	return compiledConversion{}, false
}
