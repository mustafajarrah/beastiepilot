# BeastiePilot

An AI-powered terminal assistant for FreeBSD. Describe what you want in plain
language; BeastiePilot asks a local LLM for the matching shell command, shows it
to you, and runs it only after you say yes.

```console
$ beastiepilot install nginx and enable it at boot

  > Suggested command (llama3.2)

    pkg install -y nginx && sysrc nginx_enable=YES

  ? Execute this command? [y/N]
```

## Features

- **FreeBSD-native.** A strict system prompt steers the model to `pkg`,
  `service`, `sysrc`, `pf`, `zfs` and the rest of the base system, and away from
  Linux tooling such as `apt`, `systemctl` or `iptables`.
- **Local and private.** Requests go to your own [Ollama](https://ollama.com)
  instance. Nothing leaves the machine unless you point it elsewhere.
- **Human in the loop.** Every suggestion is displayed in full and requires an
  explicit `y` before it is executed. The default answer is always *no*.
- **Pipe-friendly.** BeastiePilot's own output goes to stderr, so stdout carries
  only the output of the command you approved.
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

```console
$ beastiepilot "show the ten largest files under /var"

  > Suggested command (llama3.2)

    find /var -type f -exec du -k {} + | sort -rn | head -n 10

  ? Execute this command? [y/N] y
```

```console
$ beastiepilot take a recursive zfs snapshot of zroot named before-upgrade

  > Suggested command (llama3.2)

    zfs snapshot -r zroot@before-upgrade

  ? Execute this command? [y/N]
```

```console
$ beastiepilot -model qwen2.5-coder "which process is listening on port 443?"

  > Suggested command (qwen2.5-coder)

    sockstat -l -p 443

  ? Execute this command? [y/N]
```

Because only the approved command writes to stdout, the result can be piped or
redirected as usual:

```sh
beastiepilot list all installed packages > packages.txt
```

### Flags

| Flag        | Default                               | Description                         |
| ----------- | ------------------------------------- | ----------------------------------- |
| `-model`    | `llama3.2`                            | Ollama model to query               |
| `-endpoint` | `http://localhost:11434/api/generate` | Ollama generate API URL             |
| `-timeout`  | `2m`                                  | How long to wait for the model      |
| `-version`  |                                       | Print the version and exit          |

Flags must come before the request.

### Environment

| Variable                | Description                                |
| ----------------------- | ------------------------------------------ |
| `BEASTIEPILOT_MODEL`    | Default for `-model`                       |
| `BEASTIEPILOT_ENDPOINT` | Default for `-endpoint`                    |
| `NO_COLOR`              | Disable colored output when set            |

Colors are also disabled automatically when stderr is not a terminal.

### Exit status

| Code  | Meaning                                                              |
| ----- | -------------------------------------------------------------------- |
| *n*   | The exit status of the command, when you approved and it ran         |
| `0`   | You declined; nothing was executed                                   |
| `1`   | BeastiePilot failed, or the model could not suggest a command        |
| `2`   | Invalid usage                                                        |
| `130` | Interrupted while waiting for the model                              |

## Safety

BeastiePilot is a convenience, not a safeguard. Language models make mistakes,
and a confident-looking command can still be wrong or destructive.

- **Read every command before approving it.** The confirmation prompt is the
  only thing standing between the model and your system.
- Commands run through `/bin/sh -c` with the privileges of the user who invoked
  BeastiePilot. There is no sandbox. Avoid running it as root unless the task
  requires it.
- Only `y` or `yes` followed by Enter approves a command. Anything else,
  including an empty line or end of input, declines.
- Replies containing escape sequences or other non-printable characters are
  rejected outright, so the terminal cannot be made to display something
  different from what would be executed.
- When a request is ambiguous or not achievable, the model is instructed to say
  so instead of guessing, and BeastiePilot reports that without offering
  anything to run.

## License

Released under the [MIT License](LICENSE). Copyright (c) 2026 Mustafa Jarrah.
