package markdown

import (
	"encoding/json"
	"strconv"

	"github.com/rclone/rclone/internal/bridge/model"
)

func operationSnapshotHash(view model.DocView) string {
	return digest("block-view-v1\x00" + strconv.Itoa(view.ProjectionVersion) + "\x00" + view.Revision + "\x00" + string(view.XML))
}

// Bind both the immutable view and the addressed operation. Projection may
// legitimately update AfterHash, so it rebinds only after validating the input.
func operationProof(snapshot string, op model.Operation, p model.OperationPayload) string {
	raw, _ := json.Marshal([]string{snapshot, op.Kind, op.BlockID, op.AfterBlockID, p.XML, p.BeforeHash, p.AfterHash})
	return digest("block-operation-v1\x00" + string(raw))
}
func operationContextHash(view model.DocView, op model.Operation, p model.OperationPayload) string {
	return operationProof(operationSnapshotHash(view), op, p)
}
func bindOperationContext(result model.MergeResult, current model.DocView) (model.MergeResult, error) {
	if result.ConflictReason != "" || len(result.Operations) == 0 {
		return result, nil
	}
	snapshot := operationSnapshotHash(current)
	for i, op := range result.Operations {
		var p model.OperationPayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return model.MergeResult{}, model.ErrUnsupportedContent
		}
		p.ContextHash = operationProof(snapshot, op, p)
		encoded, err := json.Marshal(p)
		if err != nil {
			return model.MergeResult{}, err
		}
		result.Operations[i].Payload = encoded
	}
	return result, nil
}
