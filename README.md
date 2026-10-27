# grokbot-tunnel

[![CI](https://github.com/exa-pub/grokbot-tunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/exa-pub/grokbot-tunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/exa-pub/grokbot-tunnel)](https://github.com/exa-pub/grokbot-tunnel/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/exa-pub/grokbot-tunnel)](go.mod)
[![License: MIT](https://img.shields.io/github/license/exa-pub/grokbot-tunnel)](LICENSE)

**Headless Grok Bot for Linux servers.** grokbot-tunnel is a CLI that connects a
headless Linux server (VPS, cloud VM, home server, Docker container, Raspberry
Pi) to Grok Bot as a local computer. The Grok Bot agent can then run commands
and work with files on that server, the same way it does on a machine with the
Grok Bot desktop app, but without a desktop, GUI or browser.

It is a single static Go binary that does the job of both the headless
controller and the local execution daemon: it signs in, registers the machine
as a remote computer and executes the agent's requests itself. You don't need
the Grok Bot desktop app, Electron, Node.js, X11, Wayland or Xvfb on the server.

Use it to run Grok Bots on a remote server over SSH, give the agent access to a
build machine, or keep a headless connection to Grok Bot running as a systemd
service with `grokbot-tunnel install`.

## How to connect Grok Bot to a headless server

1. Install. The installer puts the binary in `~/.local/bin`. To install a
   specific release, pass its tag: `bash -s v0.1.0`.

   ```sh
   curl -fsSL https://raw.githubusercontent.com/exa-pub/grokbot-tunnel/main/install.sh | bash
   ```

2. Sign in. `login` prints a sign-in link that you open on any
   device. On a machine with a browser, plain `login` opens it for you. Use
   `--team ID` to select a team. This is how sign-in works on a headless server
   without a browser.

   ```sh
   grokbot-tunnel login
   ```

3. Connect. `connect` makes the server appear as a computer in Grok Bot and
   runs the agent's requests until you press Ctrl-C. Ctrl-C stops running
   commands; background commands keep running.

   ```sh
   grokbot-tunnel connect
   ```

4. Optionally, run it as a service. `install` sets up a systemd service that
   runs `connect` with the given flags, starts at boot and restarts on
   failure. `--permission` is required here (see below).

   ```sh
   grokbot-tunnel install --permission always
   ```

Linux on x86_64 (amd64) and arm64 is supported. The server needs bash (or zsh,
if `$SHELL` points to it). `rg` (ripgrep) is
optional; with it, directory listings respect `.gitignore`.

## CLI connect options

| Flag | Meaning |
|------|---------|
| `--root` | Directory for file operations and commands. Defaults to the home directory. |
| `--label` | Computer name in Grok Bot. Saved for later runs; defaults to the host name. |
| `--permission` | `ask` (default) asks in the terminal before each action; `always` allows everything; `never` refuses everything. |

At the `ask` prompt, `y` allows the action, `a` allows all actions until the end
of the session, anything else refuses it. Without a terminal, `ask` refuses
every request, so use `--permission always` when running without a terminal.
If Grok Bot manages permissions itself, the tunnel
does not prompt and runs only the actions Grok Bot approved.

Only one `connect` runs per user at a time: a second one, e.g. in a terminal
while the service is running, exits with a message saying how to stop the
first.

## Run as a systemd service

`grokbot-tunnel install` takes the same flags as `connect`; `--permission` is
required, since a service has no terminal to ask in. Run it again to change the
flags.

- A regular user gets a service of their systemd user manager
  (`~/.config/systemd/user/grokbot-tunnel.service`), and lingering is enabled so
  that it runs without a login session. Manage it with
  `systemctl --user status grokbot-tunnel` and
  `journalctl --user -u grokbot-tunnel -f`.
- Root gets a system service (`/etc/systemd/system/grokbot-tunnel.service`):
  `systemctl status grokbot-tunnel`, `journalctl -u grokbot-tunnel -f`.
- `install` returns once the tunnel is connected, or fails with its log.
- The service keeps the `PATH` and `SHELL` of the shell you run `install` in,
  so the agent's commands find the same programs.
- Stopping or restarting the service also stops background commands the agent
  started, unlike Ctrl-C on `connect`.
- After the login is revoked the service stops; run `grokbot-tunnel login`
  and then `install` again.
- A daily timer updates grokbot-tunnel and restarts the service. Pass
  `--no-auto-update` to `install` to turn it off.

`grokbot-tunnel uninstall` stops and removes the service and keeps the binary
and your login.

## Update

```sh
grokbot-tunnel update
```

`update` downloads the latest release (or the one given, e.g.
`grokbot-tunnel update v0.2.0`), verifies its checksum, replaces the binary and
restarts the service. `--check` only shows the versions; `--force` reinstalls
the same version or downgrades; `--no-restart` leaves the service alone.

## What the agent gets

- A persistent shell: each command continues from the working directory,
  variables, aliases and functions the previous one left. Background commands
  write their output to `<root>/terminals/<number>.txt`.
- File reads, directory listings, and uploads and downloads up to 100 MiB.
- Names of Codex and Claude Code skills, MCP servers, plugins and commands, with
  keys, links, emails and paths removed.

## Security

- Commands run with your privileges. `--root` limits only file operations and
  the starting directory, not what commands can access. Codex and Claude Code
  settings are read from the home directory even if it is outside `--root`.
- Tokens are stored unencrypted in `~/.grok/bot-tunnel/`, readable only by you.
- `connect --debug` prints requests in full, including commands and file
  contents.

To build from source, see [CONTRIBUTING.md](CONTRIBUTING.md).

grokbot-tunnel is an independent project, not affiliated with or endorsed by
xAI. Grok Bot is a trademark of its owner.
