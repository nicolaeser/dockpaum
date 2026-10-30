package banner

import (
	"fmt"
	"io"
	"os"
	"strings"
)

var font = map[rune][6]string{
	'A': {" █████╗ ", "██╔══██╗", "███████║", "██╔══██║", "██║  ██║", "╚═╝  ╚═╝"},
	'B': {"██████╗ ", "██╔══██╗", "██████╔╝", "██╔══██╗", "██████╔╝", "╚═════╝ "},
	'C': {" ██████╗", "██╔════╝", "██║     ", "██║     ", "╚██████╗", " ╚═════╝"},
	'D': {"██████╗ ", "██╔══██╗", "██║  ██║", "██║  ██║", "██████╔╝", "╚═════╝ "},
	'E': {"███████╗", "██╔════╝", "█████╗  ", "██╔══╝  ", "███████╗", "╚══════╝"},
	'F': {"███████╗", "██╔════╝", "█████╗  ", "██╔══╝  ", "██║     ", "╚═╝     "},
	'G': {" ██████╗ ", "██╔════╝ ", "██║  ███╗", "██║   ██║", "╚██████╔╝", " ╚═════╝ "},
	'H': {"██╗  ██╗", "██║  ██║", "███████║", "██╔══██║", "██║  ██║", "╚═╝  ╚═╝"},
	'I': {"██╗", "██║", "██║", "██║", "██║", "╚═╝"},
	'J': {"     ██╗", "     ██║", "     ██║", "██   ██║", "╚█████╔╝", " ╚════╝ "},
	'K': {"██╗  ██╗", "██║ ██╔╝", "█████╔╝ ", "██╔═██╗ ", "██║  ██╗", "╚═╝  ╚═╝"},
	'L': {"██╗     ", "██║     ", "██║     ", "██║     ", "███████╗", "╚══════╝"},
	'M': {"███╗   ███╗", "████╗ ████║", "██╔████╔██║", "██║╚██╔╝██║", "██║ ╚═╝ ██║", "╚═╝     ╚═╝"},
	'N': {"███╗   ██╗", "████╗  ██║", "██╔██╗ ██║", "██║╚██╗██║", "██║ ╚████║", "╚═╝  ╚═══╝"},
	'O': {" ██████╗ ", "██╔═══██╗", "██║   ██║", "██║   ██║", "╚██████╔╝", " ╚═════╝ "},
	'P': {"██████╗ ", "██╔══██╗", "██████╔╝", "██╔═══╝ ", "██║     ", "╚═╝     "},
	'Q': {" ██████╗ ", "██╔═══██╗", "██║   ██║", "██║▄▄ ██║", "╚██████╔╝", " ╚══▀▀═╝ "},
	'R': {"██████╗ ", "██╔══██╗", "██████╔╝", "██╔══██╗", "██║  ██║", "╚═╝  ╚═╝"},
	'S': {"███████╗", "██╔════╝", "███████╗", "╚════██║", "███████║", "╚══════╝"},
	'T': {"████████╗", "╚══██╔══╝", "   ██║   ", "   ██║   ", "   ██║   ", "   ╚═╝   "},
	'U': {"██╗   ██╗", "██║   ██║", "██║   ██║", "██║   ██║", "╚██████╔╝", " ╚═════╝ "},
	'V': {"██╗   ██╗", "██║   ██║", "██║   ██║", "╚██╗ ██╔╝", " ╚████╔╝ ", "  ╚═══╝  "},
	'W': {"██╗    ██╗", "██║    ██║", "██║ █╗ ██║", "██║███╗██║", "╚███╔███╔╝", " ╚══╝╚══╝ "},
	'X': {"██╗  ██╗", "╚██╗██╔╝", " ╚███╔╝ ", " ██╔██╗ ", "██╔╝ ██╗", "╚═╝  ╚═╝"},
	'Y': {"██╗   ██╗", "╚██╗ ██╔╝", " ╚████╔╝ ", "  ╚██╔╝  ", "   ██║   ", "   ╚═╝   "},
	'Z': {"███████╗", "╚══███╔╝", "  ███╔╝ ", " ███╔╝  ", "███████╗", "╚══════╝"},
	'0': {" ██████╗ ", "██╔═████╗", "██║██╔██║", "████╔╝██║", "╚██████╔╝", " ╚═════╝ "},
	'1': {" ██╗", "███║", "╚██║", " ██║", " ██║", " ╚═╝"},
	'2': {"██████╗ ", "╚════██╗", " █████╔╝", "██╔═══╝ ", "███████╗", "╚══════╝"},
	'3': {"██████╗ ", "╚════██╗", " █████╔╝", " ╚═══██╗", "██████╔╝", "╚═════╝ "},
	'4': {"██╗  ██╗", "██║  ██║", "███████║", "╚════██║", "     ██║", "     ╚═╝"},
	'5': {"███████╗", "██╔════╝", "███████╗", "╚════██║", "███████║", "╚══════╝"},
	'6': {" ██████╗ ", "██╔════╝ ", "███████╗ ", "██╔═══██╗", "╚██████╔╝", " ╚═════╝ "},
	'7': {"███████╗", "╚════██║", "    ██╔╝", "   ██╔╝ ", "   ██║  ", "   ╚═╝  "},
	'8': {" █████╗ ", "██╔══██╗", "╚█████╔╝", "██╔══██╗", "╚█████╔╝", " ╚════╝ "},
	'9': {" █████╗ ", "██╔══██╗", "╚██████║", " ╚═══██║", " █████╔╝", " ╚════╝ "},
	'-': {"      ", "      ", "█████╗", "╚════╝", "      ", "      "},
	'.': {"   ", "   ", "   ", "   ", "██╗", "╚═╝"},
	' ': {"   ", "   ", "   ", "   ", "   ", "   "},
}

