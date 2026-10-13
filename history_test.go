package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// remembering returns a pilot that keeps up to limit earlier requests, in
// file if one is named, and what it prints.
func remembering(limit int, file string) (*pilot, *bytes.Buffer) {
	var out bytes.Buffer
	cfg := defaultSettings()
	cfg.History = limit
	return &pilot{ui: styler{out: &out}, cfg: cfg, history: file}, &out
}

// requests returns the requests of past, in order.
func requests(past []exchange) string {
	var all []string
	for _, x := range past {
		all = append(all, x.Request)
	}
	return strings.Join(all, "|")
}

// fileLines returns the lines of the file at path.
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func TestHistorySection(t *testing.T) {
	if got := historySection(nil); got != "" {
		t.Errorf("with nothing before: %q, want nothing", got)
	}

	p, _ := remembering(5, "")
	p.remember("list the\ndatasets", "zfs list", "it ran and exited with status 0")
	p.remember("restart the web server", "", "you gave no command: which web server?")
	got := historySection(p.past)
	for _, want := range []string{
		"EARLIER REQUESTS",
		"reply with the command only",
		"\n[1] Request: list the datasets\n    Command: zfs list\n    Outcome: it ran and exited with status 0\n",
		"\n[2] Request: restart the web server\n    Outcome: you gave no command: which web server?\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the section lacks %q:\n%s", want, got)
		}
	}

	// Only the last HISTORY are kept, and a long one is cut.
	for i := 0; i < 8; i++ {
		p.remember(strings.Repeat("x", maxPastChars+50), "true", "the user chose not to run it")
	}
	got = historySection(p.past)
	if len(p.past) != 5 || strings.Contains(got, "zfs list") || strings.Contains(got, "[6]") {
		t.Errorf("%d requests kept, want the last 5 only:\n%s", len(p.past), got)
	}
	if want := "Request: " + strings.Repeat("x", maxPastChars) + " ...\n"; !strings.Contains(got, want) {
		t.Errorf("a long request was not cut to %d characters:\n%s", maxPastChars, got)
	}

	// With HISTORY=0 nothing is.
	p, _ = remembering(0, "")
	p.remember("list the datasets", "zfs list", "it ran and exited with status 0")
	if len(p.past) != 0 {
		t.Errorf("with HISTORY=0, %d requests were kept", len(p.past))
	}
}

func TestRecent(t *testing.T) {
	var past []exchange
	for i := 1; i <= 40; i++ {
		past = append(past, exchange{Request: fmt.Sprintf("request number %02d", i), Command: "df -h", Outcome: "it ran and exited with status 0"})
	}
	header, each := tokens(historyHeader), tokens(past[0].entry(len(past)))
	tests := []struct {
		name        string
		limit, room int
		want        int // how many go along: the last ones
	}{
		{"room for all", 100, 100000, 40},
		{"no more than the limit", 10, 100000, 10},
		{"a limit of none", 0, 100000, 0},
		{"room for three", 100, header + 3*each, 3},
		{"one token short of three", 100, header + 3*each - 1, 2},
		{"room for the header alone", 100, header, 0},
		{"no room", 100, 0, 0},
		{"less than none", 100, -500, 0},
		{"the limit before the room", 2, header + 3*each, 2},
	}
	for _, tt := range tests {
		got := recent(past, tt.limit, tt.room)
		if len(got) != tt.want {
			t.Errorf("%s: %d went along, want %d", tt.name, len(got), tt.want)
			continue
		}
		if tt.want > 0 {
			if first := fmt.Sprintf("request number %02d", 40-tt.want+1); got[0].Request != first || got[len(got)-1].Request != "request number 40" {
				t.Errorf("%s: from %q to %q, want from %q to the last", tt.name, got[0].Request, got[len(got)-1].Request, first)
			}
			if n := tokens(historySection(got)); n > tt.room {
				t.Errorf("%s: the section takes %d tokens, and there was room for %d", tt.name, n, tt.room)
			}
		}
	}

	// When the latest does not fit, none do: what goes along has no gaps.
	past = append(past, exchange{Request: strings.Repeat("x", 300), Command: strings.Repeat("y", 300), Outcome: "it ran and exited with status 0"})
	if got := recent(past, 100, header+3*each); len(got) != 0 {
		t.Errorf("%d went along past one that did not fit", len(got))
	}
}

func TestHistoryFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "beastiepilot")
	file := filepath.Join(dir, "history")

	p, out := remembering(3, file)
	p.recall() // there is none yet
	p.remember("install nginx and\nstart it", "pkg install nginx && service nginx start > /dev/null", "the user chose not to run it")
	p.remember("restart the web server", "", "you gave no command: which web server?")
	if out.Len() != 0 {
		t.Errorf("keeping two requests said %q", out.String())
	}

	// The file is its owner's alone and holds a request to a line, as it goes
	// to the model.
	for path, want := range map[string]os.FileMode{file: 0o600, dir: 0o700} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %v", path, info.Mode().Perm(), err, want)
		}
	}
	lines := fileLines(t, file)
	if len(lines) != 2 {
		t.Fatalf("the file holds %d lines, want 2:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if want := `"request":"install nginx and start it","command":"pkg install nginx && service nginx start > /dev/null","outcome":"the user chose not to run it"}`; !strings.HasSuffix(lines[0], want) {
		t.Errorf("the first line is %s, want it to end with %s", lines[0], want)
	}
	if strings.Contains(lines[1], `"command"`) {
		t.Errorf("a request that got no command was kept with one: %s", lines[1])
	}
	var first exchange
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Time.IsZero() || !first.Time.Equal(p.past[0].Time) {
		t.Errorf("the first line reads back as %+v, %v; want the time it was kept at", first, err)
	}

	// The next run has them, and adds its own after them.
	p, _ = remembering(3, file)
	p.recall()
	if got := requests(p.past); got != "install nginx and start it|restart the web server" {
		t.Errorf("the next run recalls %q", got)
	}
	p.remember("three", "true", "it ran and exited with status 0")
	p.remember("four", "true", "it ran and exited with status 0")
	if got := requests(p.past); got != "restart the web server|three|four" {
		t.Errorf("with a fourth, it has %q; want the last three", got)
	}

	// A run that asks for fewer gets the latest, and the file is left alone
	// while it holds no more than twice that.
	p, _ = remembering(2, file)
	p.recall()
	if got := requests(p.past); got != "three|four" || len(fileLines(t, file)) != 4 {
		t.Errorf("with HISTORY=2 it recalls %q from a file of %d lines; want the last two of four", got, len(fileLines(t, file)))
	}

	// A line that is no request is passed over, and one written by hand is
	// held to what a kept one may be.
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "not a request\n{\"request\":\"\",\"command\":\"true\",\"outcome\":\"nothing\"}\n\n{\"request\":%q,\"outcome\":\"two\\nlines\"}\n", strings.Repeat("y", maxPastChars+100))
	f.Close()
	p, out = remembering(10, file)
	p.recall()
	if len(p.past) != 5 || out.Len() != 0 {
		t.Fatalf("it recalls %d requests and says %q; want 5 and nothing", len(p.past), out.String())
	}
	if last := p.past[4]; last.Request != strings.Repeat("y", maxPastChars)+" ..." || last.Outcome != "two lines" {
		t.Errorf("the one written by hand came back as %+v", last)
	}

	// Once it holds more than twice HISTORY, the file is cut down to that.
	p, out = remembering(2, file)
	p.recall()
	lines = fileLines(t, file)
	if got := requests(p.past); len(p.past) != 2 || p.past[0].Request != "four" || len(lines) != 2 || !strings.Contains(lines[0], `"request":"four"`) || out.Len() != 0 {
		t.Errorf("it recalls %q from a file now of %d lines, and says %q; want the last two in both", got, len(lines), out.String())
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the file cut down has mode %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("cutting the file down left %d files behind, want the one", len(entries))
	}

	// /clear forgets them for good, and may be asked twice.
	for i := 0; i < 2; i++ {
		if err := p.forget(); err != nil || len(p.past) != 0 {
			t.Errorf("forget: %v, with %d left", err, len(p.past))
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the file is still there once forgotten: %v", err)
	}

	// With HISTORY=0 the file is neither read nor written.
	p, _ = remembering(5, file)
	p.remember("kept", "true", "it ran and exited with status 0")
	p, _ = remembering(0, file)
	p.recall()
	p.remember("not kept", "true", "it ran and exited with status 0")
	if len(p.past) != 0 || len(fileLines(t, file)) != 1 {
		t.Errorf("with HISTORY=0 it has %d requests and the file %d lines; want none and the one", len(p.past), len(fileLines(t, file)))
	}
}

