package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanCommand(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "pkg info", "pkg info"},
		{"surrounding whitespace", "  pkg info \n", "pkg info"},
		{"fenced with language", "```sh\npkg info\n```", "pkg info"},
		{"fenced without language", "```\npkg info\n```", "pkg info"},
		{"fenced on one line", "```pkg info```", "pkg info"},
		{"inline code", "`pkg info`", "pkg info"},
		{"shell prompt", "$ pkg info", "pkg info"},
		{"reasoning block", "<think>hmm</think>\npkg info", "pkg info"},
		{"reasoning then inline code", "<think>hmm</think>\n`pkg info`", "pkg info"},
		{"windows newlines", "echo a\r\necho b", "echo a\necho b"},
		{"command substitution kept", "echo `date` and `uname`", "echo `date` and `uname`"},
		{"variable kept", "echo $HOME", "echo $HOME"},
		{"only whitespace", " \n ", ""},
	}
	for _, tt := range tests {
		if got := cleanCommand(tt.in); got != tt.want {
			t.Errorf("%s: cleanCommand(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}

func TestMistypedConfigure(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"configure", false}, // the command itself
		{"cofigure", true},
		{"confgure", true},
		{"confiugre", true},
		{"configur", true},
		{"configured", true},
		{"configurar", true},
		{"Configure", true},
		{"CONFIGURE", true},
		{"config", true},
		{"conf", true},
		{"con", false},
		{"figure", false},
		{"uptime", false},
		{"reboot", false},
		{"cofigure nginx", false}, // a request like any other
		{"configure nginx to listen on port 8080", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := mistypedConfigure(tt.in); got != tt.want {
			t.Errorf("mistypedConfigure(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestFoundNothing(t *testing.T) {
	tests := []struct {
		command string
		status  int
		want    string
	}{
		{"pkg search hyperland", 1, "pkg search"},
		{"pkg search -o hyprland", 1, "pkg search"},
		{"grep -r beastie /etc", 1, "grep"},
		{"pgrep sshd", 1, "pgrep"},
		{"which hx", 1, "which"},
		{"pkg search hyperland", 0, ""},
		{"pkg search hyperland", 70, ""}, // another status is a failure
		{"grep -r beastie /etc", 2, ""},
		{"pkg install hyprland", 1, ""},
		{"pkg info hyprland", 1, ""},
		{"ls /nonexistent", 1, ""},
		{"ps aux | grep sshd", 1, ""},                // whose status it is cannot be told
		{"pkg update && pkg search hyprland", 1, ""}, // nor here
		{"grep $(cat words) file", 1, ""},
		{"", 1, ""},
	}
	for _, tt := range tests {
		if got := foundNothing(tt.command, tt.status); got != tt.want {
			t.Errorf("foundNothing(%q, %d) = %q, want %q", tt.command, tt.status, got, tt.want)
		}
	}
}

func TestWithTag(t *testing.T) {
	for name, want := range map[string]string{
		"llama3.2":                "llama3.2:latest",
		"llama3.2:1b":             "llama3.2:1b",
		"me/model":                "me/model:latest",
		"localhost:5000/me/model": "localhost:5000/me/model:latest",
		"localhost:5000/me/m:q4":  "localhost:5000/me/m:q4",
	} {
		if got := withTag(name); got != want {
			t.Errorf("withTag(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestWhySlow checks what is said about a model that did not answer in time,
// from what Ollama reports about it afterwards.
func TestWhySlow(t *testing.T) {
	tests := []struct {
		name, ps string
		status   int
		want     string
	}{
		{"on the CPU", `{"models":[{"name":"llama3.2:latest","model":"llama3.2:latest","size":3562954752,"size_vram":0}]}`, 200, "on the CPU alone"},
		{"on a GPU", `{"models":[{"name":"llama3.2:latest","model":"llama3.2:latest","size":3562954752,"size_vram":3562954752}]}`, 200, "has the model loaded now: asking again"},
		{"another model only", `{"models":[{"name":"nomic-embed-text:latest","model":"nomic-embed-text:latest","size_vram":0}]}`, 200, "does not have the model loaded"},
		{"nothing loaded", `{"models":[]}`, 200, "does not have the model loaded"},
		{"an Ollama without the list", `404 page not found`, 404, ""},
		{"not Ollama", `<html>`, 200, ""},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/ps" {
				t.Errorf("%s: asked %s %s, want GET /api/ps", tt.name, r.Method, r.URL.Path)
			}
			w.WriteHeader(tt.status)
			io.WriteString(w, tt.ps)
		}))
		cfg := defaultSettings()
		cfg.Endpoint = srv.URL + "/api/generate"
		got := whySlow(cfg)
		srv.Close()
		if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
			t.Errorf("%s: whySlow() = %q, want it to say %q", tt.name, got, tt.want)
		}
	}

	// An Ollama that is gone says nothing either.
	cfg := defaultSettings()
	cfg.Endpoint = "http://127.0.0.1:1/api/generate"
	if got := whySlow(cfg); got != "" {
		t.Errorf("with no Ollama, whySlow() = %q, want nothing", got)
	}
}

func TestEditDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "ac", 1},
		{"abc", "acb", 1}, // neighbours swapped
		{"cofigure", "configure", 1},
		{"config", "configure", 3},
		{"kitten", "sitting", 3},
	}
	for _, tt := range tests {
		if got := editDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := editDistance(tt.b, tt.a); got != tt.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tt.b, tt.a, got, tt.want)
		}
	}
}

