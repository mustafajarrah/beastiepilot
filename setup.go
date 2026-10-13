package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// The configure command: a first-time setup that brings the machine to the
// point where BeastiePilot works. It checks for Ollama, the models, the
// reference material and the index, and offers to provide what is missing.
// Every step shows what it would run and asks first, and a step whose result
// is already in place is skipped, so the command can be run again at any time.

const (
	// handbookDir is where the FreeBSD documentation packages install the
	// Handbook, the FAQ and the articles.
	handbookDir = "/usr/local/share/doc/freebsd"

	// programDir is where a program for every user of the machine goes.
	programDir = "/usr/local/bin"

	// startPatience is how long Ollama is given to answer after being started.
	startPatience = 30 * time.Second

	// startSettle is how long a service that was just started is left alone
	// before asking whether it is still running.
	startSettle = 2 * time.Second

	// serviceLogLines is how much of the Ollama service's log is shown when
	// the server does not come up.
	serviceLogLines = 10

	// ollamaHome is the home directory offered to FreeBSD's Ollama service
	// when it has none. The service keeps the models beneath it already.
	ollamaHome = "/var/lib/ollama"

	// probeTimeout is how long a question to Ollama about its state may take.
	probeTimeout = 5 * time.Second
)

// modelSizes says how much the default models download, to tell the user
// before asking. Ollama reports the size of any other model as it downloads.
var modelSizes = map[string]string{
	defaultModel:      "about 2.0 GB",
	defaultEmbedModel: "about 270 MB",
}

// sampleConfig is the documented configuration file. A new configuration
// starts as a copy of it.
//
//go:embed beastiepilot.conf.sample
var sampleConfig string

// errInterrupted is returned by machine.run when Ctrl-C ended the command.
var errInterrupted = errors.New("interrupted")

// halt is the error of a step after which the setup cannot go on. The step
// has already said why; the value is the exit code.
type halt int

func (h halt) Error() string { return fmt.Sprintf("setup stopped with exit code %d", int(h)) }

// machine is what the setup needs to know about the computer it runs on, and
// the means to change it. It is a value so that tests can stand in for a
// FreeBSD host.
type machine struct {
	goos   string
	root   bool
	exe    string // this program's file, or ""
	srcDir string // where FreeBSD installs its source tree
	docDir string // where FreeBSD installs its documentation packages

	lookPath func(name string) (string, error)
	// run executes a command with the terminal attached.
	run func(argv []string) error
	// output executes a command and returns what it printed, trimmed.
	output func(argv ...string) (string, error)
}

// thisMachine describes the computer the program is running on. Commands it
// runs talk to the user through tty.
func thisMachine(tty *os.File) machine {
	exe, _ := os.Executable()
	return machine{
		goos:     runtime.GOOS,
		root:     os.Geteuid() == 0,
		exe:      exe,
		srcDir:   defaultSourcePath,
		docDir:   handbookDir,
		lookPath: exec.LookPath,
		run: func(argv []string) error {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty

			// Ctrl-C reaches the command as well. Catch it here to outlive
			// the command and say what happened.
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			defer signal.Stop(sig)

			err := cmd.Run()
			select {
			case <-sig:
				return errInterrupted
			default:
				return err
			}
		},
		output: func(argv ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
			return strings.TrimSpace(string(out)), err
		},
	}
}

// rootCommand returns how to run chain, a line of shell commands that needs
// root, and how to show that to the user. Root runs it directly; anyone else
// goes through doas or sudo when one is installed and through su otherwise.
//
// chain must not contain a single quote, and must mean the same to sh and
// csh, since su hands it to root's shell.
func (m machine) rootCommand(chain string) (argv []string, shown string) {
	if m.root {
		return []string{"/bin/sh", "-c", chain}, chain
	}
	for _, tool := range []string{"doas", "sudo"} {
		if _, err := m.lookPath(tool); err != nil {
			continue
		}
		if !strings.ContainsAny(chain, "&|;<>") {
			// One plain command needs no shell around it.
			return append([]string{tool}, strings.Fields(chain)...), tool + " " + chain
		}
		return []string{tool, "sh", "-c", chain}, tool + " sh -c '" + chain + "'"
	}
	return []string{"su", "root", "-c", chain}, "su root -c '" + chain + "'"
}

// setup is one run of the configure command.
type setup struct {
	ui       styler
	in       io.Reader // where the answers come from: the terminal
	m        machine
	cfg      settings      // the settings in effect, including what the steps change
	saved    settings      // the settings as the files and the environment give them
	file     string        // the configuration file that changes are saved to, or ""
	patience time.Duration // how long to wait for Ollama to start
	settle   time.Duration // how long before asking whether its service stopped

	modelOK   bool // the model that writes commands is downloaded
	embedOK   bool // so is the one that indexes text
	wantIndex bool // the user wants reference material
}