// A file that cannot be used is said once, and the run still remembers.
func TestHistoryFileUnusable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, []byte("a file where a directory belongs"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, out := remembering(5, filepath.Join(blocked, "beastiepilot", "history"))
	p.recall()
	p.remember("one", "true", "it ran and exited with status 0")
	p.remember("two", "true", "it ran and exited with status 0")
	if got := requests(p.past); got != "one|two" {
		t.Errorf("it has %q, want both requests", got)
	}
	if n := strings.Count(out.String(), "!"); n != 1 || !says(out.String(), "The history file cannot be used, so requests are kept for this run only:") {
		t.Errorf("it warned %d times, want once:\n%s", n, out.String())
	}
	if err := p.forget(); err == nil {
		t.Error("forgetting a file that cannot be reached reported nothing")
	}
}

// TestHistoryGoesAlong takes requests through to the model in one run after
// another, each run a pilot of its own as each start of the program is.
func TestHistoryGoesAlong(t *testing.T) {
	isolate(t)
	var asked []generateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req generateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		asked = append(asked, req)
		io.WriteString(w, `{"response":"df -h","done":true}`)
	}))
	defer srv.Close()

	cfg := defaultSettings()
	cfg.Endpoint, cfg.Banner, cfg.Verbose = srv.URL, bannerOff, true
	// run starts the program again with one request, turns the command down
	// and returns what the model was given and what was shown.
	run := func(cfg settings, request string) (generateRequest, string) {
		t.Helper()
		var out bytes.Buffer
		p := &pilot{ui: styler{out: &out}, in: strings.NewReader("n\n"), cfg: cfg, history: historyPath()}
		p.recall()
		if code := p.answer(request); code != exitOK {
			t.Fatalf("%q ended with %d:\n%s", request, code, out.String())
		}
		return asked[len(asked)-1], out.String()
	}
	entries := func(req generateRequest) int { return strings.Count(req.System, "] Request: ") }

	// The first request there ever was goes by itself.
	req, shown := run(cfg, "show the free disk space")
	if strings.Contains(req.System, "EARLIER REQUESTS") || says(shown, "went along") {
		t.Errorf("the first request came with a history:\n%s\n%s", req.System, shown)
	}
	if req.Options["num_ctx"] != float64(defaultContext) {
		t.Errorf("the context asked for was %v, want %d", req.Options["num_ctx"], defaultContext)
	}

	// The next run sends it along.
	req, shown = run(cfg, "and for /var only")
	if want := "\n[1] Request: show the free disk space\n    Command: df -h\n    Outcome: the user chose not to run it\n"; !strings.Contains(req.System, want) {
		t.Errorf("the second run's request lacks %q:\n%s", want, req.System)
	}
	if !says(shown, "history 1 earlier request went along") {
		t.Errorf("the details do not say that one went along:\n%s", shown)
	}

	// With more kept than the context has room for, the latest go, as many
	// as fit, between the rules and the request.
	p, _ := remembering(maxHistory, historyPath())
	for i := 0; i < 150; i++ {
		p.remember(fmt.Sprintf("request number %03d, about nothing in particular", i), "ls -l /usr/local/etc/rc.d", "it ran and exited with status 0")
	}
	req, shown = run(cfg, "what next")
	n := entries(req)
	if n < 20 || n >= 100 {
		t.Fatalf("%d earlier requests went along, want as many of the last 100 as fit in %d tokens", n, cfg.Context)
	}
	if used := tokens(req.System + req.Prompt); used > cfg.Context-replyTokens {
		t.Errorf("the request takes %d tokens, which leaves less than %d of %d for the answer", used, replyTokens, cfg.Context)
	}
	if !strings.Contains(req.System, "request number 149") || strings.Contains(req.System, fmt.Sprintf("request number %03d", 149-n)) {
		t.Errorf("what went along is not the last %d:\n%s", n, req.System)
	}
	if !strings.HasPrefix(req.System, systemPrompt) || strings.Index(req.System, "HOST CONTEXT") > strings.Index(req.System, "EARLIER REQUESTS") {
		t.Errorf("the earlier requests do not follow the rules:\n%s", req.System)
	}
	if want := fmt.Sprintf("history %d of 100 earlier requests went along; CONTEXT=4096 has no room for the rest", n); !says(shown, want) {
		t.Errorf("the details lack %q:\n%s", want, shown)
	}

	// A larger context takes all that HISTORY allows.
	large := cfg
	large.Context = 16384
	req, shown = run(large, "and then")
	if entries(req) != 100 || req.Options["num_ctx"] != float64(16384) || !says(shown, "history 100 earlier requests went along") {
		t.Errorf("with CONTEXT=16384, %d went along in a context of %v:\n%s", entries(req), req.Options["num_ctx"], shown)
	}
	if !strings.Contains(req.System, "\n[99] Request: request number 149,") || !strings.Contains(req.System, "\n[100] Request: what next\n") {
		t.Errorf("the last two are not the latest two:\n%s", req.System[len(req.System)-400:])
	}

	// A smaller HISTORY sends that many, and none sends none.
	few := cfg
	few.History = 10
	if req, _ = run(few, "once more"); entries(req) != 10 || !strings.Contains(req.System, "\n[10] Request: and then\n") {
		t.Errorf("with HISTORY=10, %d went along:\n%s", entries(req), req.System)
	}
	none := cfg
	none.History = 0
	if req, shown = run(none, "alone"); strings.Contains(req.System, "EARLIER REQUESTS") || says(shown, "went along") {
		t.Errorf("with HISTORY=0 the request came with a history:\n%s", req.System)
	}
}

