package markdown

import (
	"bytes"
	"encoding/json"

	"github.com/rclone/rclone/internal/bridge/model"
)

// A single local edit can change and insert same-kind paragraphs, making their
// individual identities ambiguous despite an unchanged remote. For plain,
// supported content only, remove the first changed unique block, then let the
// normal step/readback loop insert the desired sequence. Keep all shared prefix
// IDs and never borrow the explicit-human flag to weaken verification.
func planPlainWithBase(base, local []byte, baseline, remote model.DocView) (model.MergeResult, error) {
	result, err := PlanStep(base, local, remote)
	if err != nil || result.ConflictReason != "ambiguous_mapping" || !bytes.Equal(base, baseline.Markdown) || !bytes.Equal(base, remote.Markdown) || len(baseline.Blocks) != len(remote.Blocks) {
		return result, err
	}
	for i, b := range baseline.Blocks {
		r := remote.Blocks[i]
		if b.RemoteID != r.RemoteID || b.Hash != r.Hash || !bytes.Equal(b.RawXML, r.RawXML) {
			return result, nil
		}
	}
	// Reuse the full projection/opaque/list validation, not its authorization.
	validated, err := PlanResolvedStep(base, local, remote)
	if err != nil || validated.ConflictReason != "" || len(validated.Operations) != 1 || validated.Operations[0].Kind != model.OperationBlockDelete {
		return result, err
	}
	old, err := parseMarkdown(remote.Markdown, &remote)
	if err != nil {
		return result, err
	}
	desired, err := parseMarkdown(local, nil)
	if err != nil {
		return result, err
	}
	i := 0
	for i < len(old) && i < len(desired) && old[i].Hash == desired[i].Hash {
		i++
	}
	// A wholly unanchored rewrite keeps the existing conflict protection.
	if i == 0 || i >= len(remote.Blocks) || countHash(remote.Blocks, remote.Blocks[i].Hash) != 1 {
		return result, nil
	}
	target := remote.Blocks[i]
	validated.Operations = []model.Operation{operation(model.OperationBlockDelete, target.RemoteID, "", model.OperationPayload{BeforeHash: target.Hash})}
	return validated, nil
}

// PlanResolvedStep handles explicit human intent where automatic block identity
// is ambiguous. A destructive fallback is allowed only against the exact captured
// remote projection and wholly supported top-level content. It never overwrites
// a document, invents an anchor, or removes an opaque/rich resource.
func PlanResolvedStep(base, local []byte, remote model.DocView) (model.MergeResult, error) {
	result, err := PlanStep(base, local, remote)
	if err != nil {
		return result, err
	}
	if result.ConflictReason == "" {
		for i := range result.Operations {
			var p model.OperationPayload
			if json.Unmarshal(result.Operations[i].Payload, &p) != nil {
				return model.MergeResult{}, model.ErrUnsupportedContent
			}
			p.ExplicitResolution = true
			result.Operations[i].Payload, _ = json.Marshal(p)
		}
		return result, nil
	}
	fail := func() (model.MergeResult, error) {
		return model.MergeResult{Markdown: bytes.Clone(local), ConflictReason: "unsupported_content"}, nil
	}
	if !bytes.Equal(base, remote.Markdown) {
		return result, nil
	}
	fresh, err := DecodeWithOptions(remote.XML, remote.Revision, Options{ProjectionVersion: remote.ProjectionVersion, SourceURL: remote.SourceURL, ReferenceMap: remote.ReferenceMap})
	if err != nil {
		return fail()
	}
	if !bytes.Equal(fresh.Markdown, remote.Markdown) || len(fresh.Blocks) != len(remote.Blocks) {
		return fail()
	}
	for i, b := range fresh.Blocks {
		r := remote.Blocks[i]
		if b.Opaque || b.RemoteID == "" || b.Hash != r.Hash || b.RemoteID != r.RemoteID || !bytes.Equal(b.RawXML, r.RawXML) {
			return fail()
		}
	}
	old, err := parseMarkdown(remote.Markdown, &remote)
	if err != nil {
		return fail()
	}
	desired, err := parseMarkdown(local, nil)
	if err != nil {
		return fail()
	}
	for _, atoms := range [][]atom{old, desired} {
		for _, a := range atoms {
			if a.Opaque || a.parent != "" {
				return fail()
			}
		}
	}
	if len(remote.Blocks) == 0 {
		return fail()
	}
	target := remote.Blocks[0]
	return model.MergeResult{Markdown: bytes.Clone(local), Deferred: true, Operations: []model.Operation{
		operation(model.OperationBlockDelete, target.RemoteID, "", model.OperationPayload{BeforeHash: target.Hash, ExplicitResolution: true}),
	}}, nil
}
