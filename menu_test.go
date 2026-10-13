package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// chunks hands over what a terminal sends one key at a time, as a terminal
// does: an arrow arrives whole, and Esc by itself arrives alone.
type chunks [][]byte

func (c *chunks) Read(p []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, (*c)[0])
	*c = (*c)[1:]
	return n, nil
}

// pressed returns a menu of three choices, the second in effect, that reads
// the given keys, and what it draws.
func pressed(columns int, keys ...string) (*menu, *bytes.Buffer) {
	var out bytes.Buffer
	in := &chunks{}
	for _, k := range keys {
		*in = append(*in, []byte(k))
	}
	return &menu{
		ui:      styler{out: &out},
		in:      in,
		columns: func() int { return columns },
		title:   "Type",
		about:   "what is done with each command",
		choices: []choice{{"safe", "show each command, then ask"}, {"guided", "explain each command first, then ask"}, {"third", "one more, for the test"}},
		current: 1,
	}, &out
}

func TestMenuKeys(t *testing.T) {
	const up, down, esc = "\x1b[A", "\x1b[B", "\x1b"
	tests := []struct {
		name string
		keys []string
		want int
	}{
		{"enter takes the one in effect", []string{"\r"}, 1},
		{"down", []string{down, "\r"}, 2},
		{"up", []string{up, "\r"}, 0},
		{"down goes round", []string{down, down, "\r"}, 0},
		{"up goes round", []string{up, up, "\r"}, 2},
		{"arrows in application mode", []string{"\x1bOA", "\n"}, 0},
		{"arrows with modifiers", []string{"\x1b[1;5B", "\r"}, 2},
		{"home and end", []string{"\x1b[F", "\r"}, 2},
		{"ctrl-p and ctrl-n", []string{"\x10", "\x10", "\x0e", "\r"}, 0},
		{"k and j", []string{"k", "j", "j", "\r"}, 2},
		{"tab", []string{"\t", "\r"}, 2},
		{"a digit chooses at once", []string{"1", "\r"}, 0},
		{"a digit past the end does nothing", []string{"4", "9", "0", "\r"}, 1},
		{"other keys do nothing", []string{"x", " ", "\x7f", "\x1b[3~", "\r"}, 1},
		{"esc", []string{down, esc, "\r"}, -1},
		{"ctrl-c", []string{"\x03", "\r"}, -1},
		{"ctrl-d", []string{"\x04"}, -1},
		{"q", []string{"q"}, -1},
	}
	for _, tt := range tests {
		m, _ := pressed(80, tt.keys...)
		if got, err := m.read(); err != nil || got != tt.want {
			t.Errorf("%s: read() = %d, %v; want %d", tt.name, got, err, tt.want)
		}
	}

	// A terminal that goes away chooses nothing.
	m, _ := pressed(80, down)
	if got, err := m.read(); got != -1 || !errors.Is(err, io.EOF) {
		t.Errorf("input ending: read() = %d, %v; want -1 and io.EOF", got, err)
	}
}

func TestMenuDrawing(t *testing.T) {
	t.Setenv("LC_ALL", "C")

	// The title, the choices and the hint, then up to the choice in effect;
	// moving redraws the row left and the row reached, and leaving erases it
	// all from the row it began on.
	m, out := pressed(80, "\x1b[B", "\r")
	if got, err := m.read(); err != nil || got != 2 {
		t.Fatalf("read() = %d, %v", got, err)
	}
	want := "\n  > Type  what is done with each command\n" +
		"\n      1. safe    show each command, then ask" +
		"\n    > 2. guided  explain each command first, then ask  (current)" +
		"\n      3. third   one more, for the test" +
		"\n\n    Up and Down to choose - Enter to confirm - Esc to cancel" +
		"\x1b[3A\r\x1b[4C" +
		"\r      2. guided  explain each command first, then ask  (current)\x1b[K" +
		"\x1b[1B\r    > 3. third   one more, for the test\x1b[K\r\x1b[4C" +
		"\x1b[5A\r\x1b[J"
	if out.String() != want {
		t.Errorf("the menu was drawn as\n%q\nwant\n%q", out.String(), want)
	}

	// No row is as wide as the terminal, however narrow that is.
	m, out = pressed(30, "\x1b[A", "\r")
	m.read()
	for _, row := range strings.Split(strings.NewReplacer("\x1b[K", "", "\r", "\n").Replace(out.String()), "\n") {
		if n := visibleWidth(row); n >= 30 {
			t.Errorf("a row of %d columns on a terminal of 30: %q", n, row)
		}
	}

	// The plain way lists the same choices and takes a number or a name.
	for answer, want := range map[string]int{"1\n": 0, " guided \n": 1, "THIRD\n": 2, "\n": -1, "4\n": -1, "safe guided\n": -1, "": -1} {
		m, out = pressed(80)
		if got := m.ask(strings.NewReader(answer)); got != want {
			t.Errorf("ask() with %q = %d, want %d", answer, got, want)
		}
		if drawn := out.String(); !strings.Contains(drawn, "    2. guided  explain each command first, then ask  (current)\n") || strings.Contains(drawn, "\x1b") {
			t.Errorf("the plain list was drawn as\n%q", drawn)
		}
	}
}
