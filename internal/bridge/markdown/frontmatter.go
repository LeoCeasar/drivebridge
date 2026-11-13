package markdown

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"
)

const yamlFrontMatterCaption = "DriveBridge YAML front matter v1"
const yamlFrontMatterPrefix = "# DriveBridge YAML front matter v1\n"

// yamlFrontMatterEnd recognizes only a document-leading, closed YAML mapping.
// YAML is inspected as syntax nodes; tags and aliases are never constructed or executed.
func yamlFrontMatterEnd(source []byte) (int, error) {
	first, rest, ok := bytes.Cut(source, []byte("\n"))
	if !ok || string(bytes.TrimSuffix(first, []byte("\r"))) != "---" {
		return 0, nil
	}
	start := len(source) - len(rest)
	end := start
	closed := false
	for end < len(source) {
		lineEnd := bytes.IndexByte(source[end:], '\n')
		next := len(source)
		if lineEnd >= 0 {
			next = end + lineEnd + 1
		}
		line := bytes.TrimSuffix(bytes.TrimSuffix(source[end:next], []byte("\n")), []byte("\r"))
		if string(line) == "---" {
			closed = true
			break
		}
		end = next
	}
	content := source[start:end]
	// A thematic break followed by ordinary prose remains normal Markdown.
	candidate := false
	for _, line := range bytes.Split(content, []byte("\n")) {
		if i := bytes.IndexByte(line, ':'); i > 0 && (i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t' || line[i+1] == '\r') {
			candidate = true
			break
		}
	}
	var node yaml.Node
	err := yaml.Unmarshal(content, &node)
	mapping := err == nil && len(node.Content) == 1 && node.Content[0].Kind == yaml.MappingNode
	if !mapping && !candidate {
		return 0, nil
	}
	if !closed {
		return 0, unsupported("unclosed YAML front matter; add a closing --- delimiter")
	}
	if err != nil || !mapping {
		return 0, unsupported("malformed YAML front matter; expected a valid YAML mapping")
	}
	next := bytes.IndexByte(source[end:], '\n')
	if next < 0 {
		return len(source), nil
	}
	return end + next + 1, nil
}

func maskYAMLFrontMatter(source []byte, end int) []byte {
	if end == 0 {
		return source
	}
	masked := bytes.Clone(source)
	for i := 0; i < end; i++ {
		if masked[i] != '\n' && masked[i] != '\r' {
			masked[i] = ' '
		}
	}
	return masked
}

func yamlFrontMatterXML(source []byte) string {
	return `<pre lang="yaml" caption="` + yamlFrontMatterCaption + `"><code>` + strings.ReplaceAll(esc(yamlFrontMatterPrefix+string(source)), "&#xA;", "<br/>") + `</code></pre>`
}

func isYAMLFrontMatterNode(n *xnode) bool {
	return n.tag == "pre" && n.attr("lang") == "yaml" && len(elements(n)) == 1 && elements(n)[0].tag == "code" && strings.HasPrefix(plainText(elements(n)[0]), yamlFrontMatterPrefix)
}

// HasYAMLFrontMatterXML permits fresh cloud responses to opt into projection 3.
// Existing checkpoint loads must continue to use their recorded projection version.
func HasYAMLFrontMatterXML(source []byte) bool {
	roots, err := parseXML(source)
	if err != nil {
		return false
	}
	for _, n := range roots {
		if n.tag != "title" {
			return isYAMLFrontMatterNode(n)
		}
	}
	return false
}

func yamlFrontMatterText(n *xnode) (string, error) {
	if !n.allowed("lang", "caption", "wrap") || !whitespaceOnly(n) || len(elements(n)) != 1 || elements(n)[0].tag != "code" {
		return "", unsupported("malformed YAML front matter code block")
	}
	c := elements(n)[0]
	if !c.allowed() {
		return "", unsupported("malformed YAML front matter code attributes")
	}
	for _, child := range elements(c) {
		if child.tag != "br" || !child.allowed() || len(child.kids) > 0 {
			return "", unsupported("malformed YAML front matter code child")
		}
	}
	raw := strings.TrimPrefix(plainText(c), yamlFrontMatterPrefix)
	end, err := yamlFrontMatterEnd([]byte(raw))
	if err != nil {
		return "", err
	}
	if end == 0 || end != len(raw) {
		return "", unsupported("malformed YAML front matter code content")
	}
	return raw, nil
}

// The marker changes projection meaning, so projection 3 hashes it explicitly.
func yamlFrontMatterHash(source []byte) string {
	roots, _ := parseXML([]byte(yamlFrontMatterXML(source)))
	return operationNodeHash("pre", roots[0])
}

func operationNodeHash(kind string, n *xnode) string {
	hash := hashNode(kind, n)
	if isYAMLFrontMatterNode(n) {
		return digest(yamlFrontMatterCaption + "\x00" + hash)
	}
	return hash
}
