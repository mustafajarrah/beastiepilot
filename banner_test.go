package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWaitStatus(t *testing.T) {
	tests := []struct {
		tick   int
		waited time.Duration
		want   string
	}{
		{0, 0, "Asking llama3.2 ."},
		{3, 400 * time.Millisecond, "Asking llama3.2 .."},
		{6, 4999 * time.Millisecond, "Asking llama3.2 ..."},
		{9, 5 * time.Second, "Asking llama3.2 .   5s"},
		{12, 59900 * time.Millisecond, "Asking llama3.2 ..  59s"},
		{15, 72 * time.Second, "Asking llama3.2 ... 1m12s"},
	}
	for _, tt := range tests {
		if got := waitStatus("Asking llama3.2", tt.tick, tt.waited); got != tt.want {
			t.Errorf("waitStatus(tick %d, %s) = %q, want %q", tt.tick, tt.waited, got, tt.want)
		}
	}
}

// Without the sprite, only the status line is drawn, and nothing above it is
// touched.
func TestStatusLineAlone(t *testing.T) {
	var out bytes.Buffer
	a := startAnimation(styler{out: &out}, "Asking llama3.2", false)
	time.Sleep(4 * frameInterval)
	a.stop()
	drawn := out.String()
	if !strings.HasPrefix(drawn, bannerIndent+"Asking llama3.2 .") || !strings.Contains(drawn, "\r\033[K"+bannerIndent+"Asking llama3.2 ..") {
		t.Errorf("the status line was drawn as %q", drawn)
	}
	if strings.Contains(drawn, "\0337") {
		t.Errorf("something above the status line was redrawn: %q", drawn)
	}
}

func TestSpriteGeometry(t *testing.T) {
	if len(beastieSprite)%2 != 0 {
		t.Fatalf("the sprite has %d pixel rows; two share each terminal line, so it must be even", len(beastieSprite))
	}
	for y, row := range beastieSprite {
		if len(row) != spriteWidth {
			t.Errorf("sprite row %d is %d pixels wide, want %d", y, len(row), spriteWidth)
		}
		for x := 0; x < len(row); x++ {
			if _, ok := spriteColors[row[x]]; !ok && row[x] != '.' {
				t.Errorf("sprite row %d column %d: unknown color %q", y, x, row[x])
			}
		}
	}
}

func TestSpritePatchesFit(t *testing.T) {
	for name, patches := range map[string][]patch{"blink": blink, "tailWag": tailWag, "glintMid": glintMid, "glintFar": glintFar} {
		for _, p := range patches {
			if p.row < 0 || p.row >= len(beastieSprite) || p.col < 0 || p.col+len(p.pixels) > spriteWidth {
				t.Errorf("%s: patch %+v falls outside the sprite", name, p)
			}
			for i := 0; i < len(p.pixels); i++ {
				if _, ok := spriteColors[p.pixels[i]]; !ok && p.pixels[i] != '.' {
					t.Errorf("%s: patch %+v uses unknown color %q", name, p, p.pixels[i])
				}
			}
		}
	}
}

func TestSpriteFrames(t *testing.T) {
	if got := strings.Join(spriteFrame(0), "\n"); got != strings.Join(beastieSprite, "\n") {
		t.Error("tick 0 should be the sprite at rest")
	}
	distinct := map[string]bool{}
	for tick := 0; tick < 400; tick++ {
		frame := spriteFrame(tick)
		if len(frame) != len(beastieSprite) {
			t.Fatalf("tick %d: %d rows, want %d", tick, len(frame), len(beastieSprite))
		}
		for y, row := range frame {
			if len(row) != spriteWidth {
				t.Fatalf("tick %d row %d: %d pixels wide, want %d", tick, y, len(row), spriteWidth)
			}
		}
		distinct[strings.Join(frame, "\n")] = true
	}
	if len(distinct) < 4 {
		t.Errorf("only %d distinct frames; expected the tail, eyes and goggles to move", len(distinct))
	}
	if strings.Join(beastieSprite, "") != strings.Join(spriteFrame(0), "") {
		t.Error("spriteFrame modified the base sprite")
	}
}

