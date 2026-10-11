// Command beastiepilot is an AI-powered terminal assistant for FreeBSD.
//
// It sends a natural language request to a local LLM served by Ollama, prints
// the shell command the model suggests, and runs it through /bin/sh only after
// the user explicitly confirms on the terminal.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// version is the release version. Override it at build time with
// -ldflags "-X main.version=v1.2.3".
var version = "dev"

const (
	defaultEndpoint = "http://localhost:11434/api/generate"
	defaultModel    = "llama3.2"
	defaultTimeout  = 2 * time.Minute

	// maxResponseBytes caps how much of an Ollama reply is read. A shell
	// command is tiny; anything near this size is a misbehaving server.
	maxResponseBytes = 1 << 20
)

// Process exit codes. When a suggested command is executed, its own exit code
// is returned instead.
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitInterrupted = 130
)

// systemPrompt pins the model to FreeBSD and to machine-readable output. The
// "# reason" escape hatch gives it a harmless way to decline, which run
// detects and reports instead of offering it for execution.
//
// The prompt only steers the model. Nothing here is a security control: those
// live in validateCommand, inspect and confirm.
const systemPrompt = `You are BeastiePilot, an expert FreeBSD system administrator. Translate the user's request into one shell command for FreeBSD's /bin/sh.

OUTPUT FORMAT (strict):
- Output ONLY the command, on a single line. Combine steps with &&, ; or pipes.
- No explanations, greetings, notes or warnings. No markdown, code fences, backticks or quotes around the command. No leading "$" prompt.
- If the request is ambiguous or cannot be done with a FreeBSD shell command, output a single line that starts with "# " followed by a short reason, and nothing else.

FREEBSD ONLY (strict):
- Target FreeBSD's base system and POSIX sh. No bashisms, no GNU-only flags. Remember the BSD variants of sed, grep, find, stat, date, ps and tar (for example: sed -i '' ...).
- NEVER use Linux tools or paths: apt, apt-get, dpkg, yum, dnf, rpm, pacman, snap, systemctl, systemd, journalctl, update-rc.d, chkconfig, iptables, nftables, ufw, firewalld, ip, ss, lsblk, free, useradd, usermod, groupadd, modprobe, lsmod, lvm, mdadm, /proc, /sys, /etc/sysconfig.
- Use the FreeBSD way instead:
  - Packages: pkg (pkg install, pkg upgrade, pkg info, pkg search, pkg delete). Base system: freebsd-update.
  - Services: service <name> start|stop|restart|status. Enable at boot with sysrc <name>_enable=YES.
  - Configuration: sysrc for rc.conf, sysctl for kernel state, /boot/loader.conf for loader tunables.
  - Firewall: pf and pfctl.
  - Storage: zfs, zpool, gpart, geom, camcontrol, df, du.
  - Network: ifconfig, route, netstat, sockstat.
  - Users and groups: pw, adduser, rmuser.
  - Kernel modules: kldload, kldunload, kldstat.
  - Jails and VMs: jail, jls, jexec, bhyve.
  - Logs and hardware: /var/log/messages, dmesg, pciconf, usbconfig.

SAFETY:
- Choose the least destructive command that satisfies the request. Never add force or recursive-delete flags that were not asked for.
- Append to configuration files with >>, never overwrite them with >. Prefer sysrc for rc.conf.
- Do not prefix commands with sudo or doas; the user runs this tool with the privileges they intend.`

