package tray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"strings"
	"testing"
)

func TestConflictPresentationKeepsExactCountsAndUnknownStatus(t *testing.T) {
	for _, tc := range []struct {
		count          int
		known          bool
		title, tooltip string
	}{
		{0, false, "?", "状态待确认"},
		{0, true, "", "无需合并（0）"},
		{7, true, "7", "需要合并（7）"},
		{125, true, "99+", "需要合并（125）"},
	} {
		v := View{State: "ready", ConflictCount: tc.count, ConflictsKnown: tc.known}
		if got := badgeTitle(v); got != tc.title {
			t.Errorf("title=%q, want %q", got, tc.title)
		}
		if got := trayTooltip(v); !strings.Contains(got, tc.tooltip) {
			t.Errorf("tooltip %q lacks %q", got, tc.tooltip)
		}
	}
}

func TestBadgedIconsAreValidPNGAndICOWithVisibleCount(t *testing.T) {
	base := iconData(View{State: "ready", ConflictsKnown: true}, "linux")
	for _, count := range []int{1, 12, 99, 125} {
		v := View{State: "ready", ConflictCount: count, ConflictsKnown: true}
		data := iconData(v, "linux")
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("count=%d PNG: %v", count, err)
		}
		if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 32 {
			t.Fatalf("bad PNG dimensions: %v", img.Bounds())
		}
		if bytes.Equal(base, data) {
			t.Fatalf("count=%d badge absent", count)
		}
		// A count badge must actually contain light glyphs on its red background.
		light, red := 0, 0
		for y := 20; y < 32; y++ {
			for x := 0; x < 32; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				if a > 60000 && r > 60000 && g > 60000 && b > 60000 {
					light++
				}
				if a > 60000 && r > 45000 && g < 25000 && b < 25000 {
					red++
				}
			}
		}
		if light < 10 || red < 10 {
			t.Fatalf("count=%d unreadable badge: glyph=%d background=%d", count, light, red)
		}
		ico := iconData(v, "windows")
		if len(ico) < 22 || binary.LittleEndian.Uint16(ico[2:4]) != 1 || binary.LittleEndian.Uint16(ico[4:6]) != 1 || ico[6] != 32 || ico[7] != 32 {
			t.Fatal("invalid ICO directory")
		}
		size, offset := int(binary.LittleEndian.Uint32(ico[14:18])), int(binary.LittleEndian.Uint32(ico[18:22]))
		if offset != 22 || size != len(ico)-offset {
			t.Fatal("invalid ICO payload bounds")
		}
		if !bytes.Equal(ico[offset:], data) {
			t.Fatal("Windows ICO lost count badge")
		}
	}
}

func TestUnknownConflictsCannotUseReadyIcon(t *testing.T) {
	unknown := iconData(View{State: "ready"}, "linux")
	ready := iconData(View{State: "ready", ConflictsKnown: true}, "linux")
	if bytes.Equal(unknown, ready) {
		t.Fatal("unknown conflicts shown as ready")
	}
	if _, err := png.Decode(bytes.NewReader(unknown)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(iconData(View{State: "ready", ConflictCount: 100, ConflictsKnown: true}, "linux"), iconData(View{State: "ready", ConflictCount: 1000, ConflictsKnown: true}, "linux")) {
		t.Fatal("large badge must cap at 99+")
	}
}

func TestTooltipKeepsExactCountBeforeLongStatusText(t *testing.T) {
	tooltip := trayTooltip(View{State: "error", Text: strings.Repeat("详细状态", 100), ConflictCount: 12345, ConflictsKnown: true})
	if prefix := string([]rune(tooltip)[:60]); !strings.Contains(prefix, "需要合并（12345）") {
		t.Fatal("native tooltip truncation can hide the exact conflict count")
	}
}