func TestValidateCommand(t *testing.T) {
	tests := []struct {
		name, in string
		ok       bool
	}{
		{"plain", "pkg info", true},
		{"multi-line with tab", "echo a\n\techo b", true},
		{"non-ASCII text", "echo olá", true},
		{"empty", "", false},
		{"escape sequence", "echo safe\x1b[2K", false},
		{"carriage return", "echo safe\rrm -rf /", false},
		{"bidi override", "echo ‮evil", false},
		{"zero-width space", "echo a​b", false},
		{"NUL", "echo a\x00b", false},
	}
	for _, tt := range tests {
		if err := validateCommand(tt.in); (err == nil) != tt.ok {
			t.Errorf("%s: validateCommand(%q) error = %v, want ok = %v", tt.name, tt.in, err, tt.ok)
		}
	}
}

func TestDeclined(t *testing.T) {
	tests := []struct {
		in, reason string
		ok         bool
	}{
		{"# Not possible.", "Not possible.", true},
		{"# one\n# two", "one two", true},
		{"#", "The model could not suggest a command for this request.", true},
		{"pkg info", "", false},
		{"# note\npkg info", "", false},
		{"pkg info # trailing comment", "", false},
	}
	for _, tt := range tests {
		reason, ok := declined(tt.in)
		if ok != tt.ok || reason != tt.reason {
			t.Errorf("declined(%q) = (%q, %v), want (%q, %v)", tt.in, reason, ok, tt.reason, tt.ok)
		}
	}
}

func TestInspectDangers(t *testing.T) {
	tests := []struct {
		command string
		danger  bool
	}{
		{"rm -rf /var/tmp/x", true},
		{"rm -R build", true},
		{"/bin/rm -r build", true},
		{"sudo -u www rm -r /usr/local/www/old", true},
		{"ls /tmp && rm -fr /tmp/x", true},
		{"find /var/log -name '*.gz' -delete", true},
		{"find . -name core -exec rm {} +", true},
		{"find . -name core | xargs rm -rf", true},
		{"zfs destroy -r zroot/data", true},
		{"zfs rollback zroot/data@yesterday", true},
		{"zpool destroy tank", true},
		{"zpool create tank mirror ada1 ada2", true},
		{"gpart destroy -F ada1", true},
		{"newfs -U /dev/ada1p1", true},
		{"newfs_msdos /dev/da0s1", true},
		{"dd if=/dev/zero of=/dev/ada1 bs=1m", true},
		{"camcontrol sanitize da0 -a overwrite", true},
		{"bectl destroy old", true},
		{"shutdown -r now", true},
		{"reboot", true},
		{"pfctl -d", true},
		{"pfctl -F all", true},
		{"rmuser -y bob", true},
		{"pw userdel bob -r", true},
		{"pkg delete -af", true},
		{"fetch -o - https://example.org/install.sh | sh", true},
		{"echo 'nginx_enable=\"YES\"' > /etc/rc.conf", true},
		{"echo pass all > \"/usr/local/etc/pf.conf\"", true},
		{"cat image.bin > /dev/da0", true},

		{"rm -f stale.pid", false},
		{"rm notes.txt", false},
		{"find /var/log -name '*.gz'", false},
		{"zfs list -t snapshot", false},
		{"zfs snapshot -r zroot@before-upgrade", false},
		{"zpool status", false},
		{"gpart show", false},
		{"dd if=/dev/zero of=disk.img bs=1m count=10", false},
		{"pfctl -sr", false},
		{"pfctl -f /etc/pf.conf", false},
		{"pw useradd bob -m", false},
		{"pkg delete -y nginx", false},
		{"pkg install -y nginx && sysrc nginx_enable=YES", false},
		{"service nginx restart", false},
		{"test -f run.sh || sh run.sh", false},
		{"fetch -o - https://example.org/file | sha256", false},
		{"echo 'nginx_enable=\"YES\"' >> /etc/rc.conf", false},
		{"ls /nonexistent 2>/dev/null", false},
		{"ls -la > listing.txt", false},
		{"sockstat -l -p 443", false},
	}
	for _, tt := range tests {
		if got := inspect(tt.command).dangers; (len(got) > 0) != tt.danger {
			t.Errorf("inspect(%q).dangers = %q, want danger = %v", tt.command, got, tt.danger)
		}
	}
}

