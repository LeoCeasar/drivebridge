package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
)

type stepPayload struct {
	model.OperationPayload
	BeforeSnapshot model.SnapshotID `json:"before_snapshot"`
}
type stepResult struct {
	NotDispatched bool             `json:"not_dispatched,omitempty"`
	AfterSnapshot model.SnapshotID `json:"after_snapshot"`
}

func (e *Engine) bind(op model.Operation) (model.Operation, error) {
	for _, grant := range e.source.Capabilities() {
		if grant.Verified && grant.Evidence != "" && grant.CLISourceRevision == cloud.CLIRevision && grant.Descriptor.Kind == op.Kind && grant.Descriptor.Route == cloud.ShortcutXMLRoute && grant.Descriptor.ID != "" {
			op.Route = grant.Descriptor.Route
			op.CapabilityID = grant.Descriptor.ID
			op.ID = uuid.NewString()
			return op, nil
		}
	}
	return op, model.ErrUnsupportedContent
}
func (e *Engine) syncDoc(ctx context.Context, j model.Job, en model.Entry) error {
	journal, err := e.store.Journal(ctx, j.ID)
	if err != nil {
		return err
	}
	// A dispatched operation with no readback record is reconciled before planning
	// anything else, including on restart. An absent effect never authorizes replay.
	for i, op := range journal.Operations {
		if _, ok := journal.Results[i]; ok {
			continue
		}
		var p stepPayload
		if json.Unmarshal(op.Payload, &p) != nil || p.BeforeSnapshot == "" {
			return model.ErrUnknownResult
		}
		_, before, err := e.loadDoc(ctx, p.BeforeSnapshot)
		if err != nil {
			return err
		}
		after, v, err := e.fetchDocProjection(ctx, en.Token, before.ProjectionVersion)
		if err != nil {
			_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
			return err
		}
		if err = markdown.VerifyOperation(before, v, op); err != nil {
			_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
			return err
		}
		if err = e.recordReadback(ctx, j.ID, i, after); err != nil {
			return err
		}
	}
	drift := 0
	started := time.Now()
	quantum, bounded := ctx.Value(quantumKey{}).(dispatchQuantum)
	for step := 0; step < 10000; step++ {
		if bounded && step > 0 && (step >= quantum.operations || time.Since(started) >= quantum.duration) {
			return errDispatchYield
		}
		journal, err = e.store.Journal(ctx, j.ID)
		if err != nil {
			return err
		}
		base, err := e.read(ctx, j.BaseLocal)
		if err != nil {
			return err
		}
		local, err := e.read(ctx, j.Local)
		if err != nil {
			return err
		}
		var baseRemote model.DocView
		if j.BaseRemote != "" {
			_, baseRemote, err = e.loadDoc(ctx, j.BaseRemote)
			if err != nil {
				return e.pause(ctx, j, err)
			}
		}
		// After a verified partial patch, rebase the remaining desired content on its
		// readback rather than trying to apply the original local edit twice.
		if len(journal.Operations) > 0 {
			var last stepResult
			if json.Unmarshal(journal.Results[len(journal.Operations)-1], &last) != nil || last.AfterSnapshot == "" {
				return model.ErrUnknownResult
			}
			_, prior, err := e.loadDoc(ctx, last.AfterSnapshot)
			if err != nil {
				return err
			}
			base = prior.Markdown
			baseRemote = prior
			local, err = e.read(ctx, journal.Desired)
			if err != nil {
				return err
			}
		}
		sn, current, err := e.fetchDocProjection(ctx, en.Token, baseRemote.ProjectionVersion)
		if err != nil {
			checked := e.deletionAfterReadFailure(ctx, en, err)
			if errors.Is(checked, model.ErrVersionChanged) {
				return checked
			}
			return e.pause(ctx, j, checked)
		}
		var merge model.MergeResult
		if !journal.HumanResolution && markdown.MatchesEncodedIntent(local, current) {
			merge = model.MergeResult{Markdown: bytes.Clone(local)}
		} else if journal.HumanResolution {
			merge, err = markdown.PlanResolvedStepWithBase(base, local, baseRemote, current)
		} else {
			var desired []byte
			var reason string
			desired, reason, err = e.mergeText(ctx, base, local, current.Markdown)
			if err != nil {
				return e.pause(ctx, j, err)
			}
			if reason != "" {
				return e.conflict(ctx, j, sn, reason)
			}
			// Git has merged both sides. The captured current snapshot now owns
			// block identity; the planner only needs to apply this merged intent.
			merge, err = markdown.PlanMergedStep(desired, current)
		}
		if err != nil {
			return e.pause(ctx, j, err)
		}
		if merge.ConflictReason != "" {
			if journal.HumanResolution && merge.ConflictReason != "unsupported_content" {
				return e.conflict(ctx, j, sn, merge.ConflictReason)
			}
			// A clean text merge that cannot map to safe cloud operations is a
			// conversion problem, never a request to manually merge the text.
			return e.pause(ctx, j, markdown.ExplainPlanFailure(merge.Markdown, current, merge.ConflictReason))
		}
		// A fresh recheck still is not CAS. It narrows the last check/write window.
		check, checked, err := e.fetchDocProjection(ctx, en.Token, current.ProjectionVersion)
		if err != nil {
			return e.pause(ctx, j, err)
		}
		if current.Revision != checked.Revision || !bytes.Equal(current.XML, checked.XML) {
			drift++
			if drift >= 3 {
				return e.pause(ctx, j, errRemoteDrift)
			}
			continue
		}
		if len(merge.Operations) == 0 {
			return e.confirm(ctx, j, check)
		}
		for _, candidate := range merge.Operations {
			if _, err = e.bind(candidate); err != nil {
				return e.pause(ctx, j, err)
			}
		}
		op, err := e.bind(merge.Operations[0])
		if err != nil {
			return e.pause(ctx, j, err)
		}
		op, err = markdown.ProjectOperationHash(op, current)
		if err != nil {
			return e.pause(ctx, j, err)
		}
		var payload model.OperationPayload
		if json.Unmarshal(op.Payload, &payload) != nil {
			return e.pause(ctx, j, model.ErrUnsupportedContent)
		}
		op.Payload, err = json.Marshal(stepPayload{OperationPayload: payload, BeforeSnapshot: sn.ID})
		if err != nil {
			return err
		}
		desired, err := e.store.PutBlob(ctx, bytes.NewReader(merge.Markdown))
		if err != nil {
			return e.pause(ctx, j, err)
		}
		if err = e.store.ProtectBackup(ctx, j.ID, sn.ID); err != nil {
			return e.pause(ctx, j, err)
		}
		index := len(journal.Operations)
		if index == 0 {
			err = e.store.RecordPlan(ctx, j.ID, []model.Operation{op}, desired)
		} else {
			err = e.store.AppendOperation(ctx, j.ID, op, desired)
		}
		if err != nil {
			return err
		}
		_, applyErr := e.source.ApplyDoc(ctx, en.Token, current.Revision, []model.Operation{op})
		if errors.Is(applyErr, cloud.ErrNotDispatched) {
			result, _ := json.Marshal(stepResult{AfterSnapshot: sn.ID, NotDispatched: true})
			if err = e.store.RecordOperation(context.WithoutCancel(ctx), j.ID, index, result); err != nil {
				return err
			}
			return e.pause(ctx, j, applyErr)
		}
		if applyErr != nil {
			_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
		}
		actual, view, err := e.fetchDocProjection(ctx, en.Token, current.ProjectionVersion)
		if err != nil {
			_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
			return errors.Join(applyErr, err)
		}
		if err = markdown.VerifyOperation(current, view, op); err != nil {
			_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
			return errors.Join(applyErr, err)
		}
		if err = e.recordReadback(ctx, j.ID, index, actual); err != nil {
			return err
		}
		drift = 0
	}
	return e.pause(ctx, j, model.ErrUnsupportedContent)
}
func (e *Engine) recordReadback(ctx context.Context, id model.JobID, index int, sn model.Snapshot) error {
	if err := e.store.ProtectBackup(ctx, id, sn.ID); err != nil {
		return err
	}
	encoded, _ := json.Marshal(stepResult{AfterSnapshot: sn.ID})
	return e.store.RecordOperation(ctx, id, index, encoded)
}
