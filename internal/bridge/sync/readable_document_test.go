package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

const scheme3Document = `<title id="doc">Operations</title><p id="intro">intro before</p><table id="table"><colgroup><col width="600"/></colgroup><tbody><tr><td><p id="command">cat app/.env</p><p id="status">git log</p><img id="image" src="image-token" name="image.png" mime="image/png" alt="diagram"/></td></tr></tbody></table><pre id="code" lang="bash"><code>echo hi<br/>pwd</code></pre><p id="end">tail</p>`

// The old generic fixture mutates only top-level atoms. This source also models
// a single nested paragraph replacement, changing its identity on every write.
type nestedFixtureCloud struct{ *fixtureCloud }

func (f nestedFixtureCloud) OpenDocMedia(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("fixture image bytes")), nil
}

func (f nestedFixtureCloud) ApplyDoc(ctx context.Context, token, revision string, ops []model.Operation) (cloud.ApplyResult, error) {
	view, err := markdown.Decode([]byte(f.xml), strconv.Itoa(f.revision))
	if err != nil {
		return cloud.ApplyResult{}, err
	}
	for _, block := range view.Blocks {
		for _, child := range block.Children {
			if child.RemoteID == ops[0].BlockID {
				if ops[0].Kind != model.OperationBlockReplace {
					return cloud.ApplyResult{}, model.ErrUnsupportedContent
				}
				var p model.OperationPayload
				if err = json.Unmarshal(ops[0].Payload, &p); err != nil {
					return cloud.ApplyResult{}, err
				}
				f.writes++
				at := strings.IndexByte(p.XML, '>')
				if at < 0 {
					return cloud.ApplyResult{}, model.ErrUnsupportedContent
				}
				replacement := p.XML[:at] + ` id="nested` + strconv.Itoa(f.writes) + `"` + p.XML[at:]
				f.xml = strings.Replace(f.xml, string(child.RawXML), replacement, 1)
				if f.applyHook != nil {
					f.applyHook()
					f.applyHook = nil
				}
				f.revision++
				if f.loseResponse {
					f.loseResponse = false
					return cloud.ApplyResult{}, model.ErrUnknownResult
				}
				return cloud.ApplyResult{Revision: strconv.Itoa(f.revision)}, nil
			}
		}
	}
	return f.fixtureCloud.ApplyDoc(ctx, token, revision, ops)
}

func readableEngineFixture(t *testing.T) (*store.Store, *Engine, *fixtureCloud, model.Entry) {
	t.Helper()
	s, e, source, entry := setup(t, model.KindDocx)
	source.xml = scheme3Document
	e.source = nestedFixtureCloud{source}
	sn, _, err := e.saveDoc(bg, cloud.Document{Token: entry.Token, Revision: "1", XML: []byte(source.xml)})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.OfferRemote(bg, entry.ID, entry.Generation, sn, sn.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	entry, err = s.CommitPublication(bg, candidate)
	if err != nil {
		t.Fatal(err)
	}
	return s, e, source, entry
}

func TestScheme3WholeDocumentNestedWritesAndAssetsPreserved(t *testing.T) {
	s, e, source, entry := readableEngineFixture(t)
	local := strings.Replace(blobText(t, s, entry.Local), "intro before", "intro changed", 1)
	local = strings.Replace(local, "cat app/.env", "cat app/.env.local", 1)
	local = strings.Replace(local, "echo hi", "echo hello", 1)
	stageContent(t, s, entry, local)
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Entry(bg, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusSynced || source.writes != 3 {
		t.Fatalf("status=%s writes=%d", got.Status, source.writes)
	}
	for _, required := range []string{"intro changed", "cat app/.env.local", "echo hello", `width="600"`, `src="image-token"`, `<p id="status">git log</p>`} {
		if !strings.Contains(source.xml, required) {
			t.Fatalf("lost %q: %s", required, source.xml)
		}
	}
	_, view, err := e.loadDoc(bg, got.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Assets) != 1 || view.Assets[0].Token != "image-token" || !bytes.Equal(view.Markdown, []byte(blobText(t, s, got.Visible))) {
		t.Fatal("confirmed projection or resource identity lost")
	}
	if err = s.Validate(bg); err != nil {
		t.Fatal(err)
	}
}

func TestScheme3NestedWritePreservesRemoteOnlyChange(t *testing.T) {
	s, e, source, entry := readableEngineFixture(t)
	local := strings.Replace(blobText(t, s, entry.Local), "cat app/.env", "cat app/.env.local", 1)
	stageContent(t, s, entry, local)
	source.xml = strings.Replace(source.xml, "git log", "git log --oneline", 1)
	source.revision++
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if got.Status != model.StatusSynced || source.writes != 1 || !strings.Contains(source.xml, "git log --oneline") || !strings.Contains(blobText(t, s, got.Visible), "git log --oneline") {
		t.Fatalf("remote change not retained: %s writes=%d", got.Status, source.writes)
	}
}

func TestScheme3UnsupportedStructureNeverPartiallyWrites(t *testing.T) {
	s, e, source, entry := readableEngineFixture(t)
	local := strings.Replace(blobText(t, s, entry.Local), "intro before", "intro changed", 1)
	local = strings.Replace(local, "<tbody>", "<thead>", 1)
	stageContent(t, s, entry, local)
	if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatalf("expected preserved conflict: %v", err)
	}
	if source.writes != 0 {
		t.Fatal("partially wrote supported portion before structural validation")
	}
	got, _ := s.Entry(bg, entry.ID)
	if blobText(t, s, got.Local) != local {
		t.Fatal("lost local draft")
	}
}

func TestScheme3NestedResponseLossReconcilesWithoutReplay(t *testing.T) {
	s, e, source, entry := readableEngineFixture(t)
	local := strings.Replace(blobText(t, s, entry.Local), "cat app/.env", "cat app/.env.local", 1)
	stageContent(t, s, entry, local)
	source.loseResponse = true
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, entry.ID)
	if source.writes != 1 || got.Status != model.StatusSynced {
		t.Fatalf("replayed=%d status=%s", source.writes, got.Status)
	}
}