// newSetup prepares a run of the configure command. saved holds the settings
// before the command line changed them into cfg, and file is where the
// difference may be saved.
func newSetup(ui styler, in io.Reader, cfg, saved settings, file string, m machine) *setup {
	return &setup{ui: ui, in: in, m: m, cfg: cfg, saved: saved, file: file, patience: startPatience, settle: startSettle}
}

// run performs the setup and returns the exit code: zero once BeastiePilot is
// ready to be used.
func (s *setup) run() int {
	s.heading("Setting up BeastiePilot")
	s.text("Each step says what it would do and asks first; nothing changes without a yes. " +
		"Run this again at any time: what is already in place is skipped.")
	if s.m.root {
		s.warn("You are root, so the configuration and the index made here will be root's own. " +
			"Unless root is who will use BeastiePilot, run this as that user instead: it asks for root where it needs it.")
	}

	for _, step := range []func() error{s.ollama, s.model, s.reference, s.save, s.index, s.program} {
		var h halt
		if err := step(); errors.As(err, &h) {
			if h == exitError {
				fmt.Fprintln(s.ui.out)
				s.text("BeastiePilot is not ready yet. Run beastiepilot configure again once that is sorted out.")
				fmt.Fprintln(s.ui.out)
			}
			return int(h)
		}
	}
	return s.summary()
}

// heading starts a step.
func (s *setup) heading(title string) {
	fmt.Fprintf(s.ui.out, "\n  %s %s\n", s.ui.paint(boldCyan, ">"), s.ui.paint(bold, title))
}

// text prints a paragraph.
func (s *setup) text(format string, args ...any) {
	for _, line := range wrap(fmt.Sprintf(format, args...), 72) {
		fmt.Fprintf(s.ui.out, "    %s\n", line)
	}
}

// item prints a paragraph behind a mark.
func (s *setup) item(code, mark, format string, args ...any) {
	for i, line := range wrap(fmt.Sprintf(format, args...), 70) {
		if i == 0 {
			fmt.Fprintf(s.ui.out, "    %s %s\n", s.ui.paint(code, mark), line)
		} else {
			fmt.Fprintf(s.ui.out, "      %s\n", line)
		}
	}
}

// note continues the item above it with another paragraph.
func (s *setup) note(format string, args ...any) {
	for _, line := range wrap(fmt.Sprintf(format, args...), 70) {
		fmt.Fprintf(s.ui.out, "      %s\n", line)
	}
}

// done reports something that is in place, warn something that is not, and
// skipped something left as it was.
func (s *setup) done(format string, args ...any)    { s.item(boldGreen, "+", format, args...) }
func (s *setup) warn(format string, args ...any)    { s.item(boldYellow, "!", format, args...) }
func (s *setup) skipped(format string, args ...any) { s.item(dim, "-", format, args...) }

