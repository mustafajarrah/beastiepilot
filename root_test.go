package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNeedsRoot(t *testing.T) {
	tests := []struct {
		command, want string
	}{
		// Changing the system is for root.
		{"pkg install hyprland", "pkg install"},
		{"pkg install -y nginx", "pkg install"},
		{"pkg -y upgrade", "pkg upgrade"},
		{"/usr/sbin/pkg delete nginx", "pkg delete"},
		{"pkg update && pkg upgrade -y", "pkg update"},
		{"pkg info nginx || pkg install nginx", "pkg install"},
		{"env ASSUME_ALWAYS_YES=yes pkg install nginx", "pkg install"},
		{"ASSUME_ALWAYS_YES=yes pkg install nginx", "pkg install"},
		{"service sshd start", "service sshd start"},
		{"service nginx enable", "service nginx enable"},
		{"sysrc sshd_enable=YES", "sysrc sshd_enable=YES"},
		{`sysrc kld_list+="i915kms"`, "sysrc kld_list+=i915kms"},
		{"sysrc -f /boot/loader.conf autoboot_delay=3", "sysrc autoboot_delay=3"},
		{"sysrc -x sshd_enable", "sysrc -x"},
		{"sysctl kern.ipc.somaxconn=1024", "sysctl kern.ipc.somaxconn=1024"},
		{"freebsd-update fetch install", "freebsd-update"},
		{"pw groupmod video -m mustafa", "pw groupmod"},
		{"kldload i915kms", "kldload"},
		{"zfs create zroot/data", "zfs create"},
		{"zfs snapshot -r zroot@before", "zfs snapshot"},
		{"zfs mount zroot/data", "zfs mount"},
		{"zfs mount -a", "zfs mount"},
		{"zpool scrub zroot", "zpool scrub"},
		{"mount -t msdosfs /dev/da0s1 /mnt", "mount"},
		{"umount /mnt", "umount"},
		{"newfs_msdos /dev/da0s1", "newfs_msdos"},
		{"shutdown -r now", "shutdown"},
		{"pfctl -sr", "pfctl"},
		{"make -C /usr/ports/www/nginx install clean", "make install"},

		// Asking about it is for everyone.
		{"pkg info", ""},
		{"pkg search hyprland", ""},
		{"pkg install -n nginx", ""},
		{"pkg upgrade --dry-run", ""},
		{"pkg", ""},
		{"service sshd status", ""},
		{"service -e", ""},
		{"sysrc sshd_enable", ""},
		{"sysrc -a", ""},
		{"sysctl -n hw.ncpu", ""},
		{"sysctl -a | grep kern.ostype=", ""},
		{"zfs list -t snapshot", ""},
		{"zfs mount", ""},
		{"pkg lock -l", ""},
		{"zpool status", ""},
		{"bectl list", ""},
		{"gpart show", ""},
		{"pw usershow mustafa", ""},
		{"mount", ""},
		{"mount -t ufs", ""},
		{"make install", ""},
		{"make -C /usr/ports/www/nginx config", ""},
		{"df -h", ""},
		{"kldstat", ""},

		// Words that are only arguments do not count.
		{`echo "pkg install nginx"`, ""},
		{"grep 'x; pkg install y' notes.txt", ""},
		{"man pkg install", ""},
		// The model was told to leave these out; what it writes is run as it is.
		{"sudo pkg install nginx", ""},
		{"doas service sshd start", ""},
		// A subcommand the shell would expand is not known.
		{"pkg $ACTION nginx", ""},
		// What cannot be taken apart with certainty runs as the user.
		{"pkg install $(cat list)", ""},
		{"pkg install nginx > /tmp/log", ""},
		{"pkg update\npkg upgrade", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := needsRoot(tt.command); got != tt.want {
			t.Errorf("needsRoot(%q) = %q, want %q", tt.command, got, tt.want)
		}
	}
}

func TestAsRoot(t *testing.T) {
	// only returns a lookPath that finds the tools named.
	only := func(tools ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			if contains(tools, name) {
				return "/usr/local/bin/" + name, nil
			}
			return "", errors.New("not found")
		}
	}
	const command = "pkg install 'it''s' && echo \"$HOME\""

	tests := []struct {
		tools []string
		want  elevation
		says  string
	}{
		{[]string{"doas", "sudo"}, elevation{tool: "doas", argv: []string{"doas", "/bin/sh", "-c", command}},
			"pkg install needs root, and you are not root: on a yes, the command runs as root through doas, which may ask for your password."},
		{[]string{"sudo"}, elevation{tool: "sudo", argv: []string{"sudo", "/bin/sh", "-c", command}},
			"pkg install needs root, and you are not root: on a yes, the command runs as root through sudo, which may ask for your password."},
		// su gets a line that no command can change, and the command apart.
		{nil, elevation{tool: "su", argv: []string{"su", "root", "-c", `/bin/sh -c 'eval "$BEASTIEPILOT_COMMAND"'`},
			env: []string{"BEASTIEPILOT_COMMAND=" + command}},
			"pkg install needs root, and you are not root: on a yes, the command runs as root through su, which asks for root's password."},
	}
	for _, tt := range tests {
		got := asRoot(command, only(tt.tools...))
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("with %v installed: asRoot = %+v, want %+v", tt.tools, got, tt.want)
		}
		if says := got.notice("pkg install"); says != tt.says {
			t.Errorf("with %v installed, the notice is %q, want %q", tt.tools, says, tt.says)
		}
	}
}

