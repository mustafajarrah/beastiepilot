# BeastiePilot

An AI-powered terminal assistant for FreeBSD. Describe what you want in plain
language; BeastiePilot asks a local LLM for the matching shell command, shows it
to you, and runs it only after you say yes.

```console
$ beastiepilot install nginx and enable it at boot

  █▖ ▗▄▄▖ ▗█   BeastiePilot v0.1.0
  ▝█▟████▙█▘   llama3.2 · localhost:11434
   ██▀██▀██    ~/projects/web
   ▀██▀▀██▀

  > Suggested command (llama3.2)

    pkg install -y nginx && sysrc nginx_enable=YES

  ? Execute this command? [y/N]
```

## Features

- **FreeBSD-native.** A strict system prompt steers the model to `pkg`,
  `service`, `sysrc`, `pf`, `zfs`, `jls` and the rest of the base system, and
  away from Linux tooling such as `apt`, `systemctl` or `iptables`. If a Linux
  tool slips through anyway, BeastiePilot points it out.
- **Local and private.** Requests go to your own [Ollama](https://ollama.com)
  instance. Nothing leaves the machine unless you point it elsewhere.
- **Human in the loop.** Every suggestion is displayed in full and needs an
  explicit answer typed at the terminal. The default is always *no*.
- **Extra friction where it matters.** Commands that look destructive
  (`rm -r`, `zfs destroy`, `dd` to a device, `pfctl -d`, ...) are flagged and
  need the full word `yes`, not a reflexive `y`.
- **Aware of its host.** The FreeBSD release, architecture, working directory
  and whether you are root are passed to the model so it can tailor the command.
- **Pipe-friendly.** The interface is drawn on the terminal itself, so stdin and
  stdout belong entirely to the command you approved.
- **No dependencies.** A single binary built entirely from the Go standard
  library.

## Prerequisites

- FreeBSD (any supported release)
- Go 1.21 or newer, to build: `pkg install go`
- A running Ollama instance with at least one model pulled

### Setting up Ollama on FreeBSD

```sh
# Install Ollama from packages
pkg install ollama

# Start it now and on every boot
sysrc ollama_enable=YES
service ollama start

# Download a model (BeastiePilot defaults to llama3.2)
ollama pull llama3.2
```

If you would rather not run it as a service, start the server in a spare
terminal with `ollama serve`.

Confirm the API is reachable:

```sh
fetch -qo - http://localhost:11434/api/tags
```

Any model Ollama can serve will work. Larger or code-oriented models (for
example `qwen2.5-coder`) generally produce more accurate commands than small
general-purpose ones.

## Installation

### With `go install`

```sh
go install github.com/mustafajarrah/beastiepilot@latest
```

The binary is placed in `$(go env GOPATH)/bin` (usually `~/go/bin`). Make sure
that directory is on your `PATH`.

### From source

```sh
git clone https://github.com/mustafajarrah/beastiepilot.git
cd beastiepilot
go build -o beastiepilot .

# Optional: install system-wide (as root)
install -m 0755 beastiepilot /usr/local/bin/
```

To stamp a version into the binary:

```sh
go build -ldflags "-X main.version=v0.1.0" -o beastiepilot .
```

## Usage

```
beastiepilot [flags] <what you want to do>
```

Everything after the flags is joined into a single request, so quoting is
optional. Do quote the request when it contains characters your shell would
interpret, such as `?`, `*`, `'` or `|`.

### Examples

The suggestions below are illustrative; the exact command depends on the model.
The banner is omitted for brevity.

```console
$ beastiepilot "show the ten largest files under /var"

  > Suggested command (llama3.2)

    find /var -type f -exec du -k {} + | sort -rn | head -n 10

  ? Execute this command? [y/N] y
```

```console
$ beastiepilot -model qwen2.5-coder "which process is listening on port 443?"

  > Suggested command (qwen2.5-coder)

    sockstat -l -p 443

  ? Execute this command? [y/N]
```

A command that could destroy data is flagged and asks for more than a keypress:

```console
$ beastiepilot destroy the old-data dataset and everything under it

  > Suggested command (llama3.2)

    zfs destroy -r zroot/old-data

  ! High risk: destroys ZFS datasets or snapshots
  ! You are root: the command will run with full privileges.

  ? Type "yes" to execute it anyway [yes/N]
```

Because the prompt lives on the terminal, the approved command can take part in
a pipeline on either side:

```sh
beastiepilot list all installed packages > packages.txt
cat access.log | beastiepilot "count the lines that contain 404"
```

### Flags

| Flag         | Default                               | Description                    |
| ------------ | ------------------------------------- | ------------------------------ |
| `-model`     | `llama3.2`                            | Ollama model to query          |
| `-endpoint`  | `http://localhost:11434/api/generate` | Ollama generate API URL        |
| `-timeout`   | `2m`                                  | How long to wait for the model |
| `-no-banner` |                                       | Do not draw the Beastie banner |
| `-version`   |                                       | Print the version and exit     |

Flags must come before the request.

The banner shows Beastie in his pilot cap and goggles next to the version, the
model and Ollama host in use, and the working directory. It is drawn with
Unicode block characters when the locale is UTF-8 and falls back to plain ASCII
otherwise, for example on the FreeBSD console in the `C` locale. It only
appears on an interactive terminal.

### Environment

| Variable                 | Description                     |
| ------------------------ | ------------------------------- |
| `BEASTIEPILOT_MODEL`     | Default for `-model`            |
| `BEASTIEPILOT_ENDPOINT`  | Default for `-endpoint`         |
| `BEASTIEPILOT_NO_BANNER` | Hide the banner when set        |
| `NO_COLOR`               | Disable colored output when set |

### Exit status

| Code  | Meaning                                                                    |
| ----- | -------------------------------------------------------------------------- |
| *n*   | The exit status of the command, when you approved and it ran               |
| `0`   | You declined; nothing was executed                                         |
| `1`   | BeastiePilot failed, the model had no suggestion, or there was no terminal |
| `2`   | Invalid usage                                                              |
| `130` | Interrupted while waiting for the model                                    |

## Safety

BeastiePilot is a convenience, not a safeguard. Language models make mistakes,
and a confident-looking command can still be wrong or destructive. The system
prompt only steers the model; the protections below are enforced in code.

- **Read every command before approving it.** The confirmation prompt is the
  only thing standing between the model and your system.
- **Approval comes from the terminal, never from a pipe.** The answer is read
  from `/dev/tty`, so `yes | beastiepilot ...` cannot approve anything, and
  redirecting output cannot hide the command you are approving. Without a
  terminal (cron, CI, a detached session) nothing is ever executed.
- **Only an explicit answer counts.** `y` or `yes` followed by Enter approves;
  anything else, including an empty line, declines.
- **Destructive-looking commands need the word `yes`.** Recursive deletes,
  ZFS and partition destruction, raw writes to devices, overwriting files under
  `/etc`, shutting down, disabling the firewall and piping into a shell are
  flagged. This check is a heuristic: it can raise false alarms, and a command
  it does not flag is *not* thereby safe.
- **What you see is what runs.** Replies containing escape sequences or other
  non-printable characters are rejected outright, so the terminal cannot be made
  to display something different from what would be executed.
- **The model may decline.** When a request is ambiguous or not achievable, it
  is instructed to say so instead of guessing, and BeastiePilot reports that
  without offering anything to run.
- **No sandbox, no privilege changes.** Commands run through `/bin/sh -c` with
  the privileges of the user who invoked BeastiePilot. Avoid running it as root
  unless the task requires it.

### What is sent to the model

Your request, the system prompt, and a few facts about the host: the output of
`uname -sr`, the CPU architecture, whether you are root, and the current
working directory. With the default endpoint all of it stays on your machine;
keep that in mind if you point `-endpoint` at a remote server.

## Development

```sh
go vet ./...
go test ./...
```

The tests cover the parts that guard execution: cleaning and validating the
model's reply, the risk heuristics, the confirmation prompt and the Ollama
client.

## License

Released under the [MIT License](LICENSE). Copyright (c) 2026 Mustafa Jarrah.
