package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost stands in for a machine and the Ollama on it. Commands the setup
// runs are recorded instead of executed, and have the effect the real ones
// would: starting the service makes the API answer, installing the source
// tree makes its directory appear, and so on.
type fakeHost struct {
	t   *testing.T
	srv *httptest.Server
	src string // stands for /usr/src
	doc string // stands for /usr/local/share/doc/freebsd

	mu       sync.Mutex
	running  bool
	models   map[string]bool
	pulled   []string
	programs map[string]bool   // what lookPath finds
	outputs  map[string]string // what commands print; any other command fails
	ran      []string          // the commands run, as their arguments joined by spaces
	failing  string            // a command containing this fails
	dead     bool              // the Ollama service starts and stops right away
}

// newFakeHost returns a FreeBSD 15.1 release on amd64 with nothing installed.
func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	h := &fakeHost{
		t:        t,
		src:      filepath.Join(dir, "usr", "src"),
		doc:      filepath.Join(dir, "usr", "local", "share", "doc", "freebsd"),
		models:   map[string]bool{},
		programs: map[string]bool{},
		outputs: map[string]string{
			"freebsd-version -u": "15.1-RELEASE-p2",
			"uname -m":           "amd64",
			"uname -p":           "amd64",
		},
	}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHost) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running {
		http.Error(w, "not running", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Model string   `json:"model"`
		Name  string   `json:"name"`
		Input []string `json:"input"`
	}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	switch r.URL.Path {
	case "/api/version":
		fmt.Fprint(w, `{"version":"0.40.3"}`)
	case "/api/show":
		if req.Model == "" || req.Model != req.Name {
			h.t.Errorf("show was asked about model %q, name %q; want both set alike", req.Model, req.Name)
		}
		if !h.models[req.Model] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":"model '%s' not found"}`, req.Model)
			return
		}
		fmt.Fprint(w, `{"details":{}}`)
	case "/api/pull":
		h.pulled = append(h.pulled, req.Model)
		if req.Model == "no-such-model" {
			fmt.Fprintln(w, `{"status":"pulling manifest"}`)
			fmt.Fprintln(w, `{"error":"pull model manifest: file does not exist"}`)
			return
		}
		for _, line := range []string{
			`{"status":"pulling manifest"}`,
			`{"status":"pulling aaa","digest":"sha256:aaa","total":2000000000}`,
			`{"status":"pulling aaa","digest":"sha256:aaa","total":2000000000,"completed":500000000}`,
			`{"status":"pulling bbb","digest":"sha256:bbb","total":1000,"completed":1000}`,
			`{"status":"pulling aaa","digest":"sha256:aaa","total":2000000000,"completed":2000000000}`,
			`{"status":"verifying sha256 digest"}`,
			`{"status":"success"}`,
		} {
			fmt.Fprintln(w, line)
		}
		h.models[req.Model] = true
	case "/api/embed":
		reply := embedResponse{}
		for _, text := range req.Input {
			reply.Embeddings = append(reply.Embeddings, wordVector(text))
		}
		json.NewEncoder(w).Encode(reply)
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeHost) set(change func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change()
}