// generateRequest is the subset of Ollama's /api/generate request body used here.
type generateRequest struct {
	Model   string         `json:"model"`
	System  string         `json:"system"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

// generateResponse is the subset of Ollama's /api/generate reply used here.
type generateResponse struct {
	Response string `json:"response"`
	Error    string `json:"error"`
}

// ANSI SGR parameters used by the interface.
const (
	bold       = "1"
	dim        = "2"
	boldRed    = "1;31"
	boldGreen  = "1;32"
	boldYellow = "1;33"
	boldCyan   = "1;36"
	lens       = "1;33;46" // goggle frame over cyan glass
)

// mascot is a banner picture: rows of characters and, for each character, a
// letter choosing its color from mascotColors.
type mascot struct {
	rows    []string
	colors  []string
	infoRow int // first row that has text beside it
}

// mascotColors maps the letters in mascot.colors to SGR codes: Beastie is
// red, his pilot cap and goggles are yellow.
var mascotColors = map[byte]string{'r': boldRed, 'y': boldYellow, 'g': lens}

// beastieBlocks is Beastie in a pilot cap, drawn with quadrant block
// characters so that every character cell holds two "pixels" each way. The
// goggle lenses are the lower halves of the 'g' cells.
var beastieBlocks = mascot{
	rows: []string{
		"█▖ ▗▄▄▖ ▗█",
		"▝█▟████▙█▘",
		" ██▀██▀██ ",
		" ▀██▀▀██▀ ",
	},
	colors: []string{
		"rr yyyy rr",
		"rryyyyyyrr",
		" rygyygyr ",
		" rrrrrrrr ",
	},
}

// beastieASCII is the same pilot for terminals without a UTF-8 locale, such
// as the FreeBSD console in the C locale.
var beastieASCII = mascot{
	rows: []string{
		` ,          ,`,
		`/(   ____   )\`,
		`\ \.'    './ /`,
		` \/ ______ \/`,
		` | (O)==(O) |`,
		` |   o  o   |`,
		`  \  \__/  /`,
		`   '-.__.-'`,
	},
	colors: []string{
		` r          r`,
		`rr   yyyy   rr`,
		`r ryy    yyr r`,
		` rr yyyyyy rr`,
		` r yyyyyyyy r`,
		` r   r  r   r`,
		`  r  rrrr  r`,
		`   rrrrrrrr`,
	},
	infoRow: 2,
}

// styler draws the interface. It writes to the controlling terminal when
// there is one and to stderr otherwise; never to stdout, which is reserved
// for the output of the executed command.
type styler struct {
	out   io.Writer
	tty   bool // out is an interactive terminal
	color bool // ANSI colors are wanted
}

// newStyler returns a styler for the controlling terminal tty, or a plain
// one for stderr if tty is nil.
func newStyler(tty *os.File) styler {
	if tty == nil {
		return styler{out: os.Stderr}
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	return styler{out: tty, tty: true, color: !noColor && os.Getenv("TERM") != "dumb"}
}

// paint wraps text in the given SGR code when colors are enabled.
func (s styler) paint(code, text string) string {
	if !s.color {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

// errorf reports a failure on stderr, where scripts and logs expect it.
func (s styler) errorf(format string, args ...any) {
	label := "beastiepilot:"
	if isTerminal(os.Stderr) {
		label = s.paint(boldRed, label)
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", label, fmt.Sprintf(format, args...))
}

// isTerminal reports whether f is attached to a character device.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func main() {
	os.Exit(run())
}

func run() int {
	endpoint := flag.String("endpoint", envOr("BEASTIEPILOT_ENDPOINT", defaultEndpoint), "Ollama generate API `url`")
	model := flag.String("model", envOr("BEASTIEPILOT_MODEL", defaultModel), "Ollama model `name`")
	timeout := flag.Duration("timeout", defaultTimeout, "how long to wait for the model")
	noBanner := flag.Bool("no-banner", os.Getenv("BEASTIEPILOT_NO_BANNER") != "", "do not draw the Beastie banner")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("beastiepilot", buildVersion())
		return exitOK
	}

	intent := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if intent == "" {
		usage()
		return exitUsage
	}

	// The controlling terminal is where the interface is drawn and the only
	// place approval is accepted from. Using it rather than stdin and stderr
	// means piped input can neither answer the prompt nor be swallowed by it,
	// and redirecting output cannot hide the command being approved.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		tty = nil
	} else {
		defer tty.Close()
	}
	ui := newStyler(tty)

	if ui.tty && !*noBanner {
		printBanner(ui, *model, *endpoint)
	}

	// Let Ctrl-C abort a slow model instead of leaving the user waiting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if ui.tty {
		fmt.Fprint(ui.out, ui.paint(dim, fmt.Sprintf("  Asking %s ...", *model)))
	}
	reply, err := suggest(ctx, *endpoint, *model, systemPrompt+hostContext(), intent)
	if ui.tty {
		fmt.Fprint(ui.out, "\r\033[K") // erase the status line
	}
	// Restore default signal handling so Ctrl-C works at the prompt below.
	stop()

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		ui.errorf("no answer from %s within %s (raise it with -timeout)", *model, *timeout)
		return exitError
	case errors.Is(err, context.Canceled):
		ui.errorf("interrupted")
		return exitInterrupted
	case err != nil:
		ui.errorf("%v", err)
		return exitError
	}

	command := cleanCommand(reply)
	if err := validateCommand(command); err != nil {
		ui.errorf("%v", err)
		return exitError
	}
	if reason, ok := declined(command); ok {
		fmt.Fprintf(ui.out, "\n  %s %s\n\n", ui.paint(boldYellow, "!"), reason)
		return exitError
	}

	rev := inspect(command)
	rev.notices = append(rev.notices, hostNotices()...)
	printSuggestion(ui, *model, command, rev)

	if tty == nil {
		ui.errorf("no terminal available to ask for confirmation; nothing was executed")
		return exitError
	}
	if !confirm(ui, tty, len(rev.dangers) > 0) {
		fmt.Fprintf(ui.out, "  %s\n", ui.paint(dim, "Aborted. Nothing was executed."))
		return exitOK
	}
	fmt.Fprintln(ui.out)

	code, err := execute(command)
	if err != nil {
		ui.errorf("%v", err)
		return exitError
	}
	return code
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `Usage: beastiepilot [flags] <what you want to do>

Translates a plain-language request into a FreeBSD shell command using a local
Ollama model, then asks for confirmation before running it.

Examples:
  beastiepilot install nginx and enable it at boot
  beastiepilot "show the ten largest files under /var"
  beastiepilot -model qwen2.5-coder "snapshot zroot/usr/home"

Quote the request if it contains characters your shell would interpret
(such as ? * ' or |).

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(out, `
Environment:
  BEASTIEPILOT_ENDPOINT   default for -endpoint
  BEASTIEPILOT_MODEL      default for -model
  BEASTIEPILOT_NO_BANNER  hide the banner when set
  NO_COLOR                disable colored output when set
`)
}

// envOr returns the value of the environment variable key, or fallback if it
// is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// buildVersion prefers the linker-provided version and falls back to the
// module version recorded by "go install module@version".
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// printBanner draws Beastie with the version, the model in use and the
// working directory beside him.
func printBanner(ui styler, model, endpoint string) {
	art, sep := beastieASCII, " - "
	if utf8Locale() {
		art, sep = beastieBlocks, " · "
	}
	info := []string{
		ui.paint(bold, "BeastiePilot") + " " + ui.paint(dim, bannerVersion()),
		model + sep + endpointHost(endpoint),
		ui.paint(dim, shortWorkingDir()),
	}

	width := 0
	for _, row := range art.rows {
		if n := utf8.RuneCountInString(row); n > width {
			width = n
		}
	}

	fmt.Fprintln(ui.out)
	for i := range art.rows {
		line, n := art.render(ui, i)
		if j := i - art.infoRow; j >= 0 && j < len(info) {
			line += strings.Repeat(" ", width-n+3) + info[j]
		}
		fmt.Fprintln(ui.out, "  "+strings.TrimRight(line, " "))
	}
	fmt.Fprintln(ui.out)
}

// render returns row i of the mascot with its colors applied, and the row's
// width in columns.
func (m mascot) render(ui styler, i int) (string, int) {
	cells := []rune(m.rows[i])
	var b strings.Builder
	for j, c := range cells {
		code := ""
		if j < len(m.colors[i]) {
			code = mascotColors[m.colors[i][j]]
		}
		if code == "" {
			b.WriteRune(c)
		} else {
			b.WriteString(ui.paint(code, string(c)))
		}
	}
	return b.String(), len(cells)
}

// utf8Locale reports whether the environment selects a UTF-8 locale, checking
// the variables in the order the C library does.
func utf8Locale() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToUpper(os.Getenv(key)); v != "" {
			return strings.Contains(v, "UTF-8") || strings.Contains(v, "UTF8")
		}
	}
	return false
}

