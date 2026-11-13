package markdown

import (
	"bytes"
	"github.com/rclone/rclone/internal/bridge/model"
)

// PlanMergedStep applies a previously clean text merge against one exact remote
// checkpoint. Ambiguous plain paragraph alignment is not a text conflict. If
// every old/new atom is supported, a changed unique atom may be removed and the
// normal readback/replan loop reconstructs the desired body in the same document.
// This never grants explicit human authority or discards rich/opaque resources.
func PlanMergedStep(desired []byte, current model.DocView) (model.MergeResult, error) {
	result, err := PlanStepWithBase(current.Markdown, desired, current, current)
	if err != nil || result.ConflictReason != "ambiguous_mapping" {
		return result, err
	}
	fresh, err := DecodeWithOptions(current.XML, current.Revision, Options{ProjectionVersion: current.ProjectionVersion, SourceURL: current.SourceURL, ReferenceMap: current.ReferenceMap})
	if err != nil || !unchangedDocCheckpoint(current.Markdown, fresh, current) {
		return result, err
	}
	for _, b := range fresh.Blocks {
		if b.Opaque || b.Readable || b.RemoteID == "" {
			return result, nil
		}
	}
	old, err := parseMarkdown(current.Markdown, &current)
	if err != nil {
		return result, err
	}
	next, err := parseMarkdown(desired, nil)
	if err != nil {
		return result, err
	}
	for _, atoms := range [][]atom{old, next} {
		for _, a := range atoms {
			if a.Opaque || a.Readable || a.parent != "" {
				return result, nil
			}
		}
	}
	counts := map[string]int{}
	for _, a := range next {
		counts[a.Hash]++
	}
	for i, a := range old {
		if i < len(next) && a.Hash == next[i].Hash {
			continue
		}
		if counts[a.Hash] > 0 || countHash(current.Blocks, current.Blocks[i].Hash) != 1 {
			return result, nil
		}
		target := current.Blocks[i]
		return model.MergeResult{Markdown: bytes.Clone(desired), Deferred: true, Operations: []model.Operation{operation(model.OperationBlockDelete, target.RemoteID, "", model.OperationPayload{BeforeHash: target.Hash})}}, nil
	}
	return result, nil
}
