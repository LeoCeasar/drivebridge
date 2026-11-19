package markdown

import (
	"bytes"
	"errors"
	"slices"

	"github.com/rclone/rclone/internal/bridge/model"
)

// Rendered content can stay byte-identical while cloud objects exchange places.
// Keep surviving baseline IDs associated with their established context before
// an automatic text-merged intent is planned against a fresh snapshot.
func cloudContextMoved(baseline, current model.DocView) bool {
	if len(baseline.Blocks) == 0 || bytes.Equal(baseline.XML, current.XML) {
		return false
	}
	budget := contextAlignmentBudget
	return blockContextMoved(baseline.Blocks, current.Blocks, &budget)
}
func blockContextMoved(before, after []model.Block, budget *int) bool {
	*budget -= len(before) + len(after) + 1
	if *budget < 0 {
		return true
	}
	oldByID, newByID := map[string]int{}, map[string]int{}
	for i, b := range before {
		if b.RemoteID != "" {
			oldByID[b.RemoteID] = i
		}
	}
	for i, b := range after {
		if b.RemoteID != "" {
			newByID[b.RemoteID] = i
		}
	}
	oldOrder, newOrder := []string{}, []string{}
	for _, b := range before {
		if _, ok := newByID[b.RemoteID]; ok && b.RemoteID != "" {
			oldOrder = append(oldOrder, b.RemoteID)
		}
	}
	for _, b := range after {
		if _, ok := oldByID[b.RemoteID]; ok && b.RemoteID != "" {
			newOrder = append(newOrder, b.RemoteID)
		}
	}
	if !slices.Equal(oldOrder, newOrder) {
		return true
	}
	oldAtoms, newAtoms := make([]atom, len(before)), make([]atom, len(after))
	for i, b := range before {
		oldAtoms[i] = atom{Block: b}
	}
	for i, b := range after {
		newAtoms[i] = atom{Block: b}
	}
	checkedChildren := map[int]bool{}
	mapped, err := alignContextBudgeted(oldAtoms, newAtoms, true, budget)
	if errors.Is(err, errAlignmentBudget) {
		return true
	}
	if err == nil {
		for i, j := range mapped.index {
			if j < 0 {
				continue
			}
			old := before[i]
			now := after[j]
			if _, survives := newByID[old.RemoteID]; survives && old.RemoteID != "" && old.RemoteID != now.RemoteID {
				return true
			}
			if len(old.Children) > 0 || len(now.Children) > 0 {
				checkedChildren[i] = true
				if blockContextMoved(old.Children, now.Children, budget) {
					return true
				}
			}
		}
	}
	// A changed parent may not have a textual mapping; its surviving ID still
	// provides the correct scope for checking child identity order.
	for i, old := range before {
		if j, ok := newByID[old.RemoteID]; ok && !checkedChildren[i] && (len(old.Children) > 0 || len(after[j].Children) > 0) {
			if blockContextMoved(old.Children, after[j].Children, budget) {
				return true
			}
		}
	}
	return false
}