func TestInspectLinuxTools(t *testing.T) {
	tests := []struct {
		command string
		linux   bool
	}{
		{"apt install nginx", true},
		{"sudo apt-get update", true},
		{"env DEBIAN_FRONTEND=noninteractive apt-get install -y nginx", true},
		{"systemctl enable --now nginx", true},
		{"ps aux | free -m", true},
		{"ip addr show", true},
		{"/usr/sbin/iptables -L", true},

		{"pkg install nginx", false},
		{"service nginx start", false},
		{"grep ip /etc/hosts", false},
		{"ifconfig | grep inet", false},
		{"pkg search apt", false},
		{"jls", false},
	}
	for _, tt := range tests {
		if got := inspect(tt.command).notices; (len(got) > 0) != tt.linux {
			t.Errorf("inspect(%q).notices = %q, want linux = %v", tt.command, got, tt.linux)
		}
	}
}

func TestInspectReportsEachFindingOnce(t *testing.T) {
	rev := inspect("rm -rf a; rm -rf b; apt update && apt upgrade")
	if len(rev.dangers) != 1 || len(rev.notices) != 1 {
		t.Errorf("got %d dangers and %d notices, want 1 and 1: %+v", len(rev.dangers), len(rev.notices), rev)
	}
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		input  string
		strict bool
		want   bool
	}{
		{"y\n", false, true},
		{"Y\n", false, true},
		{"yes\n", false, true},
		{" YES \r\n", false, true},
		{"\n", false, false},
		{"n\n", false, false},
		{"yep\n", false, false},
		{"", false, false},
		{"y", false, false}, // no Enter: not explicit consent

		{"yes\n", true, true},
		{" Yes \n", true, true},
		{"y\n", true, false},
		{"\n", true, false},
		{"", true, false},
	}
	ui := styler{out: io.Discard}
	for _, tt := range tests {
		// Running as root changes the words of the question, not what counts.
		for _, asRoot := range []bool{false, true} {
			if got := confirm(ui, strings.NewReader(tt.input), tt.strict, asRoot); got != tt.want {
				t.Errorf("confirm(%q, strict=%v, asRoot=%v) = %v, want %v", tt.input, tt.strict, asRoot, got, tt.want)
			}
		}
	}

	for want, asked := range map[string]string{
		"  ? Execute this command? [y/N] ":                     question(ui, false, false),
		"  ? Execute this command as root? [y/N] ":             question(ui, false, true),
		`  ? Type "yes" to execute it anyway [yes/N] `:         question(ui, true, false),
		`  ? Type "yes" to execute it as root anyway [yes/N] `: question(ui, true, true),
	} {
		if asked != want {
			t.Errorf("the question is %q, want %q", asked, want)
		}
	}
}