// ask puts a yes-or-no question to the user. Only "y" or "yes" is consent.
func (s *setup) ask(question string) bool {
	fmt.Fprintf(s.ui.out, "    %s %s %s ", s.ui.paint(boldYellow, "?"), s.ui.paint(bold, question), s.ui.paint(dim, "[y/N]"))
	answer, err := readLine(s.in)
	if err != nil {
		fmt.Fprintln(s.ui.out) // the user never pressed Enter
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// offer shows chain, a line of shell commands that needs root, and runs it if
// the user agrees. It reports whether the commands ran and succeeded.
func (s *setup) offer(question, chain string) (bool, error) {
	argv, shown := s.m.rootCommand(chain)
	who := ""
	if !s.m.root {
		who = ", as root"
	}
	fmt.Fprintf(s.ui.out, "      This would run%s:\n\n        %s\n\n", who, s.ui.paint(boldGreen, shown))
	if !s.ask(question) {
		s.skipped("Skipped.")
		return false, nil
	}

	fmt.Fprintln(s.ui.out)
	err := s.m.run(argv)
	fmt.Fprintln(s.ui.out)
	switch {
	case errors.Is(err, errInterrupted):
		s.ui.errorf("interrupted")
		return false, halt(exitInterrupted)
	case err != nil && s.m.root:
		s.warn("That did not work (%v).", err)
		return false, nil
	case err != nil:
		s.warn("That did not work (%v). To do it yourself, run this as root:", err)
		fmt.Fprintf(s.ui.out, "\n        %s\n\n", chain)
		return false, nil
	}
	return true, nil
}

// install returns the command that installs a piece of third-party software
// the way PACKAGE_MANAGER asks for.
func (s *setup) install(origin, pkg string) string {
	if s.cfg.PackageManager == "ports" {
		return "make -C /usr/ports/" + origin + " install clean"
	}
	// No -y: pkg lists what it would install, with the sizes, and asks.
	return "pkg install " + pkg
}

// ollama makes sure the Ollama server answers at ENDPOINT, offering to install
// and start it on FreeBSD. Nothing else can be done without it.
func (s *setup) ollama() error {
	s.heading("Ollama")
	where := s.cfg.Endpoint
	if u, err := url.Parse(where); err == nil && u.Host != "" {
		where = u.Scheme + "://" + u.Host
	}
	version, err := s.probe()
	if err == nil {
		s.done("Running at %s (version %s).", where, version)
		return nil
	}

	_, lookErr := s.m.lookPath("ollama")
	installed := lookErr == nil
	switch {
	case !localEndpoint(s.cfg.Endpoint):
		s.warn("Ollama does not answer at %s (%v). ENDPOINT names another machine: start Ollama there, or change ENDPOINT.", where, err)
		return halt(exitError)
	case s.m.goos != "freebsd" && installed:
		s.warn("Ollama is installed but does not answer at %s. Start it, for example with: ollama serve", where)
		return halt(exitError)
	case s.m.goos != "freebsd":
		s.warn("Ollama, the program that runs the models, is not installed. Get it from https://ollama.com/download and start it.")
		return halt(exitError)
	}

	const start = "sysrc ollama_enable=YES && service ollama start"
	chain, question, housed := start, "Start Ollama, now and at every boot?", false
	if installed {
		s.warn("Ollama is installed but does not answer at %s.", where)
		// It may have been started before and have stopped for want of a home.
		if home := s.homeless(); home != "" {
			chain, question, housed = home+" && "+start, "Give it that home and start Ollama, now and at every boot?", true
		}
	} else {
		s.warn("Ollama, the program that runs the models, is not installed.")
		chain, question = s.install("misc/ollama", "ollama")+" && "+start, "Install Ollama and start it, now and at every boot?"
	}
	for {
		ok, err := s.offer(question, chain)
		if err != nil {
			return err
		}
		if !ok {
			return halt(exitError)
		}
		if version, err = s.await(); err == nil {
			break
		}
		s.warn("Ollama was started but does not answer at %s (%v).", where, err)
		home := ""
		if !housed {
			home = s.homeless()
		}
		if home == "" {
			s.serviceLog()
			return halt(exitError)
		}
		chain, question, housed = home+" && service ollama start", "Give it that home and start Ollama again?", true
	}
	s.done("Running at %s (version %s).", where, version)
	return nil
}

// await gives Ollama time to answer after its service was started, and
// returns its version. It stops waiting once the service is seen to have
// stopped again.
func (s *setup) await() (version string, err error) {
	if s.ui.tty {
		fmt.Fprint(s.ui.out, s.ui.paint(dim, "      Waiting for Ollama to answer ..."))
		defer fmt.Fprint(s.ui.out, "\r\033[K")
	}
	started, pause := time.Now(), min(500*time.Millisecond, s.patience/4)
	for version, err = s.probe(); err != nil && time.Since(started) < s.patience; version, err = s.probe() {
		time.Sleep(pause)
		if time.Since(started) < s.settle {
			continue
		}
		if out, statusErr := s.m.output("service", "ollama", "status"); statusErr != nil && strings.Contains(out, "is not running") {
			return s.probe()
		}
	}
	return version, err
}

// homeless looks in the service's log for the way FreeBSD's Ollama service
// fails as packaged: it runs Ollama as nobody and sets no HOME for that user,
// so Ollama looks for its key under /, cannot create the directory and stops.
// When the log ends that way, homeless says so and returns the commands that
// give the service a home, through the ollama_env setting of rc.conf. It
// returns "" when that is not what happened, or when ollama_env is in use
// already.
func (s *setup) homeless() string {
	path := s.serviceLogPath()
	lines, err := lastLines(path, serviceLogLines)
	if err != nil {
		return ""
	}
	failure := ""
	for _, line := range lines {
		if strings.Contains(line, "could not create directory") && strings.Contains(line, "/.ollama") {
			failure = line
		}
	}
	user, err := s.m.output("sysrc", "-n", "ollama_user")
	if err != nil || user == "" {
		user = "nobody"
	}
	if env, err := s.m.output("sysrc", "-n", "ollama_env"); failure == "" || !plainWord(user) || (err == nil && env != "") {
		return ""
	}
	s.note("Its log, %s, says why it stopped:", path)
	fmt.Fprintf(s.ui.out, "\n        %s\n\n", s.ui.paint(dim, failure))
	s.note("The service runs Ollama as %s, with no home directory of its own, and Ollama needs one to keep its key in. %s will do.", user, ollamaHome)
	return "mkdir -p " + ollamaHome + " && chown " + user + " " + ollamaHome + " && sysrc ollama_env=HOME=" + ollamaHome
}

// serviceLogPath returns where FreeBSD's Ollama service keeps its log: what
// rc.conf says, or else the name the service script makes up from the user it
// runs as.
func (s *setup) serviceLogPath() string {
	if path, err := s.m.output("sysrc", "-n", "ollama_log"); err == nil && path != "" {
		return path
	}
	user, err := s.m.output("sysrc", "-n", "ollama_user")
	if err != nil || user == "" {
		user = "nobody"
	}
	return "/var/log/ollama-" + user + ".log"
}

// serviceLog shows the end of the log of FreeBSD's Ollama service, which is
// where a server that stopped right after it was started says why.
func (s *setup) serviceLog() {
	path := s.serviceLogPath()
	lines, err := lastLines(path, serviceLogLines)
	switch {
	case errors.Is(err, fs.ErrPermission) && plainWord(path):
		_, shown := s.m.rootCommand("tail -n 20 " + path)
		s.note("Its log should say why, and reading it needs root:")
		fmt.Fprintf(s.ui.out, "\n        %s\n", s.ui.paint(boldGreen, shown))
	case errors.Is(err, fs.ErrPermission):
		s.note("Its log should say why, and reading it needs root: %s", path)
	case len(lines) == 0:
		s.note("Its log, %s, says nothing about it.", path)
	default:
		s.note("Its log, %s, ends with:", path)
		fmt.Fprintln(s.ui.out)
		for _, line := range lines {
			fmt.Fprintf(s.ui.out, "        %s\n", s.ui.paint(dim, line))
		}
	}
}

// lastLines returns up to n of the last lines of the file at path, without
// the blank ones, made fit to print: control characters are dropped, so that
// the file cannot drive the terminal, and long lines are cut short.
func lastLines(path string, n int) ([]string, error) {
	const tail, width = 16 << 10, 160
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cut := false
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		if _, err := f.Seek(-tail, io.SeekEnd); err != nil {
			return nil, err
		}
		cut = true
	}
	raw, err := io.ReadAll(io.LimitReader(f, tail))
	if err != nil {
		return nil, err
	}
	all := strings.Split(string(raw), "\n")
	if cut {
		all = all[1:] // the rest of a line that started earlier
	}
	var lines []string
	for _, line := range all {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			switch {
			case r == '\t':
				return ' '
			case !unicode.IsPrint(r):
				return -1
			}
			return r
		}, line))
		if r := []rune(line); len(r) > width {
			line = string(r[:width]) + " ..."
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// probe asks Ollama for its version, which tells whether it is running.
func (s *setup) probe() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return ollamaVersion(ctx, s.cfg.Endpoint)
}

