package main

import (
	"strings"
	"testing"
)

func TestParseAutoRun(t *testing.T) {
	for in, want := range map[string]string{
		"":                                       "",
		" , ,":                                   "",
		"df":                                     "df",
		" df ,  zfs   list ,, service * status ": "df, zfs list, service * status",
		"df, df -h, df":                          "df, df -h",
		"/usr/local/bin/htop, pkg info":          "/usr/local/bin/htop, pkg info",
	} {
		got, err := parseAutoRun(in)
		if err != nil || got != want {
			t.Errorf("parseAutoRun(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}

	for in, want := range map[string]string{
		"*":             "must name its command",
		"df, * status":  "must name its command",
		"sh":            "runs other commands",
		"/bin/sh -c":    "runs other commands",
		"sudo zfs list": "runs other commands",
		"df, xargs":     "runs other commands",
		"env":           "runs other commands",
		"if":            "runs other commands",
		"df | sort":     "not a plain word",
		"echo $HOME":    "not a plain word",
		`rm "x"`:        "not a plain word",
		"FOO=bar df":    "not a plain word",
		"ls *.txt":      "not a plain word",
		"df; reboot":    "not a plain word",
	} {
		if got, err := parseAutoRun(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseAutoRun(%q) = (%q, %v), want an error containing %q", in, got, err, want)
		}
	}
}

// words renders split commands compactly: words are joined by "·", commands
// by " / ", and a word the shell would expand is marked with a trailing "~".
func words(cmds [][]shellWord) string {
	var out []string
	for _, cmd := range cmds {
		var ws []string
		for _, w := range cmd {
			s := w.text
			if !w.literal {
				s += "~"
			}
			ws = append(ws, s)
		}
		out = append(out, strings.Join(ws, "·"))
	}
	return strings.Join(out, " / ")
}

func TestSplitCommands(t *testing.T) {
	for in, want := range map[string]string{
		"df -h":                                "df·-h",
		"  df   -h  ":                          "df·-h",
		"a && b || c ; d & e | f":              "a / b / c / d / e / f",
		"df -h;":                               "df·-h",
		`echo "a; b" 'c | d' e\ f`:             "echo·a; b·c | d·e f",
		`echo "x\"y" 'it''s' "a\b"`:            `echo·x"y·its·a\b`,
		`echo $HOME ~ *.c "x$y" '$z' a?b [ab]`: "echo·$HOME~·~~·*.c~·x$y~·$z·a?b~·[ab]~",
		`"df" -h`:                              "df·-h",
		`echo ""`:                              "echo·",
		"du -sh /var/* 2>/dev/null | sort -h":  "du·-sh·/var/*~ / sort·-h",
		"df >/dev/null 2>&1":                   "df",
		"df > /dev/null":                       "df",
		"df >>/dev/null":                       "df",
		"df 2>/dev/null;uptime":                "df / uptime",
		"df >&2":                               "df",
		"df 2>&-":                              "df",
		"echo 2 >/dev/null":                    "echo·2",
		"echo 2>/dev/null":                     "echo",
		`echo "2">/dev/null`:                   "echo·2",
		"echo a2>/dev/null":                    "echo·a2",
		"echo a#b $#":                          "echo·a#b·$#~",
		"awk '{print $1}' /etc/passwd":         "awk·{print $1}·/etc/passwd",
		"":                                     "",
	} {
		cmds, err := splitCommands(in)
		if got := words(cmds); err != nil || got != want {
			t.Errorf("splitCommands(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}

	for _, in := range []string{
		"echo $(id)",
		"echo `id`",
		`echo "$(id)"`,
		"echo \"`id`\"",
		"echo $((1+1))",
		"echo ${x:-$(id)}",
		"echo > out.txt",
		"echo >out.txt",
		"echo >> /etc/rc.conf",
		"echo >/dev/nullx",
		"echo >/dev/null/x",
		`echo >/dev/null"x"`,
		"echo >&out",
		"echo >| /dev/null",
		"sort < file",
		"cat <<EOF",
		"(df)",
		"f() { reboot; }",
		"echo 'a",
		`echo "a`,
		`echo a\`,
		"df\nreboot",
		"df\r",
		"df # show disks",
		"# nothing",
		`echo $'a\''`,
		`echo $"a"`,
	} {
		if cmds, err := splitCommands(in); err == nil {
			t.Errorf("splitCommands(%q) = %q, want an error", in, words(cmds))
		}
	}
}

func TestAutoRun(t *testing.T) {
	list, err := parseAutoRun("df, du, sort, head, echo, cat, grep, find, cd, zfs list, service * status")
	if err != nil {
		t.Fatal(err)
	}
	allow := settings{AutoRun: list}.autoRunList()

	for _, command := range []string{
		"df -h",
		"du -sh /var/* 2>/dev/null | sort -h | head -n 5",
		"zfs list -t snapshot",
		`zfs "list"`,
		"service nginx status",
		"cd /var/log && cat messages | grep -c error",
		`echo "a; reboot"`,
		`grep -E "a|b" /etc/rc.conf`,
		"find /var -name '*.log' -size +1M",
		"echo $HOME",
		"df &",
	} {
		if ok, why := autoRun(command, allow); !ok {
			t.Errorf("autoRun(%q) = false (%s), want it to run", command, why)
		}
	}

	// Every one of these would start a command that is not on the list, or
	// could not be analyzed with certainty.
	for _, command := range []string{
		"",
		"reboot",
		"df -h; rm -rf /tmp/x",
		"df -h && reboot",
		"df -h || reboot",
		"df -h | sh",
		"df -h & reboot",
		"df -h\nreboot",
		"zfs destroy tank",
		"zfs -h list",
		"zfs",
		"service nginx stop",
		"service $X status",
		"service status",
		"echo $(reboot)",
		"echo `reboot`",
		`echo "$(reboot)"`,
		"df > /tmp/out",
		"echo sshd_enable=NO >> /etc/rc.conf",
		"PATH=/tmp/evil df",
		"/tmp/evil/df",
		"./df",
		"$CMD -h",
		"d* -h",
		"sudo df",
		"env df",
		"df | xargs rm",
		"if true; then df; fi",
		"for f in a b; do df; done",
		"! df",
		"{ df; reboot; }",
		"(df)",
		"df() { reboot; }; df",
		"find / -delete",
		"find . -exec chmod 777 {} +",
		"find . -name x -okdir rm {} ;",
		"echo hi # then; reboot",
		`echo $'a\'' ; reboot #'`,
	} {
		if ok, _ := autoRun(command, allow); ok {
			t.Errorf("autoRun(%q) = true, want it to ask", command)
		}
	}

	// With nothing listed, nothing runs unasked.
	if ok, _ := autoRun("df -h", nil); ok {
		t.Error("autoRun with an empty list must be false")
	}
}

func TestAutoRunReasons(t *testing.T) {
	allow := settings{AutoRun: "df, zfs list"}.autoRunList()
	for command, want := range map[string]string{
		"reboot":           `"reboot" is not in AUTO_RUN`,
		"df | sort -rn":    `"sort" is not in AUTO_RUN`,
		"zfs destroy tank": `"zfs destroy" is not in AUTO_RUN`,
		"df > out":         "it redirects output to a file",
		"df $(id)":         "it contains a command substitution",
	} {
		if _, why := autoRun(command, allow); why != want {
			t.Errorf("autoRun(%q) gave reason %q, want %q", command, why, want)
		}
	}
}