// bannerVersion is buildVersion, with the long pseudo-version Go assigns to
// untagged builds shortened to "dev".
func bannerVersion() string {
	v := buildVersion()
	if strings.HasPrefix(v, "v0.0.0-") {
		return "dev"
	}
	return v
}

// endpointHost returns the host:port part of the Ollama endpoint URL.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

// shortWorkingDir returns the working directory with the user's home
// directory abbreviated to "~".
func shortWorkingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "/" && (wd == home || strings.HasPrefix(wd, home+"/")) {
		return "~" + wd[len(home):]
	}
	return wd
}

// hostContext describes the machine to the model so that it can tailor the
// command instead of guessing. It is appended to the system prompt.
func hostContext() string {
	var b strings.Builder
	b.WriteString("\n\nHOST CONTEXT (facts about the machine the command will run on):\n")
	if runtime.GOOS == "freebsd" {
		if out, err := exec.Command("/usr/bin/uname", "-sr").Output(); err == nil {
			fmt.Fprintf(&b, "- System: %s (%s)\n", strings.TrimSpace(string(out)), runtime.GOARCH)
		}
	}
	if os.Geteuid() == 0 {
		b.WriteString("- User: root\n")
	} else {
		b.WriteString("- User: unprivileged (not root)\n")
	}
	if wd, err := os.Getwd(); err == nil {
		fmt.Fprintf(&b, "- Working directory: %q\n", wd)
	}
	return b.String()
}

