package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// typed returns an editor that reads keys as the bytes a terminal sends, and
// what it draws.
func typed(keys string, columns int) (*lineEditor, *bytes.Buffer) {
	var out bytes.Buffer
	return &lineEditor{
		ui:      styler{out: &out},
		in:      strings.NewReader(keys),
		columns: func() int { return columns },
		prompt:  "> ",
	}, &out
}

func TestLineEditorKeys(t *testing.T) {
	tests := []struct{ name, keys, want string }{
		{"plain", "df -h\r", "df -h"},
		{"a newline ends the line too", "df\n", "df"},
		{"backspace", "dfx\x7f -h\r", "df -h"},
		{"backspace at the start", "\x7f\x7fdf\r", "df"},
		{"left, then insert", "spce\x1b[D\x1b[Da\r", "space"},
		{"arrows in application mode", "spce\x1bOD\x1bODa\r", "space"},
		{"right stops at the end", "ab\x1b[D\x1b[C\x1b[C\x1b[Cc\r", "abc"},
		{"arrows with modifiers", "ab\x1b[1;5Dc\r", "acb"},
		{"home and end", "bc\x1b[Ha\x1b[Fd\r", "abcd"},
		{"home and end, numbered", "bc\x1b[1~a\x1b[4~d\r", "abcd"},
		{"ctrl-a and ctrl-e", "bc\x01a\x05d\r", "abcd"},
		{"ctrl-b and ctrl-f", "ac\x02b\x06d\r", "abcd"},
		{"delete", "abc\x1b[H\x1b[3~\r", "bc"},
		{"ctrl-d deletes under the cursor", "abc\x01\x04\r", "bc"},
		{"ctrl-u", "abc def\x1b[D\x1b[D\x1b[D\x15\r", "def"},
		{"ctrl-k", "abc def\x01\x06\x06\x06\x0b\r", "abc"},
		{"ctrl-w", "list  the files  \x17\x17x\r", "list  x"},
		{"ctrl-c clears the line", "rm -rf /\x03ls\r", "ls"},
		{"accented letters", "espa\xc3\xa7o\x7f\x7f\xc3\xa3o\r", "espa\u00e3o"},
		{"a tab becomes a space", "a\tb\r", "a b"},
		{"control characters are dropped", "a\x00\x07\x1fb\r", "ab"},
		{"a byte that starts no character", "a\xffb\r", "ab"},
		{"a character cut short", "a\xc3b\r", "ab"},
		{"escape, then a key", "a\x1bb\r", "ab"},
		{"an unknown sequence", "a\x1b[99zb\r", "ab"},
	}
	for _, tt := range tests {
		e, _ := typed(tt.keys, 80)
		if got, err := e.read(); err != nil || got != tt.want {
			t.Errorf("%s: read() = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}

func TestLineEditorHistory(t *testing.T) {
	tests := []struct{ name, keys, want string }{
		{"up", "\x1b[A\r", "second"},
		{"up twice", "\x1b[A\x1b[A\r", "first"},
		{"up stops at the oldest", "\x1b[A\x1b[A\x1b[A\x1b[A\r", "first"},
		{"down again", "\x1b[A\x1b[A\x1b[B\r", "second"},
		{"ctrl-p and ctrl-n", "\x10\x10\x0e\r", "second"},
		{"the line being written comes back", "third\x1b[A\x1b[A\x1b[B\x1b[B!\r", "third!"},
		{"down from the new line does nothing", "third\x1b[B\r", "third"},
		{"a recalled line can be changed", "\x1b[A\x7f\x7f\x7f\x7f\x7f\x7fthird\r", "third"},
	}
	for _, tt := range tests {
		e, _ := typed(tt.keys, 80)
		e.history = []string{"first", "second"}
		if got, err := e.read(); err != nil || got != tt.want {
			t.Errorf("%s: read() = %q, %v; want %q", tt.name, got, err, tt.want)
		}
		if len(e.history) != 2 || e.history[0] != "first" || e.history[1] != "second" {
			t.Errorf("%s: the history was changed to %q", tt.name, e.history)
		}
	}
}

func TestLineEditorLeaving(t *testing.T) {
	// Ctrl-D on an empty line.
	e, _ := typed("\x04", 80)
	if _, err := e.read(); !errors.Is(err, io.EOF) {
		t.Errorf("Ctrl-D: %v, want io.EOF", err)
	}

	// In the box, Ctrl-C on an empty line asks for another one.
	e, out := typed("\x03\x03more\r", 80)
	e.boxed, e.hint = true, "the usual hint"
	if _, err := e.read(); !errors.Is(err, io.EOF) || string(e.pending) != "more\r" {
		t.Errorf("Ctrl-C twice: %v with %q unread; want io.EOF and the rest unread", err, e.pending)
	}
	if !strings.Contains(out.String(), "Ctrl-C again to leave") {
		t.Errorf("the first Ctrl-C was not explained:\n%q", out.String())
	}

	// Any other key in between calls it off, and the hint returns.
	e, out = typed("\x03x\r", 80)
	e.boxed, e.hint = true, "the usual hint"
	if got, err := e.read(); err != nil || got != "x" {
		t.Errorf("Ctrl-C, then a key: read() = %q, %v; want \"x\"", got, err)
	}
	if n := strings.Count(out.String(), "the usual hint"); n != 2 {
		t.Errorf("the hint was drawn %d times, want 2: at the start and after the key", n)
	}

	// The box takes no empty line; a bare prompt does, and one Ctrl-C ends it.
	e, _ = typed("\r  \rdf\r", 80)
	e.boxed = true
	if got, err := e.read(); err != nil || got != "  df" {
		t.Errorf("empty lines in the box: read() = %q, %v; want \"  df\"", got, err)
	}
	e, _ = typed("\rdf\r", 80)
	if got, err := e.read(); err != nil || got != "" {
		t.Errorf("an empty line at a bare prompt: read() = %q, %v; want \"\"", got, err)
	}
	e, _ = typed("\x03y\r", 80)
	if _, err := e.read(); !errors.Is(err, io.EOF) {
		t.Errorf("Ctrl-C at a bare prompt: %v, want io.EOF", err)
	}

	// A terminal that goes away in the middle of a line.
	e, _ = typed("half a li", 80)
	if _, err := e.read(); !errors.Is(err, io.EOF) {
		t.Errorf("input ending mid-line: %v, want io.EOF", err)
	}

	// What follows the line is not carried over to the next one.
	e, _ = typed("one\rtwo\r", 80)
	if got, _ := e.read(); got != "one" {
		t.Errorf("first line %q, want \"one\"", got)
	}
	if got, err := e.read(); !errors.Is(err, io.EOF) {
		t.Errorf("second read() = %q, %v; want io.EOF, the rest having been dropped", got, err)
	}
}

func TestLineEditorDrawing(t *testing.T) {
	t.Setenv("LC_ALL", "C")

	// The box: a rule, the line, a rule and the hint, then back up to the
	// line. Leaving erases it from the first rule down.
	e, out := typed("df\r", 24)
	e.boxed, e.hint = true, "a hint"
	if _, err := e.read(); err != nil {
		t.Fatal(err)
	}
	rule := "  " + strings.Repeat("-", 20)
	drawn := out.String()
	if want := "\n" + rule + "\n\n" + rule + "\n  a hint\x1b[2A\r"; !strings.HasPrefix(drawn, want) {
		t.Errorf("the box was drawn as\n%q\nwant it to start with\n%q", drawn, want)
	}
	if want := "\r> df\x1b[K\r\x1b[4C\x1b[1A\r\x1b[J"; !strings.HasSuffix(drawn, want) {
		t.Errorf("the box was left as\n%q\nwant it to end with\n%q", drawn, want)
	}

	// A bare prompt just ends its line.
	e, out = typed("y\r", 24)
	e.prompt = "? "
	e.read()
	if want := "\r? \x1b[K\r\x1b[2C\r? y\x1b[K\r\x1b[3C\n"; out.String() != want {
		t.Errorf("a bare prompt drew\n%q\nwant\n%q", out.String(), want)
	}

	// A line longer than the terminal scrolls sideways: with 20 columns and
	// a prompt of two, 17 characters show and the last column stays empty.
	line := "abcdefghijklmnopqrstuvwxyz"
	e, out = typed(line+"\x01\r", 20)
	if got, err := e.read(); err != nil || got != line {
		t.Fatalf("read() = %q, %v", got, err)
	}
	for _, want := range []string{
		"\r> jklmnopqrstuvwxyz\x1b[K\r\x1b[19C", // the cursor at the end
		"\r> abcdefghijklmnopq\x1b[K\r\x1b[2C",  // and back at the start
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the long line was never drawn as %q:\n%q", want, out.String())
		}
	}

	// A terminal that does not say how wide it is counts as 80 columns.
	e, out = typed("\x04", 0)
	e.boxed = true
	e.read()
	if want := "\n  " + strings.Repeat("-", 76) + "\n"; !strings.HasPrefix(out.String(), want) {
		t.Errorf("with no width, the box starts with\n%q", out.String())
	}
}