func TestReadLineStopsAtNewline(t *testing.T) {
	r := strings.NewReader("y\nrest of input\n")
	line, err := readLine(r)
	if err != nil || line != "y" {
		t.Fatalf("readLine = (%q, %v), want (\"y\", nil)", line, err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "rest of input\n" {
		t.Errorf("readLine consumed too much; remaining input = %q", rest)
	}
}

func TestSuggest(t *testing.T) {
	var got generateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		switch got.Model {
		case "missing":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"model 'missing' not found"}`)
		case "broken":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "boom")
		case "garbled":
			io.WriteString(w, "not json")
		default:
			io.WriteString(w, `{"response":"pkg info","done":true}`)
		}
	}))
	defer srv.Close()

	reply, err := suggest(context.Background(), srv.URL, "llama3.2", defaultContext, "be strict", "list packages")
	if err != nil || reply != "pkg info" {
		t.Fatalf("suggest = (%q, %v), want (\"pkg info\", nil)", reply, err)
	}
	if got.System != "be strict" || got.Prompt != "list packages" || got.Stream {
		t.Errorf("unexpected request: %+v", got)
	}
	if temp, ok := got.Options["temperature"].(float64); !ok || temp != 0 {
		t.Errorf("temperature = %v, want 0", got.Options["temperature"])
	}

	for model, want := range map[string]string{
		"missing": `ollama pull missing`,
		"broken":  "500 Internal Server Error: boom",
		"garbled": "decoding Ollama response",
	} {
		if _, err := suggest(context.Background(), srv.URL, model, defaultContext, "", "x"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("model %q: error = %v, want it to mention %q", model, err, want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := suggest(ctx, srv.URL, "llama3.2", defaultContext, "", "x"); err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("cancelled context: error = %v, want context canceled", err)
	}
}

func TestSystemPromptCoversFreeBSDTools(t *testing.T) {
	for _, word := range []string{"pkg", "service", "sysrc", "pfctl", "zfs", "jls", "apt", "systemctl"} {
		if !strings.Contains(systemPrompt, word) {
			t.Errorf("system prompt does not mention %q", word)
		}
	}
}

func TestCleanExplanation(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "Lists jails.\nEffect: nothing changes.", "Lists jails.\nEffect: nothing changes."},
		{"markdown", "**jls** lists `jails`.", "jls lists jails."},
		{"blank lines and indentation", "\n  one\n\n\n  two  \n", "one\ntwo"},
		{"reasoning block", "<think>hmm</think>\nIt lists jails.", "It lists jails."},
		{"escape sequences", "safe\x1b[2J\x1b[31mtext", "safe[2J[31mtext"},
		{"carriage return and bidi", "a\rb\u202ec", "abc"},
		{"tabs", "a\tb", "a b"},
		{"empty", "  \n ", ""},
	}
	for _, tt := range tests {
		if got := cleanExplanation(tt.in); got != tt.want {
			t.Errorf("%s: cleanExplanation(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}

	long := strings.Repeat("line\n", 3*maxExplanationLines)
	if got := strings.Count(cleanExplanation(long), "\n") + 1; got != maxExplanationLines {
		t.Errorf("a long explanation was cut to %d lines, want %d", got, maxExplanationLines)
	}
	for _, r := range cleanExplanation("x\x1b]0;title\x07y\x00z") {
		if r < ' ' && r != '\n' {
			t.Errorf("control character %U survived cleaning", r)
		}
	}
}

func TestWrap(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  []string
	}{
		{"one two three", 20, []string{"one two three"}},
		{"one two three", 7, []string{"one two", "three"}},
		{"one two three", 3, []string{"one", "two", "three"}},
		{"first line\nsecond", 20, []string{"first line", "second"}},
		{"supercalifragilistic word", 5, []string{"supercalifragilistic", "word"}},
		{"ação não é ASCII", 8, []string{"ação não", "é ASCII"}},
	}
	for _, tt := range tests {
		got := wrap(tt.in, tt.width)
		if strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("wrap(%q, %d) = %q, want %q", tt.in, tt.width, got, tt.want)
		}
	}
}

func TestMascotGeometry(t *testing.T) {
	for name, m := range map[string]mascot{"blocks": beastieBlocks, "ascii": beastieASCII} {
		if len(m.rows) != len(m.colors) {
			t.Fatalf("%s: %d rows but %d color rows", name, len(m.rows), len(m.colors))
		}
		if m.infoRow+4 > len(m.rows) {
			t.Errorf("%s: no room for four lines of text from row %d", name, m.infoRow)
		}
		for i, row := range m.rows {
			if n := utf8.RuneCountInString(row); n != len(m.colors[i]) {
				t.Errorf("%s row %d: %d characters but %d color letters", name, i, n, len(m.colors[i]))
			}
			for _, c := range []byte(m.colors[i]) {
				if _, ok := mascotColors[c]; !ok && c != ' ' {
					t.Errorf("%s row %d: unknown color letter %q", name, i, c)
				}
			}
		}
	}
	for i, row := range beastieASCII.rows {
		if len(row) != utf8.RuneCountInString(row) {
			t.Errorf("ASCII mascot row %d contains non-ASCII characters", i)
		}
	}
}

func TestMascotRenderWithoutColor(t *testing.T) {
	ui := styler{out: io.Discard}
	for i, row := range beastieBlocks.rows {
		if got, n := beastieBlocks.render(ui, i); got != row || n != 10 {
			t.Errorf("row %d: render = (%q, %d), want (%q, 10)", i, got, n, row)
		}
	}
}

func TestUTF8Locale(t *testing.T) {
	tests := []struct {
		lcAll, lcCtype, lang string
		want                 bool
	}{
		{"", "", "en_US.UTF-8", true},
		{"", "", "C.utf8", true},
		{"", "", "C", false},
		{"", "", "", false},
		{"", "pt_BR.UTF-8", "C", true},
		{"C", "pt_BR.UTF-8", "en_US.UTF-8", false},
	}
	for _, tt := range tests {
		t.Setenv("LC_ALL", tt.lcAll)
		t.Setenv("LC_CTYPE", tt.lcCtype)
		t.Setenv("LANG", tt.lang)
		if got := utf8Locale(); got != tt.want {
			t.Errorf("utf8Locale() with LC_ALL=%q LC_CTYPE=%q LANG=%q = %v, want %v", tt.lcAll, tt.lcCtype, tt.lang, got, tt.want)
		}
	}
}

func TestEndpointHost(t *testing.T) {
	for in, want := range map[string]string{
		defaultEndpoint: "localhost:11434",
		"https://ollama.example.org/api/generate": "ollama.example.org",
		"not a url": "not a url",
	} {
		if got := endpointHost(in); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommented(t *testing.T) {
	installed := wordSet("pkg df du which service cd /usr/sbin/pkg")
	lookPath := func(name string) (string, error) {
		if installed[name] {
			return name, nil
		}
		return "", errors.New("not found")
	}
	tests := []struct {
		reply, want string
	}{
		// A command where the refusal goes.
		{"# pkg install xorg-wayland", "pkg install xorg-wayland"},
		{"#pkg install xorg", "pkg install xorg"},
		{"## df -h\n", "df -h"},
		{"  # du -sh .", "du -sh ."},
		{"# cd /usr/ports/www/nginx && make install clean", "cd /usr/ports/www/nginx && make install clean"},
		{"# ASSUME_ALWAYS_YES=yes pkg install nginx", "ASSUME_ALWAYS_YES=yes pkg install nginx"},
		{"# /usr/sbin/pkg info", "/usr/sbin/pkg info"},
		{"# df", "df"},

		// A refusal in words.
		{"# This cannot be done with a shell command.", ""},
		{"# Which web server?", ""},
		{"# which web server?", ""},
		{"# pkg is not installed here", ""},
		{"# pkg cannot find a package of that name", ""},
		{"# pkg não achou esse pacote", ""},
		{"# service names are needed for that.", ""},
		{"# df: which filesystem", ""},
		{"# nginx is not running", ""},
		{"# Pkg install nginx", ""},
		{"#", ""},
		{"# ", ""},
		{"# A=b", ""},
		// Not a comment at all, or more than one line: left to the rest.
		{"pkg install nginx", ""},
		{"# first\n# pkg install nginx", ""},
		{"# pkg install nginx\npkg clean", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := commented(tt.reply, lookPath); got != tt.want {
			t.Errorf("commented(%q) = %q, want %q", tt.reply, got, tt.want)
		}
	}
}
