package markdown

import (
	"bytes"
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
)

const maxDiagnosticLocations = 8

// repeatedLocations supplies related evidence, not a claim that repeated text
// is invalid. All offsets refer to the captured projection or merged intent.
func repeatedLocations(body []byte, remote *model.DocView, side string) ([]model.SyncDiagnosticLocation, int) {
	atoms, err := parseMarkdown(body, remote)
	if err != nil && remote != nil {
		atoms, err = projectedAtoms(body)
	}
	if err != nil {
		return nil, 0
	}
	// Preserve captured rich/opaque identity even when the source is merged text.
	// Display placeholders are not ordinary repeated user paragraphs.
	protected := map[string]bool{}
	if remote != nil {
		for _, block := range remote.Blocks {
			if (!block.Opaque && !block.Readable) || block.Start < 0 || block.End < block.Start || block.End > len(remote.Markdown) {
				continue
			}
			protected[string(bytes.TrimSpace(remote.Markdown[block.Start:block.End]))] = true
		}
	}
	for i, a := range atoms {
		if a.Start >= 0 && a.End >= a.Start && a.End <= len(body) && protected[string(bytes.TrimSpace(body[a.Start:a.End]))] {
			atoms[i].Opaque = true
		}
	}
	counts := map[string]int{}
	for _, a := range atoms {
		if !a.Opaque && !a.Readable {
			counts[a.Hash]++
		}
	}
	var locations []model.SyncDiagnosticLocation
	total := 0
	section := ""
	for _, a := range atoms {
		if a.Start < 0 || a.End < a.Start || a.End > len(body) {
			continue
		}
		if len(a.Kind) == 2 && a.Kind[0] == 'h' && a.Kind[1] >= '1' && a.Kind[1] <= '6' {
			section = diagnosticText(strings.TrimSpace(strings.TrimLeft(string(body[a.Start:a.End]), "#")), 80)
		}
		if a.Opaque || a.Readable || counts[a.Hash] < 2 {
			continue
		}
		end := a.End
		for end > a.Start && (body[end-1] == '\n' || body[end-1] == '\r') {
			end--
		}
		if end <= a.Start {
			continue
		}
		total++
		if len(locations) >= maxDiagnosticLocations {
			continue
		}
		text := string(body[a.Start:end])
		lines := strings.Split(text, "\n")
		if len(lines) > 4 {
			text = strings.Join(lines[:4], "\n") + "…"
		}
		locations = append(locations, model.SyncDiagnosticLocation{Kind: a.Kind, Side: side,
			Line: 1 + bytes.Count(body[:a.Start], []byte{'\n'}), EndLine: 1 + bytes.Count(body[:end], []byte{'\n'}),
			Text: diagnosticText(text, 240), Section: section})
	}
	return locations, total
}

func diagnosticText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}
