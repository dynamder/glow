//go:build repro

// Package repro holds helpers for the reproduction scripts of the terminal
// rendering review. See README.md in this directory for the index of bugs and
// how to run each of them.
//
// The helpers are plain code (not test-only) so the ui package's reproduction
// tests can import them as well.
package repro

import (
	"math"
	"strconv"
	"strings"

	"github.com/rivo/uniseg"
)

// DisplayWidth reports how many terminal columns a rendered line occupies,
// modelling OSC 66 text sizing the way a compliant terminal does.
//
// This is the measurement glow itself never performs: every width helper in the
// codebase gets these sequences wrong. muesli/reflow (ansi.PrintableRuneWidth)
// counts the payload and part of the escape as printable, while lipgloss
// measures the whole sequence - payload included - as zero width. That gap is
// why an oversized heading is neither wrapped nor clipped anywhere.
//
// Rules:
//   - CSI sequences and non-sizing OSC strings contribute 0 columns;
//   - an OSC 66 payload contributes scale*width(payload) columns, where the
//     scale is "s" (integer) combined with "n"/"d" (fraction);
//   - if the sequence carries an explicit "w=" we use it instead, because that
//     is the cell width the terminal is instructed to occupy. The exact
//     semantics of w are one of the open questions in the review, see
//     utils/textsize.go, osc66TightChunks.
func DisplayWidth(line string) int {
	width := 0
	for i := 0; i < len(line); {
		if line[i] != 0x1b {
			j := strings.IndexByte(line[i:], 0x1b)
			if j < 0 {
				j = len(line) - i
			}
			width += uniseg.StringWidth(line[i : i+j])
			i += j
			continue
		}
		if i+1 >= len(line) {
			width += uniseg.StringWidth(line[i:])
			break
		}
		switch line[i+1] {
		case '[':
			// CSI: skip to the final byte.
			k := i + 2
			for k < len(line) && (line[k] < 0x40 || line[k] > 0x7e) {
				k++
			}
			if k < len(line) {
				k++
			}
			i = k
		case ']':
			body, next := scanOSC(line, i+2)
			if strings.HasPrefix(body, "66;") {
				width += osc66Width(body)
			}
			i = next
		default:
			// Two-byte escape (e.g. ESC ( B is three, but the extra byte is a
			// printable charset id and does not reach the screen as a glyph).
			i += 2
		}
	}
	return width
}

// scanOSC reads an OSC string starting right after "\x1b]" and returns its body
// plus the offset just past the terminator (BEL or ST).
func scanOSC(s string, start int) (body string, next int) {
	for i := start; i < len(s); i++ {
		if s[i] == '\a' {
			return s[start:i], i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return s[start:i], i + 2
		}
	}
	return s[start:], len(s)
}

// osc66Width models the columns taken by one "66;<meta>;<payload>" OSC body.
func osc66Width(body string) int {
	rest := strings.TrimPrefix(body, "66;")
	meta := rest
	payload := ""
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		meta = rest[:i]
		payload = rest[i+1:]
	}
	var (
		s, n, d, w float64
	)
	for _, kv := range strings.Split(meta, ":") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		switch k {
		case "s":
			s = f
		case "n":
			n = f
		case "d":
			d = f
		case "w":
			w = f
		}
	}
	if w > 0 {
		return int(w)
	}
	scale := s
	if scale == 0 {
		scale = 1
	}
	if n > 0 && d > 0 {
		scale *= n / d
	}
	return int(math.Ceil(scale * float64(uniseg.StringWidth(payload))))
}

// PhysicalRows reports how many screen rows the given content occupies on a
// terminal that is cols columns wide. The viewport (and therefore the whole
// pager) assumes one row per "\n"-separated line, so any difference here is
// exactly the drift that pushes the layout out of sync.
func PhysicalRows(content string, cols int) int {
	if cols <= 0 {
		return 0
	}
	rows := 0
	lines := strings.Split(content, "\n")
	for _, ln := range lines {
		w := DisplayWidth(ln)
		rows += max(1, (w+cols-1)/cols)
	}
	return rows
}

// ContentRows reports how many rows the viewport will count for the same
// content: one per line.
func ContentRows(content string) int {
	return len(strings.Split(content, "\n"))
}
