// Command beastiepilot is an AI-powered terminal assistant for FreeBSD.
//
// It sends a natural language request to a local LLM served by Ollama, prints
// the shell command the model suggests, and runs it through /bin/sh once the
// user explicitly confirms on the terminal. The TYPE setting can add an
// explanation, and AUTO_RUN can name commands that need no confirmation.
// Started without a request, it takes them one after another in a session.
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
	defaultEndpoint   = "http://localhost:11434/api/generate"
	defaultModel      = "llama3.2"
	defaultEmbedModel = "nomic-embed-text"
	defaultTimeout    = 2 * time.Minute

	// maxResponseBytes caps how much of an Ollama reply is read. A shell
	// command is tiny; anything near this size is a misbehaving server.
	maxResponseBytes = 1 << 20

	// contextTokens is the context window asked of Ollama with every request.
	// It holds the system prompt, three excerpts and the request with room to
	// spare. Leaving it to the server would not do: some defaults are too
	// small, and a prompt that does not fit is cut from the top, dropping the
	// rules in systemPrompt; FreeBSD's ollama service defaults to 65536, which
	// costs gigabytes of memory for nothing. Asking for the same size every
	// time also keeps Ollama from reloading the model between requests.
	contextTokens = 4096
)

// Process exit codes. When a suggested command is executed, its own exit code
// is returned instead.
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitInterrupted = 130
)

// maxExplanationLines caps how much of a guided-mode explanation is shown.
const maxExplanationLines = 12

// systemPrompt pins the model to FreeBSD and to machine-readable output. The
// "# reason" escape hatch gives it a harmless way to decline, which run
// detects and reports instead of offering it for execution.
//
// The prompt only steers the model. Nothing here is a security control: those
// live in validateCommand, inspect, autoRun and confirm.
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