// model makes sure the model that writes the commands is downloaded.
func (s *setup) model() (err error) {
	s.heading("Model")
	s.modelOK, err = s.download(s.cfg.Model, "the model that writes the commands")
	return err
}

// download makes sure the Ollama model name is on the machine, downloading it
// if the user agrees. role says what the model is for.
func (s *setup) download(name, role string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	have, err := hasModel(ctx, s.cfg.Endpoint, name)
	cancel()
	switch {
	case err != nil:
		s.warn("Cannot tell whether %s is downloaded: %v", name, err)
		return false, nil
	case have:
		s.done("%s, %s, is downloaded.", name, role)
		return true, nil
	}

	size := ""
	if known := modelSizes[name]; known != "" {
		size = " (" + known + ")"
	}
	s.warn("%s, %s, has not been downloaded%s.", name, role, size)
	if !s.ask("Download " + name + "?") {
		s.skipped("Skipped. To get it yourself: ollama pull %s", name)
		return false, nil
	}

	// Ctrl-C stops the download; Ollama keeps what it has and resumes later.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shown := ""
	err = pullModel(ctx, s.cfg.Endpoint, name, func(done, total int64) {
		line := "Downloading " + name
		if total > 0 {
			line += fmt.Sprintf(": %d%% of %s", done*100/total, formatBytes(total))
		}
		if s.ui.tty && line != shown {
			fmt.Fprintf(s.ui.out, "\r\033[K      %s", s.ui.paint(dim, line))
			shown = line
		}
	})
	if s.ui.tty {
		fmt.Fprint(s.ui.out, "\r\033[K")
	}
	switch {
	case errors.Is(err, context.Canceled):
		s.text("Ollama keeps the part it has; the download resumes when asked for again.")
		s.ui.errorf("interrupted")
		return false, halt(exitInterrupted)
	case err != nil:
		s.warn("%s was not downloaded: %v", name, err)
		return false, nil
	}
	s.done("%s, %s, is downloaded.", name, role)
	return true, nil
}

