package syncer

import (
	"bytes"
	"context"
	"errors"
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
	if op == nil && en.Token != "" {
		return en, nil
	}
	input, err := e.read(ctx, j.Local)
	if err != nil {
		return en, err
	}
	var body []byte
	if en.Kind == model.KindDocx {
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
		if err = e.store.RecordPlan(ctx, j.ID, nil, j.Local); err != nil {
			return en, err
		}
		created, err := e.store.PrepareCreate(ctx, j.ID, parent, cloudName(en))
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
				return en, openErr
			}
			result, uploadErr := e.source.UploadFile(ctx, op.ParentToken, "", op.TemporaryName, r)
			err = errors.Join(uploadErr, r.Close())
			token = result.FileToken
		}
		if err != nil || token == "" {
			_ = e.store.MarkCreateUnknown(context.WithoutCancel(ctx), op.ID)
			_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
			return en, errors.Join(model.ErrUnknownResult, err)
		}
		bound, err := e.store.BindCreated(ctx, op.ID, token, revision)
		if err != nil {
			return en, err
		}
		op = &bound
	}
	if op.Token == "" {
		list, err := e.source.List(ctx, op.ParentToken)
		if err != nil {
			return en, err
		}
		var candidate string
		count := 0
		for _, item := range list {
			if item.Name == op.TemporaryName && item.Type == string(en.Kind) {
				candidate = item.Token
				count++
			}
		}
		if count != 1 {
			return en, model.ErrUnknownResult
		}
		revision, err := e.verifyCreated(ctx, *op, candidate, input)
		if err != nil {
			return en, err
		}
		bound, err := e.store.BindCreated(ctx, op.ID, candidate, revision)
		if err != nil {
			return en, err
		}
		op = &bound
	}
	if _, err = e.verifyCreated(ctx, *op, op.Token, input); err != nil {
		_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
		return en, err
	}
	// Reconcile a completed rename by stable token before ever sending it again.
	list, err := e.source.List(ctx, op.ParentToken)
	if err != nil {
		return en, err
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
			return en, model.ErrUnknownResult
		}
		if op.State == "renaming" {
			return en, model.ErrUnknownResult
		}
		if err = e.store.MarkCreateRenaming(ctx, op.ID); err != nil {
			return en, err
		}
		renameErr := e.source.Rename(ctx, op.Token, en.Kind, op.FinalName)
		list, err = e.source.List(ctx, op.ParentToken)
		if err != nil {
			return en, errors.Join(renameErr, err)
		}
		name = ""
		for _, item := range list {
			if item.Token == op.Token && item.Type == string(en.Kind) {
				name = item.Name
			}
		}
		if name != op.FinalName {
			return en, errors.Join(model.ErrUnknownResult, renameErr)
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
		wanted, err := markdown.Encode(input)
		if err != nil {
			return "", err
		}
		actual, err := markdown.Encode(view.Markdown)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(wanted, actual) {
			return "", model.ErrUnknownResult
		}
		return sn.Revision, nil
	}
	sn, err := e.observeFile(ctx, token)
	if err != nil {
		return "", err
	}
	if sn.FileData != op.Input {
		return "", model.ErrUnknownResult
	}
	return "", nil
}