func (h *fakeHost) write(path, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

const zfsPage = `.Dd June 30, 2026
.Dt ZFS 8
.Os
.Sh NAME
.Nm zfs
.Nd configure ZFS datasets
.Sh DESCRIPTION
The
.Nm
command configures datasets within a storage pool, and creates snapshots of them.
`

func (h *fakeHost) machine(goos string, root bool) machine {
	return machine{
		goos:   goos,
		root:   root,
		exe:    "/home/me/go/bin/beastiepilot",
		srcDir: h.src,
		docDir: h.doc,
		lookPath: func(name string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.programs[name] {
				return "/usr/local/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		run: func(argv []string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			line := strings.Join(argv, " ")
			h.ran = append(h.ran, line)
			if h.failing != "" && strings.Contains(line, h.failing) {
				return errors.New("exit status 1")
			}
			if strings.Contains(line, "pkg install ollama") || strings.Contains(line, "ports/misc/ollama") {
				h.programs["ollama"] = true
			}
			if strings.Contains(line, "service ollama start") {
				h.running = !h.dead
			}
			if strings.Contains(line, "src.txz") || strings.Contains(line, "FreeBSD-set-src") {
				h.write(filepath.Join(h.src, "sbin", "zfs", "zfs.8"), zfsPage)
				h.write(filepath.Join(h.src, "sbin", "zfs", "zfs.c"), "int main(void) { return 0; }\n")
			}
			if strings.Contains(line, "freebsd-doc") {
				book := filepath.Join(h.doc, "en", "books", "handbook")
				h.write(filepath.Join(book, "jails", "index.html"),
					"<html><body><h1>Jails</h1><p>A jail confines processes to a part of the system, with its own hostname and address.</p></body></html>")
				h.write(filepath.Join(book, "handbook_en.pdf"), "%PDF-1.7 the same book again")
			}
			if strings.Contains(line, "install -m 0755") {
				h.programs["beastiepilot"] = true
			}
			return nil
		},
		output: func(argv ...string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if out, ok := h.outputs[strings.Join(argv, " ")]; ok {
				return out, nil
			}
			return "", errors.New("exit status 1")
		},
	}
}

// settings returns the default settings pointed at the fake machine, with a
// system documentation directory that exists.
func (h *fakeHost) settings() settings {
	docs := filepath.Join(filepath.Dir(h.src), "share", "doc")
	h.write(filepath.Join(docs, "legal", "notice.txt"), "This notice says who may redistribute the firmware and under which conditions.\n")
	cfg := defaultSettings()
	cfg.Endpoint = h.srv.URL + "/api/generate"
	cfg.SourcePath = h.src
	cfg.DocsSource = docs
	return cfg
}

// configure runs the setup with the given answers, one per question, and
// returns its exit code and what it printed.
func (h *fakeHost) configure(m machine, cfg, saved settings, file, answers string) (int, string) {
	var out bytes.Buffer
	lines := ""
	for _, a := range answers {
		lines += string(a) + "\n"
	}
	s := newSetup(styler{out: &out}, strings.NewReader(lines), cfg, saved, file, m)
	s.patience = 2 * time.Second
	if h.dead {
		s.patience = 100 * time.Millisecond // it is not coming
	}
	code := s.run()
	return code, out.String()
}

func questions(output string) int { return strings.Count(output, "[y/N]") }

// says reports whether output contains want, wherever its lines were broken.
func says(output, want string) bool {
	return strings.Contains(strings.Join(strings.Fields(output), " "), strings.Join(strings.Fields(want), " "))
}

func TestConfigureFreshFreeBSD(t *testing.T) {
	dir := isolate(t)
	h := newFakeHost(t)
	cfg := h.settings()
	file := filepath.Join(dir, "xdg", "beastiepilot", "config")

	// Ollama, model, reference material, embedding model, source tree,
	// documentation, configuration, index, program.
	code, out := h.configure(h.machine("freebsd", false), cfg, cfg, file, "yyyyyyyyy")
	if code != exitOK {
		t.Fatalf("exit code %d, want 0\n%s", code, out)
	}
	if got := questions(out); got != 9 {
		t.Errorf("asked %d questions, want 9\n%s", got, out)
	}
	if !isConfigured() || !says(out, "Requests are accepted from now on: "+configuredPath()) {
		t.Errorf("the setup finished without noting it, or without saying so\n%s", out)
	}

	wantRan := []string{
		"su root -c pkg install ollama && sysrc ollama_enable=YES && service ollama start",
		"su root -c fetch -o /usr/src.txz https://download.FreeBSD.org/releases/amd64/amd64/15.1-RELEASE/src.txz && tar -xpf /usr/src.txz -C / && rm /usr/src.txz",
		"su root -c pkg install en-freebsd-doc",
		"su root -c install -m 0755 /home/me/go/bin/beastiepilot /usr/local/bin/beastiepilot",
	}
	if !reflect.DeepEqual(h.ran, wantRan) {
		t.Errorf("ran\n  %s\nwant\n  %s", strings.Join(h.ran, "\n  "), strings.Join(wantRan, "\n  "))
	}
	if want := []string{defaultModel, defaultEmbedModel}; !reflect.DeepEqual(h.pulled, want) {
		t.Errorf("pulled %q, want %q", h.pulled, want)
	}
	for _, want := range []string{
		"This would run, as root:",
		"su root -c 'pkg install ollama && sysrc ollama_enable=YES && service ollama start'",
		"Running at " + h.srv.URL + " (version 0.40.3).",
		"llama3.2, the model that writes the commands, has not been downloaded (about 2.0 GB).",
		"DOCS_SOURCE=" + cfg.DocsSource + ":" + h.doc,
		"Saved " + file,
		"Index ready:",
		"Installed /usr/local/bin/beastiepilot.",
		"> Ready",
		"Try it: beastiepilot show the free disk space",
	} {
		if !says(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	if says(out, "PDF") {
		t.Errorf("the PDF rendition of the handbook was not left out\n%s", out)
	}

	// The file is the sample with one setting changed, and loads.
	loaded := cfg
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.parse(bytes.NewReader(raw), file); err != nil {
		t.Fatal(err)
	}
	if want := cfg.DocsSource + ":" + h.doc; loaded.DocsSource != want {
		t.Errorf("saved DOCS_SOURCE is %q, want %q", loaded.DocsSource, want)
	}
	if !strings.Contains(string(raw), "#MODEL=llama3.2\n") {
		t.Errorf("the new configuration file does not document the other settings:\n%s", raw)
	}

	// All three sources are in the index.
	ix, err := openIndex(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	var indexed []string
	for _, f := range ix.header.Files {
		indexed = append(indexed, filepath.Base(f.Path))
	}
	ix.close()
	if want := []string{"zfs.8", "notice.txt", "index.html"}; !reflect.DeepEqual(indexed, want) {
		t.Errorf("indexed %q, want %q", indexed, want)
	}

	// A second run finds everything in place: it runs nothing and has
	// nothing to ask.
	ran := len(h.ran)
	code, out = h.configure(h.machine("freebsd", false), loaded, loaded, file, "")
	if code != exitOK || len(h.ran) != ran || questions(out) != 0 {
		t.Errorf("second run: exit code %d, %d commands, %d questions; want 0, 0 and 0\n%s", code, len(h.ran)-ran, questions(out), out)
	}
	for _, want := range []string{"needs no change", "Index already up to date:", "is in place."} {
		if !says(out, want) {
			t.Errorf("second run: output lacks %q\n%s", want, out)
		}
	}
	if says(out, "Requests are accepted from now on") {
		t.Errorf("second run: announced again that requests are accepted\n%s", out)
	}

	// Once a source changes, the third run asks before reading it.
	h.write(filepath.Join(cfg.DocsSource, "new.txt"), "A note about how the tape library is rotated every week.\n")
	code, out = h.configure(h.machine("freebsd", false), loaded, loaded, file, "n")
	if code != exitOK || questions(out) != 1 || !says(out, "Read these files into the index now?") {
		t.Errorf("third run: exit code %d, %d questions; want 0 and the one about the index\n%s", code, questions(out), out)
	}
}

func TestConfigureDeclined(t *testing.T) {
	t.Run("Ollama", func(t *testing.T) {
		isolate(t)
		h := newFakeHost(t)
		cfg := h.settings()
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "n")
		if code != exitError || len(h.ran) != 0 || questions(out) != 1 {
			t.Errorf("exit code %d, %d commands, %d questions; want 1, 0 and 1\n%s", code, len(h.ran), questions(out), out)
		}
		if !says(out, "not ready yet") {
			t.Errorf("output does not say it is not ready\n%s", out)
		}
		if isConfigured() {
			t.Error("the setup was noted as done without Ollama")
		}
	})

	t.Run("everything else", func(t *testing.T) {
		isolate(t)
		h := newFakeHost(t)
		h.running = true
		cfg := h.settings()
		file := filepath.Join(t.TempDir(), "config")
		// Model, reference material, configuration file, program.
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, file, "")
		if code != exitError || len(h.ran) != 0 || len(h.pulled) != 0 || questions(out) != 4 {
			t.Errorf("exit code %d, %d commands, %d downloads, %d questions; want 1, 0, 0 and 4\n%s", code, len(h.ran), len(h.pulled), questions(out), out)
		}
		if _, err := os.Stat(file); err == nil {
			t.Error("a configuration file was written without consent")
		}
		if _, err := os.Stat(indexPath()); err == nil {
			t.Error("an index was built without consent")
		}
		if !says(out, "> Not ready yet") {
			t.Errorf("output does not say it is not ready\n%s", out)
		}
		if isConfigured() {
			t.Error("the setup was noted as done without the model")
		}
	})

	t.Run("nowhere to note it", func(t *testing.T) {
		dir := isolate(t)
		h := newFakeHost(t)
		h.running = true
		h.models[defaultModel] = true
		h.programs["beastiepilot"] = true
		cfg := h.settings()
		// The state directory cannot be made: a file is in the way.
		writeFile(t, filepath.Join(dir, "state"), "")
		// Reference material and configuration file: no.
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "nn")
		if code != exitError || isConfigured() || !says(out, "> Not ready yet") || !says(out, "could not be noted down") {
			t.Errorf("exit code %d, configured %v; want 1, false and the reason\n%s", code, isConfigured(), out)
		}
	})

	t.Run("index", func(t *testing.T) {
		isolate(t)
		h := newFakeHost(t)
		h.running = true
		h.models[defaultModel], h.models[defaultEmbedModel] = true, true
		h.programs["beastiepilot"] = true
		cfg := h.settings()
		// Reference material yes; source tree, documentation, configuration
		// file and index no.
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "ynnnn")
		if code != exitOK || len(h.ran) != 0 || questions(out) != 5 {
			t.Errorf("exit code %d, %d commands, %d questions; want 0, 0 and 5\n%s", code, len(h.ran), questions(out), out)
		}
		if _, err := os.Stat(indexPath()); err == nil {
			t.Error("an index was built without consent")
		}
		if !says(out, "there is no index of reference material") {
			t.Errorf("the summary does not mention the missing index\n%s", out)
		}
	})
}

func TestServiceLogPath(t *testing.T) {
	h := newFakeHost(t)
	s := newSetup(styler{out: io.Discard}, strings.NewReader(""), h.settings(), h.settings(), "", h.machine("freebsd", false))
	if got, want := s.serviceLogPath(), "/var/log/ollama-nobody.log"; got != want {
		t.Errorf("with nothing in rc.conf: %q, want %q", got, want)
	}
	h.outputs["sysrc -n ollama_user"] = "ollama"
	if got, want := s.serviceLogPath(), "/var/log/ollama-ollama.log"; got != want {
		t.Errorf("with ollama_user: %q, want %q", got, want)
	}
	h.outputs["sysrc -n ollama_log"] = "/var/log/models.log"
	if got, want := s.serviceLogPath(), "/var/log/models.log"; got != want {
		t.Errorf("with ollama_log: %q, want %q", got, want)
	}
}

func TestLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&b, "line %d of a log that is longer than what is read from its end\n\n", i)
	}
	b.WriteString("\ttabs\tand \x07bells\n" + strings.Repeat("x", 500) + "\n")
	writeFile(t, path, b.String())

	lines, err := lastLines(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"line 1999 of a log that is longer than what is read from its end",
		"line 2000 of a log that is longer than what is read from its end",
		"tabs and bells",
		strings.Repeat("x", 160) + " ...",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("lastLines = %q, want %q", lines, want)
	}
	// Cut off at a line boundary, far from the start of the file.
	if lines, _ = lastLines(path, 5000); len(lines) < 100 || len(lines) > 400 || !strings.HasPrefix(lines[0], "line ") {
		t.Errorf("reading the whole tail gave %d lines, the first %q", len(lines), lines[:1])
	}
	if _, err := lastLines(filepath.Join(t.TempDir(), "none"), 4); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file gave %v", err)
	}
}