// explainPrompt is the system prompt for the second request made in guided
// mode. Its answer is only ever displayed, never executed.
const explainPrompt = `You are BeastiePilot, a patient FreeBSD mentor. The user asked for something and was given the shell command shown below. Explain it so that they can decide whether to run it.

- Start with one sentence saying what the command does overall and why it answers the request.
- Then one short line for each part that matters (each command, option and argument), saying what it is for.
- End with one line that starts with "Effect:" and says what will change on the system, or that nothing changes if the command only reads information.
- Plain text only: no markdown, no code fences, no headings. At most 8 lines in total.
- Write in the same language as the user's request.
- Do not suggest a different command.`

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
	def := defaultSettings()
	flag.String("model", def.Model, "Ollama model `name`")
	flag.String("embed-model", def.EmbedModel, "Ollama embedding model `name`, for the index")
	flag.String("endpoint", def.Endpoint, "Ollama generate API `url`")
	flag.Duration("timeout", def.Timeout, "how long to wait for the model")
	flag.String("type", def.Type, "what to do with a suggestion: `guided|safe`")
	flag.String("auto-run", def.AutoRun, "comma-separated `commands` that safe mode runs without asking")
	flag.String("package-manager", def.PackageManager, "install software with `pkg|ports`")
	flag.String("source-path", def.SourcePath, "`directory` holding the FreeBSD source tree")
	flag.String("docs-source", def.DocsSource, "`directories` holding documentation, separated by \":\"")
	flag.String("rag-source", def.RagSource, "`directories` holding your own reference files and books")
	flag.Bool("verbose", false, "show how the suggestion was obtained")
	noBanner := flag.Bool("no-banner", false, "do not draw the Beastie banner")
	configPath := flag.String("config", "", "read settings from `file` instead of the default locations")
	buildIdx := flag.Bool("index", false, "build or update the index of reference material, then exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("beastiepilot", buildVersion())
		return exitOK
	}

	intent := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if intent != "" && *buildIdx {
		// A request and -index exclude each other.
		usage()
		return exitUsage
	}
	// With neither, requests are taken one after another, on the terminal.
	interactive := intent == "" && !*buildIdx
	// The one request that is not for the model: the first-time setup.
	configure := intent == "configure"

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

	if interactive && tty == nil {
		usage()
		return exitUsage
	}
	if mistypedConfigure(intent) {
		ui.errorf("%q looks like a mistyped \"configure\" and was not sent to the model; to set this machine up, run \"beastiepilot configure\"", intent)
		return exitUsage
	}
	// Nothing goes to a model, neither a request nor text to index, until the
	// setup has finished once: before that there may be no Ollama to ask.
	if !configure && !isConfigured() {
		ui.errorf("not set up yet: run \"beastiepilot configure\" first; nothing was sent to the model")
		return exitError
	}

	// Settings come from the defaults, then the configuration files, then the
	// environment; flags given on the command line override them all.
	cfg, configFiles, err := loadSettings(*configPath)
	saved := cfg
	flagKeys := map[string]string{
		"model": "MODEL", "embed-model": "EMBED_MODEL", "endpoint": "ENDPOINT", "timeout": "TIMEOUT", "type": "TYPE",
		"auto-run": "AUTO_RUN", "package-manager": "PACKAGE_MANAGER", "verbose": "VERBOSE",
		"source-path": "SOURCE_PATH", "docs-source": "DOCS_SOURCE", "rag-source": "RAG_SOURCE",
	}
	flag.Visit(func(f *flag.Flag) {
		if key, ok := flagKeys[f.Name]; ok && err == nil {
			if setErr := cfg.set(key, f.Value.String()); setErr != nil {
				err = fmt.Errorf("-%s: %w", f.Name, setErr)
			}
		}
	})
	if err != nil {
		ui.errorf("%v", err)
		return exitUsage
	}
	if *noBanner {
		cfg.Banner = bannerOff
	}

	var art bannerArt
	if ui.tty && cfg.Banner != bannerOff {
		art = printBanner(ui, cfg)
	}

	if *buildIdx {
		return runIndex(ui, cfg, nil)
	}
	if configure {
		if tty == nil {
			ui.errorf("configure asks questions and needs a terminal")
			return exitError
		}
		file := *configPath
		if file == "" {
			file = os.Getenv("BEASTIEPILOT_CONFIG")
		}
		if file == "" {
			file = userConfigPath()
		}
		return newSetup(ui, tty, cfg, saved, file, thisMachine(tty)).run()
	}

	p := &pilot{ui: ui, cfg: cfg, configFiles: configFiles, root: os.Geteuid() == 0}
	if tty != nil {
		p.tty, p.in = tty, tty
	}
	if interactive {
		return p.session()
	}
	p.animate = art.animated
	return p.answer(intent)
}

// pilot answers requests. It holds what stays the same from one request to
// the next.
type pilot struct {
	ui          styler
	tty         *os.File  // the controlling terminal, or nil without one
	in          io.Reader // where approval is read from: the terminal, or nil
	cfg         settings
	configFiles []string
	root        bool       // whoever runs this is root
	animate     bool       // the banner is right above, to animate while waiting
	inSession   bool       // requests come one after another, on the terminal
	past        []exchange // the earlier requests of the session
}

