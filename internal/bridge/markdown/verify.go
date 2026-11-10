package markdown

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
)

// VerifyOperation recognizes one observed effect in its unchanged whole-document
// context. It cannot prove causality or authorize replay when the effect is absent.
func VerifyOperation(before, after model.DocView, op model.Operation) error {
	unknown := model.ErrUnknownResult
	var p model.OperationPayload
	if json.Unmarshal(op.Payload, &p) != nil {
		return unknown
	}
	index := -1
	for i, b := range before.Blocks {
		if b.RemoteID == op.BlockID {
			if index != -1 {
				return unknown
			}
			index = i
		}
	}
	position := index
	switch op.Kind {
	case model.OperationBlockReplace, model.OperationBlockDelete:
		if index < 0 || p.BeforeHash == "" || before.Blocks[index].Hash != p.BeforeHash || (!p.ExplicitResolution && countHash(before.Blocks, p.BeforeHash) != 1) {
			return unknown
		}
	case model.OperationBlockInsertAfter:
		position = 0
		if op.AfterBlockID != "0" {
			position = -1
			for i, b := range before.Blocks {
				if b.RemoteID == op.AfterBlockID {
					position = i + 1
				}
			}
			if position < 0 {
				return unknown
			}
		}
	default:
		return unknown
	}
	delta := 0
	if op.Kind == model.OperationBlockDelete {
		delta = -1
	}
	if op.Kind == model.OperationBlockInsertAfter {
		delta = 1
	}
	if len(after.Blocks) != len(before.Blocks)+delta {
		return unknown
	}
	oldID, newID := "", ""
	if op.Kind != model.OperationBlockInsertAfter {
		oldID = before.Blocks[index].RemoteID
	}
	if op.Kind != model.OperationBlockDelete {
		if p.AfterHash == "" || position >= len(after.Blocks) || after.Blocks[position].Hash != p.AfterHash || (!p.ExplicitResolution && countHash(after.Blocks, p.AfterHash) != 1) {
			return unknown
		}
		newID = after.Blocks[position].RemoteID
		if newID == "" {
			return unknown
		}
	}
	// Raw slices of every non-target body atom must remain exactly intact.
	for i, j := 0, 0; i < len(before.Blocks) || j < len(after.Blocks); {
		if oldID != "" && i == index {
			i++
			continue
		}
		if newID != "" && j == position {
			j++
			continue
		}
		if i >= len(before.Blocks) || j >= len(after.Blocks) || before.Blocks[i].RemoteID != after.Blocks[j].RemoteID || !bytes.Equal(before.Blocks[i].RawXML, after.Blocks[j].RawXML) {
			return unknown
		}
		i++
		j++
	}
	a, err := withoutNode(before.XML, oldID)
	if err != nil {
		return unknown
	}
	b, err := withoutNode(after.XML, newID)
	if err != nil || a != b {
		return unknown
	}
	return nil
}
func countHash(blocks []model.Block, hash string) int {
	n := 0
	for _, b := range blocks {
		if b.Hash == hash {
			n++
		}
	}
	return n
}
func withoutNode(xml []byte, id string) (string, error) {
	nodes, err := parseXML(xml)
	if err != nil {
		return "", err
	}
	count := 0
	var walk func([]*xnode) []*xnode
	walk = func(nodes []*xnode) []*xnode {
		out := make([]*xnode, 0, len(nodes))
		for _, n := range nodes {
			if id != "" && n.attr("id") == id {
				count++
				continue
			}
			if n.tag != "" {
				n.kids = walk(n.kids)
			}
			out = append(out, n)
		}
		return out
	}
	nodes = walk(nodes)
	if id != "" && count != 1 {
		return "", model.ErrUnknownResult
	}
	var out strings.Builder
	for _, n := range nodes {
		out.WriteString(serialize(n))
	}
	return out.String(), nil
}