func TestMarkConfigured(t *testing.T) {
	dir := isolate(t)
	want := filepath.Join(dir, "state", "beastiepilot", "configured")
	if got := configuredPath(); got != want {
		t.Fatalf("configuredPath() = %q, want %q", got, want)
	}
	if isConfigured() {
		t.Fatal("configured before any setup")
	}
	if noted, err := markConfigured(); err != nil || !noted {
		t.Fatalf("markConfigured() = %v, %v; want true, nil", noted, err)
	}
	if !isConfigured() {
		t.Fatal("not configured after the setup was noted")
	}
	before, err := os.ReadFile(want)
	if err != nil || !strings.Contains(string(before), "beastiepilot configure") {
		t.Fatalf("the note reads %q, %v", before, err)
	}
	// Noting it again is not news and leaves the file alone.
	if noted, err := markConfigured(); err != nil || noted {
		t.Errorf("second markConfigured() = %v, %v; want false, nil", noted, err)
	}

	// Without XDG_STATE_HOME it is under the home directory.
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/beastie")
	if got, want := configuredPath(), "/home/beastie/.local/state/beastiepilot/configured"; got != want {
		t.Errorf("configuredPath() = %q, want %q", got, want)
	}
}

func TestConfigureVariants(t *testing.T) {
	// ready returns a host where only the reference material is missing.
	ready := func(t *testing.T) *fakeHost {
		isolate(t)
		h := newFakeHost(t)
		h.running = true
		h.models[defaultModel], h.models[defaultEmbedModel] = true, true
		h.programs["beastiepilot"] = true
		return h
	}

	t.Run("doas", func(t *testing.T) {
		h := ready(t)
		h.set(func() { h.running = false; h.programs["doas"], h.programs["sudo"] = true, true })
		cfg := h.settings()
		// Ollama, reference material, source tree no, documentation, the rest no.
		_, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "yynynn")
		want := []string{
			"doas sh -c pkg install ollama && sysrc ollama_enable=YES && service ollama start",
			"doas pkg install en-freebsd-doc",
		}
		if !reflect.DeepEqual(h.ran, want) {
			t.Errorf("ran %q, want %q", h.ran, want)
		}
		if !says(out, "doas sh -c 'pkg install ollama && sysrc ollama_enable=YES && service ollama start'") {
			t.Errorf("the command is not shown as it runs\n%s", out)
		}
	})

	t.Run("root", func(t *testing.T) {
		h := ready(t)
		cfg := h.settings()
		_, out := h.configure(h.machine("freebsd", true), cfg, cfg, filepath.Join(t.TempDir(), "config"), "yynnn")
		want := []string{"/bin/sh -c fetch -o /usr/src.txz https://download.FreeBSD.org/releases/amd64/amd64/15.1-RELEASE/src.txz && tar -xpf /usr/src.txz -C / && rm /usr/src.txz"}
		if !reflect.DeepEqual(h.ran, want) {
			t.Errorf("ran %q, want %q", h.ran, want)
		}
		if !says(out, "You are root") || strings.Contains(out, "as root:") {
			t.Errorf("root is not told what running as root means, or is told to become root\n%s", out)
		}
	})

	t.Run("installed but stopped", func(t *testing.T) {
		h := ready(t)
		h.set(func() { h.running = false; h.programs["ollama"] = true })
		cfg := h.settings()
		h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "ynn")
		if want := []string{"su root -c sysrc ollama_enable=YES && service ollama start"}; !reflect.DeepEqual(h.ran, want) {
			t.Errorf("ran %q, want %q", h.ran, want)
		}
	})

	t.Run("started but not answering", func(t *testing.T) {
		h := ready(t)
		h.set(func() { h.running, h.dead = false, true })
		h.programs["ollama"] = true
		log := filepath.Join(t.TempDir(), "ollama.log")
		h.outputs["sysrc -n ollama_log"] = log
		h.write(log, "time=2026-10-12T21:16:55 level=INFO msg=starting\n\n"+
			"Error: could not create directory \x1b[31mmkdir /.ollama\x1b[0m: permission denied\n")
		cfg := h.settings()
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "y")
		if code != exitError || isConfigured() {
			t.Errorf("exit code %d, configured %v; want 1 and false\n%s", code, isConfigured(), out)
		}
		for _, want := range []string{
			"Ollama was started but does not answer at " + h.srv.URL,
			"Its log, " + log + ", ends with: time=2026-10-12T21:16:55 level=INFO msg=starting " +
				"Error: could not create directory [31mmkdir /.ollama[0m: permission denied",
			"BeastiePilot is not ready yet.",
		} {
			if !says(out, want) {
				t.Errorf("output lacks %q\n%s", want, out)
			}
		}
		if strings.Contains(out, "\x1b") {
			t.Errorf("an escape sequence from the log reached the terminal\n%q", out)
		}

		// A log that only root may read: the command that shows it.
		if os.Geteuid() != 0 {
			if err := os.Chmod(log, 0); err != nil {
				t.Fatal(err)
			}
			_, out = h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "y")
			if want := "reading it needs root: su root -c 'tail -n 20 " + log + "'"; !says(out, want) {
				t.Errorf("output lacks %q\n%s", want, out)
			}
		}

		// No log at all.
		h.outputs["sysrc -n ollama_log"] = filepath.Join(t.TempDir(), "none.log")
		_, out = h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "y")
		if !says(out, "says nothing about it.") {
			t.Errorf("output does not say that the log is empty\n%s", out)
		}
	})

	t.Run("ports", func(t *testing.T) {
		h := ready(t)
		h.set(func() { h.running = false })
		cfg := h.settings()
		cfg.PackageManager = "ports"
		h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "yynynn")
		want := []string{
			"su root -c make -C /usr/ports/misc/ollama install clean && sysrc ollama_enable=YES && service ollama start",
			"su root -c make -C /usr/ports/misc/freebsd-doc-en install clean",
		}
		if !reflect.DeepEqual(h.ran, want) {
			t.Errorf("ran %q, want %q", h.ran, want)
		}
	})

	t.Run("base system from packages", func(t *testing.T) {
		h := ready(t)
		h.outputs["pkg -N"] = "pkg is installed and registered 412 packages"
		h.outputs["pkg which -q /bin/sh"] = "FreeBSD-runtime-15.1"
		cfg := h.settings()
		h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "yynnn")
		if want := []string{"su root -c pkg install FreeBSD-set-src"}; !reflect.DeepEqual(h.ran, want) {
			t.Errorf("ran %q, want %q", h.ran, want)
		}
	})

	t.Run("not a release", func(t *testing.T) {
		h := ready(t)
		h.outputs["freebsd-version -u"] = "16.0-CURRENT"
		cfg := h.settings()
		// Reference material; no question about the source tree; documentation,
		// configuration file and index no.
		_, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "ynnn")
		if len(h.ran) != 0 || questions(out) != 4 || !says(out, "16.0-CURRENT is not a release") {
			t.Errorf("%d commands and %d questions; want none and 4, with the reason\n%s", len(h.ran), questions(out), out)
		}
	})

	t.Run("a step fails", func(t *testing.T) {
		h := ready(t)
		h.failing = "src.txz"
		cfg := h.settings()
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "yyyyy")
		if code != exitOK {
			t.Errorf("exit code %d, want 0: a failed optional step does not stop the setup\n%s", code, out)
		}
		for _, want := range []string{
			"That did not work (exit status 1). To do it yourself, run this as root:",
			"run this as root: fetch -o /usr/src.txz https://download.FreeBSD.org/releases/amd64/amd64/15.1-RELEASE/src.txz && tar -xpf /usr/src.txz -C / && rm /usr/src.txz ",
			"Index ready:",
		} {
			if !says(out, want) {
				t.Errorf("output lacks %q\n%s", want, out)
			}
		}
	})

	t.Run("settings from the command line", func(t *testing.T) {
		h := ready(t)
		h.models["qwen2.5-coder"] = true
		saved := h.settings()
		cfg := saved
		cfg.Model, cfg.Type, cfg.AutoRun = "qwen2.5-coder", typeGuided, "df, zfs list"
		file := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(file, []byte("# mine\nmodel = llama3.2 # the small one\nTIMEOUT=5m\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Reference material no, configuration file yes.
		_, out := h.configure(h.machine("freebsd", false), cfg, saved, file, "ny")
		raw, _ := os.ReadFile(file)
		want := "# mine\nMODEL=qwen2.5-coder\nTIMEOUT=5m\nTYPE=guided\nAUTO_RUN=\"df, zfs list\"\n"
		if string(raw) != want {
			t.Errorf("the file is now\n%s\nwant\n%s\n%s", raw, want, out)
		}
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the file's mode changed: %v, %v", info.Mode(), err)
		}
	})

	t.Run("another system", func(t *testing.T) {
		h := ready(t)
		h.set(func() { h.programs["beastiepilot"] = false })
		cfg := h.settings()
		// Reference material, configuration file no, index.
		code, out := h.configure(h.machine("darwin", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "yny")
		if code != exitOK || len(h.ran) != 0 || questions(out) != 3 {
			t.Errorf("exit code %d, %d commands, %d questions; want 0, 0 and 3\n%s", code, len(h.ran), questions(out), out)
		}
		for _, want := range []string{
			"There is no source tree at " + h.src,
			"Add that directory to your PATH",
			"Try it: /home/me/go/bin/beastiepilot show the free disk space",
		} {
			if !says(out, want) {
				t.Errorf("output lacks %q\n%s", want, out)
			}
		}
	})

	t.Run("another system without Ollama", func(t *testing.T) {
		h := ready(t)
		h.set(func() { h.running = false })
		cfg := h.settings()
		code, out := h.configure(h.machine("darwin", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "yyyy")
		if code != exitError || len(h.ran) != 0 || questions(out) != 0 || !says(out, "https://ollama.com/download") {
			t.Errorf("exit code %d, %d commands, %d questions; want 1, 0 and 0, with where to get Ollama\n%s", code, len(h.ran), questions(out), out)
		}
	})

	t.Run("a model that does not exist", func(t *testing.T) {
		h := ready(t)
		cfg := h.settings()
		cfg.Model = "no-such-model"
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "ynn")
		if code != exitError || !says(out, "no-such-model was not downloaded: pull model manifest: file does not exist") {
			t.Errorf("exit code %d, want 1 and Ollama's reason\n%s", code, out)
		}
	})

	t.Run("reference material turned off", func(t *testing.T) {
		h := ready(t)
		cfg := h.settings()
		cfg.SourcePath, cfg.DocsSource = "", ""
		code, out := h.configure(h.machine("freebsd", false), cfg, cfg, filepath.Join(t.TempDir(), "config"), "n")
		if code != exitOK || questions(out) != 1 || !says(out, "Turned off") {
			t.Errorf("exit code %d, %d questions; want 0 and only the one about the configuration file\n%s", code, questions(out), out)
		}
	})
}

func TestRootCommand(t *testing.T) {
	have := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			if contains(names, name) {
				return "/usr/local/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		}
	}
	const chain, plain = "sysrc ollama_enable=YES && service ollama start", "pkg install en-freebsd-doc"
	tests := []struct {
		name  string
		m     machine
		chain string
		argv  []string
		shown string
	}{
		{"root", machine{root: true, lookPath: have("doas")}, chain, []string{"/bin/sh", "-c", chain}, chain},
		{"doas", machine{lookPath: have("doas", "sudo")}, chain, []string{"doas", "sh", "-c", chain}, "doas sh -c '" + chain + "'"},
		{"doas, one command", machine{lookPath: have("doas")}, plain, []string{"doas", "pkg", "install", "en-freebsd-doc"}, "doas " + plain},
		{"sudo", machine{lookPath: have("sudo")}, chain, []string{"sudo", "sh", "-c", chain}, "sudo sh -c '" + chain + "'"},
		{"su", machine{lookPath: have()}, plain, []string{"su", "root", "-c", plain}, "su root -c '" + plain + "'"},
	}
	for _, tt := range tests {
		argv, shown := tt.m.rootCommand(tt.chain)
		if !reflect.DeepEqual(argv, tt.argv) || shown != tt.shown {
			t.Errorf("%s: got %q shown as %q, want %q shown as %q", tt.name, argv, shown, tt.argv, tt.shown)
		}
	}
}

func TestReleaseName(t *testing.T) {
	for version, want := range map[string]string{
		"15.1-RELEASE":      "15.1-RELEASE",
		"15.1-RELEASE-p2\n": "15.1-RELEASE",
		"14.5-RC1":          "14.5-RC1",
		"15.2-BETA3":        "15.2-BETA3",
		"15.1-STABLE":       "",
		"16.0-CURRENT":      "",
		"15.1-PRERELEASE":   "",
		"":                  "",
		"15.1-RELEASE; rm":  "",
	} {
		if got, ok := releaseName(version); got != want || ok != (want != "") {
			t.Errorf("releaseName(%q) = %q, %v; want %q", version, got, ok, want)
		}
	}
}

func TestLocalEndpoint(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"http://localhost:11434/api/generate":   true,
		"http://127.0.0.1:11434/api/generate":   true,
		"http://[::1]:11434/api/generate":       true,
		"http://10.0.0.5:11434/api/generate":    false,
		"https://ollama.example/api/generate":   false,
		"http://localhost.example/api/generate": false,
		"://":                                   false,
	} {
		if got := localEndpoint(endpoint); got != want {
			t.Errorf("localEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{2019393189: "2.0 GB", 274302450: "274 MB", 1000: "1 KB", 0: "0 KB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestOllamaModels(t *testing.T) {
	h := newFakeHost(t)
	h.running = true
	h.models["here"] = true
	endpoint := h.srv.URL + "/api/generate"
	ctx := context.Background()

	if v, err := ollamaVersion(ctx, endpoint); v != "0.40.3" || err != nil {
		t.Errorf("ollamaVersion = %q, %v", v, err)
	}
	for name, want := range map[string]bool{"here": true, "elsewhere": false} {
		if got, err := hasModel(ctx, endpoint, name); got != want || err != nil {
			t.Errorf("hasModel(%q) = %v, %v; want %v", name, got, err, want)
		}
	}

	var steps [][2]int64
	if err := pullModel(ctx, endpoint, "elsewhere", func(done, total int64) { steps = append(steps, [2]int64{done, total}) }); err != nil {
		t.Fatalf("pullModel: %v", err)
	}
	if last := steps[len(steps)-1]; last != [2]int64{2000001000, 2000001000} {
		t.Errorf("progress ended at %v, want everything of both parts; steps %v", last, steps)
	}
	if got, _ := hasModel(ctx, endpoint, "elsewhere"); !got {
		t.Error("the model is not there after the download")
	}
	if err := pullModel(ctx, endpoint, "no-such-model", nil); err == nil || err.Error() != "pull model manifest: file does not exist" {
		t.Errorf("pullModel of a missing model: %v", err)
	}

	// Ollama not running, or something else on the port.
	h.set(func() { h.running = false })
	if _, err := ollamaVersion(ctx, endpoint); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("ollamaVersion of a stopped server: %v", err)
	}
	if _, err := hasModel(ctx, endpoint, "here"); err == nil {
		t.Error("hasModel of a stopped server reported no error")
	}
	if err := pullModel(ctx, endpoint, "here", nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("pullModel from a stopped server: %v", err)
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pull" {
			fmt.Fprintln(w, `{"status":"pulling manifest"}`) // and then nothing
			return
		}
		fmt.Fprint(w, "<html>It works!</html>")
	}))
	defer other.Close()
	if _, err := ollamaVersion(ctx, other.URL+"/api/generate"); err == nil {
		t.Error("a web server that is not Ollama was taken for it")
	}
	if err := pullModel(ctx, other.URL+"/api/generate", "x", nil); err == nil || !strings.Contains(err.Error(), "before the download was complete") {
		t.Errorf("pullModel that ends early: %v", err)
	}
}

