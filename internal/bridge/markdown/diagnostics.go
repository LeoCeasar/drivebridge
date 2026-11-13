package markdown

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/yuin/goldmark/ast"
)

type locatedContentError struct {
	cause      error
	start, end int
	sourceHash string
}

func (e *locatedContentError) Error() string { return e.cause.Error() }
func (e *locatedContentError) Unwrap() error { return e.cause }
func locateContent(err error, n ast.Node, src []byte) error {
	if err == nil {
		return nil
	}
	var prior *locatedContentError
	if errors.As(err, &prior) {
		return err
	}
	var content *ContentError
	if !errors.As(err, &content) {
		return err
	}
	start := n.Pos()
	if start < 0 || start > len(src) {
		return err
	}
	end := start
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if p := c.Pos(); p >= 0 && p <= len(src) {
				end = max(end, p)
			}
		}
		return ast.WalkContinue, nil
	})
	return &locatedContentError{err, start, end, digest(string(src))}
}

// DiagnoseContent describes an immutable failed input without re-reading cloud
// data. Locations are reported only when the parser identifies a source span.
func DiagnoseContent(body []byte, err error) *model.SyncDiagnostic {
	var planned *planContentError
	if errors.As(err, &planned) {
		d := planned.diagnostic
		return &d
	}
	var c *ContentError
	if !errors.As(err, &c) {
		return nil
	}
	kind, message := c.Detail, "该内容暂时不能安全转换为飞书在线文档。"
	switch c.Detail {
	case "RawHTML", "HTMLBlock", "readable HTML shape":
		kind, message = "unsupported_html", "包含尚未配置安全转换的 HTML 标签。"
	case "front matter":
		kind, message = "front_matter", "文档开头包含 YAML/TOML 元信息，需要可还原的转换规则。"
	case "formula delimiter":
		kind, message = "formula", "包含当前在线文档写回不支持的公式分隔符。"
	case "Mermaid":
		kind, message = "mermaid", "Mermaid 图暂不支持直接写回。"
	case "code info attributes":
		kind, message = "code_attributes", "代码块含有语言以外的附加属性。"
	case "link title":
		kind, message = "link_title", "链接标题属性尚不支持安全写回。"
	}
	if c.Detail == "TOML front matter" {
		kind = "front_matter"
		message = "TOML 元信息暂不支持无损转换，已保留本地原文并停止此文件写回。"
	}
	if strings.Contains(c.Detail, "YAML front matter") {
		kind = "front_matter"
		message = "YAML 元信息不完整或格式无效；请检查开头及结束的 --- 和键值缩进。"
	}
	d := &model.SyncDiagnostic{Kind: kind, Message: message, Side: "local"}
	var at *locatedContentError
	verifiedFrontMatter := false
	if !errors.As(err, &at) || at.sourceHash != digest(string(body)) {
		at = nil
		// A planner may have reduced a parse error to a content code. A local-only
		// parse recovers its exact range; never assign a line to an unrelated error.
		_, parseErr := parseMarkdown(body, nil)
		var pc *ContentError
		verifiedFrontMatter = errors.As(parseErr, &pc) && pc.Detail == c.Detail && strings.Contains(pc.Detail, "front matter")
		if errors.As(parseErr, &at) {
			var pc *ContentError
			if errors.As(parseErr, &pc) && pc.Detail != c.Detail {
				at = nil
			}
		}
	}
	start, end := -1, -1
	if at != nil {
		start, end = at.start, at.end
	} else if verifiedFrontMatter {
		start = 0
		end = 0
	}
	if start < 0 || start > len(body) {
		d.Side = ""
		return d
	}
	end = min(max(end, start), len(body))
	d.Line = 1 + bytes.Count(body[:start], []byte{'\n'})
	d.EndLine = 1 + bytes.Count(body[:end], []byte{'\n'})
	first := bytes.LastIndexByte(body[:start], '\n') + 1
	last := bytes.IndexByte(body[start:], '\n')
	if last < 0 {
		last = len(body)
	} else {
		last += start
	}
	snippet := strings.TrimSuffix(string(body[first:last]), "\r")
	const maxSnippet = 240
	if utf8.RuneCountInString(snippet) > maxSnippet {
		snippet = string([]rune(snippet)[:maxSnippet]) + "…"
	}
	d.Text = snippet
	return d
}

type planContentError struct{ diagnostic model.SyncDiagnostic }

func (e *planContentError) Error() string { return "unsupported_content: " + e.diagnostic.Kind }
func (e *planContentError) Unwrap() error { return unsupported(e.diagnostic.Kind) }

// ExplainPlanFailure keeps structural mapping failures distinct from text
// conflicts. Unknown mappings have no invented line number. Protected blocks
// use the captured remote projection's range, never the current local file.
func ExplainPlanFailure(desired []byte, current model.DocView, reason string) error {
	if _, err := Encode(desired); err != nil {
		return err
	}
	d := model.SyncDiagnostic{Kind: "block_mapping", Message: "文字已合并，但无法将变更唯一映射到飞书内容块；云端尚未覆盖。"}
	if reason == "ambiguous_mapping" {
		d.Kind = "ambiguous_mapping"
		d.Message = "文字已合并，但重复段落或列表使云端内容块的对应关系不唯一。"
	}
	for _, b := range current.Blocks {
		if !b.Opaque && !b.Readable {
			continue
		}
		if b.Start < 0 || b.End < b.Start || b.End > len(current.Markdown) {
			continue
		}
		raw := current.Markdown[b.Start:b.End]
		if bytes.Contains(desired, bytes.TrimSpace(raw)) {
			continue
		}
		at := &locatedContentError{unsupported("protected_cloud_block"), b.Start, max(b.Start, b.End-1), digest(string(current.Markdown))}
		located := DiagnoseContent(current.Markdown, at)
		if located != nil {
			d = *located
			d.Side = "remote"
			d.Kind = "protected_cloud_block"
			d.Message = "修改涉及尚不能无损写回的云端内容块，需保留其样式或资源。"
		}
		break
	}
	return &planContentError{d}
}