// reference prepares the material the index is built from: the model that
// indexes text, the FreeBSD source tree and the documentation.
func (s *setup) reference() (err error) {
	s.heading("Reference material")
	if s.cfg.SourcePath == "" && s.cfg.DocsSource == "" && s.cfg.RagSource == "" {
		s.skipped("Turned off: SOURCE_PATH, DOCS_SOURCE and RAG_SOURCE are all empty.")
		return nil
	}
	if _, statErr := os.Stat(indexPath()); statErr != nil {
		s.text("BeastiePilot can read manual pages and documentation into an index, and show the model the passages " +
			"closest to each request so that it need not answer from memory. That takes a second, small model and some disk space.")
		if !s.ask("Set up reference material?") {
			s.skipped("Skipped.")
			return nil
		}
	}
	s.wantIndex = true

	if s.embedOK, err = s.download(s.cfg.EmbedModel, "the model that indexes text"); err != nil {
		return err
	}
	if err := s.sourceTree(); err != nil {
		return err
	}
	return s.documentation()
}

// sourceTree looks for the FreeBSD source tree named by SOURCE_PATH and offers
// to install it where FreeBSD keeps it.
func (s *setup) sourceTree() error {
	for _, root := range filepath.SplitList(s.cfg.SourcePath) {
		switch {
		case populated(root):
			s.done("The source tree at %s is in place.", root)
		case root != s.m.srcDir || s.m.goos != "freebsd":
			s.warn("There is no source tree at %s; the index does without it.", root)
		default:
			if err := s.installSource(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *setup) installSource() error {
	chain, size, why := s.sourceCommand()
	if chain == "" {
		s.warn("The FreeBSD source tree is not installed at %s, and %s. The index does without it.", s.m.srcDir, why)
		return nil
	}
	s.warn("The FreeBSD source tree is not installed at %s. Only its manual pages are read, but it comes whole: %s.", s.m.srcDir, size)
	ok, err := s.offer("Install the FreeBSD source tree?", chain)
	if err != nil || !ok {
		return err
	}
	if populated(s.m.srcDir) {
		s.done("The source tree at %s is in place.", s.m.srcDir)
	} else {
		s.warn("%s is still empty; the index does without it.", s.m.srcDir)
	}
	return nil
}

// releasePattern matches the versions that have distribution sets to download.
var releasePattern = regexp.MustCompile(`^(\d+\.\d+)-(RELEASE|RC\d+|BETA\d+)(?:-p\d+)?$`)

// releaseName returns the name a FreeBSD version has on the download site:
// "15.1-RELEASE" for "15.1-RELEASE-p2". It reports false for versions that
// are not releases, such as 16.0-CURRENT.
func releaseName(version string) (string, bool) {
	m := releasePattern.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return "", false
	}
	return m[1] + "-" + m[2], true
}

// sourceCommand returns the command that installs the source tree of the
// running FreeBSD version, and how large that is. When it cannot tell how, it
// returns no command and why says so, completing "and ...".
func (s *setup) sourceCommand() (chain, size, why string) {
	// A base system installed from packages takes its source from them too.
	// pkg -N says whether pkg itself is installed, without offering to fetch it.
	if _, err := s.m.output("pkg", "-N"); err == nil {
		if owner, err := s.m.output("pkg", "which", "-q", "/bin/sh"); err == nil && strings.HasPrefix(owner, "FreeBSD-") {
			return "pkg install FreeBSD-set-src", "pkg says how large before it starts", ""
		}
	}

	// Otherwise the source is the distribution set of the release, the same
	// archive the installer offers.
	version, _ := s.m.output("freebsd-version", "-u")
	release, ok := releaseName(version)
	if !ok {
		if version == "" {
			version = "this system"
		}
		return "", "", fmt.Sprintf("%s is not a release with a source archive to download", version)
	}
	arch, _ := s.m.output("uname", "-m")
	cpu, _ := s.m.output("uname", "-p")
	if !plainWord(arch) || !plainWord(cpu) {
		return "", "", "the architecture of this machine could not be determined"
	}
	// Download it whole before unpacking, so that a connection that drops
	// leaves no half a tree behind. It is kept beside its destination, which
	// only root can write to and which has the room; /tmp may have neither.
	archive := defaultSourcePath + ".txz"
	from := "https://download.FreeBSD.org/releases/" + arch + "/" + cpu + "/" + release + "/src.txz"
	return "fetch -o " + archive + " " + from + " && tar -xpf " + archive + " -C / && rm " + archive,
		"about 250 MB to download and well over 1 GB on disk", ""
}

// documentation looks for the FreeBSD Handbook, FAQ and articles, offers to
// install them, and adds the place they live in to DOCS_SOURCE.
func (s *setup) documentation() error {
	if s.cfg.DocsSource == "" {
		s.skipped("DOCS_SOURCE is empty: no documentation is read.")
		return nil
	}
	dir := s.m.docDir
	if !populated(dir) {
		if s.m.goos != "freebsd" {
			return nil // FreeBSD's documentation packages exist nowhere else
		}
		s.warn("The FreeBSD Handbook, FAQ and articles are not installed.")
		ok, err := s.offer("Install the FreeBSD documentation?", s.install("misc/freebsd-doc-en", "en-freebsd-doc"))
		if err != nil || !ok {
			return err
		}
		if !populated(dir) {
			s.warn("%s is still empty; the index does without it.", dir)
			return nil
		}
	}
	if contains(filepath.SplitList(s.cfg.DocsSource), dir) {
		s.done("The documentation at %s is in place.", dir)
		return nil
	}
	if err := s.cfg.set("DOCS_SOURCE", s.cfg.DocsSource+string(filepath.ListSeparator)+dir); err != nil {
		s.warn("The documentation at %s cannot be added to DOCS_SOURCE: %v", dir, err)
		return nil
	}
	s.done("The documentation at %s is in place; DOCS_SOURCE now includes it.", dir)
	return nil
}

// save offers to write the settings that differ from the saved ones, whether
// they came from the command line or from the steps above, to the
// configuration file.
func (s *setup) save() error {
	s.heading("Configuration")
	if s.file == "" {
		s.warn("There is nowhere to save settings: the home directory is unknown.")
		return nil
	}
	var keys []string
	for _, key := range settingKeys {
		if s.cfg.get(key) != s.saved.get(key) {
			keys = append(keys, key)
		}
	}
	_, statErr := os.Stat(s.file)
	switch {
	case len(keys) == 0 && statErr == nil:
		s.done("%s needs no change.", s.file)
		return nil
	case len(keys) == 0:
		s.text("There is no configuration file; the defaults are in use.")
		if !s.ask("Create " + s.file + " with every setting commented out, for you to edit?") {
			s.skipped("Skipped.")
			return nil
		}
	default:
		s.text("These settings are not saved yet:")
		fmt.Fprintln(s.ui.out)
		for _, key := range keys {
			line, err := configLine(key, s.cfg.get(key))
			if err != nil {
				s.warn("%v", err)
				return nil
			}
			fmt.Fprintf(s.ui.out, "        %s\n", s.ui.paint(boldGreen, line))
		}
		fmt.Fprintln(s.ui.out)
		if !s.ask("Save them to " + s.file + "?") {
			s.skipped("Not saved: they hold for this run only.")
			return nil
		}
	}
	if err := writeConfig(s.file, s.cfg, keys); err != nil {
		s.warn("%s was not written: %v", s.file, err)
		return nil
	}
	s.done("Saved %s.", s.file)
	return nil
}

// index offers to build or update the index, once the files are counted.
func (s *setup) index() error {
	if !s.wantIndex {
		return nil
	}
	if !s.embedOK {
		s.heading("Index")
		s.warn("Nothing was indexed: that needs %s.", s.cfg.EmbedModel)
		return nil
	}
	code := runIndex(s.ui, s.cfg, func() bool {
		s.text("Reading them can take a while. Ctrl-C stops it and keeps what is done.")
		if !s.ask("Read these files into the index now?") {
			s.skipped("Skipped. To do it later: beastiepilot -index")
			return false
		}
		fmt.Fprintln(s.ui.out)
		return true
	})
	if code == exitInterrupted {
		return halt(exitInterrupted)
	}
	return nil
}

// program offers to put the program where every user's shell finds it, which
// "go install" does not do.
func (s *setup) program() error {
	if s.m.exe == "" {
		return nil
	}
	if _, err := s.m.lookPath("beastiepilot"); err == nil {
		return nil
	}
	s.heading("Program")
	dir := filepath.Dir(s.m.exe)
	s.warn("%s is not on your PATH, so the shell does not find beastiepilot by name.", dir)
	if s.m.goos != "freebsd" || !plainWord(s.m.exe) {
		s.text("Add that directory to your PATH, or copy the program to a directory that is on it.")
		return nil
	}
	target := programDir + "/beastiepilot"
	ok, err := s.offer("Copy it to "+programDir+", for every user of this machine?", "install -m 0755 "+s.m.exe+" "+target)
	if err != nil {
		return err
	}
	if ok {
		s.done("Installed %s.", target)
		s.m.exe = target
	} else {
		s.text("Otherwise add %s to your PATH.", dir)
	}
	return nil
}

// summary says whether BeastiePilot is ready and returns the exit code.
func (s *setup) summary() int {
	if !s.modelOK {
		s.heading("Not ready yet")
		s.text("BeastiePilot needs %s to write commands. Run beastiepilot configure again to finish.", s.cfg.Model)
		fmt.Fprintln(s.ui.out)
		return exitError
	}
	noted, err := markConfigured()
	if err != nil {
		s.heading("Not ready yet")
		s.text("Everything BeastiePilot needs is in place, but that could not be noted down, and until it is no request is accepted: %v", err)
		fmt.Fprintln(s.ui.out)
		return exitError
	}
	s.heading("Ready")
	if ix, err := openIndex(indexPath()); err == nil {
		s.text("Commands come from %s, with %s from %s of reference material at hand.",
			s.cfg.Model, plural(ix.total, "excerpt"), plural(len(ix.header.Files), "file"))
		ix.close()
	} else {
		s.text("Commands come from %s alone: there is no index of reference material.", s.cfg.Model)
	}
	if noted {
		s.text("Requests are accepted from now on: %s says that the setup is done.", configuredPath())
	}
	name := "beastiepilot"
	if _, err := s.m.lookPath(name); err != nil && s.m.exe != "" && s.m.exe != programDir+"/"+name {
		name = s.m.exe
	}
	fmt.Fprintf(s.ui.out, "\n    Try it:\n\n        %s\n\n", s.ui.paint(boldGreen, name+" show the free disk space"))
	return exitOK
}

// configuredPath returns the file whose presence says that the setup has
// finished, with BeastiePilot ready, for this user. Until it exists nothing is
// sent to a model. It lives under the user's state directory, honoring
// XDG_STATE_HOME, because it describes this machine and is not a setting. It
// is "" if that directory is unknown.
func configuredPath() string {
	return statePath("configured")
}

// statePath returns where the file called name is kept under the user's
// state directory, honoring XDG_STATE_HOME, or "" if that directory is
// unknown.
func statePath(name string) string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "beastiepilot", name)
}