// hostNotices returns warnings about where and as whom the command would run.
func hostNotices() []string {
	var notices []string
	if runtime.GOOS != "freebsd" {
		notices = append(notices, fmt.Sprintf("This host runs %s, not FreeBSD; the command may not work here.", runtime.GOOS))
	}
	if os.Geteuid() == 0 {
		notices = append(notices, "You are root: the command will run with full privileges.")
	}
	return notices
}

// suggest asks the Ollama model at endpoint to translate intent into a shell
// command under the given system prompt and returns the model's raw reply.
func suggest(ctx context.Context, endpoint, model, system, intent string) (string, error) {
	body, err := json.Marshal(generateRequest{
		Model:  model,
		System: system,
		Prompt: intent,
		Stream: false,
		// Temperature 0 keeps suggestions as deterministic as the model allows.
		Options: map[string]any{"temperature": 0},
	})
	if err != nil {
		return "", fmt.Errorf("encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach Ollama at %s (is it running? try \"service ollama start\"): %w", endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("reading Ollama response: %w", err)
	}

	var reply generateResponse
	decodeErr := json.Unmarshal(raw, &reply)

	if resp.StatusCode != http.StatusOK {
		msg := reply.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("Ollama returned %s: %s (try \"ollama pull %s\")", resp.Status, msg, model)
		}
		return "", fmt.Errorf("Ollama returned %s: %s", resp.Status, msg)
	}
	if decodeErr != nil {
		return "", fmt.Errorf("decoding Ollama response: %w", decodeErr)
	}
	if reply.Error != "" {
		return "", fmt.Errorf("Ollama error: %s", reply.Error)
	}
	return reply.Response, nil
}

// cleanCommand strips the decoration models tend to add despite being told
// not to: reasoning blocks, Markdown code fences, backticks and "$ " prompts.
func cleanCommand(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))

	// Reasoning models may emit their scratchpad first; keep what follows it.
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = strings.TrimSpace(s[i+len("</think>"):])
	}

	// A fenced block: drop the opening line (fence plus optional language
	// tag) and the closing fence.
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			s = strings.TrimPrefix(s, "```")
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}

	// A command wrapped in a single pair of inline-code backticks.
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' && !strings.Contains(s[1:len(s)-1], "`") {
		s = s[1 : len(s)-1]
	}

	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$ "))
}

// validateCommand rejects replies that cannot be shown to the user faithfully.
// Escape sequences, carriage returns and invisible Unicode could make the
// terminal display something other than what /bin/sh would run, which would
// defeat the confirmation prompt.
func validateCommand(command string) error {
	if command == "" {
		return errors.New("the model returned an empty response; try rephrasing the request")
	}
	for _, r := range command {
		if r != '\n' && r != '\t' && !unicode.IsPrint(r) {
			return fmt.Errorf("the model's reply contains a non-printable character (%U); refusing to offer it for execution", r)
		}
	}
	return nil
}

// declined reports whether the reply consists solely of shell comments, which
// is how the system prompt tells the model to decline a request. It returns
// the model's stated reason.
func declined(command string) (reason string, ok bool) {
	var parts []string
	for _, line := range strings.Split(command, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			return "", false
		}
		if text := strings.TrimSpace(strings.TrimLeft(line, "#")); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return "The model could not suggest a command for this request.", true
	}
	return strings.Join(parts, " "), true
}

// review is what inspect found in a suggested command.
type review struct {
	dangers []string // ways the command could destroy data or cut off access
	notices []string // other things worth knowing before approving
}

