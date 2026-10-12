package main

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// AUTO_RUN lists the commands that safe mode runs without asking. An entry is
// a command name, optionally followed by its first arguments, with "*"
// standing for any one argument:
//
//	AUTO_RUN=df, zfs list, service * status
//
// A suggestion skips the question only when every command the shell would
// start is covered by an entry; autoRun decides that. Whatever it cannot be
// sure about falls back to asking.

// unlistable are the names AUTO_RUN refuses: shells, the commands and builtins
// whose job is to run another command, and the shell's reserved words. Any of
// them would let a command that is not on the list run unasked. The list
// covers the well-known ones; it cannot know every program that runs others.
var unlistable = wordSet(`sh bash csh tcsh ksh zsh env xargs sudo doas su eval exec command
	builtin . source trap nice nohup time timeout lockf daemon chroot jexec ssh script
	if then elif else fi for while until do done case esac in`)

// parseAutoRun validates the value of AUTO_RUN and returns it in canonical
// form: entries separated by ", " and their words by single spaces.
func parseAutoRun(value string) (string, error) {
	var entries []string
	for _, entry := range strings.Split(value, ",") {
		words := strings.Fields(entry)
		if len(words) == 0 {
			continue
		}
		switch name := words[0]; {
		case name == "*":
			return "", fmt.Errorf(`entry %q must name its command ("*" can only stand for an argument)`, strings.Join(words, " "))
		case unlistable[path.Base(name)]:
			return "", fmt.Errorf("%q runs other commands, so listing it would let any command through", name)
		}
		for _, w := range words {
			if w != "*" && !plainWord(w) {
				return "", fmt.Errorf(`%q is not a plain word (an entry is a command name and its first arguments, such as "zfs list" or "service * status")`, w)
			}
		}
		entries = appendUnique(entries, strings.Join(words, " "))
	}
	return strings.Join(entries, ", "), nil
}

// plainWord reports whether w consists only of characters that mean nothing
// special to the shell or to the configuration file.
func plainWord(w string) bool {
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_-./:@%+", r):
		default:
			return false
		}
	}
	return w != ""
}

// autoRunList returns the entries of AUTO_RUN, each split into words.
func (s settings) autoRunList() [][]string {
	var list [][]string
	for _, entry := range strings.Split(s.AutoRun, ",") {
		if words := strings.Fields(entry); len(words) > 0 {
			list = append(list, words)
		}
	}
	return list
}

// shellWord is one word of a command line after quote removal.
type shellWord struct {
	text    string
	literal bool // the shell expands nothing in it: at run time it is exactly text
}

var (
	errSubstitution = errors.New("it contains a command substitution")
	errUnbalanced   = errors.New("its quoting is unbalanced")
	errRedirect     = errors.New("it redirects output to a file")
)

