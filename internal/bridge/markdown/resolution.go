package markdown

import (
	"bytes"
	"encoding/json"

	"github.com/rclone/rclone/internal/bridge/model"
)

// A single local edit can change and insert same-kind paragraphs, making their
// individual identities ambiguous despite an unchanged remote. For plain,
// supported content only, remove the first changed unique block, then let the
// normal step/readback loop insert the desired sequence. Keep shared anchor
// IDs and never borrow the explicit-human flag to weaken verification.
func planPlainWithBase(base, local []byte, baseline, remote model.DocView) (model.MergeResult, error) {
	unchanged := unchangedDocCheckpoint(base, baseline, remote)
	result, err := merge(base, local, remote, true, unchanged)
	if err != nil || result.ConflictReason != "ambiguous_mapping" || !bytes.Equal(base, baseline.Markdown) || !bytes.Equal(base, remote.Markdown) || len(baseline.Blocks) != len(remote.Blocks) {
		return result, err
	}
	for i, b := range baseline.Blocks {
		r := remote.Blocks[i]
		if b.RemoteID != r.RemoteID || b.Hash != r.Hash || !bytes.Equal(b.RawXML, r.RawXML) {
			return result, nil
		}
	}
	fresh, err := DecodeWithOptions(remote.XML, remote.Revision, Options{ProjectionVersion: remote.ProjectionVersion, SourceURL: remote.SourceURL, ReferenceMap: remote.ReferenceMap})
	if err != nil || !unchangedDocCheckpoint(base, fresh, remote) {
		return result, err
	}
	for _, block := range fresh.Blocks {
		if block.Opaque || block.RemoteID == "" {
			return result, nil
		}
	}
	old, err := parseMarkdown(remote.Markdown, &remote)
	if err != nil {
		return result, err
	}
	desired, err := parseMarkdown(local, nil)
	if err != nil {
		return result, err
	}
	desiredCounts := map[string]int{}
	for _, a := range desired {
		if a.Opaque {
			return result, nil
		}
		desiredCounts[a.Hash]++
	}
	i := 0
	for i < len(old) && i < len(desired) && old[i].Hash == desired[i].Hash {
		i++
	}
	// Preserve an unchanged section that appears later in the local intent.
	// New unique content can be inserted at the shared prefix instead of
	// deleting that section just to disambiguate paragraph alignment.
	if i > 0 && i < len(old) && i < len(desired) && desiredCounts[old[i].Hash] > 0 && countHash(remote.Blocks, desired[i].Hash) == 0 {
		anchor := old[i-1]
		anchor.Block = remote.Blocks[i-1]
		if anchor.RemoteID != "" && rootInsertionAnchor(remote, anchor, desired[i].parent) {
			a := desired[i]
			return model.MergeResult{Markdown: bytes.Clone(local), Deferred: true, Operations: []model.Operation{operation(model.OperationBlockInsertAfter, "", anchor.RemoteID, model.OperationPayload{XML: insertionXML(a), AfterHash: a.Hash})}}, nil
		}
	}
	if i == len(old) && i > 0 && i < len(desired) {
		anchor := old[i-1]
		anchor.Block = remote.Blocks[i-1]
		if rootInsertionAnchor(remote, anchor, desired[i].parent) {
			a := desired[i]
			return model.MergeResult{Markdown: bytes.Clone(local), Deferred: true, Operations: []model.Operation{operation(model.OperationBlockInsertAfter, "", anchor.RemoteID, model.OperationPayload{XML: insertionXML(a), AfterHash: a.Hash})}}, nil
		}
	}
	// A wholly unanchored rewrite keeps the existing conflict protection.
	if i >= len(remote.Blocks) || countHash(remote.Blocks, remote.Blocks[i].Hash) != 1 || desiredCounts[old[i].Hash] != 0 {
		return result, nil
	}
	if i == 0 {
		// A unique surviving interior anchor also fixes identity when a leading
		// paragraph becomes a heading. Only remove a block absent from the desired
		// document; the retained anchor itself must never be the fallback target.
		anchored := false
		for j := 1; j < len(old); j++ {
			if countHash(remote.Blocks, remote.Blocks[j].Hash) == 1 && desiredCounts[old[j].Hash] == 1 {
				anchored = true
				break
			}
		}
		if !anchored || desiredCounts[old[0].Hash] != 0 {
			return result, nil
		}
	}
	target := remote.Blocks[i]
	return model.MergeResult{Markdown: bytes.Clone(local), Deferred: true, Operations: []model.Operation{operation(model.OperationBlockDelete, target.RemoteID, "", model.OperationPayload{BeforeHash: target.Hash})}}, nil
}

// A structural edit is unilateral only when the saved and observed identities
// and complete body agree. Matching rendered text alone cannot authorize it.
func unchangedDocCheckpoint(base []byte, baseline, remote model.DocView) bool {
	if !bytes.Equal(base, baseline.Markdown) || !bytes.Equal(base, remote.Markdown) || !bytes.Equal(baseline.XML, remote.XML) || len(baseline.Blocks) != len(remote.Blocks) {
		return false
	}
	for i, b := range baseline.Blocks {
		r := remote.Blocks[i]
		if b.RemoteID != r.RemoteID || b.Hash != r.Hash || !bytes.Equal(b.RawXML, r.RawXML) {
			return false
		}
	}
	return true
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