// answer takes one request through to the end: it asks the model for a
// command, shows it and, once the user agrees, runs it. It returns the exit
// code the request ends with.
func (p *pilot) answer(intent string) int {
	ui, cfg := p.ui, p.cfg

	// With an index, look up the documentation closest to the request and
	// let the model read it. Failing to is not fatal: the model can still
	// answer from what it knows, and the user is told.
	var excerpts []hit
	var retrieveErr error
	if _, statErr := os.Stat(indexPath()); statErr == nil {
		_, retrieveErr = waiting(ui, cfg, p.animate, "Searching the reference material", func(ctx context.Context) (err error) {
			excerpts, err = retrieve(ctx, cfg, intent)
			return err
		})
		if errors.Is(retrieveErr, context.Canceled) {
			return p.queryFailure(retrieveErr)
		}
	}

	system := systemPrompt + cfg.promptPreferences() + hostContext() + sessionSection(p.past) + referenceSection(excerpts)
	reply, elapsed, err := ask(ui, cfg, p.animate, "Asking "+cfg.Model, system, intent)
	if err != nil {
		return p.queryFailure(err)
	}

	command := cleanCommand(reply)
	if cfg.Verbose {
		printDetails(ui, cfg, p.configFiles, elapsed, reply, command, excerpts)
	}
	if err := validateCommand(command); err != nil {
		p.fail("%v", err)
		return exitError
	}
	if reason, ok := declined(command); ok {
		fmt.Fprintf(ui.out, "\n  %s %s\n", ui.paint(boldYellow, "!"), reason)
		if !p.inSession {
			fmt.Fprintln(ui.out)
		}
		p.remember(intent, "", "you gave no command: "+reason)
		return exitError
	}

	rev := inspect(command)
	rev.notices = append(rev.notices, hostNotices()...)
	// A command that only root can run goes through the tool this machine has
	// for that, once the user has agreed to it.
	var raise *elevation
	if name := needsRoot(command); name != "" && !p.root {
		e := asRoot(command, exec.LookPath)
		raise = &e
		rev.notices = append(rev.notices, e.notice(name))
	}
	if retrieveErr != nil {
		reason := retrieveErr.Error()
		if errors.Is(retrieveErr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("%s did not answer within %s", cfg.EmbedModel, cfg.Timeout)
		}
		rev.notices = append(rev.notices, "The reference material was not consulted: "+reason)
	}
	printSuggestion(ui, cfg.Model, command, rev)

	if cfg.Type == typeGuided {
		raw, _, err := ask(ui, cfg, false, "Explaining the command", explainPrompt, "Request: "+intent+"\nCommand: "+command)
		explanation := cleanExplanation(raw)
		switch {
		case errors.Is(err, context.Canceled):
			return p.queryFailure(err)
		case err != nil || explanation == "":
			// The command is still valid; the user decides without the commentary.
			fmt.Fprintf(ui.out, "  %s %s\n\n", ui.paint(boldYellow, "!"), "The model gave no explanation for this command.")
		default:
			printExplanation(ui, explanation)
		}
	}

	// Decide whether to run it. The question is skipped only in safe mode, for
	// a command made entirely of AUTO_RUN entries that inspect did not flag as
	// destructive and that runs as the user.
	destructive := len(rev.dangers) > 0
	auto, why := false, ""
	switch {
	case cfg.Type != typeSafe || cfg.AutoRun == "":
	case destructive:
		why = "it looks destructive"
	case raise != nil:
		why = "it would run as root"
	default:
		auto, why = autoRun(command, cfg.autoRunList())
	}
	if cfg.Verbose && why != "" {
		fmt.Fprintf(ui.out, "  %s\n\n", ui.paint(dim, "Not run automatically because "+why+"."))
	}
	switch {
	case auto:
		fmt.Fprintf(ui.out, "  %s %s\n\n", ui.paint(boldYellow, "!"), "Listed in AUTO_RUN: running without confirmation.")
	case p.in == nil:
		p.fail("no terminal available to ask for confirmation; nothing was executed")
		return exitError
	default:
		if !p.confirm(destructive, raise != nil) {
			fmt.Fprintf(ui.out, "  %s\n", ui.paint(dim, "Aborted. Nothing was executed."))
			p.remember(intent, command, "the user chose not to run it")
			return exitOK
		}
		fmt.Fprintln(ui.out)
	}

	run, how := func() (int, error) { return execute(command) }, "it ran"
	if raise != nil {
		run, how = raise.execute, "it ran as root"
	}
	code, err := run()
	if err != nil {
		p.fail("%v", err)
		p.remember(intent, command, "it could not be run")
		return exitError
	}
	outcome, status := fmt.Sprintf("%s and exited with status %d", how, code), fmt.Sprintf("Exit status %d", code)
	if name := foundNothing(command, code); name != "" {
		// Said to the model as well, so that the next request can build on it.
		outcome += ", as " + name + " does when nothing matches"
		status += ": " + name + " ends that way when nothing matches."
	}
	p.remember(intent, command, outcome)
	if p.inSession && code != exitOK {
		fmt.Fprintf(ui.out, "\n  %s\n", ui.paint(dim, status))
	}
	return code
}

// searchers are the commands that end with status 1, usually having printed
// nothing, when what they looked for is not there: an answer more than a
// failure, and one that the status alone does not get across.
var searchers = [][]string{{"grep"}, {"egrep"}, {"fgrep"}, {"pgrep"}, {"which"}, {"pkg", "search"}}