// isConfigured reports whether the setup has finished for this user.
func isConfigured() bool {
	path := configuredPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// markConfigured notes that the setup has finished for this user. It reports
// whether that is news.
func markConfigured() (noted bool, err error) {
	path := configuredPath()
	if path == "" {
		return false, errors.New("the home directory is unknown (set XDG_STATE_HOME)")
	}
	if isConfigured() {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	note := fmt.Sprintf("\"beastiepilot configure\" finished for this user on %s (%s).\n"+
		"While this file exists, BeastiePilot sends requests to the model.\n",
		time.Now().Format("2006-01-02"), buildVersion())
	if err := os.WriteFile(path, []byte(note), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// populated reports whether dir is a directory with something in it.
func populated(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// localEndpoint reports whether the Ollama at endpoint is on this machine,
// where the setup can install and start it.
func localEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// formatBytes writes a size the way Ollama does, in decimal units.
func formatBytes(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d KB", n/1000)
}

// apiURL derives the URL of another part of Ollama's API from the URL of its
// generate API: they are siblings.
func apiURL(generate, name string) (string, error) {
	u, err := url.Parse(generate)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid endpoint %q", generate)
	}
	return u.ResolveReference(&url.URL{Path: name}).String(), nil
}

// apiError describes a reply from Ollama that is not a success.
func apiError(resp *http.Response, raw []byte) error {
	var reply struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &reply) == nil && reply.Error != "" {
		msg = reply.Error
	}
	if msg == "" {
		return fmt.Errorf("Ollama returned %s", resp.Status)
	}
	return fmt.Errorf("Ollama returned %s: %s", resp.Status, msg)
}

// ollamaVersion returns the version of the Ollama behind endpoint, the
// generate API URL. An error means it is not running there.
func ollamaVersion(ctx context.Context, endpoint string) (string, error) {
	target, err := apiURL(endpoint, "version")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err // without the URL, which the caller shows
		}
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp, raw)
	}
	var reply struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Version == "" {
		return "", errors.New("the answer is not Ollama's")
	}
	return reply.Version, nil
}

