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
	if index < 0 && before.ProjectionVersion == 2 && after.ProjectionVersion == 2 && op.Kind == model.OperationBlockReplace {
		return verifyNestedParagraph(before, after, op, p)
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
		if p.AfterHash == "" || position >= len(after.Blocks) || after.Blocks[position].Hash != p.AfterHash || (!p.ExplicitResolution && op.Kind != model.OperationBlockInsertAfter && countHash(after.Blocks, p.AfterHash) != 1) {
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
			// Empty synthetic list wrappers have no block identity. The remote
			// serializer omits them after deleting the only/last list item.
			if (n.tag == "ul" || n.tag == "ol") && n.allowed() && len(n.kids) == 0 {
				continue
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

// Nested edits are verified at the same structural location, with the complete
// outside XML unchanged. A server-generated new ID is accepted only at that node.
func verifyNestedParagraph(before, after model.DocView, op model.Operation, p model.OperationPayload) error {
	unknown := model.ErrUnknownResult
	a, e := parseXML(before.XML)
	if e != nil {
		return unknown
	}
	b, e := parseXML(after.XML)
	if e != nil {
		return unknown
	}
	var old, new *xnode
	matches := 0
	var walk func([]*xnode, []*xnode, bool)
	walk = func(aa, bb []*xnode, inTable bool) {
		if len(aa) != len(bb) {
			return
		}
		for i, n := range aa {
			m := bb[i]
			inside := inTable || n.tag == "table"
			if n.attr("id") == op.BlockID {
				matches++
				if inTable {
					old = n
					new = m
				}
			}
			if n.tag == m.tag {
				walk(n.kids, m.kids, inside)
			}
		}
	}
	walk(a, b, false)
	if matches != 1 || old == nil || new == nil || old.tag != "p" || new.tag != "p" || len(elements(old)) != 0 || len(elements(new)) != 0 || !old.allowed("align") || !new.allowed("align") || new.attr("id") == "" || p.BeforeHash == "" || p.AfterHash == "" || hashNode("p", old) != p.BeforeHash || hashNode("p", new) != p.AfterHash {
		return unknown
	}
	if !p.ExplicitResolution {
		for _, roots := range [][]*xnode{a, b} {
			count := 0
			var countWalk func(*xnode)
			countWalk = func(n *xnode) {
				if n.tag == "p" && (hashNode("p", n) == p.BeforeHash || hashNode("p", n) == p.AfterHash) {
					count++
				}
				for _, c := range n.kids {
					countWalk(c)
				}
			}
			for _, n := range roots {
				countWalk(n)
			}
			if count != 1 {
				return unknown
			}
		}
	}
	outsideA := append(append([]byte(nil), before.XML[:old.start]...), before.XML[old.end:]...)
	outsideB := append(append([]byte(nil), after.XML[:new.start]...), after.XML[new.end:]...)
	if !bytes.Equal(outsideA, outsideB) {
		return unknown
	}
	return nil
}
