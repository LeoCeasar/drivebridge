// Package cloud converts the independent official CLI protocol into canonical
// bridge values. It owns neither queues, journal persistence nor reconciliation.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

// CLIRevision is the VCS revision embedded by the verified official 1.0.99 binaries.
const CLIRevision = "8d8357d39dd15ac5e9aa815d4ab2639d56bd3239"

// InspectedSourceRevision is the later checkout used during initial source review.
// Drive/Docx/auth/runtime contracts are unchanged between these two revisions.
const InspectedSourceRevision = "1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a"
const ShortcutXMLRoute = "docs_shortcut_xml"

var ErrUnsupportedBatch = errors.New("unsupported_batch")

// CapabilityGrant is supplied by the composition layer after checking trusted
// evidence and the installed CLI revision. User configuration is not authority.
// Verified approves only the described operation semantics, never CAS/replay.
type CapabilityGrant struct {
	Descriptor        model.OperationCapability
	CLISourceRevision string
	Evidence          string
	Verified          bool
}
type Options struct {
	CheckMutation func(context.Context) error
	CheckIdentity func(context.Context) error
	TempDir       string
	Capabilities  []CapabilityGrant
}
type Client struct {
	checkMutation func(context.Context) error
	checkIdentity func(context.Context) error
	drive         *larkcli.Drive
	docs          *larkcli.Docx
	capabilities  []CapabilityGrant
}

func New(r *larkcli.Runner, o Options) (*Client, error) {
	if _, ok := r.ManagedIdentity(); !ok {
		return nil, model.ErrIdentityChanged
	}
	return &Client{checkMutation: o.CheckMutation, checkIdentity: o.CheckIdentity, drive: larkcli.NewDrive(r, o.TempDir), docs: larkcli.NewDocx(r), capabilities: append([]CapabilityGrant(nil), o.Capabilities...)}, nil
}

type FileMeta struct {
	Token, Version string
	Size           int64
	ModTime        time.Time
}

// Candidate creation evidence may be returned alongside an error; retain both
// and reconcile before adopting or confirming any returned identity.
type Document struct {
	Result                     string
	Data                       json.RawMessage
	Token, Revision, SourceURL string
	XML                        []byte
	ReferenceMap               json.RawMessage
	Warnings                   []string
}

// Completed means CLI-reported transport completion, not after-write confirmation.
// T07 must journal and independently reconcile before publishing.
type ApplyResult struct {
	Revision, Result, SourceURL string
	Completed                   []int
	Warnings                    []string
	Capabilities                []CapabilityGrant
	Data                        json.RawMessage
}
type FileObservation struct {
	Token    string
	Identity model.FileIdentity
	Size     int64
	lease    *larkcli.FileObservation
}

func (o *FileObservation) Open() (io.ReadCloser, error) { return o.lease.Open() }
func (o *FileObservation) Close() error                 { return o.lease.Close() }
func canonicalError(err error) error {
	if err == nil {
		return nil
	}
	mapped := []error{err}
	if errors.Is(err, larkcli.ErrNotDispatched) {
		mapped = append(mapped, ErrNotDispatched)
	}
	if errors.Is(err, larkcli.ErrUnknownResult) {
		mapped = append(mapped, model.ErrUnknownResult)
	}
	if errors.Is(err, larkcli.ErrIdentity) {
		mapped = append(mapped, model.ErrIdentityChanged)
	}
	if errors.Is(err, larkcli.ErrUnsupportedCapability) {
		mapped = append(mapped, model.ErrUnsupportedContent)
	}
	return errors.Join(mapped...)
}

