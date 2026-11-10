# T04 implementation report

Implemented on `feature/feishu-sync-markdown`, isolated worktree `/Users/rrong/.codex/worktrees/4351/drivebridge`, base `a83a786edd22ceeb0694ba6bcd43a1d8365db714`.

Implementation commits: `a5c604076553aa088f6e762507a149ba87aff339` (codec/merge/tests/dependency), `bf79ac057` (opaque ambiguity classification regression). Only `internal/bridge/markdown`, the planned goldmark v1.8.7 requirement/checksums, and this report changed. No store/model/cloud/product changes. No remote/account/credential/system/driver action, subagent/reviewer, push or Goal was used. The only network action was the authorized public goldmark dependency download. Parent owns combined Block B/full-suite verification.

## Exact API and helper mapping

- `Decode(xml []byte, revision string) (model.DocView,error)` delegates to `DecodeWithOptions(xml,revision,Options{})`.
- `DecodeWithOptions(xml []byte, revision string, Options{SourceURL string; ReferenceMap json.RawMessage})` retains transport context and original XML, omits title metadata from body Markdown and block ranges. It accepts sibling XML fragments, rejects partial fragment/excerpt envelopes, malformed XML, directives, duplicate attributes/IDs and excessive XML complexity.
- `Encode(markdown []byte) ([]byte,error)` generates XML **body only**, including valid empty body; no title, source IDs, resource replay or placeholder reconstruction.
- `Merge(baseLocal,local []byte,remote model.DocView) (model.MergeResult,error)` returns original local bytes plus structured reason and **zero operations** on conflicts. Content-validation errors become `unsupported_content`/`body_too_large`/`merge_complexity` conflicts. Alignment/overlap/delete-vs-edit/table conflicts are explicit; opaque ambiguity is `unsupported_content`.
- `ContentError{Reason,Detail}` exposes Encode/Decode input-validation failures. `MaxMarkdownBytes` is 10 MiB; invalid UTF-8 and XML-incompatible controls are rejected.
- `parseMarkdown` uses goldmark AST positions and original source slices. Spans are UTF-8 byte offsets, include syntax/fences and the final content-line terminator, exclude following blank lines. Local spelling, CRLF, fences and missing final newline survive unmodified regions.
- `nodeXML` is a whitelist AST renderer, with XML escaping and CommonMark text/entity handling. Supported mappings: p, h1–h6, b/em, code, pre/code/br, blockquote, ul/ol/li, a[href], hr, rectangular GFM tables. Ordered lists emit explicit sequences. Task lists, raw HTML/images, Mermaid, link titles, reference definitions, indented code and other unsupported AST forms are rejected rather than dropped. Formula delimiters outside code are deliberately conservative (unescaped dollar text is blocked; escape a literal dollar).
- `parseXML` captures every subtree's **original byte span**, never reconstructed RawXML. `xmlMarkdown`, `tableMD`, `inlineMD`, `projectableList` project only approved shapes. Semantic round-trip checks catch delimiter whitespace, unrepresentable inline styling and table pipe loss; failed projection makes the whole affected subtree opaque. Lists with unsupported children remain opaque at the wrapper, preventing unsafe edits beneath an unmapped resource parent.
- Unknown attributes/inline/block shapes remain opaque. Opaque placeholders are readable and linked using SourceURL when supplied; no tenant URL or ID is invented. Without SourceURL they explicitly state that online source is unavailable. ReferenceMap is copied into the sidecar and never replayed.
- `semanticHash`/`hashNode` exclude remote IDs and retained display metadata; `opaqueSemantic` hashes actual opaque content/attributes with IDs removed. Public Block.Hash and operation payload hashes use actual code content, not artificial fence separator syntax.
- `align` uses unique content hashes and surrounding anchors, rejects duplicates/reorders rather than assigning identities by position, and infers at most one unmatched edit between anchors. `mergeText` uses existing go-difflib `NewMatcherWithJunk(...,false,nil)` on exact rune sequences, with a 4,000,000 comparison-product bound. It merges disjoint same-paragraph/code edits, deduplicates equal edits and conflicts on overlaps/same-position different insertions. Tables are whole merge atoms. `exactLines` preserves conflict context without manufacturing terminal newlines; conflict hunks currently carry a whole-candidate context hunk with line indexes.
- `replacementXML`/`graft` preserve approved pre caption/wrap, table colgroup widths/cell layout, and applicable list metadata. Unsupported structural changes fail. Non-target code/table/callout RawXML remains exact and is never uploaded.
- `operation` serializes canonical model.OperationPayload keys `xml`, `before_hash`, `after_hash`. Kinds are only block_replace, block_insert_after, block_delete. Actual current BlockID/AfterBlockID is used, except documented document-start sentinel `0`. Route/capability/journal ID/client token remain empty for engine binding. No whole-overwrite, string-replace, move or guessed-ID fallback exists. ParentID/LocalID are left empty: no idless wrapper ID is invented. Idless ul/ol items target their actual li IDs; tables/quotes are outer atoms and do not expose overlapping child mutations.