// splitCommands breaks a command line into the simple commands /bin/sh would
// start, each as its words. It understands quoting, the operators that join
// commands (; & && | ||) and redirections that cannot write anywhere, such as
// 2>/dev/null and 2>&1.
//
// It returns an error for every construct that could start a command it would
// not see, or write to a file: command substitution, subshells and functions,
// other redirections, comments and multi-line input. The error says why, as
// the end of a sentence beginning "The command was not run automatically
// because".
func splitCommands(line string) ([][]shellWord, error) {
	if strings.ContainsAny(line, "\n\r") {
		return nil, errors.New("it spans several lines")
	}

	var (
		cmds    [][]shellWord
		cmd     []shellWord
		text    strings.Builder
		inWord  bool
		quoted  bool // part of the current word was quoted or escaped
		literal = true
	)
	endWord := func() {
		if inWord {
			cmd = append(cmd, shellWord{text.String(), literal})
		}
		text.Reset()
		inWord, quoted, literal = false, false, true
	}
	endCommand := func() {
		endWord()
		if len(cmd) > 0 {
			cmds = append(cmds, cmd)
		}
		cmd = nil
	}
	// at returns the byte at position i, or 0 past the end of the line.
	at := func(i int) byte {
		if i < len(line) {
			return line[i]
		}
		return 0
	}
	// delimited reports whether a word ends at position i.
	delimited := func(i int) bool {
		return i >= len(line) || strings.IndexByte(" \t;&|", line[i]) >= 0
	}

	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case ' ', '\t':
			endWord()

		case '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				return nil, errUnbalanced
			}
			text.WriteString(line[i+1 : i+1+end])
			i += end + 1
			inWord, quoted = true, true

		case '"':
			inWord, quoted = true, true
			for i++; ; i++ {
				if i >= len(line) {
					return nil, errUnbalanced
				}
				d := line[i]
				if d == '"' {
					break
				}
				switch d {
				case '`':
					return nil, errSubstitution
				case '$':
					if at(i+1) == '(' {
						return nil, errSubstitution
					}
					literal = false
				case '\\':
					// Inside double quotes a backslash only escapes these.
					if n := at(i + 1); n != 0 && strings.IndexByte("$`\"\\", n) >= 0 {
						i++
						d = n
					}
				}
				text.WriteByte(d)
			}

		case '\\':
			if i+1 >= len(line) {
				return nil, errUnbalanced
			}
			i++
			text.WriteByte(line[i])
			inWord, quoted = true, true

		case '`':
			return nil, errSubstitution

		case '$':
			switch at(i + 1) {
			case '(':
				return nil, errSubstitution
			case '\'', '"':
				// $'...' has escapes of its own, which would change where the
				// quoted text ends.
				return nil, errors.New("it uses $'...' quoting")
			}
			text.WriteByte(c)
			inWord, literal = true, false

		case '*', '?', '[', '~':
			text.WriteByte(c)
			inWord, literal = true, false

		case '#':
			if !inWord {
				return nil, errors.New("it contains a comment")
			}
			text.WriteByte(c)

		case ';':
			endCommand()

		case '&', '|':
			if at(i+1) == c {
				i++
			}
			endCommand()

		case '(', ')':
			return nil, errors.New("it uses a subshell or defines a function")

		case '<':
			return nil, errors.New("it redirects input")

		case '>':
			// Digits right before ">" name the descriptor being redirected;
			// they are not an argument.
			if inWord && !quoted && isDigits(text.String()) {
				text.Reset()
				inWord = false
			}
			endWord()

			j := i + 1
			if at(j) == '&' {
				// Duplicating a descriptor (2>&1, >&2) or closing it (>&-).
				j++
				start := j
				for at(j) >= '0' && at(j) <= '9' {
					j++
				}
				if j == start && at(j) == '-' {
					j++
				}
				if j == start || !delimited(j) {
					return nil, errRedirect
				}
			} else {
				if at(j) == '>' {
					j++
				}
				for at(j) == ' ' || at(j) == '\t' {
					j++
				}
				const sink = "/dev/null"
				if !strings.HasPrefix(line[j:], sink) || !delimited(j+len(sink)) {
					return nil, errRedirect
				}
				j += len(sink)
			}
			i = j - 1

		default:
			text.WriteByte(c)
			inWord = true
		}
	}
	endCommand()
	return cmds, nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// listed reports whether a command is covered by an entry of allow: the entry's
// words match the command's first words, with "*" matching any one word. Only
// literal words can match, because the shell may expand any other into
// something else, or into several words.
func listed(words []shellWord, allow [][]string) bool {
	for _, entry := range allow {
		if len(entry) > len(words) {
			continue
		}
		match := true
		for i, want := range entry {
			if !words[i].literal || (want != "*" && words[i].text != want) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// findActions are the find primaries that run a command or delete files.
var findActions = wordSet("-exec -execdir -ok -okdir -delete")

// autoRun reports whether command may run without confirmation because every
// command in it is covered by an entry of allow. When it may not, why says so
// in a form that completes "not run automatically because".
//
// The list is the user's statement that those commands are harmless whatever
// else is on the line, so the rest of their arguments is not examined, with
// one exception: find is refused when asked to run or delete what it finds.
func autoRun(command string, allow [][]string) (ok bool, why string) {
	cmds, err := splitCommands(command)
	if err != nil {
		return false, err.Error()
	}
	if len(cmds) == 0 {
		return false, "it runs nothing"
	}
	for _, words := range cmds {
		if !listed(words, allow) {
			name := words[0].text
			if len(words) > 1 && !strings.HasPrefix(words[1].text, "-") {
				name += " " + words[1].text
			}
			return false, fmt.Sprintf("%q is not in AUTO_RUN", name)
		}
		if words[0].text == "find" {
			for _, w := range words[1:] {
				if findActions[w.text] {
					return false, "find would run a command on, or delete, what it finds"
				}
			}
		}
	}
	return true, ""
}