func TestRenderSprite(t *testing.T) {
	red, navy := spriteColors['r'].index, spriteColors['n'].index
	fg := func(n int) string { return fmt.Sprintf("\033[38;5;%dm", n) }
	bg := func(n int) string { return fmt.Sprintf("\033[48;5;%dm", n) }
	const reset = "\033[0m"

	tests := []struct {
		name   string
		pixels []string
		want   string
	}{
		{"transparent", []string{"..", ".."}, "  "},
		{"top only", []string{"r", "."}, fg(red) + "▀" + reset},
		{"bottom only", []string{".", "r"}, fg(red) + "▄" + reset},
		{"both, two colors", []string{"r", "n"}, fg(red) + bg(navy) + "▀" + reset},
		{"both, same color", []string{"r", "r"}, fg(red) + bg(red) + "▀" + reset},
		{"color is not repeated", []string{"rr", ".."}, fg(red) + "▀▀" + reset},
		{"background is cleared before a gap", []string{"r.", "n."}, fg(red) + bg(navy) + "▀" + reset + " "},
		{"foreground survives a gap", []string{"r.r", "..."}, fg(red) + "▀ ▀" + reset},
	}
	for _, tt := range tests {
		got := renderSprite(tt.pixels, false)
		if len(got) != 1 || got[0] != tt.want {
			t.Errorf("%s: renderSprite(%q) = %q, want %q", tt.name, tt.pixels, got, tt.want)
		}
	}

	if got := renderSprite([]string{"r", "."}, true); got[0] != "\033[38;2;208;53;43m▀"+reset {
		t.Errorf("truecolor rendering = %q", got[0])
	}
}

func TestSpriteLinesAreExactlyAsWideAsTheSprite(t *testing.T) {
	// The animation overwrites lines in place; a line of any other width
	// would smear or leave stale pixels behind.
	for tick := 0; tick < 80; tick++ {
		for _, truecolor := range []bool{false, true} {
			lines := renderSprite(spriteFrame(tick), truecolor)
			if len(lines) != len(beastieSprite)/2 {
				t.Fatalf("tick %d: %d lines, want %d", tick, len(lines), len(beastieSprite)/2)
			}
			for i, line := range lines {
				if n := visibleWidth(line); n != spriteWidth {
					t.Fatalf("tick %d line %d is %d columns wide, want %d", tick, i, n, spriteWidth)
				}
			}
		}
	}
}

func TestRedraw(t *testing.T) {
	shown := []string{"a", "b", "c", "d"}
	if got := redraw(shown, shown); got != "" {
		t.Errorf("redrawing an unchanged sprite wrote %q", got)
	}
	// Line 1 of 4 is three sprite lines plus the blank line above the cursor.
	want := "\0337\033[4A\r" + bannerIndent + "B" + "\0338"
	if got := redraw(shown, []string{"a", "B", "c", "d"}); got != want {
		t.Errorf("redraw = %q, want %q", got, want)
	}
	// The last line is two rows up: itself and the blank line.
	if got := redraw(shown, []string{"a", "b", "c", "D"}); !strings.Contains(got, "\033[2A") {
		t.Errorf("redraw of the last line = %q, want it to move up 2 rows", got)
	}
}