// foundNothing returns the name of the command if it is one of the searchers
// and status is the one they report no match with, and "" otherwise. Only a
// command on its own counts: with several on the line, the status may be
// another's.
func foundNothing(command string, status int) string {
	if status != 1 {
		return ""
	}
	commands, err := splitCommands(command)
	if err != nil || len(commands) != 1 {
		return ""
	}
	for _, name := range searchers {
		if listed(commands[0], [][]string{name}) {
			return strings.Join(name, " ")
		}
	}
	return ""
}

// ask sends one request to the model and returns its raw reply and how long
// it took.
func ask(ui styler, cfg settings, animate bool, status, system, prompt string) (string, time.Duration, error) {
	var reply string
	elapsed, err := waiting(ui, cfg, animate, status, func(ctx context.Context) (err error) {
		reply, err = suggest(ctx, cfg.Endpoint, cfg.Model, system, prompt)
		return err
	})
	return reply, elapsed, err
}

// waiting runs request, which talks to Ollama, and returns how long it took.
// Meanwhile it shows status on the terminal, with how long the wait has been
// once it is a long one, animating the banner printed just above when animate
// is set. request is cancelled by Ctrl-C and after TIMEOUT.
func waiting(ui styler, cfg settings, animate bool, status string, request func(context.Context) error) (time.Duration, error) {
	// Let Ctrl-C abort a slow model instead of leaving the user waiting. The
	// handler is removed on return, so Ctrl-C works normally at the prompt.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	if ui.tty {
		defer fmt.Fprint(ui.out, "\r\033[K") // erase the status line, after the animation has stopped
		defer startAnimation(ui, status, animate).stop()
	}

	start := time.Now()
	err := request(ctx)
	return time.Since(start), err
}

// fail reports that a request could not be seen through: on stderr, where
// scripts and logs expect it, or within the session when there is one.
func (p *pilot) fail(format string, args ...any) {
	if !p.inSession {
		p.ui.errorf(format, args...)
		return
	}
	fmt.Fprintln(p.ui.out)
	for i, line := range wrap(fmt.Sprintf(format, args...), 70) {
		mark := " "
		if i == 0 {
			mark = p.ui.paint(boldRed, "!")
		}
		fmt.Fprintf(p.ui.out, "  %s %s\n", mark, line)
	}
}

// queryFailure reports why a request to the model failed and returns the exit
// code to finish with.
func (p *pilot) queryFailure(err error) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		p.fail("no answer from %s within %s.%s To wait longer, set TIMEOUT in the configuration or start with -timeout, as in: beastiepilot -timeout 10m",
			p.cfg.Model, p.cfg.Timeout, whySlow(p.cfg))
		return exitError
	case errors.Is(err, context.Canceled):
		p.fail("interrupted")
		return exitInterrupted
	default:
		p.fail("%v", err)
		return exitError
	}
}

// whySlow says what Ollama reports about the model that did not answer in
// time, as a sentence to follow the complaint, or "" when Ollama says nothing
// that bears on it. It asks now, after the wait: a request given up on while
// the model was still being loaded leaves no model behind.
func whySlow(cfg settings) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loaded, cpuOnly, err := modelLoaded(ctx, cfg.Endpoint, cfg.Model)
	switch {
	case err != nil:
		return ""
	case !loaded:
		return " Ollama does not have the model loaded: loading it may take longer than that here, or Ollama may be busy with other work, such as indexing."
	case cpuOnly:
		return " Ollama has the model loaded now and runs it on the CPU alone, without a GPU, so answers take a while and the first one the longest: asking again may be enough."
	}
	return " Ollama has the model loaded now: asking again may be enough."
}

// mistypedConfigure reports whether intent is a single word that is not
// "configure" but was probably meant to be: a typing slip, a shortened form
// or another capitalization. Sent to the model, it would only come back as a
// command nobody asked for.
func mistypedConfigure(intent string) bool {
	const want = "configure"
	word := strings.ToLower(intent)
	if intent == want || strings.ContainsAny(word, " \t\r\n") {
		return false
	}
	if len(word) >= 4 && strings.HasPrefix(want, word) {
		return true
	}
	if d := len(word) - len(want); d < -2 || d > 2 {
		return false
	}
	return editDistance(word, want) <= 2
}

