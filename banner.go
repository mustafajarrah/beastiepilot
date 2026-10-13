package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// The banner comes in three sizes, chosen by chooseBanner:
//
//   - beastieSprite, a pixel-art Beastie that is animated while the model is
//     thinking. It needs a UTF-8 terminal with 256 colors and enough room.
//   - beastieBlocks, a four-line mascot for small terminals or BANNER=compact.
//   - beastieASCII, for terminals without a UTF-8 locale.
//
// All three depict the BSD Daemon, which is copyright Marshall Kirk McKusick.

const (
	bannerIndent = "  " // left margin of every banner line
	bannerGap    = 3    // columns between the mascot and the text beside it
)

// bannerArt is a mascot ready to print: one string per terminal row, each
// occupying exactly width columns.
type bannerArt struct {
	lines    []string
	width    int
	infoRow  int  // first row that has text beside it
	animated bool // the lines are the sprite and can be redrawn in place
}

// mascot is a small banner picture: rows of characters and, for each
// character, a letter choosing its color from mascotColors.
type mascot struct {
	rows    []string
	colors  []string
	infoRow int
}

// mascotColors maps the letters in mascot.colors to SGR codes: Beastie is
// red, his pilot cap and goggles are yellow.
var mascotColors = map[byte]string{'r': boldRed, 'y': boldYellow, 'g': lens}

// beastieBlocks is Beastie in a pilot cap, drawn with quadrant block
// characters so that every character cell holds two "pixels" each way. The
// goggle lenses are the lower halves of the 'g' cells.
var beastieBlocks = mascot{
	rows: []string{
		"█▖ ▗▄▄▖ ▗█",
		"▝█▟████▙█▘",
		" ██▀██▀██ ",
		" ▀██▀▀██▀ ",
	},
	colors: []string{
		"rr yyyy rr",
		"rryyyyyyrr",
		" rygyygyr ",
		" rrrrrrrr ",
	},
}