// modelRequest names a model to Ollama. Releases before 0.5 read the name
// from "name", later ones from "model"; sending both suits them all.
type modelRequest struct {
	Model  string `json:"model"`
	Name   string `json:"name"`
	Stream bool   `json:"stream"`
}

// postModel sends a request about the model name to the part of Ollama's API
// called api.
func postModel(ctx context.Context, endpoint, api, name string, stream bool) (*http.Response, error) {
	target, err := apiURL(endpoint, api)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(modelRequest{Model: name, Name: name, Stream: stream})
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", target, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Ollama at %s: %w", target, err)
	}
	return resp, nil
}

// hasModel reports whether the Ollama behind endpoint has downloaded the
// model name.
func hasModel(ctx context.Context, endpoint, name string) (bool, error) {
	resp, err := postModel(ctx, endpoint, "show", name, false)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case err != nil:
		return false, err
	}
	return false, apiError(resp, raw)
}

// withTag returns a model name with the tag Ollama assumes when none is given.
func withTag(name string) string {
	if strings.Contains(name[strings.LastIndexByte(name, '/')+1:], ":") {
		return name
	}
	return name + ":latest"
}

// modelLoaded asks the Ollama behind endpoint whether it has the model name
// in memory at the moment and, if so, whether it runs it without any help
// from a GPU.
func modelLoaded(ctx context.Context, endpoint, name string) (loaded, cpuOnly bool, err error) {
	target, err := apiURL(endpoint, "ps")
	if err != nil {
		return false, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, false, fmt.Errorf("invalid endpoint %q: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false, fmt.Errorf("cannot reach Ollama at %s: %w", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return false, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, false, apiError(resp, raw)
	}
	var reply struct {
		Models []struct {
			Name     string `json:"name"`
			Model    string `json:"model"`
			SizeVRAM int64  `json:"size_vram"` // how much of it is in a GPU's memory
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return false, false, errors.New("the answer is not Ollama's")
	}
	want := strings.ToLower(withTag(name))
	for _, m := range reply.Models {
		if strings.ToLower(withTag(m.Name)) == want || strings.ToLower(withTag(m.Model)) == want {
			return true, m.SizeVRAM == 0, nil
		}
	}
	return false, false, nil
}

// pullStatus is one line of the progress Ollama reports while downloading.
type pullStatus struct {
	Status    string `json:"status"`
	Digest    string `json:"digest"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Error     string `json:"error"`
}

// pullModel has the Ollama behind endpoint download the model name. progress,
// if not nil, is called with the bytes downloaded so far and the total known
// so far, which grows as Ollama learns of the model's parts.
func pullModel(ctx context.Context, endpoint, name string, progress func(done, total int64)) error {
	resp, err := postModel(ctx, endpoint, "pull", name, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return apiError(resp, raw)
	}

	type part struct{ done, total int64 }
	parts := make(map[string]part)
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 64<<10), maxResponseBytes)
	for lines.Scan() {
		var st pullStatus
		if len(bytes.TrimSpace(lines.Bytes())) == 0 || json.Unmarshal(lines.Bytes(), &st) != nil {
			continue
		}
		switch {
		case st.Error != "":
			return errors.New(st.Error)
		case st.Status == "success":
			return nil
		}
		if st.Digest != "" {
			parts[st.Digest] = part{st.Completed, st.Total}
		}
		if progress != nil {
			var done, total int64
			for _, p := range parts {
				done, total = done+p.done, total+p.total
			}
			progress(done, total)
		}
	}
	if err := lines.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("reading Ollama's progress: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("Ollama stopped before the download was complete")
}

// configLine returns the line that sets key to value in a configuration
// file, quoting the value when it must be.
func configLine(key, value string) (string, error) {
	line := key + "=" + value
	switch {
	case strings.ContainsAny(value, "\r\n"):
		return "", fmt.Errorf("%s: a value of several lines cannot be saved", key)
	case value == "" || !strings.ContainsAny(value, " \t#\"'"):
	case !strings.Contains(value, `"`):
		line = key + `="` + value + `"`
	case !strings.Contains(value, "'"):
		line = key + "='" + value + "'"
	}
	// Whatever the reason, never write a line that reads back differently.
	if _, raw, _ := strings.Cut(line, "="); configValue(raw) != value {
		return "", fmt.Errorf("%s: the value %q cannot be saved in a configuration file", key, value)
	}
	return line, nil
}

// setConfigKey returns the configuration text with key set to value. The line
// that sets key is replaced; without one, the commented-out "#KEY=" line that
// documents the default is taken over; without that, a line is added.
func setConfigKey(text, key, value string) (string, error) {
	line, err := configLine(key, value)
	if err != nil {
		return "", err
	}
	lines := strings.Split(text, "\n")
	at := -1
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if k, _, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(l, "#") && strings.EqualFold(strings.TrimSpace(k), key) {
			at = i // the last one, which is the one that counts when reading
		}
	}
	for i := 0; at < 0 && i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "#"+key+"=") {
			at = i
		}
	}
	if at < 0 {
		return strings.TrimRight(text, "\n") + "\n" + line + "\n", nil
	}
	lines[at] = line
	return strings.Join(lines, "\n"), nil
}

// writeConfig sets keys to the values they have in cfg in the configuration
// file at path, keeping everything else in it. A file that does not exist
// starts as the documented sample.
func writeConfig(path string, cfg settings, keys []string) error {
	// Write through a symbolic link rather than replace it with a file.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	text, mode := sampleConfig, fs.FileMode(0o644)
	switch raw, err := os.ReadFile(path); {
	case err == nil:
		text = string(raw)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	for _, key := range keys {
		var err error
		if text, err = setConfigKey(text, key, cfg.get(key)); err != nil {
			return err
		}
	}

	// Never leave behind a file that would not load, or that says something else.
	check := defaultSettings()
	if err := check.parse(strings.NewReader(text), path); err != nil {
		return err
	}
	for _, key := range keys {
		if check.get(key) != cfg.get(key) {
			return fmt.Errorf("%s would read back as %q", key, check.get(key))
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once it has been renamed
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
