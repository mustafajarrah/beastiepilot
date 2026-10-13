package main

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
)

// A command that only root can run is of no use to anyone else: "pkg install"
// ends with "Insufficient privileges" and nothing installed. The answer is not
// to run BeastiePilot as root, where every suggestion would have the run of
// the machine, but to run that one command as root: the user is told so before
// the question, and on a yes the command goes through doas, sudo or su, which
// ask for the password themselves.

var (
	// rootAlways are the commands that do nothing for anyone but root.
	rootAlways = wordSet(`adduser rmuser kldload kldunload umount shutdown reboot halt poweroff
		freebsd-update pfctl ipfw dhclient tzsetup`)

	// rootSubcommands are, for the commands anyone can ask questions with, the
	// subcommands that change the system: "pkg info" and "zfs list" are for
	// everyone, "pkg install" and "zfs create" are not.
	rootSubcommands = map[string]map[string]bool{
		"pkg":   wordSet("install add delete remove upgrade update autoremove clean fetch"),
		"zfs":   wordSet("create destroy snapshot snap rollback clone promote rename set inherit mount unmount umount send receive recv"),
		"zpool": wordSet("create destroy add remove attach detach replace online offline clear scrub trim export import set labelclear"),
		"bectl": wordSet("create destroy activate rename mount unmount umount"),
		"gpart": wordSet("create destroy add delete modify resize bootcode set unset recover restore commit undo"),
		"pw":    wordSet("useradd usermod userdel groupadd groupmod groupdel lock unlock"),
	}

	// serviceActions are what "service name ..." needs root for; asking for
	// the status of a service does not.
	serviceActions = wordSet(`start stop restart reload onestart onestop onerestart onereload
		forcestart forcestop forcerestart enable disable delete`)
)

// needsRoot returns the first command on the line that only root can run,
// named as in "pkg install", or "" if it sees none.
//
// It knows the common commands that change a FreeBSD system, as that system
// is installed, and nothing else: any other command, and any line it cannot
// take apart with certainty, runs as the user and fails by itself if it needed
// more.
func needsRoot(command string) string {
	commands, err := splitCommands(command)
	if err != nil {
		return ""
	}
	for _, words := range commands {
		if name := rootOnly(words); name != "" {
			return name
		}
	}
	return ""
}

// rootOnly returns how to name the command made of words if only root can run
// it, and "" otherwise.
func rootOnly(words []shellWord) string {
	// VAR=value before the command, with or without env, is not the command.
	for len(words) > 0 && words[0].literal && (words[0].text == "env" || isAssignment(words[0].text)) {
		words = words[1:]
	}
	if len(words) == 0 || !words[0].literal {
		return ""
	}
	name := path.Base(words[0].text)

	// The operands are the arguments that are not options. An option's own
	// value counts as one, which the rules below allow for. A word the shell
	// would expand is kept as it is written, and matches nothing by name.
	var args, operands []string
	for _, w := range words[1:] {
		args = append(args, w.text)
		if !strings.HasPrefix(w.text, "-") {
			operands = append(operands, w.text)
		}
	}
	operand := func(i int) string {
		if i < len(operands) {
			return operands[i]
		}
		return ""
	}

	switch {
	case rootAlways[name], strings.HasPrefix(name, "newfs"):
		return name
	case name == "pkg" && (hasFlag(args, "n") || contains(args, "--dry-run")):
		// Only says what it would do.
	case name == "zfs" && len(args) == 1 && args[0] == "mount":
		// Lists what is mounted.
	case rootSubcommands[name][operand(0)]:
		return name + " " + operand(0)
	case name == "service" && serviceActions[operand(1)]:
		return name + " " + operand(0) + " " + operand(1)
	case name == "sysrc" && hasFlag(args, "x"):
		return name + " -x"
	case name == "sysrc", name == "sysctl":
		// name=value sets; a name alone asks.
		for _, o := range operands {
			if strings.Contains(o, "=") {
				return name + " " + o
			}
		}
	case name == "mount" && len(operands) >= 2:
		// By itself, or with a type alone, it lists what is mounted.
		return name
	case name == "make":
		// Building a port installs into /usr/local.
		ports, installs := false, false
		for _, o := range operands {
			ports = ports || o == "/usr/ports" || strings.HasPrefix(o, "/usr/ports/")
			installs = installs || o == "install" || o == "reinstall" || o == "deinstall"
		}
		if ports && installs {
			return "make install"
		}
	}
	return ""
}

// commandVariable carries the command to the shell that su starts.
const commandVariable = "BEASTIEPILOT_COMMAND"

// elevation is the way a command gets to run as root.
type elevation struct {
	tool string   // doas, sudo or su
	argv []string // what to start
	env  []string // what to add to the environment, as "NAME=value"
}

// asRoot returns the way to run command, a line for /bin/sh, as root: through
// doas or sudo when one is installed and through su otherwise, as the setup
// does for its own commands.
//
// doas and sudo start /bin/sh themselves. su hands its argument to root's
// login shell, which may be csh and would read the line its own way, so the
// argument is one fixed line that both shells take the same, and /bin/sh gets
// the command from the environment, which su leaves as it finds it.
func asRoot(command string, lookPath func(string) (string, error)) elevation {
	for _, tool := range []string{"doas", "sudo"} {
		if _, err := lookPath(tool); err == nil {
			return elevation{tool: tool, argv: []string{tool, "/bin/sh", "-c", command}}
		}
	}
	return elevation{
		tool: "su",
		argv: []string{"su", "root", "-c", `/bin/sh -c 'eval "$` + commandVariable + `"'`},
		env:  []string{commandVariable + "=" + command},
	}
}

// notice says, before the question, that the command would run as root and
// who will ask for a password. name is the command that needs it.
func (e elevation) notice(name string) string {
	password := "may ask for your password"
	if e.tool == "su" {
		password = "asks for root's password"
	}
	return fmt.Sprintf("%s needs root, and you are not root: on a yes, the command runs as root through %s, which %s.", name, e.tool, password)
}

// execute runs the command as root with the caller's stdin, stdout and stderr
// attached, as execute does for any other, and returns its exit code. A
// password refused shows as the exit code of the tool that asked for it.
func (e elevation) execute() (int, error) {
	cmd := exec.Command(e.argv[0], e.argv[1:]...)
	cmd.Env = append(os.Environ(), e.env...)
	return runCommand(cmd)
}
