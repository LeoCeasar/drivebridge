# Markdown writeback compatibility

User authorized implementation: improve automatic merge and normalize simple HTML breaks for Feishu writeback. Existing evidence: both captured remote bodies equal their conflict base; failure is unsupported conversion, not concurrent edits. Git retains versions; block-aware planning still owns safe online mutations.

## Approach

Retain the existing three-way/block-aware engine and per-operation readback. Text-only Git merging cannot produce safe Feishu block operations. Whole-document recreation would change identity or remove rich resources. Use bounded Markdown compatibility and conservative unilateral structural planning instead.

- Recognize plain `<br>`, `<br/>`, `<br />` (case-insensitive) at Markdown AST boundaries. Paragraphs emit XML breaks; table cells normalize breaks to `; `. Preserve inline/fenced code literals, escaped HTML, resources, arbitrary HTML restrictions and unsafe URL rejection.
- Preserve a relative Markdown link as readable text plus its path when no online target is known; never reinterpret it as an executable URL or discard its destination.
- Permit supported table shape changes only when both saved checkpoints and current remote identity/content agree. Preserve compatible column layout. Keep concurrent edits, opaque resources, merged cells, and unknown semantics protected. Never use the explicit-human authorization flag for automatic changes.
- Distinguish unsupported conversion from actual text conflict; expose a useful reason and retain local/Git copies. Existing conflict drafts must not be silently replaced.
- Append after complete list groups and create new list items with explicit XML wrappers. Use verified shared-prefix insertions before retained sections; support repeated inserted text only with exact-position and unchanged-context readback.
- Verify captured real examples locally without uploading their private data or changing user documents. Any production retry uses normal generation/revision guards and background sync.

## Acceptance

Actual plain text merge of both captures chooses local content. Conversion and full step/readback planning of equivalent sanitized fixtures succeeds for supported cases. Same-block concurrent changes and protected rich content continue to stop. No cloud calls, Git work, or normalization move into foreground save. Cache and five-minute upload cadence remain unchanged.
