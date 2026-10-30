package cli

import (
	"io"
	"strings"
	"unicode"
)

const (
	styleReset  = "\x1b[0m"
	styleBold   = "\x1b[1m"
	styleDim    = "\x1b[2m"
	styleRed    = "\x1b[31m"
	styleGreen  = "\x1b[32m"
	styleYellow = "\x1b[33m"
	styleCyan   = "\x1b[36m"
)

func paint(s, style string, color bool) string {
	if !color || style == "" || s == "" {
		return s
	}
	return style + s + styleReset
}

func writeTable(w io.Writer, header []string, rows [][]string, color bool, style func(row, col int) string) error {
	widths := make([]int, len(header))
	for _, r := range append([][]string{header}, rows...) {
		for i, c := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], displayWidth(c))
			}
		}
	}
	var b strings.Builder
	line := func(row int, cells []string) {
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			b.WriteString(paint(c, style(row, i), color))
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(c)+2))
			}
		}
		b.WriteByte('\n')
	}
	line(-1, header)
	for i, r := range rows {
		line(i, r)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		case wideRune(r):
			n += 2
		default:
			n++
		}
	}
	return n
}

func wideRune(r rune) bool {
	switch {
	case r < 0x1100:
		return false
	case r <= 0x115f,
		r >= 0x2e80 && r <= 0x303e,
		r >= 0x3041 && r <= 0x33ff,
		r >= 0x3400 && r <= 0x4dbf,
		r >= 0x4e00 && r <= 0x9fff,
		r >= 0xa000 && r <= 0xa4cf,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f,
		r >= 0x1f680 && r <= 0x1f6ff,
		r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x1fa70 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	return false
}
