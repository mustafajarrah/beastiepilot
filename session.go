package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// The interactive session: "beastiepilot" with no request takes requests one
// after another on the terminal, each handled as it would be from the command
// line, until the user leaves. What was asked before goes along with each new
// request, so that it can build on the last.

const (
	// maxPast is how many earlier requests accompany a new one, and
	// maxPastChars how much of each is kept. Together they bound what the
	// session adds to the prompt, which has contextTokens to fit in.
	maxPast      = 5
	maxPastChars = 300
)

// exchange is a request made earlier in the session and what came of it.
type exchange struct {
	request string
	command string // what the model suggested, or "" if it gave none
	outcome string
}

// remember keeps a request for the ones that follow it in the session.
func (p *pilot) remember(request, command, outcome string) {
	p.past = append(p.past, exchange{request, command, outcome})
	if len(p.past) > maxPast {
		p.past = p.past[len(p.past)-maxPast:]
	}
}

// clip puts s on one line and cuts it to maxPastChars.
func clip(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > maxPastChars {
		return string(r[:maxPastChars]) + " ..."
	}
	return string(r)
}

// sessionSection formats the earlier requests of a session as a section of
// the system prompt. Like the rest of the prompt it steers the model and
// guards nothing: whatever comes back is reviewed as any other suggestion.
func sessionSection(past []exchange) string {
	if len(past) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nEARLIER IN THIS SESSION (the user's previous requests, oldest first, and what you answered):\n" +
		"- Use them only to understand what the new request refers to, such as \"it\", \"the same\", \"that one\" or \"again\".\n" +
		"- Answer the new request only. Do not repeat an earlier command unless the user asks for it again.\n" +
		"- The OUTPUT FORMAT rules above still apply: reply with the command only.\n")
	for i, x := range past {
		fmt.Fprintf(&b, "\n[%d] Request: %s\n", i+1, clip(x.request))
		if x.command != "" {
			fmt.Fprintf(&b, "    Command: %s\n", clip(x.command))
		}
		fmt.Fprintf(&b, "    Outcome: %s\n", x.outcome)
	}
	return b.String()
}

// errNoRawInput says that the terminal cannot be read key by key, so the
// plain way of reading a line has to do.
var errNoRawInput = errors.New("the terminal cannot be read key by key")

// edit reads a line with ed, with the terminal handing over each key as it is
// pressed. With fresh set, only what is typed from now on counts. It returns
// errNoRawInput when the terminal cannot be read that way.
func (p *pilot) edit(ed *lineEditor, fresh bool) (string, error) {
	if p.tty == nil || os.Getenv("TERM") == "dumb" {
		return "", errNoRawInput
	}
	restore, err := rawInput(p.tty, fresh)
	if err != nil {
		return "", errNoRawInput
	}
	defer restore()

	// A signal that ends the program must not leave the terminal as it is
	// now, without echo.
	sig, done := make(chan os.Signal, 1), make(chan struct{})
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	defer close(done)
	go func() {
		select {
		case s := <-sig:
			restore()
			fmt.Fprintln(p.ui.out)
			code := exitError
			if n, ok := s.(syscall.Signal); ok {
				code = 128 + int(n)
			}
			os.Exit(code)
		case <-done:
		}
	}()
	return ed.read()
}

// confirm asks whether to run the suggested command. An answer typed before
// the question was on screen does not count: it could only approve a command
// that had not been seen. In a session the answer is read key by key, so that
// Ctrl-C means no and not the end of the session.
func (p *pilot) confirm(strict bool) bool {
	if p.inSession {
		ed := &lineEditor{ui: p.ui, in: p.tty, columns: p.columns, prompt: question(p.ui, strict)}
		answer, err := p.edit(ed, true)
		if err == nil {
			return consent(answer, strict)
		}
		if !errors.Is(err, errNoRawInput) {
			return false
		}
	}
	if p.tty != nil {
		discardInput(p.tty)
	}
	return confirm(p.ui, p.in, strict)
}

// columns returns the width of the terminal, or 0 if it is unknown.
func (p *pilot) columns() int {
	if p.tty == nil {
		return 0
	}
	cols, _, ok := termSize(p.tty)
	if !ok {
		return 0
	}
	return cols
}