func TestConfigLine(t *testing.T) {
	for _, tt := range []struct{ value, want string }{
		{"llama3.2", "K=llama3.2"},
		{"", "K="},
		{"/usr/share/doc:/usr/local/share/doc/freebsd", "K=/usr/share/doc:/usr/local/share/doc/freebsd"},
		{"df, zfs list", `K="df, zfs list"`},
		{"/home/me/My Books", `K="/home/me/My Books"`},
		{"a#b", `K="a#b"`},
		{`say "hi" now`, `K='say "hi" now'`},
		{"it's", `K="it's"`},
		{`both "kinds" of 'quotes'`, `K=both "kinds" of 'quotes'`},
	} {
		got, err := configLine("K", tt.value)
		if got != tt.want || err != nil {
			t.Errorf("configLine(%q) = %q, %v; want %q", tt.value, got, err, tt.want)
			continue
		}
		if _, raw, _ := strings.Cut(got, "="); configValue(raw) != tt.value {
			t.Errorf("%q reads back as %q", got, configValue(raw))
		}
	}
	for _, value := range []string{"two\nlines", `"both" 'kinds' first`, `'both' "kinds" #and a comment`} {
		if got, err := configLine("K", value); err == nil {
			t.Errorf("configLine(%q) = %q, want an error", value, got)
		}
	}
}

