package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionCommand(t *testing.T) {
	tests := []struct {
		line, name, arg string
		ok              bool
	}{
		{"/help", "help", "", true},
		{"/EXIT", "exit", "", true},
		{"/nope", "nope", "", true},
		{"/etc", "etc", "", true}, // one word: not a request, so it is told what exists
		{"/type", "type", "", true},
		{"/type guided", "type", "guided", true},
		{"/Type  SAFE", "type", "SAFE", true},
		{"/type of filesystem on /var", "", "", false},
		{"/help me", "", "", false}, // only /type takes a word
		{"/var is full, what takes the space?", "", "", false},
		{"help", "", "", false},
		{"show /etc/rc.conf", "", "", false},
	}
	for _, tt := range tests {
		if name, arg, ok := sessionCommand(tt.line); name != tt.name || arg != tt.arg || ok != tt.ok {
			t.Errorf("sessionCommand(%q) = %q, %q, %v; want %q, %q, %v", tt.line, name, arg, ok, tt.name, tt.arg, tt.ok)
		}
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
		"/type guided",
		"and for /var only", "",
		"/nope",
		"/guided", // gone: /type does that now
		"/type verbose",
		"cofigure",
		"/clear",
		"show the free disk space", "no",
		"/type", "1", // the menu, the plain way: by number
		"show the free disk space", "n",
		"/type", "", // and left alone
		"/type", "GUIDED", // or by name
		"/type", "3", // not one of them
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

	// Five requests for a command, and an explanation for each one made
	// while the type was guided.
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
		"show the free disk space", // after "/type" and "1": safe again, no explanation
	}
	if strings.Join(prompts, "|") != strings.Join(want, "|") {
		t.Fatalf("the model was asked %q, want %q", prompts, want)
	}

	// The first request goes alone; the third goes with the two before it.
	if strings.Contains(asked[0].System, "EARLIER REQUESTS") {
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
		if strings.Contains(asked[i].System, "EARLIER REQUESTS") {
			t.Errorf("request %d came with a history, and should not:\n%s", i+1, asked[i].System)
		}
	}

	shown := out.String()
	for _, want := range []string{
		"Say what you want to do",
		"Session commands",
		"/clear clear the screen and forget the earlier requests, for good",
		"Up to 100 earlier requests, from this session and the ones before it, go to the model along with each new one,",
		"The earlier requests are forgotten.",
		"Which web server?",
		"Guided mode: each command comes with an explanation.",
		"Shows how full each filesystem is.",
		"There is no /nope here.",
		"There is no /guided here.",
		`There is no type "verbose": the types are safe and guided.`,
		"The setup is not run from here.",
		"Type  what is done with each command",
		"1. safe    show each command, then ask",
		"2. guided  explain each command first, then ask  (current)",
		"Which one? [1-2, or Enter to leave it]",
		"Safe mode: each command is shown and asked about, without an explanation.",
		"Kept safe mode.",
		"Kept guided mode.",
	} {
		if !says(shown, want) {
			t.Errorf("the session never showed %q:\n%s", want, shown)
		}
	}
	if n := strings.Count(shown, "Aborted. Nothing was executed."); n != 4 {
		t.Errorf("%d commands were turned down, want 4:\n%s", n, shown)
	}
	if p.cfg.Type != typeGuided {
		t.Errorf("the session ended in %s mode, want guided: the last choice made was GUIDED, and 3 is none", p.cfg.Type)
	}

	// The terminal going away ends the session as well.
	p = &pilot{ui: styler{out: io.Discard}, in: strings.NewReader("show the free disk space\n"), cfg: cfg}
	if code := p.session(); code != exitOK {
		t.Errorf("the session ended with %d when its input did, want 0", code)
	}
}

// TestSessionSetbacks covers a model that does not answer in time and a
// command that ends with a status worth explaining: the session says what
// happened, carries on, and tells the model about it with the next request.
func TestSessionSetbacks(t *testing.T) {
	isolate(t)
	var asked []generateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			io.WriteString(w, `{"models":[{"name":"llama3.2:latest","model":"llama3.2:latest","size_vram":0}]}`)
			return
		}
		var req generateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		asked = append(asked, req)
		switch {
		case strings.Contains(req.Prompt, "slowly"):
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		case strings.Contains(req.Prompt, "look for"):
			io.WriteString(w, `{"response":"grep -q beastiepilot /dev/null","done":true}`)
		default:
			io.WriteString(w, `{"response":"df -h","done":true}`)
		}
	}))
	defer srv.Close()

	cfg := defaultSettings()
	cfg.Endpoint, cfg.Banner, cfg.Timeout = srv.URL+"/api/generate", bannerOff, 200*time.Millisecond
	var out bytes.Buffer
	in := strings.NewReader("answer slowly\nlook for it\ny\nand elsewhere\nn\n")
	p := &pilot{ui: styler{out: &out}, in: in, cfg: cfg}
	if code := p.session(); code != exitOK {
		t.Errorf("the session ended with %d, want 0", code)
	}
	if len(asked) != 3 {
		t.Fatalf("the model was asked %d times, want 3", len(asked))
	}

	shown := out.String()
	for _, want := range []string{
		"no answer from llama3.2 within 200ms.",
		"Ollama has the model loaded now and runs it on the CPU alone",
		"asking again may be enough.",
		"beastiepilot -timeout 10m",
		"Exit status 1: grep ends that way when nothing matches.",
	} {
		if !says(shown, want) {
			t.Errorf("the session never showed %q:\n%s", want, shown)
		}
	}
	// A request that got no answer leaves nothing to build on; the search
	// that found nothing does.
	if strings.Contains(asked[2].System, "answer slowly") {
		t.Errorf("the request that timed out went along with a later one:\n%s", asked[2].System)
	}
	if want := "Command: grep -q beastiepilot /dev/null\n    Outcome: it ran and exited with status 1, as grep does when nothing matches\n"; !strings.Contains(asked[2].System, want) {
		t.Errorf("the last request lacks %q:\n%s", want, asked[2].System)
	}
}