// editDistance counts the insertions, deletions and substitutions of one
// character, and the swaps of two neighbours, that turn a into b.
func editDistance(a, b string) int {
	before, prev, cur := make([]int, len(b)+1), make([]int, len(b)+1), make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				cur[j] = min(cur[j], before[j-2]+1)
			}
		}
		before, prev, cur = prev, cur, before
	}
	return prev[len(b)]
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `Usage: beastiepilot [flags] <what you want to do>
       beastiepilot [flags]
       beastiepilot [flags] configure
       beastiepilot [flags] -index

Translates a plain-language request into a FreeBSD shell command using a local
Ollama model, then asks for confirmation before running it. With -type guided
it also explains the command. Commands listed in AUTO_RUN run without asking.

With no request it opens a session on the terminal and takes requests one
after another, each building on the ones before. /help lists what else can be
typed there; /exit or Ctrl-D leaves.

The single word "configure" sets the machine up instead: it offers to install
and start Ollama, download the models, install the FreeBSD source tree and
documentation, save your settings and build the index. It asks before each
step, and can be run again at any time. Until it has finished once, with
BeastiePilot ready, requests and -index are refused and nothing is sent to a
model.

With -index it reads the manual pages, documentation and files of your own
under SOURCE_PATH, DOCS_SOURCE and RAG_SOURCE into an index. From then on the
passages closest to each request are given to the model along with it.

Examples:
  beastiepilot install nginx and enable it at boot
  beastiepilot "show the ten largest files under /var"
  beastiepilot -model qwen2.5-coder "snapshot zroot/usr/home"

Quote the request if it contains characters your shell would interpret
(such as ? * ' or |).

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(out, `
Configuration:
  Settings are read from %s and then
  %s, as KEY=value lines.
  Keys: %s.
  Each can also be set as BEASTIEPILOT_<KEY> in the environment; flags win.

Environment:
  BEASTIEPILOT_CONFIG  read this file instead of the default locations
  NO_COLOR             disable colored output when set
`, systemConfigPath, userConfigPath(), strings.Join(wrap(strings.Join(settingKeys, ", "), 70), "\n        "))
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
	// Temperature 0 keeps suggestions as deterministic as the model allows.
	options := map[string]any{"temperature": 0, "num_ctx": contextTokens}
	body, err := json.Marshal(generateRequest{
		Model:   model,
		System:  system,
		Prompt:  intent,
		Stream:  false,
		Options: options,
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
		return "", fmt.Errorf("cannot reach Ollama at %s (is it running? try \"beastiepilot configure\"): %w", endpoint, err)
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
	s = afterReasoning(strings.ReplaceAll(s, "\r\n", "\n"))

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

// afterReasoning drops the scratchpad some reasoning models emit before their
// answer and returns what follows it, trimmed.
func afterReasoning(s string) string {
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

// cleanExplanation prepares a guided-mode explanation for display. Unlike a
// command it is never executed, so instead of being rejected it is simply
// stripped of anything that could drive the terminal, of Markdown decoration
// and of blank lines, and cut to maxExplanationLines.
func cleanExplanation(s string) string {
	s = afterReasoning(strings.ReplaceAll(s, "\r\n", "\n"))
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	s = strings.NewReplacer("**", "", "`", "").Replace(s)

	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
		if len(lines) == maxExplanationLines {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// wrap breaks text into lines of at most width columns at word boundaries,
// keeping the line breaks it already has. A word longer than width gets a
// line of its own.
func wrap(text string, width int) []string {
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Fields(paragraph) {
			switch {
			case line == "":
				line = word
			case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
				line += " " + word
			default:
				lines = append(lines, line)
				line = word
			}
		}
		lines = append(lines, line)
	}
	return lines
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

// printDetails shows, for VERBOSE, where the suggestion came from. The raw
// reply is quoted so that nothing in it can affect the terminal.
func printDetails(ui styler, cfg settings, configFiles []string, elapsed time.Duration, reply, command string, excerpts []hit) {
	config := "none (defaults)"
	if len(configFiles) > 0 {
		config = strings.Join(configFiles, ", ")
	}
	rows := [][2]string{
		{"config", config},
		{"endpoint", cfg.Endpoint},
		{"model", fmt.Sprintf("%s, answered in %s", cfg.Model, elapsed.Round(10*time.Millisecond))},
		{"packages", cfg.PackageManager},
	}
	if cfg.AutoRun != "" {
		rows = append(rows, [2]string{"auto-run", cfg.AutoRun})
	}
	for _, e := range excerpts {
		// Excerpts were cleaned of control characters when they were indexed.
		rows = append(rows, [2]string{"reference", fmt.Sprintf("%s (%.2f) %s", docTitle(e.path), e.score, e.path)})
	}
	if strings.TrimSpace(reply) != command {
		rows = append(rows, [2]string{"raw reply", fmt.Sprintf("%q", reply)})
	}
	fmt.Fprintln(ui.out)
	for _, row := range rows {
		fmt.Fprintf(ui.out, "  %s\n", ui.paint(dim, fmt.Sprintf("%-10s %s", row[0], row[1])))
	}
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
		// A long one continues under its own words, not under the mark.
		mark := ui.paint(boldYellow, "!")
		for _, line := range wrap(n, textWidth(ui)) {
			fmt.Fprintf(ui.out, "  %s %s\n", mark, line)
			mark = " "
		}
	}
	if len(rev.dangers)+len(rev.notices) > 0 {
		fmt.Fprintln(ui.out)
	}
}

