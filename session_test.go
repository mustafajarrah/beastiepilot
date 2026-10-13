package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionCommand(t *testing.T) {
	tests := []struct {
		line, name string
		ok         bool
	}{
		{"/help", "help", true},
		{"/EXIT", "exit", true},
		{"/nope", "nope", true},
		{"/etc", "etc", true}, // one word: not a request, so it is told what exists
		{"/var is full, what takes the space?", "", false},
		{"help", "", false},
		{"show /etc/rc.conf", "", false},
	}
	for _, tt := range tests {
		if name, ok := sessionCommand(tt.line); name != tt.name || ok != tt.ok {
			t.Errorf("sessionCommand(%q) = %q, %v; want %q, %v", tt.line, name, ok, tt.name, tt.ok)
		}
	}
}

func TestSessionSection(t *testing.T) {
	if got := sessionSection(nil); got != "" {
		t.Errorf("with nothing before: %q, want nothing", got)
	}

	p := &pilot{}
	p.remember("list the\ndatasets", "zfs list", "it ran and exited with status 0")
	p.remember("restart the web server", "", "you gave no command: which web server?")
	got := sessionSection(p.past)
	for _, want := range []string{
		"EARLIER IN THIS SESSION",
		"reply with the command only",
		"\n[1] Request: list the datasets\n    Command: zfs list\n    Outcome: it ran and exited with status 0\n",
		"\n[2] Request: restart the web server\n    Outcome: you gave no command: which web server?\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the section lacks %q:\n%s", want, got)
		}
	}

	// Only the last few are kept, and a long one is cut.
	for i := 0; i < maxPast+3; i++ {
		p.remember(strings.Repeat("x", maxPastChars+50), "true", "the user chose not to run it")
	}
	got = sessionSection(p.past)
	if len(p.past) != maxPast || strings.Contains(got, "zfs list") || strings.Contains(got, "[6]") {
		t.Errorf("%d requests kept, want the last %d only:\n%s", len(p.past), maxPast, got)
	}
	if want := "Request: " + strings.Repeat("x", maxPastChars) + " ...\n"; !strings.Contains(got, want) {
		t.Errorf("a long request was not cut to %d characters:\n%s", maxPastChars, got)
	}
}

// TestSession runs a session the plain way, with the lines a terminal would
// hand over, against a stand-in for Ollama.
func TestSession(t *testing.T) {
	isolate(t)
	var asked []generateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req generateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		asked = append(asked, req)
		switch {
		case strings.Contains(req.System, "patient FreeBSD mentor"):
			io.WriteString(w, `{"response":"Shows how full each filesystem is.\nEffect: nothing changes.","done":true}`)
		case strings.Contains(req.Prompt, "web server"):
			io.WriteString(w, `{"response":"# Which web server?","done":true}`)
		default:
			io.WriteString(w, `{"response":"df -h","done":true}`)
		}
	}))
	defer srv.Close()

	cfg := defaultSettings()
	cfg.Endpoint, cfg.Banner = srv.URL, bannerOff
	var out bytes.Buffer
	lines := strings.Join([]string{
		"/help",
		"",
		"show the free disk space", "n",
		"restart the web server",
		"/guided",
		"and for /var only", "",
		"/nope",
		"cofigure",
		"/clear",
		"show the free disk space", "no",
		"exit",
		"this is never read",
	}, "\n") + "\n"
	in := strings.NewReader(lines)
	p := &pilot{ui: styler{out: &out}, in: in, cfg: cfg}
	if code := p.session(); code != exitOK {
		t.Errorf("the session ended with %d, want 0", code)
	}
	if rest, _ := io.ReadAll(in); string(rest) != "this is never read\n" {
		t.Errorf("after exit, %q was left unread; want the last line only", rest)
	}

	// Four requests for a command, and an explanation for each one made
	// after /guided.
	var prompts []string
	for _, req := range asked {
		prompts = append(prompts, req.Prompt)
	}
	want := []string{
		"show the free disk space",
		"restart the web server",
		"and for /var only",
		"Request: and for /var only\nCommand: df -h",
		"show the free disk space",
		"Request: show the free disk space\nCommand: df -h",
	}
	if strings.Join(prompts, "|") != strings.Join(want, "|") {
		t.Fatalf("the model was asked %q, want %q", prompts, want)
	}

	// The first request goes alone; the third goes with the two before it.
	if strings.Contains(asked[0].System, "EARLIER IN THIS SESSION") {
		t.Errorf("the first request came with a history:\n%s", asked[0].System)
	}
	for _, want := range []string{
		"[1] Request: show the free disk space\n    Command: df -h\n    Outcome: the user chose not to run it\n",
		"[2] Request: restart the web server\n    Outcome: you gave no command: Which web server?\n",
	} {
		if !strings.Contains(asked[2].System, want) {
			t.Errorf("the third request lacks %q:\n%s", want, asked[2].System)
		}
	}
	// The explanation is about one command and takes no history, and /clear
	// forgets what came before it.
	for _, i := range []int{3, 4, 5} {
		if strings.Contains(asked[i].System, "EARLIER IN THIS SESSION") {
			t.Errorf("request %d came with a history, and should not:\n%s", i+1, asked[i].System)
		}
	}

	shown := out.String()
	for _, want := range []string{
		"Say what you want to do",
		"Session commands",
		"Which web server?",
		"Guided mode: each command comes with an explanation.",
		"Shows how full each filesystem is.",
		"There is no /nope here.",
		"The setup is not run from here.",
	} {
		if !says(shown, want) {
			t.Errorf("the session never showed %q:\n%s", want, shown)
		}
	}
	if n := strings.Count(shown, "Aborted. Nothing was executed."); n != 3 {
		t.Errorf("%d commands were turned down, want 3:\n%s", n, shown)
	}

	// The terminal going away ends the session as well.
	p = &pilot{ui: styler{out: io.Discard}, in: strings.NewReader("show the free disk space\n"), cfg: cfg}
	if code := p.session(); code != exitOK {
		t.Errorf("the session ended with %d when its input did, want 0", code)
	}
}