var (
	// linuxOnly lists tools found on Linux but not on FreeBSD. The system
	// prompt forbids them; this catches a model that ignores it.
	linuxOnly = wordSet(`apt apt-get apt-cache aptitude dpkg yum dnf rpm zypper pacman apk
		snap flatpak systemctl journalctl update-rc.d chkconfig iptables ip6tables nft ufw
		firewall-cmd ip ss lsblk free useradd usermod userdel groupadd groupdel modprobe
		lsmod insmod rmmod mdadm update-grub lsb_release`)

	// destructive lists the commands danger knows how to judge.
	destructive = wordSet(`rm find zfs zpool gpart dd camcontrol bectl shutdown reboot halt
		poweroff pfctl rmuser pw pkg`)

	// wrappers are words that come before the real command name.
	wrappers = wordSet(`sudo doas env nice nohup time exec command xargs if then else do while until !`)

	// separators splits a command line at the operators that begin a new
	// simple command.
	separators = strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "&", "\n",
		"$(", "\n", "(", "\n", ")", "\n", "`", "\n", "{", "\n", "}", "\n")

	// pipeToShell matches output being piped into a shell interpreter.
	pipeToShell = regexp.MustCompile(`(?:^|[^|])\|\s*(?:sudo\s+|doas\s+)?(?:sh|bash|csh|tcsh|ksh|zsh)\b`)

	// truncatingRedirect captures the absolute path a ">" (not ">>") writes to.
	truncatingRedirect = regexp.MustCompile(`(?:^|[^>])>\s*["']?(/[^\s;&|)"']+)`)
)

func wordSet(words string) map[string]bool {
	set := make(map[string]bool)
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}

// inspect looks for signs that a suggested command is destructive or was
// written for the wrong operating system.
//
// It is a heuristic, not a parser: quoting is ignored, so it may flag text
// that is only an argument, and a command it does not know about passes
// silently. Its findings add friction for the user; the absence of findings
// says nothing about safety.
func inspect(command string) review {
	var r review
	for _, words := range simpleCommands(command) {
		name, args := words[0], words[1:]
		if linuxOnly[name] {
			r.notices = appendUnique(r.notices, fmt.Sprintf("%q is a Linux tool that FreeBSD does not have; the model ignored its instructions.", name))
		}
		if why := danger(name, args); why != "" {
			r.dangers = appendUnique(r.dangers, why)
		}
	}
	if pipeToShell.MatchString(command) {
		r.dangers = appendUnique(r.dangers, "pipes data straight into a shell")
	}
	for _, m := range truncatingRedirect.FindAllStringSubmatch(command, -1) {
		switch target := m[1]; {
		case hasAnyPrefix(target, "/etc/", "/boot/", "/usr/local/etc/"):
			r.dangers = appendUnique(r.dangers, "overwrites "+target+" (> replaces the whole file)")
		case strings.HasPrefix(target, "/dev/") && !wordSet("/dev/null /dev/stdout /dev/stderr /dev/tty")[target]:
			r.dangers = appendUnique(r.dangers, "writes directly to the device "+target)
		}
	}
	return r
}

// simpleCommands splits a command line into its simple commands, each as a
// slice of words whose first element is the command's base name.
func simpleCommands(command string) [][]string {
	var cmds [][]string
	for _, segment := range strings.Split(separators.Replace(command), "\n") {
		words := strings.Fields(segment)

		// Skip leading VAR=value assignments and wrappers such as sudo.
		wrapped := false
		for len(words) > 0 && (wrappers[words[0]] || isAssignment(words[0])) {
			wrapped = wrapped || wrappers[words[0]]
			words = words[1:]
		}
		// A wrapper may be followed by options of its own ("sudo -u www rm"),
		// so look ahead for the first command we have an opinion about.
		if wrapped {
			for i, w := range words {
				if name := path.Base(w); linuxOnly[name] || destructive[name] || strings.HasPrefix(name, "newfs") {
					words = words[i:]
					break
				}
			}
		}
		if len(words) > 0 {
			words[0] = path.Base(words[0])
			cmds = append(cmds, words)
		}
	}
	return cmds
}