// standIns puts stand-ins for pkg and for the tools that run a command as root
// in a directory that becomes the whole PATH, and returns the file they note
// what they were asked in. The stand-ins for the tools run the command as
// whoever runs the test.
func standIns(t *testing.T, tools ...string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	scripts := map[string]string{
		"pkg":   "echo \"pkg $*\" >> " + log + "\nexit 3\n",
		"sysrc": "echo \"sysrc $*\" >> " + log + "\n",
		"true":  "exit 0\n",
		"doas":  "echo \"doas $*\" >> " + log + "\nexec \"$@\"\n",
		"sudo":  "echo \"sudo $*\" >> " + log + "\nexec \"$@\"\n",
		// Root's shell here is sh; with csh the line would mean the same.
		"su": "echo \"su $*\" >> " + log + "\nexec /bin/sh -c \"$3\"\n",
	}
	for _, name := range append([]string{"pkg"}, tools...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+scripts[name]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return log
}

// TestRunsAsRoot takes a command that needs root from the suggestion to the
// tool that runs it, in a session the plain way.
func TestRunsAsRoot(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"response":"pkg install -y 'two words' && pkg clean","done":true}`)
	}))
	defer srv.Close()
	cfg := defaultSettings()
	cfg.Endpoint, cfg.Banner = srv.URL, bannerOff
	// Listing pkg does not let it run as root unasked.
	cfg.AutoRun = "pkg"

	session := func(p *pilot, lines string) string {
		t.Helper()
		var out bytes.Buffer
		p.ui, p.in, p.cfg = styler{out: &out}, strings.NewReader(lines), cfg
		if code := p.session(); code != exitOK {
			t.Errorf("the session ended with %d, want 0", code)
		}
		return out.String()
	}
	noted := func(log string) string {
		t.Helper()
		raw, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return string(raw)
	}

	// Through su, which gets the command by the environment.
	log := standIns(t, "su")
	p := &pilot{}
	shown := session(p, "install it\ny\nagain\nn\n")
	for _, want := range []string{
		"pkg install needs root, and you are not root: on a yes, the command runs as root through su, which asks for root's password.",
		"Execute this command as root? [y/N]",
		"Exit status 3",
		"Aborted. Nothing was executed.",
	} {
		if !says(shown, want) {
			t.Errorf("the session never showed %q:\n%s", want, shown)
		}
	}
	// The second pkg is not reached: the first one ends with 3.
	if got, want := noted(log), "su root -c /bin/sh -c 'eval \"$BEASTIEPILOT_COMMAND\"'\npkg install -y two words\n"; got != want {
		t.Errorf("this ran:\n%s\nwant:\n%s", got, want)
	}
	if want := "it ran as root and exited with status 3"; len(p.past) != 2 || p.past[0].Outcome != want {
		t.Errorf("the session remembers %+v, want the first to end with %q", p.past, want)
	}

	// Through doas when it is there, which starts the shell itself.
	log = standIns(t, "su", "sudo", "doas")
	shown = session(&pilot{}, "install it\ny\n")
	if want := "through doas, which may ask for your password."; !says(shown, want) {
		t.Errorf("the session never showed %q:\n%s", want, shown)
	}
	if got, want := noted(log), "doas /bin/sh -c pkg install -y 'two words' && pkg clean\npkg install -y two words\n"; got != want {
		t.Errorf("this ran:\n%s\nwant:\n%s", got, want)
	}

	// Root runs it as any other command, and AUTO_RUN holds again.
	log = standIns(t, "su", "doas")
	shown = session(&pilot{root: true}, "install it\n")
	if strings.Contains(shown, "needs root") || strings.Contains(shown, "as root?") || !says(shown, "Listed in AUTO_RUN") {
		t.Errorf("root was treated as someone else:\n%s", shown)
	}
	if got, want := noted(log), "pkg install -y two words\n"; got != want {
		t.Errorf("this ran:\n%s\nwant:\n%s", got, want)
	}
}

// TestNoticeWraps checks that a notice too long for one line continues under
// its own words, as the one about root is.
func TestNoticeWraps(t *testing.T) {
	var out bytes.Buffer
	none := func(string) (string, error) { return "", errors.New("not found") }
	notice := asRoot("pkg install nginx", none).notice("pkg install")
	printSuggestion(styler{out: &out}, "llama3.2", "pkg install nginx", review{notices: []string{"Short.", notice}})
	want := "\n  > Suggested command (llama3.2)\n\n    pkg install nginx\n\n" +
		"  ! Short.\n" +
		"  ! pkg install needs root, and you are not root: on a yes, the command runs as\n" +
		"    root through su, which asks for root's password.\n\n"
	if out.String() != want {
		t.Errorf("the suggestion was shown as\n%q\nwant\n%q", out.String(), want)
	}
}

// TestCommandInComment covers a model that puts its command where a refusal
// goes: the command is offered, with a word about it, and asked about even
// when AUTO_RUN lists it. A refusal in words stays one.
func TestCommandInComment(t *testing.T) {
	isolate(t)
	log := standIns(t)
	var asked []generateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req generateRequest
		json.NewDecoder(r.Body).Decode(&req)
		asked = append(asked, req)
		if strings.Contains(req.Prompt, "list") {
			io.WriteString(w, `{"response":"# pkg info -a","done":true}`)
			return
		}
		io.WriteString(w, `{"response":"# pkg has no package of that name","done":true}`)
	}))
	defer srv.Close()
	cfg := defaultSettings()
	cfg.Endpoint, cfg.Banner, cfg.AutoRun = srv.URL, bannerOff, "pkg info"

	var out bytes.Buffer
	p := &pilot{ui: styler{out: &out}, in: strings.NewReader("list them\ny\ninstall that one\n"), cfg: cfg, root: true}
	if code := p.session(); code != exitOK {
		t.Errorf("the session ended with %d, want 0", code)
	}
	shown := out.String()
	for _, want := range []string{
		"Suggested command (llama3.2)",
		"The model gave this as a comment, where it is told to say that a request is unclear or cannot be done: it may not be what you asked for.",
		"Execute this command? [y/N]",
		"Exit status 3",
		"! pkg has no package of that name",
	} {
		if !says(shown, want) {
			t.Errorf("the session never showed %q:\n%s", want, shown)
		}
	}
	if says(shown, "Listed in AUTO_RUN") {
		t.Errorf("a command given as a comment ran unasked:\n%s", shown)
	}
	raw, _ := os.ReadFile(log)
	if got, want := string(raw), "pkg info -a\n"; got != want {
		t.Errorf("this ran:\n%s\nwant:\n%s", got, want)
	}
	if len(p.past) != 2 || p.past[0].Command != "pkg info -a" || p.past[0].Outcome != "it ran and exited with status 3" ||
		p.past[1].Command != "" || p.past[1].Outcome != "you gave no command: pkg has no package of that name" {
		t.Errorf("the session remembers %+v", p.past)
	}
	// The model is told of the command it gave, not of a refusal.
	if want := "Command: pkg info -a\n    Outcome: it ran and exited with status 3\n"; len(asked) != 2 || !strings.Contains(asked[1].System, want) {
		t.Errorf("the second request lacks %q", want)
	}
}

// TestSeveralLinesAsRoot is a session a small model was seen to give: asked
// to install something and enable it, it answered with a command to a line,
// as comments one time and plainly the next.
func TestSeveralLinesAsRoot(t *testing.T) {
	isolate(t)
	log := standIns(t, "su", "sysrc", "true")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req generateRequest
		json.NewDecoder(r.Body).Decode(&req)
		reply := "# pkg install nginx\n# sysrc nginx_enable=\"YES\""
		if strings.Contains(req.Prompt, "plainly") {
			reply = "true\nsysrc nginx_enable=YES\npkg install nginx\nsysrc sshd_enable=YES"
		}
		json.NewEncoder(w).Encode(generateResponse{Response: reply})
	}))
	defer srv.Close()
	cfg := defaultSettings()
	cfg.Endpoint, cfg.Banner = srv.URL, bannerOff

	var out bytes.Buffer
	p := &pilot{ui: styler{out: &out}, in: strings.NewReader("install nginx and enable it at boot\ny\nand plainly\ny\n"), cfg: cfg}
	if code := p.session(); code != exitOK {
		t.Errorf("the session ended with %d, want 0", code)
	}
	shown := out.String()
	for _, want := range []string{
		"    pkg install nginx && sysrc nginx_enable=\"YES\"\n",
		"The model gave this as a comment, where it is told to say that a request is unclear",
		"The model gave 2 lines with a command to each, which are joined here with &&: each command runs only if the one before it worked.",
		"pkg install needs root, and you are not root: on a yes, the command runs as root through su, which asks for root's password.",
		"    true && sysrc nginx_enable=YES && pkg install nginx && sysrc sshd_enable=YES\n",
		"The model gave 4 lines with a command to each,",
		"sysrc nginx_enable=YES needs root,",
	} {
		if !says(shown, want) {
			t.Errorf("the session never showed %q:\n%s", want, shown)
		}
	}
	if n := strings.Count(shown, "Execute this command as root? [y/N]"); n != 2 {
		t.Errorf("root was asked for %d times, want twice:\n%s", n, shown)
	}
	// Each line went to su whole, and stopped where a command failed: the
	// stand-in for pkg ends with status 3.
	raw, _ := os.ReadFile(log)
	want := "su root -c /bin/sh -c 'eval \"$BEASTIEPILOT_COMMAND\"'\npkg install nginx\n" +
		"su root -c /bin/sh -c 'eval \"$BEASTIEPILOT_COMMAND\"'\nsysrc nginx_enable=YES\npkg install nginx\n"
	if string(raw) != want {
		t.Errorf("this ran:\n%s\nwant:\n%s", raw, want)
	}
	if len(p.past) != 2 || p.past[0].Command != `pkg install nginx && sysrc nginx_enable="YES"` || p.past[0].Outcome != "it ran as root and exited with status 3" {
		t.Errorf("the session remembers %+v", p.past)
	}
}