func TestVisibleWidth(t *testing.T) {
	for in, want := range map[string]int{
		"":                               0,
		"plain":                          5,
		"\033[1;31mred\033[0m":           3,
		"\033[38;5;160m▀▄\033[0m ":       3,
		"llama3.2 · localhost:11434":     26,
		"\033[1mBeastiePilot\033[0m dev": 16,
	} {
		if got := visibleWidth(in); got != want {
			t.Errorf("visibleWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestChooseBannerWithoutATerminal(t *testing.T) {
	// With no terminal to measure, the sprite must never be chosen: its
	// animation depends on knowing that everything fits on screen.
	t.Setenv("LC_ALL", "en_US.UTF-8")
	ui := styler{out: &strings.Builder{}, tty: true, color: true}
	if art := chooseBanner(ui, defaultSettings(), 30); art.animated || len(art.lines) != len(beastieBlocks.rows) {
		t.Errorf("chose a banner of %d lines (animated=%v), want the compact mascot", len(art.lines), art.animated)
	}
	t.Setenv("LC_ALL", "C")
	if art := chooseBanner(ui, defaultSettings(), 30); len(art.lines) != len(beastieASCII.rows) {
		t.Errorf("chose a banner of %d lines in the C locale, want the ASCII mascot", len(art.lines))
	}
}

// The picture of Beastie that the README shows, and how many pixels of the
// page one pixel of the sprite takes there.
const (
	picturePath  = "docs/beastie.svg"
	pictureScale = 8
)

// spritePicture draws that picture: the sprite as the banner animates it, as
// an SVG image. What never changes is drawn once. A pixel that comes and goes
// is drawn in a layer with the others that are there at the same ticks, and a
// style sheet shows and hides each layer as the ticks go by.
func spritePicture() string {
	// The tail goes there and back in 8 ticks, a blink comes every 28 and a
	// glint every 40, so every 280 it all starts again.
	const round = 280
	frames := make([][]string, round)
	for tick := range frames {
		frames[tick] = spriteFrame(tick)
	}

	// Where each letter is: for good, or at the ticks marked in when.
	type layer struct {
		when   string
		pixels [][]byte
	}
	blank := func() [][]byte {
		rows := make([][]byte, len(beastieSprite))
		for y := range rows {
			rows[y] = bytes.Repeat([]byte{'.'}, len(beastieSprite[y]))
		}
		return rows
	}
	still, layers := blank(), []*layer{}
	for y, row := range beastieSprite {
		for x := range row {
			seen := ""
			for tick := range frames {
				if c := frames[tick][y][x]; c != '.' && !strings.Contains(seen, string(c)) {
					seen += string(c)
				}
			}
			for _, c := range []byte(seen) {
				when := make([]byte, round)
				for tick := range frames {
					when[tick] = '0'
					if frames[tick][y][x] == c {
						when[tick] = '1'
					}
				}
				if !strings.Contains(string(when), "0") {
					still[y][x] = c
					continue
				}
				var in *layer
				for _, l := range layers {
					if l.when == string(when) {
						in = l
					}
				}
				if in == nil {
					in = &layer{string(when), blank()}
					layers = append(layers, in)
				}
				in.pixels[y][x] = c
			}
		}
	}

	// paths draws pixels, each color as one path made of the runs of it.
	letters := "bdgkmnprswy"
	paths := func(pixels [][]byte) string {
		var b strings.Builder
		for _, c := range []byte(letters) {
			var d strings.Builder
			for y, row := range pixels {
				for x := 0; x < len(row); x++ {
					if row[x] != c {
						continue
					}
					n := 1
					for x+n < len(row) && row[x+n] == c {
						n++
					}
					fmt.Fprintf(&d, "M%d %dh%dv1h-%dz", x, y, n, n)
					x += n - 1
				}
			}
			if d.Len() > 0 {
				color := spriteColors[c]
				fmt.Fprintf(&b, "<path fill=\"#%02x%02x%02x\" d=\"%s\"/>\n", color.r, color.g, color.b, d.String())
			}
		}
		return b.String()
	}
	number := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

	var style, drawn strings.Builder
	for i, l := range layers {
		// The layer comes round sooner than the whole does.
		period := round
		for p := 1; p < round; p++ {
			if round%p == 0 && strings.Repeat(l.when[:p], round/p) == l.when {
				period = p
				break
			}
		}
		fmt.Fprintf(&style, ".l%d{opacity:%c;animation:l%d %ss step-end infinite}\n@keyframes l%d{", i, l.when[0], i,
			number(float64(period)*frameInterval.Seconds()), i)
		for tick := 0; tick < period; tick++ {
			if tick == 0 || l.when[tick] != l.when[tick-1] {
				fmt.Fprintf(&style, "%s%%{opacity:%c}", number(float64(tick*1000000/period)/10000), l.when[tick])
			}
		}
		style.WriteString("}\n")
		fmt.Fprintf(&drawn, "<g class=\"l%d\">\n%s</g>\n", i, paths(l.pixels))
	}

	const about = "Beastie, the BSD daemon, in a pilot cap and goggles, holding the yoke of an airplane"
	width, height := len(beastieSprite[0]), len(beastieSprite)
	return fmt.Sprintf("<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 %d %d\" width=\"%d\" height=\"%d\" shape-rendering=\"crispEdges\" role=\"img\" aria-label=\"%s\">\n"+
		"<title>%s</title>\n<style>\n%s@media (prefers-reduced-motion:reduce){g{animation:none}}\n</style>\n%s%s</svg>\n",
		width, height, width*pictureScale, height*pictureScale, about, about, style.String(), paths(still), drawn.String())
}

// TestSpritePicture holds the picture in the README to the sprite that the
// banner draws. After changing the sprite, draw the picture again with
//
//	BEASTIEPILOT_DRAW=1 go test -run TestSpritePicture
func TestSpritePicture(t *testing.T) {
	want := spritePicture()
	if os.Getenv("BEASTIEPILOT_DRAW") != "" {
		if err := os.MkdirAll(filepath.Dir(picturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(picturePath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(picturePath)
	if err != nil || string(got) != want {
		t.Errorf("%s is not the sprite as it is now (%v); draw it again with: BEASTIEPILOT_DRAW=1 go test -run TestSpritePicture", picturePath, err)
	}

	// Every color of the sprite has to be one that the picture knows.
	for letter := range spriteColors {
		if !strings.Contains("bdgkmnprswy", string(letter)) {
			t.Errorf("the picture does not draw the color %q", letter)
		}
	}
}