func TestSetConfigKey(t *testing.T) {
	tests := []struct{ name, text, key, value, want string }{
		{"replaces the line", "A=1\nMODEL=old\nB=2\n", "MODEL", "new", "A=1\nMODEL=new\nB=2\n"},
		{"whatever its spelling", "  model = old # note\n", "MODEL", "new", "MODEL=new\n"},
		{"the last one, which counts", "MODEL=a\nMODEL=b\n", "MODEL", "c", "MODEL=a\nMODEL=c\n"},
		{"takes over the documented default", "# The model.\n#MODEL=llama3.2\n\n#TYPE=safe\n", "MODEL", "new", "# The model.\nMODEL=new\n\n#TYPE=safe\n"},
		{"but not an example in a comment", "#   MODEL=example\n# MODEL=another\n", "MODEL", "new", "#   MODEL=example\n# MODEL=another\nMODEL=new\n"},
		{"nor another key", "#EMBED_MODEL=x\nEMBED_MODEL=y\n", "MODEL", "new", "#EMBED_MODEL=x\nEMBED_MODEL=y\nMODEL=new\n"},
		{"adds to an empty file", "", "MODEL", "new", "\nMODEL=new\n"},
		{"adds after a last line without a newline", "A=1", "MODEL", "new", "A=1\nMODEL=new\n"},
		{"quotes", "#AUTO_RUN=\n", "AUTO_RUN", "df, du", "AUTO_RUN=\"df, du\"\n"},
	}
	for _, tt := range tests {
		got, err := setConfigKey(tt.text, tt.key, tt.value)
		if got != tt.want || err != nil {
			t.Errorf("%s: got %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}

// The sample documents the default of every setting on a line of its own, which
// is what lets configure set one without disturbing the rest.
func TestSampleConfigTakesEverySetting(t *testing.T) {
	values := map[string]string{
		"MODEL": "m", "EMBED_MODEL": "e", "ENDPOINT": "http://h/api/generate", "TIMEOUT": "7m0s", "TYPE": typeGuided,
		"AUTO_RUN": "df, zfs list", "PACKAGE_MANAGER": "ports", "SOURCE_PATH": "/s", "DOCS_SOURCE": "/d:/e",
		"RAG_SOURCE": "/my books", "BANNER": bannerCompact, "VERBOSE": "true",
	}
	text := sampleConfig
	for _, key := range settingKeys {
		value, ok := values[key]
		if !ok {
			t.Fatalf("this test has no value for %s", key)
		}
		var err error
		if text, err = setConfigKey(text, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := strings.Count(text, "\n"), strings.Count(sampleConfig, "\n"); got != want {
		t.Errorf("setting every key changed the sample from %d lines to %d: some key has no \"#KEY=\" line", want, got)
	}
	s := defaultSettings()
	if err := s.parse(strings.NewReader(text), "sample"); err != nil {
		t.Fatal(err)
	}
	for _, key := range settingKeys {
		if s.get(key) != values[key] {
			t.Errorf("%s reads back as %q, want %q", key, s.get(key), values[key])
		}
	}
	if !strings.Contains(text, "#   AUTO_RUN=df, du, uptime") {
		t.Error("the example in the comments was changed")
	}
}

func TestWriteConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultSettings()
	cfg.Type = typeGuided

	// A new file, in a directory that does not exist yet.
	path := filepath.Join(dir, "a", "b", "config")
	if err := writeConfig(path, cfg, []string{"TYPE"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if want := strings.Replace(sampleConfig, "#TYPE=safe\n", "TYPE=guided\n", 1); string(raw) != want {
		t.Errorf("the new file is not the sample with TYPE set:\n%s", raw)
	}

	// Through a symbolic link, which stays one.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	cfg.Model = "other"
	if err := writeConfig(link, cfg, []string{"MODEL"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symbolic link was replaced: %v, %v", info.Mode(), err)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "\nMODEL=other\n") || !strings.Contains(string(raw), "\nTYPE=guided\n") {
		t.Errorf("the file behind the link was not updated in place:\n%s", raw)
	}

	// Nothing is left behind, and a value that cannot be written changes nothing.
	cfg.Model = "two\nlines"
	if err := writeConfig(path, cfg, []string{"MODEL"}); err == nil {
		t.Error("a value of two lines was written")
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, raw) {
		t.Error("a failed write changed the file")
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temporary files were left behind: %v", entries)
	}
}
