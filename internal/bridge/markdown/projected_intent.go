package markdown

import (
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	"strings"
)

// MatchesEncodedIntent compares complete, supported XML semantics. Markdown
// cannot express consecutive/trailing breaks losslessly; readable HTML remains
// a valid display of those blocks without changing older projection snapshots.
func MatchesEncodedIntent(intent []byte, remote model.DocView) bool {
	encoded, err := Encode(intent)
	if err != nil {
		return false
	}
	wanted, err := parseXML(encoded)
	if err != nil {
		return false
	}
	actual, err := parseXML(remote.XML)
	if err != nil {
		return false
	}
	if len(actual) > 0 && actual[0].tag == "title" {
		actual = actual[1:]
	}
	if len(wanted) != len(actual) {
		return false
	}
	for i, n := range actual {
		if _, err := xmlMarkdown(n); err != nil {
			return false
		}
		if isYAMLFrontMatterNode(n) != isYAMLFrontMatterNode(wanted[i]) || semanticXML(n) != semanticXML(wanted[i]) {
			return false
		}
	}
	return true
}

// ProjectOperationHash predicts the existing decoder's readback hash rather
// than changing its immutable projection. Only fully supported XML may acquire
// a readable/opaque hash; resource-bearing XML cannot use this compatibility.
func ProjectOperationHash(op model.Operation, before model.DocView) (model.Operation, error) {
	var p model.OperationPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return op, model.ErrUnsupportedContent
	}
	contextual := p.ContextHash != "" && p.ContextHash == operationContextHash(before, op, p)
	if p.ContextHash != "" && !contextual {
		return op, model.ErrUnsupportedContent
	}
	if p.XML == "" || op.Kind == model.OperationBlockDelete {
		return op, nil
	}
	version := before.ProjectionVersion
	if version == 2 && HasYAMLFrontMatterXML([]byte(p.XML)) {
		version = 3
	}
	projectionXML := p.XML
	// A replacement list item is serialized without its unchanged list wrapper.
	// Decode it in that parent's context, not as an unsupported standalone <li>.
	if op.Kind == model.OperationBlockReplace {
		for _, old := range before.Blocks {
			if old.RemoteID == op.BlockID && strings.HasSuffix(old.Kind, "/li") {
				roots, parseErr := parseXML([]byte(p.XML))
				if parseErr != nil || len(roots) != 1 || roots[0].tag != "li" {
					return op, model.ErrUnsupportedContent
				}
				parent := strings.TrimSuffix(old.Kind, "/li")
				if parent != "ul" && parent != "ol" {
					return op, model.ErrUnsupportedContent
				}
				projectionXML = "<" + parent + ">" + p.XML + "</" + parent + ">"
				break
			}
		}
	}
	view, err := DecodeWithOptions([]byte(projectionXML), "", Options{ProjectionVersion: version})
	if err != nil || len(view.Blocks) != 1 {
		return op, model.ErrUnsupportedContent
	}
	b := view.Blocks[0]
	if !b.Opaque && !b.Readable && !HasYAMLFrontMatterXML([]byte(p.XML)) {
		return op, nil
	}
	if op.Kind == model.OperationBlockReplace {
		topLevel := false
		for _, old := range before.Blocks {
			if old.RemoteID == op.BlockID {
				topLevel = true
				break
			}
		}
		if !topLevel {
			return op, model.ErrUnsupportedContent
		}
		for _, old := range before.Blocks {
			if old.RemoteID != op.BlockID && old.Hash == b.Hash && !contextual {
				return op, model.ErrUnsupportedContent
			}
		}
	}
	roots, err := parseXML([]byte(p.XML))
	if err != nil || len(roots) != 1 {
		return op, model.ErrUnsupportedContent
	}
	if _, err = xmlMarkdown(roots[0]); err != nil {
		return op, model.ErrUnsupportedContent
	}
	p.AfterHash = b.Hash
	if contextual {
		p.ContextHash = operationContextHash(before, op, p)
	}
	op.Payload, err = json.Marshal(p)
	return op, err
}