func TestHistoryRemarks(t *testing.T) {
	cfg := defaultSettings()
	if got := historyHelp(cfg); !strings.Contains(got, "Up to 100 earlier requests, from this session and the ones before it,") || !strings.Contains(got, "/clear forgets them") {
		t.Errorf("the help says %q", got)
	}
	cfg.History = 1
	if got := historyHelp(cfg); !strings.Contains(got, "Up to 1 earlier request, ") {
		t.Errorf("with one, the help says %q", got)
	}
	cfg.History = 0
	if got := historyHelp(cfg); !strings.Contains(got, "HISTORY is 0") {
		t.Errorf("with none, the help says %q", got)
	}

	// A model that did not answer in time may have had much to read.
	p := &pilot{sent: manyPast - 1}
	if got := p.muchToRead(); got != "" {
		t.Errorf("with few earlier requests: %q, want nothing", got)
	}
	p.sent = 40
	if got, want := p.muchToRead(), " The model had 40 earlier requests to read with this one: a lower HISTORY gives it less."; got != want {
		t.Errorf("muchToRead() = %q, want %q", got, want)
	}
	p.inSession = true
	if got := p.muchToRead(); !strings.HasSuffix(got, "a lower HISTORY gives it less, and /clear forgets them.") {
		t.Errorf("in a session, muchToRead() = %q", got)
	}
}