// danger returns why running the command name with args could destroy data or
// cut off access to the machine, or "" if it sees no reason.
func danger(name string, args []string) string {
	// sub is the subcommand: the first argument that is not an option.
	sub := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}

	switch {
	case name == "rm" && hasFlag(args, "rR"):
		return "deletes files recursively"
	case name == "find" && (contains(args, "-delete") || contains(args, "rm")):
		return "deletes the files it finds"
	case name == "zfs" && (sub == "destroy" || sub == "rollback"):
		return "destroys ZFS datasets or snapshots"
	case name == "zpool" && (sub == "destroy" || sub == "labelclear" || sub == "create"):
		return "destroys or overwrites a ZFS pool"
	case name == "gpart" && (sub == "destroy" || sub == "delete"):
		return "removes disk partitions"
	case strings.HasPrefix(name, "newfs"):
		return "creates a filesystem, erasing what is on the target"
	case name == "dd" && hasArgPrefix(args, "of=/dev/"):
		return "writes raw data to a device"
	case name == "camcontrol" && (sub == "format" || sub == "sanitize"):
		return "erases a disk"
	case name == "bectl" && sub == "destroy":
		return "destroys a boot environment"
	case name == "shutdown", name == "reboot", name == "halt", name == "poweroff":
		return "shuts down or reboots the system"
	case name == "pfctl" && hasFlag(args, "dF"):
		return "disables the firewall or flushes its rules"
	case name == "rmuser", name == "pw" && (contains(args, "userdel") || contains(args, "groupdel") || contains(args, "del")):
		return "removes users or groups"
	case name == "pkg" && (sub == "delete" || sub == "remove") && hasFlag(args, "af"):
		return "removes packages without the usual safeguards"
	}
	return ""
}

// isAssignment reports whether word looks like a VAR=value prefix.
func isAssignment(word string) bool {
	return strings.Contains(word, "=") && !strings.HasPrefix(word, "-")
}

// hasFlag reports whether args contain a short option using any of letters,
// alone or bundled (as in "-rf").
func hasFlag(args []string, letters string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], letters) {
			return true
		}
	}
	return false
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func appendUnique(list []string, s string) []string {
	if contains(list, s) {
		return list
	}
	return append(list, s)
}

// printSuggestion displays the command exactly as it will be passed to
// /bin/sh, followed by anything inspect found.
func printSuggestion(ui styler, model, command string, rev review) {
	fmt.Fprintf(ui.out, "\n  %s %s %s\n\n",
		ui.paint(boldCyan, ">"),
		ui.paint(bold, "Suggested command"),
		ui.paint(dim, "("+model+")"))
	for _, line := range strings.Split(command, "\n") {
		fmt.Fprintf(ui.out, "    %s\n", ui.paint(boldGreen, line))
	}
	fmt.Fprintln(ui.out)

	for _, d := range rev.dangers {
		fmt.Fprintf(ui.out, "  %s %s %s\n", ui.paint(boldRed, "!"), ui.paint(boldRed, "High risk:"), d)
	}
	for _, n := range rev.notices {
		fmt.Fprintf(ui.out, "  %s %s\n", ui.paint(boldYellow, "!"), n)
	}
	if len(rev.dangers)+len(rev.notices) > 0 {
		fmt.Fprintln(ui.out)
	}
}

// confirm asks the user whether to run the suggested command, reading the
// answer from in. Only explicit consent counts: "y" or "yes" normally, and
// the full word "yes" when strict is set because the command looks
// destructive. An empty line, any other input, end of input or a read error
// all mean no.
func confirm(ui styler, in io.Reader, strict bool) bool {
	if strict {
		fmt.Fprintf(ui.out, "  %s %s %s ",
			ui.paint(boldRed, "?"),
			ui.paint(bold, `Type "yes" to execute it anyway`),
			ui.paint(dim, "[yes/N]"))
	} else {
		fmt.Fprintf(ui.out, "  %s %s %s ",
			ui.paint(boldYellow, "?"),
			ui.paint(bold, "Execute this command?"),
			ui.paint(dim, "[y/N]"))
	}

	answer, err := readLine(in)
	if err != nil {
		fmt.Fprintln(ui.out) // the user never pressed Enter
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "yes" || (answer == "y" && !strict)
}

// readLine reads one line from r a byte at a time, so it never consumes
// input past the newline.
func readLine(r io.Reader) (string, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return string(line), nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return "", err
		}
	}
}

// execute runs command through /bin/sh with the caller's stdin, stdout and
// stderr attached and returns the command's exit code. The error is non-nil
// only if the shell could not be run at all.
func execute(command string) (int, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// The terminal delivers Ctrl-C to the child as well. Catch it here so we
	// outlive the child and can report its exit status. Catching (rather than
	// ignoring) matters: an ignored signal would be inherited by the child.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	err := cmd.Run()
	if err == nil {
		return exitOK, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
		// Killed by a signal: follow the shell convention of 128+signal.
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exitError, nil
	}
	return exitError, fmt.Errorf("running /bin/sh: %w", err)
}