// beastieASCII is the same pilot for terminals without a UTF-8 locale, such
// as the FreeBSD console in the C locale.
var beastieASCII = mascot{
	rows: []string{
		` ,          ,`,
		`/(   ____   )\`,
		`\ \.'    './ /`,
		` \/ ______ \/`,
		` | (O)==(O) |`,
		` |   o  o   |`,
		`  \  \__/  /`,
		`   '-.__.-'`,
	},
	colors: []string{
		` r          r`,
		`rr   yyyy   rr`,
		`r ryy    yyr r`,
		` rr yyyyyy rr`,
		` r yyyyyyyy r`,
		` r   r  r   r`,
		`  r  rrrr  r`,
		`   rrrrrrrr`,
	},
	infoRow: 2,
}

// render returns row i of the mascot with its colors applied, and the row's
// width in columns.
func (m mascot) render(ui styler, i int) (string, int) {
	cells := []rune(m.rows[i])
	var b strings.Builder
	for j, c := range cells {
		code := ""
		if j < len(m.colors[i]) {
			code = mascotColors[m.colors[i][j]]
		}
		if code == "" {
			b.WriteRune(c)
		} else {
			b.WriteString(ui.paint(code, string(c)))
		}
	}
	return b.String(), len(cells)
}

// art renders the whole mascot, padding every row to the same width.
func (m mascot) art(ui styler) bannerArt {
	a := bannerArt{infoRow: m.infoRow}
	for _, row := range m.rows {
		if n := utf8.RuneCountInString(row); n > a.width {
			a.width = n
		}
	}
	for i := range m.rows {
		line, n := m.render(ui, i)
		a.lines = append(a.lines, line+strings.Repeat(" ", a.width-n))
	}
	return a
}

// beastieSprite is Beastie as pixel art, one letter per pixel ('.' is
// transparent, the rest are keys of spriteColors). Two pixel rows share a
// terminal row, so the picture is 28 columns by 17 lines.
var beastieSprite = []string{
	".............nnnnnn.........", // pilot cap
	"...........nnnnnnnnnn.......",
	"..........nnyyyrryyynn......", // wings badge
	".........nnnnnyrrynnnnn.....",
	"......r..nnnnnnnnnnnnnn..r..", // horn tips
	"......rr.nnsssnnnnsssnn.rr..",
	"......rrrbswggsbbswggsbrrr..", // goggles on their strap
	"......rrrbsgggsbbsgggsbrrr..",
	".......rrmmsssmmmmsssmmrr...",
	".......rrmmmmmmmmmmmmmmrr...", // visor
	"........rddddddddddddddr....",
	".......rrrrrrrrrrrrrrrrrr...",
	"....r..rrrrwwkrrrrwwkrrrr..r", // ears and eyes
	"....rdrrrrrwkkrrrrwkkrrrrrdr",
	".....rrrrrrkkkrrrrkkkrrrrrr.",
	"......drrpprrrrrrrrrrpprrd..", // cheeks
	".......rrrrrrrrpprrrrrrrr...", // nose
	"........rrrkrrrrrrrrkrrr....",
	"........rrrrkkkkkkkkrrrr....", // grin
	".........rrrrrrrrrrrrrr.....",
	"..r....gg..drrrrrrrrd..gg...", // tail tip, chin, grips of the yoke
	".rrr...ss....dddddd....ss...",
	"rrrrr..ss.rrrrrrrrrrrr.ss...",
	"..r...rrrrrrrrrrrrrrrrrrrr..", // arms out to the fists around the grips
	"..r...rrrrdrrrrrrrrrrdrrrr..",
	"..rr...ss.rrrrrrrrrrrr.ss...", // the wheel comes down and round
	"...r...sssrrrrrrrrrrrrsss...",
	"...r....sssrrrrrrrrrrsss....",
	"....r....ssssskkkksssss.....", // to the hub, in front of the belt
	"....rr....rrrskyyksrrr......",
	".....rrrrrrrrrrkkrrrrr......", // tail joins the body
	"..........rrrr...rrrr.......",
	"..........rrr....rrr........",
	"........ddddd....ddddd......", // feet
}

const spriteWidth = 28

// spriteColor is a palette entry: the exact color, for terminals that
// advertise 24-bit support, and the nearest xterm 256-color number.
type spriteColor struct {
	r, g, b uint8
	index   int
}

var spriteColors = map[byte]spriteColor{
	'r': {0xd0, 0x35, 0x2b, 160}, // Beastie red
	'd': {0x9a, 0x26, 0x22, 124}, // red in shadow
	'p': {0xe2, 0x60, 0x4f, 203}, // cheeks and nose
	'n': {0x2b, 0x41, 0x60, 24},  // cap
	'm': {0x1e, 0x2a, 0x42, 17},  // cap in shadow, visor
	'y': {0xe0, 0xa0, 0x30, 214}, // badge, cap of the yoke's hub
	's': {0x8a, 0x9e, 0xa3, 247}, // steel: goggle frames, yoke
	'g': {0x5a, 0x78, 0x88, 67},  // goggle glass, grips of the yoke
	'b': {0x6c, 0x3c, 0x2c, 94},  // leather strap
	'w': {0xe8, 0xe6, 0xd0, 255}, // eye highlights, glint
	'k': {0x14, 0x17, 0x1a, 16},  // pupils, grin, hub of the yoke
}

// sgr returns the escape sequence selecting c as the foreground (layer 38)
// or background (layer 48) color.
func (c spriteColor) sgr(layer int, truecolor bool) string {
	if truecolor {
		return fmt.Sprintf("\033[%d;2;%d;%d;%dm", layer, c.r, c.g, c.b)
	}
	return fmt.Sprintf("\033[%d;5;%dm", layer, c.index)
}

// A patch overwrites part of one sprite row to make an animation frame.
type patch struct {
	row, col int
	pixels   string
}

var (
	// blink closes both eyes.
	blink = []patch{
		{12, 11, "rrr"}, {12, 18, "rrr"},
		{13, 11, "kkk"}, {13, 18, "kkk"},
		{14, 11, "rrr"}, {14, 18, "rrr"},
	}
	// tailWag swings the tip of the tail one pixel to the right.
	tailWag = []patch{
		{20, 0, "...r.."}, {21, 0, "..rrr."}, {22, 0, ".rrrrr"},
		{23, 0, "...r.."}, {24, 0, "...r.."}, {25, 0, "...r.."},
	}
	// glintMid and glintFar move the highlight across the goggle lenses.
	glintMid = []patch{{6, 10, "sgwgs"}, {6, 17, "sgwgs"}}
	glintFar = []patch{{6, 10, "sggws"}, {6, 17, "sggws"}}
)

// frameInterval is the time between animation frames.
const frameInterval = 120 * time.Millisecond

// spriteFrame returns the sprite's pixels at the given animation tick: the
// tail wags steadily, and now and then Beastie blinks or his goggles glint.
// Tick 0 is the sprite at rest.
func spriteFrame(tick int) []string {
	rows := append([]string(nil), beastieSprite...)
	apply := func(patches []patch) {
		for _, p := range patches {
			r := rows[p.row]
			rows[p.row] = r[:p.col] + p.pixels + r[p.col+len(p.pixels):]
		}
	}
	if (tick/4)%2 == 1 {
		apply(tailWag)
	}
	switch tick % 28 {
	case 22, 23:
		apply(blink)
	}
	switch tick % 40 {
	case 33, 35:
		apply(glintMid)
	case 34:
		apply(glintFar)
	}
	return rows
}

// renderSprite converts pixel rows into terminal lines, two pixel rows per
// line: the upper-half block draws the top pixel in the foreground color
// over the bottom pixel in the background color.
func renderSprite(pixels []string, truecolor bool) []string {
	var lines []string
	for y := 0; y+1 < len(pixels); y += 2 {
		var b strings.Builder
		var fg, bg byte // colors in effect; 0 is the terminal's default

		use := func(wantFg, wantBg byte) {
			if bg != 0 && wantBg == 0 {
				b.WriteString("\033[0m") // only a reset clears the background
				fg, bg = 0, 0
			}
			if wantFg != 0 && wantFg != fg {
				b.WriteString(spriteColors[wantFg].sgr(38, truecolor))
				fg = wantFg
			}
			if wantBg != 0 && wantBg != bg {
				b.WriteString(spriteColors[wantBg].sgr(48, truecolor))
				bg = wantBg
			}
		}

		top, bottom := pixels[y], pixels[y+1]
		for x := 0; x < len(top) && x < len(bottom); x++ {
			switch t, u := top[x], bottom[x]; {
			case t == '.' && u == '.':
				use(0, 0)
				b.WriteByte(' ')
			case u == '.':
				use(t, 0)
				b.WriteString("▀")
			case t == '.':
				use(u, 0)
				b.WriteString("▄")
			default:
				use(t, u)
				b.WriteString("▀")
			}
		}
		if fg != 0 || bg != 0 {
			b.WriteString("\033[0m")
		}
		lines = append(lines, b.String())
	}
	return lines
}

// spriteArt returns the sprite at rest as a banner.
func spriteArt() bannerArt {
	return bannerArt{
		lines:    renderSprite(spriteFrame(0), truecolor()),
		width:    spriteWidth,
		infoRow:  6,
		animated: true,
	}
}

// truecolor reports whether the terminal advertises 24-bit color.
func truecolor() bool {
	v := os.Getenv("COLORTERM")
	return v == "truecolor" || v == "24bit"
}

// spriteFits reports whether the terminal behind ui has room for the sprite
// with infoWidth columns of text beside it. The animation moves the cursor
// by relative amounts, so the whole banner must be on screen and no line may
// wrap.
func spriteFits(ui styler, infoWidth int) bool {
	f, ok := ui.out.(*os.File)
	if !ok {
		return false
	}
	cols, rows, ok := termSize(f)
	return ok &&
		cols > len(bannerIndent)+spriteWidth+bannerGap+infoWidth &&
		rows >= len(beastieSprite)/2+4
}

// chooseBanner picks the richest mascot the terminal can show.
func chooseBanner(ui styler, cfg settings, infoWidth int) bannerArt {
	switch {
	case !utf8Locale():
		return beastieASCII.art(ui)
	case cfg.Banner == bannerFull && ui.color && spriteFits(ui, infoWidth):
		return spriteArt()
	default:
		return beastieBlocks.art(ui)
	}
}

// printBanner draws Beastie with the version, the model in use, the working
// directory and the mode beside him. It returns what it drew so that the
// caller can animate it.
func printBanner(ui styler, cfg settings) bannerArt {
	sep := " - "
	if utf8Locale() {
		sep = " · "
	}
	mode := cfg.Type + " mode"
	if n := len(cfg.autoRunList()); n > 0 && cfg.Type == typeSafe {
		mode += sep + fmt.Sprintf("%d auto-run", n)
	}
	info := []string{
		ui.paint(bold, "BeastiePilot") + " " + ui.paint(dim, bannerVersion()),
		cfg.Model + sep + endpointHost(cfg.Endpoint),
		ui.paint(dim, shortWorkingDir()),
		mode,
	}
	infoWidth := 0
	for _, line := range info {
		if n := visibleWidth(line); n > infoWidth {
			infoWidth = n
		}
	}

	art := chooseBanner(ui, cfg, infoWidth)
	fmt.Fprintln(ui.out)
	for i, line := range art.lines {
		if j := i - art.infoRow; j >= 0 && j < len(info) {
			line += strings.Repeat(" ", bannerGap) + info[j]
		}
		fmt.Fprintln(ui.out, bannerIndent+line)
	}
	fmt.Fprintln(ui.out)
	return art
}

// clockAfter is how long a wait lasts before the status line starts to say
// how long it has been. A quick answer needs no clock.
const clockAfter = 5 * time.Second

// waitStatus is the line shown while Ollama is working: what it is doing,
// dots that move so that the wait is seen to be alive and, once it has lasted
// a while, for how long.
func waitStatus(status string, tick int, waited time.Duration) string {
	s := status + " " + strings.Repeat(".", 1+(tick/3)%3)
	if waited < clockAfter {
		return s
	}
	// The clock keeps its column while the dots come and go.
	return fmt.Sprintf("%-*s %s", len(status)+4, s, waited.Truncate(time.Second))
}

// animator keeps a status line at the cursor up to date while the model is
// thinking and, with sprite set, replays the sprite above it.
type animator struct {
	out    io.Writer
	sprite bool
	status func(tick int) string
	stopCh chan struct{}
	done   chan struct{}
}

// startAnimation draws the status line at the cursor and keeps it moving.
// With sprite set it also animates the banner that was just printed; the
// cursor must then still be on the line directly below the banner's trailing
// blank line. Call stop before printing anything else.
func startAnimation(ui styler, status string, sprite bool) *animator {
	start := time.Now()
	a := &animator{
		out:    ui.out,
		sprite: sprite,
		status: func(tick int) string {
			return ui.paint(dim, bannerIndent+waitStatus(status, tick, time.Since(start)))
		},
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	io.WriteString(a.out, a.status(0))
	go a.run()
	return a
}

func (a *animator) run() {
	defer close(a.done)
	tc := truecolor()
	var shown []string
	if a.sprite {
		shown = renderSprite(spriteFrame(0), tc)
	}
	status := a.status(0)

	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-a.stopCh:
			if a.sprite {
				// Leave Beastie at rest.
				io.WriteString(a.out, redraw(shown, renderSprite(spriteFrame(0), tc)))
			}
			return
		case <-ticker.C:
			out := ""
			if a.sprite {
				next := renderSprite(spriteFrame(tick), tc)
				out, shown = redraw(shown, next), next
			}
			if s := a.status(tick); s != status {
				out += "\r\033[K" + s
				status = s
			}
			if out != "" {
				io.WriteString(a.out, out) // one write per frame, to avoid tearing
			}
		}
	}
}

// stop ends the animation and waits for the last frame to be drawn.
func (a *animator) stop() {
	close(a.stopCh)
	<-a.done
}

// redraw returns the escape sequences that turn the sprite lines shown into
// next, rewriting only the lines that differ. Line i of the sprite is
// len(next)-i+1 rows above the cursor: the remaining sprite lines, then the
// blank line under the banner. The cursor is put back where it was.
func redraw(shown, next []string) string {
	var b strings.Builder
	for i := range next {
		if i < len(shown) && next[i] == shown[i] {
			continue
		}
		// ESC 7 and ESC 8 save and restore the cursor position.
		fmt.Fprintf(&b, "\0337\033[%dA\r%s%s\0338", len(next)-i+1, bannerIndent, next[i])
	}
	return b.String()
}

// visibleWidth returns the number of columns s occupies once its SGR escape
// sequences are discounted.
func visibleWidth(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case inEscape:
			inEscape = r != 'm'
		case r == '\033':
			inEscape = true
		default:
			n++
		}
	}
	return n
}

// utf8Locale reports whether the environment selects a UTF-8 locale, checking
// the variables in the order the C library does.
func utf8Locale() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToUpper(os.Getenv(key)); v != "" {
			return strings.Contains(v, "UTF-8") || strings.Contains(v, "UTF8")
		}
	}
	return false
}

// bannerVersion is buildVersion, with the long pseudo-version Go assigns to
// untagged builds shortened to "dev".
func bannerVersion() string {
	v := buildVersion()
	if strings.HasPrefix(v, "v0.0.0-") {
		return "dev"
	}
	return v
}

// endpointHost returns the host:port part of the Ollama endpoint URL.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

// shortWorkingDir returns the working directory with the user's home
// directory abbreviated to "~".
func shortWorkingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "/" && (wd == home || strings.HasPrefix(wd, home+"/")) {
		return "~" + wd[len(home):]
	}
	return wd
}
