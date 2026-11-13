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
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/rclone/rclone/lib/file"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

//go:embed conversion-rules.default.json
var defaultConversionRulesJSON []byte

// ConversionRule describes an HTML AST token, literal text, or fixed structural
// policy. HTML targets are safe inline Markdown, or a single newline for a break.
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
	xml      string
	markdown string
	spacing  bool
}

// ConversionRules is an immutable, validated runtime snapshot.
// Its contents are deliberately private, including all lookup maps.
type ConversionRules struct {
	exact        map[string]compiledConversion
	breaks       map[string]compiledConversion
	fingerprint  string
	document     []byte
	tableResize  bool
	literals     map[string]*strings.Replacer
	safeDefaults map[string]bool
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
	f, err := file.Open(path)
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
	rules, err := PureParseConversionRules(b)
	if err != nil {
		return nil, fmt.Errorf("conversion rules %s: %w", path, err)
	}
	return rules, nil
}

// PureParseConversionRules validates and compiles defaults plus user overrides
// without installing or changing the process's active conversion snapshot.
func PureParseConversionRules(b []byte) (*ConversionRules, error) {
	if len(b) > maxConversionRulesBytes {
		return nil, errors.New("conversion rules exceeds 8 MiB")
	}
	doc, err := readConversionRuleDocument(b)
	if err != nil {
		return nil, err
	}
	defaults, _ := readConversionRuleDocument(defaultConversionRulesJSON)
	positions := map[string]int{}
	for i, r := range defaults.Rules {
		positions[r.ID] = i
	}
	seen := map[string]bool{}
	for _, r := range doc.Rules {
		if seen[r.ID] {
			return nil, fmt.Errorf("duplicate id %q", r.ID)
		}
		seen[r.ID] = true
		if i, ok := positions[r.ID]; ok {
			old := defaults.Rules[i]
			if r.Source != old.Source || r.Context != old.Context || r.Match != old.Match {
				return nil, fmt.Errorf("rule %q changes a default match boundary", r.ID)
			}
			defaults.Rules[i] = r
		} else {
			defaults.Rules = append(defaults.Rules, r)
		}
	}
	result, err := compileConversionRules(defaults)
	if err != nil {
		return nil, err
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
	out := &ConversionRules{exact: map[string]compiledConversion{}, breaks: map[string]compiledConversion{}, literals: map[string]*strings.Replacer{}, safeDefaults: map[string]bool{}}
	literalRules := map[string][]ConversionRule{}
	ids := map[string]bool{}
	for _, r := range d.Rules {
		fail := func(s string) (*ConversionRules, error) { return nil, fmt.Errorf("rule %q: %s", r.ID, s) }
		if strings.TrimSpace(r.ID) == "" || ids[r.ID] {
			return fail("nonempty unique id required")
		}
		ids[r.ID] = true
		if expected, ok := safeDefaultRule(r.ID); ok {
			if r != expected {
				return fail("built-in structural rules and their lossless targets are fixed")
			}
			out.safeDefaults[r.ID] = true
			continue
		}
		if r.Match == "front_matter" || r.Match == "table_resize" || r.Context == "document" || r.ID == "yaml-front-matter" || r.ID == "plain-table-resize" {
			if r == (ConversionRule{ID: "plain-table-resize", Source: "table", Target: "preserve_cells", Context: "document", Match: "table_resize"}) {
				out.tableResize = true
				continue
			}
			if r != (ConversionRule{ID: "yaml-front-matter", Source: "---", Target: "yaml_code_block", Context: "document", Match: "front_matter"}) {
				return fail("built-in structural rules and their lossless targets are fixed")
			}
			continue
		}
		if r.Context != "paragraph" && r.Context != "table" {
			return fail("context must be paragraph or table")
		}
		if len(r.Source) > 65536 || len(r.Target) > 65536 {
			return fail("source/target exceeds 64 KiB")
		}
		if r.Match != "exact" && r.Match != "simple_html_break" && r.Match != "literal" {
			return fail("match must be exact, literal or simple_html_break")
		}
		if r.Match == "simple_html_break" && r.Source != "<br>" {
			return fail("simple_html_break source must be <br>")
		}
		if r.Match == "literal" {
			if r.Source == "" || strings.ContainsAny(r.Source, "\r\n") || validate([]byte(r.Source)) != nil {
				return fail("literal source must be nonempty single-line UTF-8 text")
			}
		} else if !rawHTMLToken(r.Source) {
			return fail("source must be one complete inline HTML token")
		}
		var target compiledConversion
		var err error
		if r.Match == "literal" {
			if strings.ContainsAny(r.Target, "\r\n") {
				return fail("literal target must be single-line plain text")
			}
			if err = validate([]byte(r.Target)); err == nil {
				target.xml = esc(r.Target)
			}
		} else {
			target, err = compileConversionTarget(r.Target)
		}
		if err != nil {
			return fail("unsafe or unsupported target: " + err.Error())
		}
		nodes, err := parseXML([]byte("<p>" + target.xml + "</p>"))
		if err != nil {
			return fail("invalid compiled target")
		}
		target.markdown, err = inlineMD(nodes[0])
		if err != nil {
			return fail("unrepresentable target")
		}
		if r.Match == "literal" {
			literalRules[r.Context] = append(literalRules[r.Context], ConversionRule{ID: r.ID, Source: r.Source, Target: target.markdown})
			continue
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
	for context, rules := range literalRules {
		sort.Slice(rules, func(i, j int) bool {
			if len(rules[i].Source) != len(rules[j].Source) {
				return len(rules[i].Source) > len(rules[j].Source)
			}
			return rules[i].Source < rules[j].Source
		})
		pairs := []string{}
		seen := map[string]bool{}
		for _, r := range rules {
			if seen[r.Source] {
				return nil, fmt.Errorf("rule %q: duplicate literal source", r.ID)
			}
			seen[r.Source] = true
			pairs = append(pairs, r.Source, r.Target)
		}
		probePairs := []string{}
		for _, r := range rules {
			probePairs = append(probePairs, r.Source, "\x00")
		}
		probe := strings.NewReplacer(probePairs...)
		for _, r := range rules {
			if probe.Replace(r.Target) != r.Target {
				return nil, fmt.Errorf("rule %q: target matches a literal source; chained replacements are not stable", r.ID)
			}
		}
		for key, v := range out.exact {
			if strings.HasPrefix(key, context+"\x00") && probe.Replace(v.markdown) != v.markdown {
				return nil, errors.New("HTML target matches a literal source")
			}
		}
		if v, ok := out.breaks[context]; ok && probe.Replace(v.markdown) != v.markdown {
			return nil, errors.New("break target matches a literal source")
		}
		out.literals[context] = strings.NewReplacer(pairs...)
	}
	b, _ := json.Marshal(d)
	out.fingerprint = digest(string(b))
	out.document = b
	return out, nil
}

// EffectiveDocumentJSON returns a fresh JSON copy of all defaults and overrides.
func (r *ConversionRules) EffectiveDocumentJSON() json.RawMessage {
	if r == nil {
		return nil
	}
	return bytes.Clone(r.document)
}

// Fingerprint identifies the complete validated effective rule document.
func (r *ConversionRules) Fingerprint() string {
	if r == nil {
		return ""
	}
	return r.fingerprint
}

// ActiveConversionRulesFingerprint identifies the process's immutable snapshot.
func ActiveConversionRulesFingerprint() string { return currentConversionRules().Fingerprint() }

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
