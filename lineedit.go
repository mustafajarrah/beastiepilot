package main

import (
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The line editor of the interactive session: it reads one line from a
// terminal that hands over each key as it is pressed (see rawInput), with the
// keys a shell has taught people to expect and the earlier lines within reach
// of the arrows. The line is drawn between two rules, with a hint beneath:
//
//	──────────────────────────────────────────
//	> show the free disk space
//	──────────────────────────────────────────
//	/help for commands · Ctrl-D to leave
//
// It never lets the line wrap: a line longer than the terminal is wide
// scrolls sideways, which keeps every redraw to the one row the cursor is on.

const (
	// editorIndent sets the box off from the left edge, like the rest of the
	// interface.
	editorIndent = "  "

	// defaultColumns is the width assumed for a terminal that does not say.
	defaultColumns = 80
)

// lineEditor reads lines from a terminal in raw input mode.
type lineEditor struct {
	ui      styler
	in      io.Reader
	columns func() int // the width of the terminal, or 0 if unknown
	prompt  string     // what the line starts with, already painted
	hint    string     // shown under the box; "" for a prompt without a box
	boxed   bool       // draw the rules and the hint
	history []string   // earlier lines, oldest first

	text    []rune
	cursor  int    // where in text the next character goes
	left    int    // the first character of text that is on screen
	at      int    // the history entry being shown; len(history) is the new line
	draft   []rune // the new line, kept while the arrows visit the history
	armed   bool   // Ctrl-C was just pressed on an empty line
	pending []byte // read from the terminal and not looked at yet
}

// width returns the number of columns to draw in.
func (e *lineEditor) width() int {
	if e.columns != nil {
		if n := e.columns(); n >= 20 {
			return n
		}
	}
	return defaultColumns
}

// rule returns the line drawn above and below the text.
func (e *lineEditor) rule() string {
	glyph := "-"
	if utf8Locale() {
		glyph = "─"
	}
	return editorIndent + e.ui.paint(dim, strings.Repeat(glyph, e.width()-2*len(editorIndent)))
}

// open draws the box and leaves the cursor on the row the text goes in.
func (e *lineEditor) open() {
	if !e.boxed {
		return
	}
	io.WriteString(e.ui.out, "\n"+e.rule()+"\n\n"+e.rule()+"\n"+editorIndent+e.ui.paint(dim, e.hint)+"\033[2A\r")
}

// close erases the box, leaving the cursor where the box began. Without a
// box, it ends the line.
func (e *lineEditor) close() {
	if !e.boxed {
		io.WriteString(e.ui.out, "\n")
		return
	}
	io.WriteString(e.ui.out, "\033[1A\r\033[J")
}

// note replaces the hint under the box until the next key.
func (e *lineEditor) note(text string) {
	if e.boxed {
		io.WriteString(e.ui.out, "\033[2B\r"+editorIndent+e.ui.paint(dim, text)+"\033[K\033[2A")
	}
}

// refresh redraws the row of the text and puts the cursor in its place.
func (e *lineEditor) refresh() {
	// The last column stays empty: writing in it would wrap on some terminals.
	room := e.width() - visibleWidth(e.prompt) - 1
	if room < 1 {
		room = 1
	}
	switch {
	case e.cursor < e.left:
		e.left = e.cursor
	case e.cursor > e.left+room:
		e.left = e.cursor - room
	}
	shown := e.text[e.left:]
	if len(shown) > room {
		shown = shown[:room]
	}
	out := "\r" + e.prompt + string(shown) + "\033[K\r"
	if n := visibleWidth(e.prompt) + e.cursor - e.left; n > 0 {
		out += "\033[" + strconv.Itoa(n) + "C"
	}
	io.WriteString(e.ui.out, out)
}

// next returns the next byte from the terminal.
func (e *lineEditor) next() (byte, error) {
	if len(e.pending) == 0 {
		buf := make([]byte, 256)
		n, err := e.in.Read(buf)
		if n == 0 {
			if err == nil {
				err = io.ErrNoProgress
			}
			return 0, err
		}
		e.pending = buf[:n]
	}
	b := e.pending[0]
	e.pending = e.pending[1:]
	return b, nil
}

// read lets the user write a line and returns it once Enter is pressed. It
// returns io.EOF when the user leaves instead: Ctrl-D on an empty line, or
// Ctrl-C on one, twice if there is a box to say so in. In the box, a line of
// nothing but spaces is not accepted.
func (e *lineEditor) read() (string, error) {
	e.text, e.cursor, e.left, e.draft, e.armed, e.pending = nil, 0, 0, nil, false, nil
	e.at = len(e.history)
	e.open()
	for {
		e.refresh()
		b, err := e.next()
		if err != nil {
			e.close()
			return "", err
		}
		wasArmed := e.armed
		e.armed = false
		if wasArmed && b != 0x03 {
			e.note(e.hint)
		}
		switch b {
		case '\r', '\n':
			if e.boxed && strings.TrimSpace(string(e.text)) == "" {
				continue
			}
			e.close()
			return string(e.text), nil
		case 0x03: // Ctrl-C: clear the line; on an empty one, twice leaves
			if len(e.text) > 0 {
				e.set(nil)
				continue
			}
			if wasArmed || !e.boxed {
				e.close()
				return "", io.EOF
			}
			e.armed = true
			e.note("Ctrl-C again to leave")
		case 0x04: // Ctrl-D: leave on an empty line, else delete under the cursor
			if len(e.text) == 0 {
				e.close()
				return "", io.EOF
			}
			e.delete(e.cursor, e.cursor+1)
		case 0x7f, 0x08:
			e.delete(e.cursor-1, e.cursor)
		case 0x01: // Ctrl-A
			e.cursor = 0
		case 0x05: // Ctrl-E
			e.cursor = len(e.text)
		case 0x02: // Ctrl-B
			e.move(-1)
		case 0x06: // Ctrl-F
			e.move(1)
		case 0x10: // Ctrl-P
			e.recall(-1)
		case 0x0e: // Ctrl-N
			e.recall(1)
		case 0x15: // Ctrl-U
			e.delete(0, e.cursor)
		case 0x0b: // Ctrl-K
			e.delete(e.cursor, len(e.text))
		case 0x17: // Ctrl-W
			e.delete(e.wordStart(), e.cursor)
		case 0x0c: // Ctrl-L
			io.WriteString(e.ui.out, "\033[H\033[2J")
			e.open()
		case 0x1b:
			if err := e.escape(); err != nil {
				e.close()
				return "", err
			}
		default:
			if err := e.insert(b); err != nil {
				e.close()
				return "", err
			}
		}
	}
}

// escape handles what follows an ESC: the sequences the arrows and the keys
// around them send.
func (e *lineEditor) escape() error {
	b, err := e.next()
	if err != nil {
		return err
	}
	if b != '[' && b != 'O' {
		// ESC by itself, then another key: that key stands.
		e.pending = append([]byte{b}, e.pending...)
		return nil
	}
	var params []byte
	for {
		if b, err = e.next(); err != nil {
			return err
		}
		if b >= 0x40 && b <= 0x7e {
			break
		}
		if params = append(params, b); len(params) > 16 {
			return nil // not a key
		}
	}
	switch b {
	case 'A':
		e.recall(-1)
	case 'B':
		e.recall(1)
	case 'C':
		e.move(1)
	case 'D':
		e.move(-1)
	case 'H':
		e.cursor = 0
	case 'F':
		e.cursor = len(e.text)
	case '~':
		switch string(params) {
		case "1", "7":
			e.cursor = 0
		case "4", "8":
			e.cursor = len(e.text)
		case "3":
			e.delete(e.cursor, e.cursor+1)
		}
	}
	return nil
}

// insert puts the character that starts with byte b at the cursor. Bytes
// that do not make a printable character are dropped.
func (e *lineEditor) insert(b byte) error {
	size := 1
	switch {
	case b < 0x80:
	case b >= 0xc2 && b <= 0xdf:
		size = 2
	case b >= 0xe0 && b <= 0xef:
		size = 3
	case b >= 0xf0 && b <= 0xf4:
		size = 4
	default:
		return nil // no character starts with this byte
	}
	raw := []byte{b}
	for len(raw) < size {
		c, err := e.next()
		if err != nil {
			return err
		}
		if c&0xc0 != 0x80 {
			// Not the rest of this character: a key of its own.
			e.pending = append([]byte{c}, e.pending...)
			return nil
		}
		raw = append(raw, c)
	}
	r, _ := utf8.DecodeRune(raw)
	if r == '\t' {
		r = ' '
	}
	if r == utf8.RuneError || !unicode.IsPrint(r) {
		return nil
	}
	e.text = append(e.text[:e.cursor], append([]rune{r}, e.text[e.cursor:]...)...)
	e.cursor++
	return nil
}

// set replaces the whole line and puts the cursor at its end.
func (e *lineEditor) set(text []rune) {
	e.text, e.cursor, e.left = text, len(text), 0
}

// move shifts the cursor by n characters, within the line.
func (e *lineEditor) move(n int) {
	e.cursor = min(max(e.cursor+n, 0), len(e.text))
}

// delete removes the characters from index from up to, not including, to.
func (e *lineEditor) delete(from, to int) {
	from, to = max(from, 0), min(to, len(e.text))
	if from >= to {
		return
	}
	e.text = append(e.text[:from:from], e.text[to:]...)
	if e.cursor > from {
		e.cursor = max(from, e.cursor-(to-from))
	}
}

// wordStart returns where the word before the cursor begins.
func (e *lineEditor) wordStart() int {
	i := e.cursor
	for i > 0 && e.text[i-1] == ' ' {
		i--
	}
	for i > 0 && e.text[i-1] != ' ' {
		i--
	}
	return i
}

// recall shows the history entry n places from the one on screen. Past the
// newest entry is the line that was being written.
func (e *lineEditor) recall(n int) {
	to := e.at + n
	if to < 0 || to > len(e.history) {
		return
	}
	if e.at == len(e.history) {
		e.draft = e.text
	}
	e.at = to
	if to == len(e.history) {
		e.set(e.draft)
		return
	}
	e.set([]rune(e.history[to]))
}
