package tray

import (
	"bytes"
	"embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strconv"
	"strings"
)

//go:embed assets/*.png assets/*.ico
var icons embed.FS

func badgeTitle(v View) string {
	if !v.ConflictsKnown {
		return "?"
	}
	if v.ConflictCount <= 0 {
		return ""
	}
	if v.ConflictCount > 99 {
		return "99+"
	}
	return strconv.Itoa(v.ConflictCount)
}

func trayTooltip(v View) string {
	// Keep the exact total first: some native tooltip surfaces truncate text.
	parts := []string{"DriveBridge", conflictLabel(v), stateLabel(v.State)}
	if text := strings.TrimSpace(v.Text); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, " · ")
}

func iconData(v View, platform string) []byte {
	state := v.State
	switch state {
	case "ready", "syncing", "conflict", "error", "stopped":
	case "auth":
		state = "error"
	default:
		state = "unavailable"
	}
	if !v.ConflictsKnown && state == "ready" {
		state = "unavailable"
	}
	data, _ := icons.ReadFile("assets/" + state + ".png")
	if platform != "darwin" && v.ConflictsKnown && v.ConflictCount > 0 {
		data = overlayBadge(data, badgeTitle(v))
	}
	if platform == "windows" {
		// A single PNG image in an ICO directory is supported by modern Win32.
		ico := make([]byte, 22+len(data))
		binary.LittleEndian.PutUint16(ico[2:4], 1)
		binary.LittleEndian.PutUint16(ico[4:6], 1)
		ico[6], ico[7] = 32, 32
		binary.LittleEndian.PutUint16(ico[10:12], 1)
		binary.LittleEndian.PutUint16(ico[12:14], 32)
		binary.LittleEndian.PutUint32(ico[14:18], uint32(len(data)))
		binary.LittleEndian.PutUint32(ico[18:22], 22)
		copy(ico[22:], data)
		return ico
	}
	return data
}

// These small bitmap digits keep badges local and readable without font files.
var badgeGlyphs = map[rune][5]string{
	'0': {"111", "101", "101", "101", "111"},
	'1': {"010", "110", "010", "010", "111"},
	'2': {"111", "001", "111", "100", "111"},
	'3': {"111", "001", "111", "001", "111"},
	'4': {"101", "101", "111", "001", "001"},
	'5': {"111", "100", "111", "001", "111"},
	'6': {"111", "100", "111", "101", "111"},
	'7': {"111", "001", "010", "010", "010"},
	'8': {"111", "101", "111", "101", "111"},
	'9': {"111", "101", "111", "001", "111"},
	'+': {"000", "010", "111", "010", "000"},
}

func overlayBadge(data []byte, text string) []byte {
	base, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	img := image.NewRGBA(base.Bounds())
	draw.Draw(img, img.Bounds(), base, base.Bounds().Min, draw.Src)
	width := len(text)*8 + 2
	x0 := 32 - width
	draw.Draw(img, image.Rect(x0, 19, 32, 32), image.NewUniform(color.NRGBA{R: 206, G: 41, B: 54, A: 255}), image.Point{}, draw.Src)
	for i, digit := range text {
		for y, row := range badgeGlyphs[digit] {
			for x, pixel := range row {
				if pixel == '1' {
					left, top := x0+2+i*8+x*2, 21+y*2
					draw.Draw(img, image.Rect(left, top, left+2, top+2), image.White, image.Point{}, draw.Src)
				}
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return data
	}
	return out.Bytes()
}