func (c *Client) ObserveFile(ctx context.Context, token string) (*FileObservation, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	o, err := c.drive.ObserveFile(ctx, token)
	if err != nil {
		return nil, canonicalError(err)
	}
	identity := model.FileIdentity{Kind: model.FileIdentityObservation, ObservationID: o.ObservationID, SHA256: o.SHA256, StartedAt: o.StartedAt, CompletedAt: o.CompletedAt}
	if err := identity.Validate(); err != nil {
		o.Close()
		return nil, err
	}
	return &FileObservation{Token: o.Token, Identity: identity, Size: o.Size, lease: o}, nil
}
func (c *Client) FileMeta(ctx context.Context, token string) (FileMeta, error) {
	if err := c.check(ctx); err != nil {
		return FileMeta{}, err
	}
	m, err := c.drive.FileMeta(ctx, token)
	return FileMeta{m.Token, m.Version, m.Size, m.ModTime}, canonicalError(err)
}
func convertDoc(d larkcli.Document) Document {
	return Document{Result: d.Result, Data: d.Data, Token: d.Token, Revision: d.Revision, SourceURL: d.SourceURL, XML: d.XML, ReferenceMap: d.ReferenceMap, Warnings: d.Warnings}
}
func (c *Client) FetchDoc(ctx context.Context, token, revision string) (Document, error) {
	if err := c.check(ctx); err != nil {
		return Document{}, err
	}
	d, err := c.docs.Fetch(ctx, token, revision)
	return convertDoc(d), canonicalError(err)
}
func (c *Client) CreateDoc(ctx context.Context, parent, title string, xml []byte) (Document, error) {
	if err := c.guardMutation(ctx); err != nil {
		return Document{}, err
	}
	d, err := c.docs.Create(ctx, parent, title, xml)
	return convertDoc(d), canonicalError(err)
}
func (c *Client) Capabilities() []CapabilityGrant {
	result := make([]CapabilityGrant, 0, 3+len(c.capabilities))
	for _, kind := range []string{model.OperationBlockReplace, model.OperationBlockDelete, model.OperationBlockInsertAfter} {
		result = append(result, CapabilityGrant{Descriptor: model.OperationCapability{Kind: kind, Route: ShortcutXMLRoute}, CLISourceRevision: CLIRevision, Evidence: "offline: pinned source and subprocess transport fixtures; preservation/reconciliation live gate pending"})
	}
	return append(result, c.capabilities...)
}
func (c *Client) ApplyDoc(ctx context.Context, token, revision string, ops []model.Operation) (ApplyResult, error) {
	if len(ops) == 0 {
		return ApplyResult{}, larkcli.ErrProtocol
	}
	if len(ops) != 1 {
		return ApplyResult{}, ErrUnsupportedBatch
	}
	op := ops[0]
	if op.ClientToken != "" {
		return ApplyResult{}, canonicalError(larkcli.ErrUnsupportedCapability)
	}
	authorized := false
	for _, cap := range c.capabilities {
		if cap.Verified && cap.Evidence != "" && cap.CLISourceRevision == CLIRevision && cap.Descriptor.ID != "" && cap.Descriptor.ID == op.CapabilityID && cap.Descriptor.Route == ShortcutXMLRoute && op.Route == cap.Descriptor.Route && cap.Descriptor.Kind == op.Kind {
			authorized = true
			break
		}
	}
	if !authorized {
		return ApplyResult{}, canonicalError(larkcli.ErrUnsupportedCapability)
	}
	var payload model.OperationPayload
	if json.Unmarshal(op.Payload, &payload) != nil {
		return ApplyResult{}, larkcli.ErrProtocol
	}
	block := op.BlockID
	if op.Kind == model.OperationBlockInsertAfter {
		block = op.AfterBlockID
	}
	if err := c.guardMutation(ctx); err != nil {
		return ApplyResult{}, err
	}
	r, err := c.docs.Apply(ctx, token, revision, larkcli.DocMutation{Kind: op.Kind, BlockID: block, XML: payload.XML})
	result := ApplyResult{Revision: r.Revision, Result: r.Result, SourceURL: r.SourceURL, Warnings: r.Warnings, Capabilities: c.Capabilities(), Data: r.Data}
	if err == nil {
		result.Completed = []int{0}
	}
	return result, canonicalError(err)
}

// List returns complete typed items; missing remote timestamps stay explicitly
// unknown. The durable consumer owns a persisted first-observed fallback.
func (c *Client) List(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	items, err := c.drive.List(ctx, parent)
	return items, canonicalError(err)
}
func (c *Client) UploadFile(ctx context.Context, parent, token, name string, in io.Reader) (larkcli.UploadResult, error) {
	if err := c.guardMutation(ctx); err != nil {
		return larkcli.UploadResult{}, err
	}
	r, err := c.drive.Upload(ctx, parent, token, name, in)
	return r, canonicalError(err)
}
func (c *Client) CreateFolder(ctx context.Context, parent, name string) (string, error) {
	if err := c.guardMutation(ctx); err != nil {
		return "", err
	}
	token, err := c.drive.CreateFolder(ctx, parent, name)
	return token, canonicalError(err)
}
func (c *Client) RenameFile(ctx context.Context, token, name string) error {
	if err := c.guardMutation(ctx); err != nil {
		return err
	}
	return canonicalError(c.drive.Rename(ctx, token, "file", name))
}
func (c *Client) MoveFile(ctx context.Context, token, parent string) error {
	if err := c.guardMutation(ctx); err != nil {
		return err
	}
	return canonicalError(c.drive.Move(ctx, token, "file", parent))
}
func (c *Client) DeleteFile(ctx context.Context, token string) error {
	if err := c.guardMutation(ctx); err != nil {
		return err
	}
	return canonicalError(c.drive.Delete(ctx, token, "file"))
}

// ErrNotDispatched proves a guard, credential wait, or transport start rejected
// an invocation before its CLI child started.
var ErrNotDispatched = errors.New("managed RPC rejected before dispatch")

func (c *Client) check(ctx context.Context) error {
	if c.checkIdentity != nil {
		if err := c.checkIdentity(ctx); err != nil {
			return errors.Join(ErrNotDispatched, err)
		}
	}
	return nil
}

func (c *Client) guardMutation(ctx context.Context) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	if c.checkMutation != nil {
		if err := c.checkMutation(ctx); err != nil {
			return errors.Join(ErrNotDispatched, err)
		}
	}
	return nil
}
