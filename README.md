# BeastiePilot

An AI-powered terminal assistant for FreeBSD. Describe what you want in plain
language; BeastiePilot asks a local LLM for the matching shell command, shows it
to you, and runs it only after you say yes.

```console
$ beastiepilot install nginx and enable it at boot

  █▖ ▗▄▄▖ ▗█   BeastiePilot v0.1.0
  ▝█▟████▙█▘   llama3.2 · localhost:11434
   ██▀██▀██    ~/projects/web
   ▀██▀▀██▀    safe mode

  > Suggested command (llama3.2)

    pkg install -y nginx && sysrc nginx_enable=YES

  ? Execute this command? [y/N]
```

On a roomy color terminal the mascot above is replaced by a pixel-art Beastie
who wags his tail and blinks while the model is thinking; see
[The banner](#the-banner).

## Features

- **FreeBSD-native.** A strict system prompt steers the model to `pkg`,
  `service`, `sysrc`, `pf`, `zfs`, `jls` and the rest of the base system, and
  away from Linux tooling such as `apt`, `systemctl` or `iptables`. If a Linux
  tool slips through anyway, BeastiePilot points it out.
- **Local and private.** Requests go to your own [Ollama](https://ollama.com)
  instance. Nothing leaves the machine unless you point it elsewhere.
- **Human in the loop.** Every suggestion is displayed in full and needs an
  explicit answer typed at the terminal; the default answer is *no*.
- **A session when you want one.** Run `beastiepilot` by itself and it takes
  requests one after another, with a line editor, the earlier requests within
  reach of the arrow keys, and each request able to build on the last.
- **Two modes.** `safe` shows the command and asks; `guided` also explains what
  each part of it does and why. See [Modes](#modes).
- **You choose what needs no question.** List the commands you consider
  harmless, such as `df` or `zfs list`, and suggestions made only of those run
  straight away. See [Running trusted commands without asking](#running-trusted-commands-without-asking).
- **Extra friction where it matters.** Commands that look destructive
  (`rm -r`, `zfs destroy`, `dd` to a device, `pfctl -d`, ...) are flagged and
  need the full word `yes`, not a reflexive `y`.
- **Reads the manual first.** Optionally, BeastiePilot indexes the manual
  pages, the system documentation and your own notes and books, and hands the
  model the passages closest to each request. See
  [Reference material](#reference-material).
- **Sets itself up.** `beastiepilot configure` offers to install Ollama, the
  models and the documentation and to build the index, asking before each
  step. See [Quick start](#quick-start).
- **Aware of its host.** The FreeBSD release, architecture, working directory
  and whether you are root are passed to the model so it can tailor the command.
- **Pipe-friendly.** The interface is drawn on the terminal itself, so stdin and
  stdout belong entirely to the command you approved.
- **No dependencies.** A single binary built entirely from the Go standard
  library.

## Quick start

On FreeBSD:

```sh
# As root: Go to build with, then BeastiePilot itself, into /usr/local/bin
pkg install go
env GOBIN=/usr/local/bin go install github.com/mustafajarrah/beastiepilot@latest

# As the user who will use it
beastiepilot configure
```

`/usr/local/bin` is where FreeBSD keeps programs from outside the base system,
so every user's shell finds `beastiepilot` there by name. With `doas` or
`sudo`, put it before each of the first two commands
(`doas env GOBIN=/usr/local/bin go install ...`). To update, run the
`go install` line again.

If you would rather not build as root, a plain
`go install github.com/mustafajarrah/beastiepilot@latest` puts the program in
`~/go/bin`. Start it from there, as `~/go/bin/beastiepilot configure`, and the
last step offers to copy it to `/usr/local/bin`.

`configure` does the rest. It checks what the machine has and offers to
provide what is missing, one step at a time:

| Step               | What it offers                                                                                                   |
| ------------------ | ---------------------------------------------------------------------------------------------------------------- |
| Ollama             | Install the `ollama` package, enable the service and start it                                                    |
| Model              | Download `MODEL` (`llama3.2`, about 2 GB)                                                                        |
| Reference material | Download `EMBED_MODEL`, install the FreeBSD source tree in `/usr/src` and the Handbook, FAQ and articles         |
| Configuration      | Save the settings it changed, or that you gave as flags, to `~/.config/beastiepilot/config`                      |
| Index              | Read the reference material into the [index](#reference-material)                                                |
| Program            | Copy the program to `/usr/local/bin`, when the shell cannot find it by name                                      |

```console
$ beastiepilot configure

  > Setting up BeastiePilot
    Each step says what it would do and asks first; nothing changes without
    a yes. Run this again at any time: what is already in place is skipped.

  > Ollama
    ! Ollama, the program that runs the models, is not installed.
      This would run, as root:

        su root -c 'pkg install ollama && sysrc ollama_enable=YES && service ollama start'

    ? Install Ollama and start it, now and at every boot? [y/N]
```

Things worth knowing about it:

- **It comes first.** Until `configure` has finished once with BeastiePilot
  ready, no request is sent to a model: the program says to run `configure`
  and stops, and so does `-index`. When it finishes, it notes that in
  `~/.local/state/beastiepilot/configured` (or under `$XDG_STATE_HOME`), the
  only file it writes without a question. Each user runs it once.
- **Nothing happens without a yes.** Every command is shown exactly as it will
  run before the question, and anything but `y` or `yes` skips the step. `pkg`
  is run without `-y`, so it lists what it would install and asks once more.
- **It can be run again.** A step whose result is already in place is skipped
  without a question, so running it again only does what is still missing.
- **Run it as yourself, not as root.** The configuration and the index belong
  to the user who runs it. The steps that need root go through `doas` or
  `sudo` when one is installed, and through `su` otherwise.
- **Your choices come from flags.** `beastiepilot -model qwen2.5-coder -type
  guided configure` downloads that model instead and offers to save both
  settings. An existing configuration file keeps everything else in it.
- **The source tree** comes from the distribution set of the release you run
  (`src.txz`, about 250 MB), or from the `FreeBSD-set-src` package when the
  base system was installed from packages. A system that is not a release,
  such as -CURRENT, is left for you to provide.
- **When Ollama does not come up** after being started, `configure` shows the
  last lines of the service's log (`/var/log/ollama-nobody.log` unless
  `rc.conf` says otherwise), or the command that shows them when reading the
  log needs root. One cause it recognizes and offers to fix: the packaged
  service runs Ollama as `nobody`, with no home directory to keep its key in,
  and Ollama stops with `could not create directory mkdir /.ollama`.
  `configure` then offers to give the service `/var/lib/ollama` as its home,
  through `ollama_env` in `rc.conf`, and to start it again.
- **Some steps are optional.** Only Ollama and the model are needed. Say no to
  the reference material and BeastiePilot answers from the model alone.

It exits with status 0 once BeastiePilot is ready to use, and it needs a
terminal.

## Manual installation

Everything `configure` installs can be installed by hand. Run
`beastiepilot configure` once at the end all the same: requests are refused
until it has found BeastiePilot ready. With everything already in place it
runs nothing as root and has little left to ask.

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

If the service stops right away and `/var/log/ollama-nobody.log` ends with
`could not create directory mkdir /.ollama: permission denied`, it has no home
directory to keep its key in. Give it one and start it again:

```sh
mkdir -p /var/lib/ollama && chown nobody /var/lib/ollama
sysrc ollama_env=HOME=/var/lib/ollama
service ollama start
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

To use [reference material](#reference-material) you also need an embedding
model:

```sh
ollama pull nomic-embed-text
```

### Building BeastiePilot

With `go install`:

```sh
go install github.com/mustafajarrah/beastiepilot@latest
```

The binary is placed in `$(go env GOPATH)/bin` (usually `~/go/bin`). Make sure
that directory is on your `PATH`, or set `GOBIN` to have it placed elsewhere,
as the [quick start](#quick-start) does.

From source:

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
beastiepilot [flags]
beastiepilot [flags] configure
beastiepilot [flags] -index
```

Everything after the flags is joined into a single request, so quoting is
optional. Do quote the request when it contains characters your shell would
interpret, such as `?`, `*`, `'` or `|`.

The single word `configure` is not sent to the model: it runs the
[setup](#quick-start). A longer request that starts with it, such as
`beastiepilot configure nginx to listen on port 8080`, is a request like any
other. A single word that looks like a mistyped `configure`, such as
`cofigure` or `config`, is not sent to the model either: BeastiePilot points
to the right spelling and stops.

Nothing is sent to a model before the setup: until `configure` has finished
once with BeastiePilot ready, any other request ends with

```
beastiepilot: not set up yet: run "beastiepilot configure" first; nothing was sent to the model
```

### The session

With no request, BeastiePilot opens a session on the terminal and takes
requests one after another:

```console
$ beastiepilot

  Say what you want to do, in plain language. The command is shown first
  and runs only once you agree.

  ──────────────────────────────────────────────────────────────────────
  > show the free disk space
  ──────────────────────────────────────────────────────────────────────
  /help for commands · Ctrl-D to leave
```

Each request is handled as it would be from the command line: the command is
shown, explained in `guided` mode, and run once you agree. Then the box comes
back for the next one.

- **Requests build on each other.** The last five requests go to the model
  with each new one, together with the command it gave and what came of it, so
  that `the same for /var` or `now only the snapshots` mean something. The
  output of the commands is not sent. `/clear` forgets them.
- **Editing.** Left, Right, Home, End, Backspace and Delete, and the Emacs
  keys a shell has (Ctrl-A, Ctrl-E, Ctrl-U, Ctrl-K, Ctrl-W). Up and Down bring
  back the earlier requests of the session. A line longer than the terminal
  scrolls sideways.
- **Ctrl-C stops what is going on**, not the session: it clears the line,
  abandons a request the model has not answered, means no at the question, and
  interrupts a command that is running. On an empty line, pressing it twice
  leaves.
- **Leaving.** Ctrl-D on an empty line, `/exit`, `exit` or `quit`.

| Command   | What it does                                            |
| --------- | ------------------------------------------------------- |
| `/guided` | Explain each command before asking, from now on         |
| `/safe`   | Show each command and ask, without the explanation      |
| `/clear`  | Clear the screen and forget the earlier requests        |
| `/help`   | List these commands                                     |
| `/exit`   | Leave                                                   |

A command is a single word that starts with a slash, so a request such as
`/var is full, what takes the space?` is still a request. Flags apply to the
whole session: `beastiepilot -type guided -model qwen2.5-coder`. The session
needs a terminal; on one that cannot be read key by key (`TERM=dumb`) it falls
back to a plain prompt without the box and the arrow keys.

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

### Modes

The `TYPE` setting (or `-type`) decides what happens to a suggestion.

| Type     | Explains the command | Asks before running                                 |
| -------- | -------------------- | --------------------------------------------------- |
| `guided` | yes                  | always                                              |
| `safe`   | no                   | yes, except for commands you listed in `AUTO_RUN`   |

`safe` is the default.

**`guided`** is for learning, or for when you want a second look. After the
suggestion, the model is asked to explain it: what the command does, what each
option is for, and what it will change. It costs a second request to the model.

```console
$ beastiepilot -type guided list the jails with their details

  > Suggested command (llama3.2)

    jls -v

  > Why this command

    Lists every running jail along with its details, which is what was asked.
    jls: the base-system tool that reports on jails.
    -v: verbose output, adding the hostname, path and addresses of each jail.
    Effect: nothing changes on the system; it only reads information.

  ? Execute this command? [y/N]
```

The explanation comes from the same model that wrote the command, so it can be
just as wrong. Treat it as an aid to reading the command, not as a guarantee.

### Running trusted commands without asking

Answering `y` to `df -h` gets old quickly. The `AUTO_RUN` setting lists the
commands that `safe` mode runs without the question:

```sh
# ~/.config/beastiepilot/config
AUTO_RUN=df, du, sort, head, uptime, zfs list, zpool status, pkg info, service * status
```

```console
$ beastiepilot show the datasets with their used space

  > Suggested command (llama3.2)

    zfs list -o name,used

  ! Listed in AUTO_RUN: running without confirmation.

NAME              USED
zroot             112G
zroot/usr/home   87.4G
```

The list is empty by default, so nothing runs unasked until you add to it.

**What an entry matches.** An entry is a command name, optionally followed by
its first arguments; `*` stands for any one argument. `zfs list` covers
`zfs list -t snapshot` but not `zfs destroy`, and `service * status` covers the
status of any service but not `service nginx stop`. The name must match
exactly: `df` does not cover `/tmp/df` or `./df`.

**When a suggestion runs unasked.** Only when all of these hold:

- Every command on the line is listed. `df -h | sort -rn` needs both `df` and
  `sort`; `df -h && reboot` asks.
- The line contains nothing that could start a command out of sight or write
  to a file: no `$(...)` or backticks, no subshells or functions, no
  redirection other than to `/dev/null` (`2>/dev/null` and `2>&1` are fine), no
  comments, a single line.
- It was not flagged as destructive (see [Safety](#safety)).
- The mode is `safe`. `guided` always asks.

Anything else falls back to the usual question. With `-verbose`, BeastiePilot
says why a suggestion was not run automatically.

> **Choose the entries with care.** Beyond the words you list, the arguments
> are whatever the model wrote, and they are not checked. List only commands
> that cannot change anything no matter how they are called. `zfs`, `pkg`,
> `service` or `sysctl` on their own would cover their destructive subcommands
> too, and tools such as `sed`, `awk`, `tee`, `tar` or `find` can write, delete
> or run things. (`find` is refused when it uses `-exec`, `-ok` or `-delete`.)
> Shells and commands that exist to run other commands, among them `sh`,
> `env`, `xargs`, `sudo` and `doas`, are rejected as entries.

Listed commands also run when there is no terminal to ask on, which makes
read-only requests usable from scripts and cron. Anything not listed is refused
there, as before.

### Exit status

| Code  | Meaning                                                                                              |
| ----- | ---------------------------------------------------------------------------------------------------- |
| *n*   | The exit status of the command, when it ran                                                          |
| `0`   | You declined; nothing was executed. A session ends with `0` too                                      |
| `1`   | BeastiePilot failed, the setup was not done, the model had no suggestion, or there was no terminal   |
| `2`   | Invalid usage or configuration                                                                       |
| `130` | Interrupted while waiting for the model, or while indexing                                           |

## Configuration

Settings can live in a configuration file, so you do not have to repeat flags.
BeastiePilot reads, in this order:

1. `/usr/local/etc/beastiepilot.conf`, for everyone on the machine
2. `~/.config/beastiepilot/config` (or `$XDG_CONFIG_HOME/beastiepilot/config`),
   for your user

Both are optional. The format is `KEY=value`, one per line, in the style of
`rc.conf`: values may be quoted and lines starting with `#` are comments. Keys
are written in uppercase and values in lowercase. Other spellings are accepted
(`type=GUIDED` works), except for command names in `AUTO_RUN` and for paths,
which are taken exactly as written.

```sh
# ~/.config/beastiepilot/config
MODEL=qwen2.5-coder
PACKAGE_MANAGER=ports
TYPE=safe
AUTO_RUN=df, du, zfs list, pkg info
TIMEOUT=5m
BANNER=compact
VERBOSE=true
```

[`beastiepilot.conf.sample`](beastiepilot.conf.sample) documents every setting
and is a good starting point:

```sh
mkdir -p ~/.config/beastiepilot
cp beastiepilot.conf.sample ~/.config/beastiepilot/config
```

### Settings

Every setting has a key for the file, an environment variable and a flag.

| Key               | Environment variable           | Flag               | Default                               | Description                                                   |
| ----------------- | ------------------------------ | ------------------ | ------------------------------------- | ------------------------------------------------------------- |
| `MODEL`           | `BEASTIEPILOT_MODEL`           | `-model`           | `llama3.2`                            | Ollama model to query                                         |
| `EMBED_MODEL`     | `BEASTIEPILOT_EMBED_MODEL`     | `-embed-model`     | `nomic-embed-text`                    | Ollama model that embeds text for the index                   |
| `ENDPOINT`        | `BEASTIEPILOT_ENDPOINT`        | `-endpoint`        | `http://localhost:11434/api/generate` | Ollama generate API URL                                       |
| `TIMEOUT`         | `BEASTIEPILOT_TIMEOUT`         | `-timeout`         | `2m`                                  | How long to wait for the model                                |
| `TYPE`            | `BEASTIEPILOT_TYPE`            | `-type`            | `safe`                                | `guided` or `safe`; see [Modes](#modes)                       |
| `AUTO_RUN`        | `BEASTIEPILOT_AUTO_RUN`        | `-auto-run`        | *(empty)*                             | Commands `safe` mode runs without asking, comma-separated     |
| `PACKAGE_MANAGER` | `BEASTIEPILOT_PACKAGE_MANAGER` | `-package-manager` | `pkg`                                 | Install software with `pkg` (binary packages) or `ports`      |
| `SOURCE_PATH`     | `BEASTIEPILOT_SOURCE_PATH`     | `-source-path`     | `/usr/src`                            | FreeBSD source tree to index; see [Reference material](#reference-material) |
| `DOCS_SOURCE`     | `BEASTIEPILOT_DOCS_SOURCE`     | `-docs-source`     | `/usr/share/doc`                      | Documentation to index; several places are separated by `:`   |
| `RAG_SOURCE`      | `BEASTIEPILOT_RAG_SOURCE`      | `-rag-source`      | *(empty)*                             | Your own files and books to index                             |
| `BANNER`          | `BEASTIEPILOT_BANNER`          | `-no-banner`       | `full`                                | `full`, `compact` or `off`; see [The banner](#the-banner)     |
| `VERBOSE`         | `BEASTIEPILOT_VERBOSE`         | `-verbose`         | `false`                               | Show the configuration, endpoint, model, timing and raw reply |

Booleans accept `true`/`false`, `yes`/`no`, `on`/`off` and `1`/`0`.

**Precedence**, lowest to highest: built-in defaults, the system file, your
user file, environment variables, command-line flags.

Other flags and variables:

| Name                                  | Description                                                |
| ------------------------------------- | ---------------------------------------------------------- |
| `-config FILE`, `BEASTIEPILOT_CONFIG` | Read only this file instead of the two default locations   |
| `configure`                           | Set the machine up, step by step; see [Quick start](#quick-start) |
| `-index`                              | Build or update the index of reference material, then exit |
| `-version`                            | Print the version and exit                                 |
| `NO_COLOR`                            | Disable colored output when set                            |

Flags must come before the request. An unknown key or an invalid value is an
error, reported with the file and line number, rather than being silently
ignored. A configuration file in the current directory is never read, so a
directory someone else prepared cannot add to your `AUTO_RUN` list or point
your requests at another server.

### Reference material

A model recalls FreeBSD from its training, and recalls it imperfectly.
BeastiePilot can let it read instead: build an index of the documentation on
your machine, and from then on the passages closest to each request are handed
to the model along with it.

```sh
ollama pull nomic-embed-text    # once
beastiepilot -index
```

`beastiepilot configure` does both, and installs the material to read if you
let it.

```console
$ beastiepilot -index

  > Indexing reference material (nomic-embed-text)

    /usr/src         2914 files
    /usr/share/doc   187 files, 12 of other types left out
    /home/me/books   5 files

  > Index ready: 21350 excerpts from 3106 files, 81.2 MB
    21350 excerpts embedded now, 0 files unchanged
    /home/me/.cache/beastiepilot/index
```

(The numbers are an illustration, not a measurement.)

**What is indexed.** Three settings name the places to read:

| Setting       | Default          | What is read from it                                                                                                    |
| ------------- | ---------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `SOURCE_PATH` | `/usr/src`       | The manual pages for commands, devices, file formats and administration (sections 1, 4, 5, 7 and 8). The code is not indexed. |
| `DOCS_SOURCE` | `/usr/share/doc` | Every file in a format listed below                                                                                     |
| `RAG_SOURCE`  | *(empty)*        | The same, for directories of your own: notes, runbooks, books                                                           |

Each takes one path or several separated by `:`, as in `PATH`. Paths must be
absolute; `~` stands for your home directory, an empty value means none, and a
place that does not exist is skipped.

The Handbook, the FAQ and the articles are not part of the base system: they
come with the `en-freebsd-doc` package, in `/usr/local/share/doc/freebsd`.
`beastiepilot configure` offers to install it and then sets
`DOCS_SOURCE=/usr/share/doc:/usr/local/share/doc/freebsd`.

If the source tree is not installed, the installed manual pages do the same
job: add `/usr/share/man` to `DOCS_SOURCE`. That takes every section, so the
index is larger.

**Formats.** Plain text and Markdown (`.txt`, `.md`, `.rst`, `.adoc`, `.org`
and files with no extension), manual pages, HTML and EPUB, all of them also
when gzipped. PDF is read if `pdftotext` is installed
(`pkg install poppler-utils`); otherwise PDF files are skipped and the index
command says so. Of an HTML page, the menus and footers around the text are
left out. A PDF that is the same document again, the way the FreeBSD
documentation packages put `handbook_en.pdf` beside the Handbook's HTML pages,
is not read at all.

**How it works.** Each file is turned into text and cut into excerpts of about
a thousand characters. `EMBED_MODEL` turns every excerpt into a vector, and
the vectors are stored in `~/.cache/beastiepilot/index` (or under
`$XDG_CACHE_HOME`). For each request BeastiePilot embeds the request, finds the
three closest excerpts and appends them to the system prompt. `-verbose` shows
which excerpts were used and how similar they were. Every request asks Ollama
for a context of 4096 tokens, which holds all of that, whatever the server's
own default is.

**Keeping it current.** Run `beastiepilot -index` again after adding files or
upgrading the system. Only new and changed files are embedded, and a run you
interrupt keeps what it had done. Changing `EMBED_MODEL` means embedding
everything again. To stop using reference material, empty the three settings
and run `beastiepilot -index`, or delete the index file.

**Limits worth knowing.**

- The first build sends every excerpt through the embedding model, which takes
  a while for a whole set of manual pages.
- There is no relevance threshold: the closest excerpts are attached even when
  nothing in the index is really about the request. Useful thresholds differ
  from one embedding model to the next.
- Manual pages are converted by a small built-in reader, not a full formatter.
  Options and descriptions come through; tables and unusual macros come out
  rough.
- If retrieval fails (the embedding model is not pulled, say), the request is
  answered without it and a notice says why.

### The banner

The banner shows Beastie in his pilot cap and goggles next to the version, the
model and Ollama host in use, the working directory and the mode. It only
appears on an interactive terminal, and comes in three forms:

- **Pixel art**, used with `BANNER=full` when the locale is UTF-8, colors are
  enabled and the terminal has room for it (17 lines tall). While the model is
  thinking Beastie wags his tail, blinks, and his goggles catch the light. It
  uses 256 colors, or 24-bit color when `COLORTERM` says the terminal has it.
- **Compact**, the four-line mascot shown at the top of this page. It is used
  with `BANNER=compact`, with `NO_COLOR`, and whenever the terminal is too
  small for the pixel art.
- **ASCII**, when the locale is not UTF-8, for example on the FreeBSD console
  in the `C` locale.

`BANNER=off` or `-no-banner` hides it altogether.

## Safety

BeastiePilot is a convenience, not a safeguard. Language models make mistakes,
and a confident-looking command can still be wrong or destructive. The system
prompt only steers the model; the protections below are enforced in code.

- **Read every command before approving it.** The confirmation prompt is the
  only thing standing between the model and your system.
- **Nothing is sent to a model before the setup.** Until
  `beastiepilot configure` has finished once for your user, a request goes
  nowhere: the program points to `configure` and stops.
- **Nothing runs unasked unless you listed it.** The only commands that skip
  the prompt are the ones in your own `AUTO_RUN` list, which is empty by
  default. The list is matched against every command on the line, and a line
  that cannot be analyzed with certainty asks. It is only as safe as its
  entries; see
  [Running trusted commands without asking](#running-trusted-commands-without-asking).
- **Approval comes from the terminal, never from a pipe.** The answer is read
  from `/dev/tty`, so `yes | beastiepilot ...` cannot approve anything, and
  redirecting output cannot hide the command you are approving. Without a
  terminal (cron, CI, a detached session) nothing outside `AUTO_RUN` is ever
  executed.
- **Only an explicit answer counts.** `y` or `yes` followed by Enter approves;
  anything else, including an empty line, declines.
- **Only an answer to the question counts.** Whatever was typed or pasted
  before the question appeared is discarded, so a `y` typed ahead, or the
  second line of a paste, cannot approve a command you have not seen.
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
- **Indexed files can steer the model.** Excerpts of your reference material
  become part of the prompt, so a document that says "run this" can influence
  the suggestion. The model is told to treat excerpts as data, which is advice
  to the model and not a safeguard. Everything above still applies to whatever
  it suggests, but index only material you trust, particularly alongside
  `AUTO_RUN`.
- **No sandbox, no privilege changes.** Commands run through `/bin/sh -c` with
  the privileges of the user who invoked BeastiePilot. Avoid running it as root
  unless the task requires it.
- **`configure` is the one place that asks for root.** Its commands are fixed
  in the program, not written by the model: installing packages, enabling the
  Ollama service, unpacking the source tree, copying the program. Each is
  shown and needs a yes, and it is `doas`, `sudo` or `su` that asks for the
  password, not BeastiePilot. The source tree is downloaded over HTTPS from
  `download.FreeBSD.org`, without the checksum comparison the installer makes.

### What is sent to the model

Your request, the system prompt, and a few facts about the host: the output of
`uname -sr`, the CPU architecture, whether you are root, and the current
working directory. In `guided` mode a second request carries your request and
the suggested command. In a [session](#the-session), up to five earlier
requests go along as well, each with the command suggested for it and whether
it ran and with what exit status; the output of the commands does not.

With an index, the request is also sent to the embedding model, and up to three
excerpts of the indexed files travel with it to the model that writes the
command. Building the index sends the text of every indexed file to the
embedding model.

With the default endpoint all of it stays on your machine; keep that in mind
if you point `ENDPOINT` at a remote server, above all with private notes in
`RAG_SOURCE`.

## Development

```sh
go vet ./...
go test ./...
```

The tests cover the parts that guard execution (cleaning and validating the
model's reply, the risk heuristics, the `AUTO_RUN` matcher, the confirmation
prompt), the configuration loader, the Ollama client, the index (reading
files, building, updating and searching it), the setup and the banner's
rendering. They use a stand-in for the embedding model, so they say nothing
about how well a real one retrieves, and a stand-in for the machine, so the
setup's commands are checked for what they are, not run.

## License

Released under the [MIT License](LICENSE). Copyright (c) 2026 Mustafa Jarrah.

The BSD Daemon ("Beastie") is copyright Marshall Kirk McKusick and is depicted
here with affection; the MIT License above covers the code, not the mascot.
FreeBSD is a registered trademark of The FreeBSD Foundation. BeastiePilot is an
independent project, not affiliated with or endorsed by either.