// separator returns what goes between two remarks on one line.
func separator() string {
	if utf8Locale() {
		return " · "
	}
	return " - "
}

// sessionCommand reports whether line is a command for the session rather
// than a request, and returns its name. Commands are a single word that
// starts with a slash, which no request is.
func sessionCommand(line string) (name string, ok bool) {
	if !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \t") {
		return "", false
	}
	return strings.ToLower(line[1:]), true
}

// session is the interactive session. It returns the exit code.
func (p *pilot) session() int {
	ui := p.ui
	p.inSession = true

	// Ctrl-C stops what is going on, a request to the model or a command that
	// is running, and never the session: those catch it themselves, and
	// catching it here as well keeps one that arrives in between from ending
	// the program.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	p.greet()
	mark := editorIndent + ui.paint(boldGreen, ">") + " "
	ed := &lineEditor{ui: ui, in: p.tty, columns: p.columns, prompt: mark, boxed: true,
		hint: "/help for commands" + separator() + "Ctrl-D to leave"}
	for {
		line, err := p.edit(ed, false)
		raw := !errors.Is(err, errNoRawInput)
		if !raw {
			// The plain way: the terminal edits the line and hands it over.
			fmt.Fprint(ui.out, "\n"+mark)
			line, err = readLine(p.in)
		}
		if err != nil {
			if !raw {
				fmt.Fprintln(ui.out)
			}
			if !errors.Is(err, io.EOF) {
				ui.errorf("reading from the terminal: %v", err)
				return exitError
			}
			return exitOK
		}
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if n := len(ed.history); n == 0 || ed.history[n-1] != line {
			ed.history = append(ed.history, line)
		}
		if raw {
			// The box is gone; the request stays on screen in its place.
			fmt.Fprintln(ui.out, mark+ui.paint(bold, line))
		}

		name, isCommand := sessionCommand(line)
		switch {
		case line == "exit" || line == "quit" || name == "exit" || name == "quit":
			return exitOK
		case isCommand:
			p.command(name)
		case line == "configure" || mistypedConfigure(line):
			p.say("The setup is not run from here. Leave with /exit and run: beastiepilot configure")
		default:
			p.answer(line)
		}
	}
}

// say prints a remark of the session.
func (p *pilot) say(text string) {
	fmt.Fprintln(p.ui.out)
	for _, line := range wrap(text, 72) {
		fmt.Fprintf(p.ui.out, "%s%s\n", editorIndent, line)
	}
}

// greet says how the session works.
func (p *pilot) greet() {
	for _, line := range wrap("Say what you want to do, in plain language. The command is shown first and runs only once you agree.", 72) {
		fmt.Fprintf(p.ui.out, "%s%s\n", editorIndent, p.ui.paint(dim, line))
	}
}

// command carries out a command of the session.
func (p *pilot) command(name string) {
	ui := p.ui
	switch name {
	case "help":
		fmt.Fprintf(ui.out, "\n  %s %s\n\n", ui.paint(boldCyan, ">"), ui.paint(bold, "Session commands"))
		for _, c := range [][2]string{
			{"/guided", "explain each command before asking"},
			{"/safe", "show each command and ask, without the explanation"},
			{"/clear", "clear the screen and forget the earlier requests"},
			{"/help", "this list"},
			{"/exit", "leave; so do exit, quit and Ctrl-D"},
		} {
			fmt.Fprintf(ui.out, "    %-9s %s\n", c[0], ui.paint(dim, c[1]))
		}
		p.say("Up and Down bring back earlier requests. Ctrl-C clears the line, stops a request that is waiting for the model and stops a command that is running.")
		p.say(fmt.Sprintf("The last %d requests go to the model along with each new one, so a request can build on the one before: \"the same for /var\".", maxPast))
	case "guided":
		p.cfg.Type = typeGuided
		p.say("Guided mode: each command comes with an explanation.")
	case "safe":
		p.cfg.Type = typeSafe
		p.say("Safe mode: each command is shown and asked about, without an explanation.")
	case "clear":
		p.past = nil
		fmt.Fprint(ui.out, "\033[H\033[2J")
		if p.cfg.Banner != bannerOff {
			printBanner(ui, p.cfg)
		}
		p.greet()
	default:
		p.say("There is no /" + name + " here. /help lists the commands.")
	}
}