## Evidence used and preservation limits

The committed sanitized `docx-subset-probe-fixtures.json` was copied into package testdata. Tests assert actual h6, inline tags, idless-list item targeting, code, quote and table forms, and exact original raw slices. The advisory preflight report was read. Parent-provided `/Users/rrong/.codex/worktrees/5b46/drivebridge/.superpowers/sdd/development/docx-prepend-probe-result.json` confirms one concrete block_insert_after `0` inserted the first non-title body and restored original XML exactly after removing that insertion. T04 uses only that start-position semantic; this is not replay/atomicity proof.

Remote code without terminal LF is projected with the required fence separator newline. Ordinary and disjoint text edits remove exactly that projection separator on upload, preserving actual no-LF content; hashes are checked against reread Decode. **Changing terminal blank-line count of a no-LF source conflicts.** Concrete ambiguity: source `one` projects as `one\n` before the fence; user adds one blank line, yielding `one\n\n`; stripping the synthetic separator would upload `one\n`, which a subsequent LF-present projection renders differently again. This narrow case needs persistent projection metadata or a separate UX and is not guessed. LF-present code can add meaningful terminal blank lines, with exact payload tests.

Conservative unsupported boundaries remain visible:

- Multiple adjacent unmatched replacements, duplicates, ambiguous moves and multiple new blocks requiring a newly assigned insertion anchor conflict. A single document-start insertion uses `0`; a single later insertion uses a real current sibling anchor.
- List insertion/deletion/reparenting, list-kind changes and unsupported renumbering cannot be treated as arbitrary wrapper replacement; item text changes use actual li targets. Some complex nested/loose-list shapes are opaque when exact projection cannot be proved.
- Nonconsecutive ordered sequences (recorded seq1/3), merged/multiblock/resource tables, table dimension changes, unknown attrs/resources, unsupported code/inline combinations and rich content remain opaque or blocked. No arbitrary rich-text round-trip claim.
- RawXML byte identity is guaranteed for retained source slices/non-target structures; changed supported blocks are explicitly serialized and may normalize representational spelling while retaining approved metadata.
- BaseLocal supplies no historical raw XML/IDs, so current remote XML authorizes every patch. Public hashes describe current actual semantic content. Every mutation invalidates assumptions about old IDs; the caller must read back, decode and replan before dispatching another mutation. Revision is not CAS; check/write races are not claimed eliminated.

## Actual red/green evidence

Initial `go test ./internal/bridge/markdown -count=1` against API stubs failed on missing angle preservation, code output, UTF-8 ranges, disjoint merge, insert/delete/repeated/table conflicts and opaque handling (empty stub also caused an index panic in the opaque test). The implementation then passed the initial suite.

Additional narrow tests were run before each repair and observed failing:

- Unsupported emphasis whitespace/empty styling, table pipe double escaping, auto/nested list mapping.
- No-terminal-LF projection, then no-LF terminal-count ambiguity and ordered renumbering; invalid sidecar ranges initially exposed a bounds panic.
- Opaque content hash previously ignored raw content; corrected to exclude IDs and include actual semantics.
- Corrupt current raw shape/duplicate attrs and missing exact conflict hunks.
- Unsupported list child previously split the unsafe wrapper; now retained whole and opaque.
- New document-start sentinel tests initially returned unsupported_content; parent evidence enabled only the documented sentinel.
- Opaque relocation initially returned ambiguous_mapping; corrected to unsupported_content.

Final verification after all code edits: `go test ./internal/bridge/markdown -count=1` **PASS**, 31 named focused tests, package output `ok ... 0.508s`; `git diff --check` clean. Per user override, no full-project/race/vet/crossbuild or external audit/re-review was repeated here. Parent performs block integration and full-suite checks.