var Brand = [][3]int{{250, 204, 21}, {34, 211, 238}, {37, 99, 235}}

var PinkRed = [][3]int{{244, 114, 182}, {220, 38, 38}}

const (
	reset = "\x1b[0m"
	dim   = "\x1b[38;2;120;120;130m"
	bold  = "\x1b[1m"
)

type Options struct {
	Big     string
	Big2    string
	Product string
	Tagline string
	Version string
	Commit  string
	Date    string
	Extra   string
	Stops   [][3]int
	Color   bool
}

func Print(w io.Writer, o Options) {
	stops := o.Stops
	if stops == nil {
		stops = Brand
	}

	art := renderWord(o.Big)
	width := runeLen(art[0])

	var art2 [6]string
	has2 := o.Big2 != ""
	if has2 {
		art2 = renderWord(o.Big2)
		if w2 := runeLen(art2[0]); w2 > width {
			width = w2
		}
	}

	var b strings.Builder
	b.WriteByte('\n')

	for _, line := range art {
		b.WriteString(paintColumns(line, 0, width, stops, o.Color) + "\n")
	}
	if has2 {
		for _, line := range art2 {
			b.WriteString(paintColumns(line, 0, width, stops, o.Color) + "\n")
		}
	}

	if o.Product != "" {
		rule := productRule(o.Product, width)
		b.WriteString(paintColumns(rule, 0, width, stops, o.Color) + "\n")
	}

	if meta := buildMeta(o); meta != "" {
		b.WriteString(centerDim(meta, width, o.Color) + "\n")
	}
	if o.Tagline != "" {
		b.WriteString(centerDim(o.Tagline, width, o.Color) + "\n")
	}

	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
}

func renderWord(text string) [6]string {
	var rows [6]string
	for _, ch := range strings.ToUpper(text) {
		g, ok := font[ch]
		if !ok {
			g = font[' ']
		}
		for i := 0; i < 6; i++ {
			rows[i] += g[i]
		}
	}
	if rows[0] == "" {
		return font[' ']
	}
	return rows
}

func productRule(product string, width int) string {
	spaced := strings.Join(strings.Split(strings.ToUpper(product), ""), " ")
	inner := "  " + spaced + "  "
	if runeLen(inner) >= width {
		return inner
	}
	side := (width - runeLen(inner)) / 2
	return strings.Repeat("─", side) + inner + strings.Repeat("─", width-side-runeLen(inner))
}

func paintColumns(s string, startCol, total int, stops [][3]int, color bool) string {
	if !color {
		return s
	}
	if total < 2 {
		total = 2
	}
	var b strings.Builder
	b.WriteString(bold)
	last := ""
	for i, r := range []rune(s) {
		t := float64(startCol+i) / float64(total-1)
		c := escFor(interp(stops, t))
		if c != last {
			b.WriteString(c)
			last = c
		}
		b.WriteRune(r)
	}
	b.WriteString(reset)
	return b.String()
}

func centerDim(s string, width int, color bool) string {
	pad := 0
	if runeLen(s) < width {
		pad = (width - runeLen(s)) / 2
	}
	out := strings.Repeat(" ", pad) + s
	if color {
		return dim + out + reset
	}
	return out
}

func buildMeta(o Options) string {
	parts := make([]string, 0, 4)
	if v := o.Version; v != "" {
		if v[0] >= '0' && v[0] <= '9' {
			v = "v" + v
		}
		parts = append(parts, v)
	}
	if o.Commit != "" && o.Commit != "unknown" && o.Commit != "none" {
		c := o.Commit
		if len(c) > 7 {
			c = c[:7]
		}
		parts = append(parts, c)
	}
	if o.Date != "" && o.Date != "unknown" {
		parts = append(parts, o.Date)
	}
	if o.Extra != "" {
		parts = append(parts, o.Extra)
	}
	return strings.Join(parts, "  ·  ")
}

func interp(stops [][3]int, t float64) [3]int {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	if len(stops) == 1 {
		return stops[0]
	}
	seg := len(stops) - 1
	pos := t * float64(seg)
	i := int(pos)
	if i >= seg {
		i = seg - 1
	}
	f := pos - float64(i)
	a, c := stops[i], stops[i+1]
	return [3]int{
		a[0] + int(float64(c[0]-a[0])*f),
		a[1] + int(float64(c[1]-a[1])*f),
		a[2] + int(float64(c[2]-a[2])*f),
	}
}

func escFor(c [3]int) string {
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c[0], c[1], c[2])
}

func runeLen(s string) int { return len([]rune(s)) }

func ColorEnabled() bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

func ColorFor(w io.Writer) bool { return ColorEnabled() && IsTerminal(w) }

func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
