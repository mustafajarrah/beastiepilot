package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The menu of the interactive session: a short list to pick one entry from
// with the arrows, as "/type" shows it:
//
//	> Type  what is done with each command
//
//	  > 1. safe    show each command, then ask  (current)
//	    2. guided  explain each command first, then ask
//
//	  Up and Down to choose · Enter to confirm · Esc to cancel
//
// Like the line editor it reads a terminal that hands over each key as it is
// pressed, never lets a row wrap, and erases itself once it is done.

// menuIndent sets the entries off from the title above them.
const menuIndent = "    "

// choice is one entry of a menu.
type choice struct {
	name  string
	about string
}

// menu lets the user pick one of a few choices.
type menu struct {
	ui      styler
	in      io.Reader
	columns func() int // the width of the terminal, or 0 if unknown
	title   string
	about   string // what the choice is about, beside the title
	choices []choice
	current int // the choice in effect now, marked as such

	at      int    // the choice the cursor is on
	pending []byte // read from the terminal and not looked at yet
}

// cut shortens s to at most n characters.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:max(n, 0)])
	}
	return s
}

// heading returns the line with the title.
func (m *menu) heading() string {
	room := usableColumns(m.columns) - 1 - len(editorIndent) - 2 - len(m.title) - 2
	return editorIndent + m.ui.paint(boldCyan, ">") + " " + m.ui.paint(bold, m.title) + "  " + m.ui.paint(dim, cut(m.about, room))
}

// row returns the line of choice i. With pointer set, the line shows where the
// cursor is; without, it is a plain list.
func (m *menu) row(i int, pointer bool) string {
	width := 0
	for _, c := range m.choices {
		width = max(width, len(c.name))
	}
	c := m.choices[i]
	label := fmt.Sprintf("%d. %-*s", i+1, width, c.name)
	about := c.about
	if i == m.current {
		about += "  (current)"
	}
	// The last column stays empty: writing in it would wrap on some terminals.
	about = cut(about, usableColumns(m.columns)-1-len(menuIndent)-2-len(label)-2)

	mark := " "
	if pointer && i == m.at {
		mark, label = m.ui.paint(boldGreen, ">"), m.ui.paint(bold, label)
	}
	return menuIndent + mark + " " + label + "  " + m.ui.paint(dim, about)
}

// park returns what puts the cursor on the pointer, from the start of its row.
func (m *menu) park() string {
	return "\r\033[" + strconv.Itoa(len(menuIndent)) + "C"
}

// open draws the menu and leaves the cursor on the choice it starts at. The
// menu begins on the row the cursor was on, which stays empty.
func (m *menu) open() {
	var b strings.Builder
	b.WriteString("\n" + m.heading() + "\n")
	for i := range m.choices {
		b.WriteString("\n" + m.row(i, true))
	}
	hint := "Up and Down to choose" + separator() + "Enter to confirm" + separator() + "Esc to cancel"
	b.WriteString("\n\n" + menuIndent + m.ui.paint(dim, cut(hint, usableColumns(m.columns)-1-len(menuIndent))))
	// From the hint up to the choice: the empty row, then the choices below it.
	fmt.Fprintf(&b, "\033[%dA%s", len(m.choices)+1-m.at, m.park())
	io.WriteString(m.ui.out, b.String())
}

// move takes the pointer to choice to, redrawing the two rows that change.
func (m *menu) move(to int) {
	from := m.at
	if to == from {
		return
	}
	m.at = to
	out := "\r" + m.row(from, true) + "\033[K"
	if to > from {
		out += "\033[" + strconv.Itoa(to-from) + "B"
	} else {
		out += "\033[" + strconv.Itoa(from-to) + "A"
	}
	io.WriteString(m.ui.out, out+"\r"+m.row(to, true)+"\033[K"+m.park())
}

// close erases the menu, leaving the cursor where it was before open.
func (m *menu) close() {
	// Above the choice: the choices before it, an empty row, the title and
	// the row the menu began on.
	fmt.Fprintf(m.ui.out, "\033[%dA\r\033[J", m.at+3)
}

// next returns the next byte from the terminal.
func (m *menu) next() (byte, error) {
	if len(m.pending) == 0 {
		buf := make([]byte, 256)
		n, err := m.in.Read(buf)
		if n == 0 {
			if err == nil {
				err = io.ErrNoProgress
			}
			return 0, err
		}
		m.pending = buf[:n]
	}
	b := m.pending[0]
	m.pending = m.pending[1:]
	return b, nil
}

// read shows the menu and returns the number of the choice made, or -1 if
// the user left it with Esc, Ctrl-C, Ctrl-D or q. The arrows, Ctrl-P and
// Ctrl-N, k and j and Tab move the pointer, which goes round at the ends;
// Enter takes the choice it is on, and a digit the choice of that number.
func (m *menu) read() (int, error) {
	m.at, m.pending = max(m.current, 0), nil
	m.open()
	last := len(m.choices) - 1
	step := func(by int) {
		m.move((m.at + by + len(m.choices)) % len(m.choices))
	}
	for {
		b, err := m.next()
		if err != nil {
			m.close()
			return -1, err
		}
		switch {
		case b == '\r' || b == '\n':
			m.close()
			return m.at, nil
		case b >= '1' && int(b-'1') <= last:
			m.close()
			return int(b - '1'), nil
		case b == 0x03 || b == 0x04 || b == 'q':
			m.close()
			return -1, nil
		case b == 0x10 || b == 'k':
			step(-1)
		case b == 0x0e || b == 'j' || b == '\t':
			step(1)
		case b == 0x1b:
			if len(m.pending) == 0 {
				// Esc by itself: an arrow sends what follows it in one go.
				m.close()
				return -1, nil
			}
			if b, _ = m.next(); b != '[' && b != 'O' {
				continue
			}
			// The sequence ends with a letter or a tilde; what comes before
			// are parameters, as in "1;5A".
			for len(m.pending) > 0 {
				if b, _ = m.next(); b >= 0x40 && b <= 0x7e {
					break
				}
			}
			switch b {
			case 'A':
				step(-1)
			case 'B':
				step(1)
			case 'H':
				m.move(0)
			case 'F':
				m.move(last)
			}
		}
	}
}

// ask is the plain way, for a terminal that cannot be read key by key: it
// lists the choices and reads a line with the number or the name of one. It
// returns -1 for anything else, an empty line included.
func (m *menu) ask(in io.Reader) int {
	fmt.Fprintln(m.ui.out, "\n"+m.heading()+"\n")
	for i := range m.choices {
		fmt.Fprintln(m.ui.out, m.row(i, false))
	}
	fmt.Fprintf(m.ui.out, "\n  %s %s ", m.ui.paint(boldYellow, "?"),
		m.ui.paint(bold, fmt.Sprintf("Which one? [1-%d, or Enter to leave it]", len(m.choices))))
	line, err := readLine(in)
	if err != nil {
		fmt.Fprintln(m.ui.out)
		return -1
	}
	answer := strings.TrimSpace(line)
	for i, c := range m.choices {
		if answer == strconv.Itoa(i+1) || strings.EqualFold(answer, c.name) {
			return i
		}
	}
	return -1
}