// textWidth returns how wide a line of running text may be: what fits the
// terminal with a margin on both sides, and no more than reads well.
func textWidth(ui styler) int {
	width := 76
	if f, ok := ui.out.(*os.File); ok {
		if cols, _, ok := termSize(f); ok && cols-6 < width {
			width = cols - 6
		}
	}
	return max(width, 20)
}

// printExplanation shows the model's account of the suggested command,
// wrapped to fit the terminal.
func printExplanation(ui styler, explanation string) {
	fmt.Fprintf(ui.out, "  %s %s\n\n", ui.paint(boldCyan, ">"), ui.paint(bold, "Why this command"))
	for _, line := range wrap(explanation, textWidth(ui)) {
		fmt.Fprintf(ui.out, "    %s\n", line)
	}
	fmt.Fprintln(ui.out)
}

// confirm asks the user whether to run the suggested command, reading the
// answer from in. Only explicit consent counts: "y" or "yes" normally, and
// the full word "yes" when strict is set because the command looks
// destructive. An empty line, any other input, end of input or a read error
// all mean no. asRoot says that the command would run as root, which the
// question then spells out.
func confirm(ui styler, in io.Reader, strict, asRoot bool) bool {
	fmt.Fprint(ui.out, question(ui, strict, asRoot))
	answer, err := readLine(in)
	if err != nil {
		fmt.Fprintln(ui.out) // the user never pressed Enter
		return false
	}
	return consent(answer, strict)
}

// question returns the words that ask for approval, up to where the answer
// is typed.
func question(ui styler, strict, asRoot bool) string {
	who := ""
	if asRoot {
		who = " as root"
	}
	if strict {
		return fmt.Sprintf("  %s %s %s ",
			ui.paint(boldRed, "?"),
			ui.paint(bold, `Type "yes" to execute it`+who+` anyway`),
			ui.paint(dim, "[yes/N]"))
	}
	return fmt.Sprintf("  %s %s %s ",
		ui.paint(boldYellow, "?"),
		ui.paint(bold, "Execute this command"+who+"?"),
		ui.paint(dim, "[y/N]"))
}

// consent reports whether answer approves the command.
func consent(answer string, strict bool) bool {
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
	return runCommand(exec.Command("/bin/sh", "-c", command))
}

// runCommand runs cmd as execute describes.
func runCommand(cmd *exec.Cmd) (int, error) {
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
	return exitError, fmt.Errorf("running %s: %w", cmd.Args[0], err)
}
