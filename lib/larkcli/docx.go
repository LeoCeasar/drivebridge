package larkcli

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
)

type Docx struct{ runner *Runner }

func NewDocx(r *Runner) *Docx { return &Docx{runner: r} }

type Document struct {
	Token, Revision, SourceURL string
	XML                        []byte
	ReferenceMap               json.RawMessage
	Warnings                   []string
}
type DocMutation struct{ Kind, BlockID, XML string }
type ApplyResult struct {
	Revision, SourceURL, Result string
	Warnings                    []string
	Data                        json.RawMessage
}

func revisionArg(revision string, latest bool) (string, error) {
	if revision == "" && latest {
		return "-1", nil
	}
	n, err := strconv.ParseInt(revision, 10, strconv.IntSize)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != revision {
		return "", ErrProtocol
	}
	return revision, nil
}
func decodeDocument(data []byte, content bool) (Document, error) {
	var out struct {
		Document *struct {
			ID           string          `json:"document_id"`
			Revision     json.RawMessage `json:"revision_id"`
			Content      *string         `json:"content"`
			URL          string          `json:"url"`
			ReferenceMap json.RawMessage `json:"reference_map"`
		} `json:"document"`
		Warnings []string `json:"warnings"`
	}
	if singleJSON(data, &out) != nil || out.Document == nil || out.Document.ID == "" || (content && out.Document.Content == nil) {
		return Document{}, ErrProtocol
	}
	n, err := strictInt(out.Document.Revision, false)
	if err != nil || n <= 0 {
		return Document{}, ErrProtocol
	}
	doc := Document{Token: out.Document.ID, Revision: strconv.FormatInt(n, 10), SourceURL: out.Document.URL, ReferenceMap: out.Document.ReferenceMap, Warnings: out.Warnings}
	if out.Document.Content != nil {
		doc.XML = []byte(*out.Document.Content)
	}
	return doc, nil
}
func (d *Docx) Fetch(ctx context.Context, token, revision string) (Document, error) {
	if token == "" {
		return Document{}, ErrProtocol
	}
	rev, err := revisionArg(revision, true)
	if err != nil {
		return Document{}, err
	}
	r, err := d.runner.Run(ctx, Request{Args: []string{"docs", "+fetch", "--doc", token, "--doc-format", "xml", "--detail", "full", "--scope", "full", "--revision-id", rev}})
	if err != nil {
		return Document{}, err
	}
	doc, err := decodeDocument(r.Data, true)
	if err != nil {
		return Document{}, err
	}
	if doc.Token != token || (revision != "" && doc.Revision != revision) {
		return Document{}, ErrIdentity
	}
	return doc, nil
}
func (d *Docx) Create(ctx context.Context, parent, title string, xml []byte) (Document, error) {
	args := []string{"docs", "+create", "--doc-format", "xml", "--content", "-"}
	if parent != "" {
		args = append(args, "--parent-token", parent)
	}
	if title != "" {
		args = append(args, "--title", title)
	}
	r, err := d.runner.Run(ctx, Request{Args: args, Stdin: xml})
	if err != nil {
		return Document{}, errors.Join(ErrUnknownResult, err)
	}
	if r.Result != "success" {
		return Document{}, errors.Join(ErrUnknownResult, ErrProtocol)
	}
	doc, err := decodeDocument(r.Data, false)
	if err != nil {
		return Document{}, errors.Join(ErrUnknownResult, err)
	}
	return doc, nil
}

// Apply sends exactly one logical shortcut mutation. It does not retry or assert
// server CAS/idempotency. The product must gate semantic capabilities and journal
// each call; success still requires a separate after-write observation.
func (d *Docx) Apply(ctx context.Context, token, revision string, op DocMutation) (ApplyResult, error) {
	rev, err := revisionArg(revision, false)
	if err != nil {
		return ApplyResult{}, err
	}
	if token == "" || op.BlockID == "" {
		return ApplyResult{}, ErrProtocol
	}
	switch op.Kind {
	case "block_replace", "block_insert_after":
		if op.XML == "" {
			return ApplyResult{}, ErrProtocol
		}
	case "block_delete":
		if op.XML != "" {
			return ApplyResult{}, ErrProtocol
		}
	default:
		return ApplyResult{}, ErrUnsupportedCapability
	}
	args := []string{"docs", "+update", "--doc", token, "--command", op.Kind, "--block-id", op.BlockID, "--doc-format", "xml", "--revision-id", rev}
	if op.Kind != "block_delete" {
		args = append(args, "--content", "-")
	}
	r, err := d.runner.Run(ctx, Request{Args: args, Stdin: []byte(op.XML)})
	result := ApplyResult{Result: r.Result, Warnings: r.Warnings, Data: r.Data}
	if err != nil {
		return result, errors.Join(ErrUnknownResult, err)
	}
	if r.Result != "success" {
		return result, errors.Join(ErrUnknownResult, ErrProtocol)
	}
	var raw struct {
		Document *struct {
			Revision json.RawMessage `json:"revision_id"`
			URL      string          `json:"url"`
		} `json:"document"`
	}
	if singleJSON(r.Data, &raw) != nil || raw.Document == nil {
		return result, errors.Join(ErrUnknownResult, ErrProtocol)
	}
	n, err := strictInt(raw.Document.Revision, false)
	if err != nil || n <= 0 {
		return result, errors.Join(ErrUnknownResult, ErrProtocol)
	}
	result.Revision = strconv.FormatInt(n, 10)
	result.SourceURL = raw.Document.URL
	return result, nil
}
