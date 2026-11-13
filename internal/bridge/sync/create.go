package syncer

import (
	"bytes"
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"path"
	"strings"

	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func (e *Engine) parentToken(ctx context.Context, remote string) (string, error) {
	parent := path.Dir(remote)
	if parent == "." || parent == "" {
		sc, err := e.store.Scope(ctx)
		return sc.FolderToken, err
	}
	entry, err := e.store.EntryByRemote(ctx, parent)
	if err != nil {
		return "", err
	}
	if entry.Kind != model.KindFolder || entry.Token == "" {
		return "", store.ErrBusy
	}
	return entry.Token, nil
}
func cloudName(en model.Entry) string {
	name := path.Base(en.Remote)
	if en.Kind == model.KindDocx {
		name = strings.TrimSuffix(name, path.Ext(name))
	}
	return name
}
func (e *Engine) create(ctx context.Context, j model.Job, en model.Entry) (model.Entry, error) {
	creates, err := e.store.PendingCreates(ctx)
	if err != nil {
		return en, err
	}
	var op *model.CreateOp
	for i := range creates {
		if creates[i].JobID == j.ID {
			op = &creates[i]
			break
		}
	}
	if op != nil && op.NeedsAttention {
		return en, store.ErrCreateNeedsAttention
	}
	if op == nil && en.Token != "" {
		return en, nil
	}
	var input []byte
	if en.Kind == model.KindDocx {
		input, err = e.read(ctx, j.Local)
		if err != nil {
			return en, err
		}
	}
	if op == nil && en.Kind == model.KindFile && isEmpty(j.Local) && !e.opt.EmptyFileUploadVerified {
		if err = e.store.PauseJob(ctx, j.ID, model.StatusBlocked, "empty_file_unsupported"); err != nil {
			return en, err
		}
		return en, model.ErrUnsupportedContent
	}
	var body []byte
	if en.Kind == model.KindDocx {
		input, err = markdown.NormalizeForMerge(input)
		if err != nil {
			return en, e.pause(ctx, j, err)
		}
		body, err = markdown.Encode(input)
		if err != nil {
			return en, e.pause(ctx, j, err)
		}
	}
	if op == nil {
		parent, err := e.parentToken(ctx, en.Remote)
		if err != nil {
			return en, e.pause(ctx, j, err)
		}
		journal, err := e.store.Journal(ctx, j.ID)
		if err != nil {
			return en, err
		}
		if journal.Job.Plan == "" {
			if err = e.store.RecordPlan(ctx, j.ID, nil, j.Local); err != nil {
				return en, err
			}
		} else if journal.Desired != j.Local || len(journal.Operations) != 0 {
			return en, model.ErrUnknownResult
		}
		name := cloudName(en)
		if journal.HumanResolution && en.CloudName != "" {
			// Restoring a deleted resource retains its original cloud name;
			// the local spelling may contain escaping or a collision suffix.
			name = en.CloudName
		}
		created, err := e.store.PrepareCreate(ctx, j.ID, parent, name)
		if err != nil {
			return en, err
		}
		op = &created
		var token, revision string
		if en.Kind == model.KindDocx {
			d, createErr := e.source.CreateDoc(ctx, op.ParentToken, op.TemporaryName, body)
			err = createErr
			token = d.Token
			revision = d.Revision
		} else {
			r, openErr := e.store.ReadBlob(ctx, j.Local)
			if openErr != nil {
				resetErr := e.store.RejectUndispatchedCreate(context.WithoutCancel(ctx), op.ID)
				return en, errors.Join(e.pause(ctx, j, openErr), resetErr)
			}
			result, uploadErr := e.source.UploadFile(ctx, op.ParentToken, "", op.TemporaryName, r)
			err = errors.Join(uploadErr, r.Close())
			token = result.FileToken
		}
		if errors.Is(err, cloud.ErrNotDispatched) {
			if resetErr := e.store.RejectUndispatchedCreate(context.WithoutCancel(ctx), op.ID); resetErr != nil {
				return en, errors.Join(err, resetErr)
			}
			return en, e.pause(ctx, j, err)
		}
		if err != nil || token == "" {
			saveErr := e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, token, revision, creationFailureCode(err), e.now())
			return en, errors.Join(model.ErrUnknownResult, err, saveErr)
		}
		bound, err := e.store.BindCreated(ctx, op.ID, token, revision)
		if err != nil {
			saveErr := e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, token, revision, creationFailureCode(err), e.now())
			return en, errors.Join(model.ErrUnknownResult, err, saveErr)
		}
		op = &bound
	}
	if op.Token == "" {
		list, listErr := e.source.List(ctx, op.ParentToken)
		if listErr != nil {
			return en, e.failedCreateCheck(ctx, *op, creationCheckReason(listErr), listErr)
		}
		var candidate string
		count := 0
		for _, item := range list {
			match := item.Name == op.TemporaryName
			if op.CandidateToken != "" {
				match = item.Token == op.CandidateToken && (item.Name == op.TemporaryName || item.Name == op.FinalName)
			}
			if match && item.Type == string(en.Kind) {
				candidate = item.Token
				count++
			}
		}
		if count != 1 {
			reason := "not_found"
			if count > 1 {
				reason = "multiple_matches"
			}
			return en, e.failedCreateCheck(ctx, *op, reason, model.ErrUnknownResult)
		}
		// A uniquely observed token remains evidence even if body verification
		// fails or the object later moves. Retain it without granting ownership.
		uncertainAt := op.FirstUncertainAt
		if uncertainAt.IsZero() {
			journal, journalErr := e.store.Journal(ctx, j.ID)
			if journalErr != nil {
				return en, journalErr
			}
			uncertainAt = journal.StagedAt
		}
		if uncertainAt.IsZero() {
			uncertainAt = e.now()
		}
		if saveErr := e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, candidate, "", op.FailureCode, uncertainAt); saveErr != nil {
			return en, errors.Join(model.ErrUnknownResult, saveErr)
		}
		op.CandidateToken = candidate
		revision, verifyErr := e.verifyCreated(ctx, *op, candidate, input)
		if verifyErr != nil {
			return en, e.failedCreateCheck(ctx, *op, creationCheckReason(verifyErr), verifyErr)
		}
		bound, bindErr := e.store.BindCreated(ctx, op.ID, candidate, revision)
		if bindErr != nil {
			saveErr := e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, candidate, revision, creationFailureCode(bindErr), e.now())
			return en, errors.Join(model.ErrUnknownResult, bindErr, saveErr)
		}
		op = &bound
	}
	if _, err = e.verifyCreated(ctx, *op, op.Token, input); err != nil {
		return en, e.failedCreateCheck(ctx, *op, creationCheckReason(err), err)
	}
	// Reconcile a completed rename by stable token before ever sending it again.
	list, err := e.source.List(ctx, op.ParentToken)
	if err != nil {
		return en, e.failedCreateCheck(ctx, *op, creationCheckReason(err), err)
	}
	name := ""
	for _, item := range list {
		if item.Token == op.Token && item.Type == string(en.Kind) {
			name = item.Name
		}
	}
	if name != op.FinalName {
		// A bound journal may have dispatched this rename before a crash. Renaming
		// the same token is idempotent in intent, but retain unknown until observed.
		if name != op.TemporaryName {
			return en, e.failedCreateCheck(ctx, *op, "location_changed", model.ErrUnknownResult)
		}
		if op.State == "renaming" {
			return en, e.failedCreateCheck(ctx, *op, "rename_unconfirmed", model.ErrUnknownResult)
		}
		if err = e.store.MarkCreateRenaming(ctx, op.ID); err != nil {
			return en, err
		}
		renameErr := e.source.Rename(ctx, op.Token, en.Kind, op.FinalName)
		if errors.Is(renameErr, cloud.ErrNotDispatched) {
			if resetErr := e.store.RejectCreateRename(context.WithoutCancel(ctx), op.ID); resetErr != nil {
				return en, errors.Join(renameErr, resetErr)
			}
			saveErr := e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, "", "", creationFailureCode(renameErr), e.now())
			return en, errors.Join(saveErr, e.failedCreateCheck(ctx, *op, creationCheckReason(renameErr), renameErr))
		}
		list, err = e.source.List(ctx, op.ParentToken)
		if err != nil {
			_ = e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, "", "", creationFailureCode(renameErr), e.now())
			return en, e.failedCreateCheck(ctx, *op, creationCheckReason(err), errors.Join(renameErr, err))
		}
		name = ""
		for _, item := range list {
			if item.Token == op.Token && item.Type == string(en.Kind) {
				name = item.Name
			}
		}
		if name != op.FinalName {
			_ = e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, "", "", creationFailureCode(renameErr), e.now())
			return en, e.failedCreateCheck(ctx, *op, "rename_unconfirmed", errors.Join(model.ErrUnknownResult, renameErr))
		}
	}
	if err = e.store.FinishCreate(ctx, op.ID); err != nil {
		return en, err
	}
	return e.store.Entry(ctx, en.ID)
}
func (e *Engine) verifyCreated(ctx context.Context, op model.CreateOp, token string, input []byte) (string, error) {
	if op.Kind == model.KindDocx {
		sn, view, err := e.fetchDoc(ctx, token)
		if err != nil {
			return "", err
		}
		if markdown.MatchesEncodedIntent(input, view) {
			return sn.Revision, nil
		}
		wanted, err := markdown.Encode(input)
		if err != nil {
			return "", err
		}
		actual, err := markdown.Encode(view.Markdown)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(wanted, actual) {
			return "", errors.Join(model.ErrUnknownResult, errCreationContentMismatch)
		}
		return sn.Revision, nil
	}
	sn, err := e.observeFile(ctx, token)
	if err != nil {
		return "", err
	}
	if sn.FileData != op.Input {
		return "", errors.Join(model.ErrUnknownResult, errCreationContentMismatch)
	}
	return "", nil
}
