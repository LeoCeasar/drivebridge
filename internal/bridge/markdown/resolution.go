package markdown

import (
	"bytes"
	"encoding/json"

	"github.com/rclone/rclone/internal/bridge/model"
)

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
