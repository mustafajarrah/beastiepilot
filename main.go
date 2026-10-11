// Command beastiepilot is an AI-powered terminal assistant for FreeBSD.
//
// It sends a natural language request to a local LLM served by Ollama, prints
// the shell command the model suggests, and runs it through /bin/sh only after
// the user explicitly confirms.
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
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unicode"
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
)

// styler renders the interface. Everything BeastiePilot itself prints goes to
// stderr so that stdout carries only the output of the executed command.
type styler struct {
	tty   bool // stderr is a terminal
	color bool // ANSI colors are wanted
}

func newStyler() styler {
	tty := isTerminal(os.Stderr)
	_, noColor := os.LookupEnv("NO_COLOR")
	return styler{tty: tty, color: tty && !noColor && os.Getenv("TERM") != "dumb"}
}

// paint wraps text in the given SGR code when colors are enabled.
func (s styler) paint(code, text string) string {
	if !s.color {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func (s styler) errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", s.paint(boldRed, "beastiepilot:"), fmt.Sprintf(format, args...))
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

	ui := newStyler()

	// Let Ctrl-C abort a slow model instead of leaving the user waiting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if ui.tty {
		fmt.Fprint(os.Stderr, ui.paint(dim, fmt.Sprintf("  Asking %s ...", *model)))
	}
	reply, err := suggest(ctx, *endpoint, *model, intent)
	if ui.tty {
		fmt.Fprint(os.Stderr, "\r\033[K") // erase the status line
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
		fmt.Fprintf(os.Stderr, "\n  %s %s\n\n", ui.paint(boldYellow, "!"), reason)
		return exitError
	}

	printSuggestion(ui, *model, command)
	if !confirm(ui, os.Stdin) {
		fmt.Fprintf(os.Stderr, "  %s\n", ui.paint(dim, "Aborted. Nothing was executed."))
		return exitOK
	}
	fmt.Fprintln(os.Stderr)

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
  BEASTIEPILOT_ENDPOINT  default for -endpoint
  BEASTIEPILOT_MODEL     default for -model
  NO_COLOR               disable colored output when set
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

// suggest asks the Ollama model at endpoint to translate intent into a shell
// command and returns the model's raw reply.
func suggest(ctx context.Context, endpoint, model, intent string) (string, error) {
	body, err := json.Marshal(generateRequest{
		Model:  model,
		System: systemPrompt,
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

// printSuggestion displays the command exactly as it will be passed to /bin/sh.
func printSuggestion(ui styler, model, command string) {
	fmt.Fprintf(os.Stderr, "\n  %s %s %s\n\n",
		ui.paint(boldCyan, ">"),
		ui.paint(bold, "Suggested command"),
		ui.paint(dim, "("+model+")"))
	for _, line := range strings.Split(command, "\n") {
		fmt.Fprintf(os.Stderr, "    %s\n", ui.paint(boldGreen, line))
	}
	fmt.Fprintln(os.Stderr)
}

// confirm asks the user whether to run the suggested command. Only an
// explicit "y" or "yes" counts as consent; an empty line, any other input,
// end of input or a read error all mean no.
func confirm(ui styler, in io.Reader) bool {
	fmt.Fprintf(os.Stderr, "  %s %s %s ",
		ui.paint(boldYellow, "?"),
		ui.paint(bold, "Execute this command?"),
		ui.paint(dim, "[y/N]"))

	answer, err := readLine(in)
	if err != nil {
		fmt.Fprintln(os.Stderr) // the user never pressed Enter
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// readLine reads one line from r a byte at a time. Unlike a buffered reader
// it never consumes input past the newline, so whatever follows on stdin is
// still there for the command that may run next.
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

// execute runs command through /bin/sh with the terminal attached and returns
// the command's exit code. The error is non-nil only if the shell could not be
// run at all.
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
