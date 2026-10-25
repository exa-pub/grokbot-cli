# grokbot-tunnel

[![CI](https://github.com/exa-pub/grokbot-tunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/exa-pub/grokbot-tunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/exa-pub/grokbot-tunnel)](https://github.com/exa-pub/grokbot-tunnel/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/exa-pub/grokbot-tunnel)](go.mod)
[![License: MIT](https://img.shields.io/github/license/exa-pub/grokbot-tunnel)](LICENSE)

grokbot-tunnel connects a Linux server to Grok Bot as a computer. The Grok Bot
agent can then run commands and work with files on that server, the same way
it does on a machine with the Grok Bot desktop app, but without a desktop.

## Quick start

```sh
curl -fsSL https://raw.githubusercontent.com/exa-pub/grokbot-tunnel/main/install.sh | bash
grokbot-tunnel login --no-browser
grokbot-tunnel connect --root "$HOME/projects"
```

1. The installer puts the binary in `~/.local/bin`. Pass a tag to install a
   specific release: `... | bash -s v0.1.0`.
2. `login --no-browser` prints a sign-in link that you open on any device. On a
   machine with a browser, plain `login` opens it for you. Use `--team ID` to
   select a team.
3. `connect` makes the server appear as a computer in Grok Bot and runs the
   agent's requests until you press Ctrl-C. Ctrl-C stops running commands;
   background commands keep running.

The server needs bash (or zsh, if `$SHELL` points to it). `rg` (ripgrep) is
optional; with it, directory listings respect `.gitignore`.

## Connect options

| Flag | Meaning |
|------|---------|
| `--root` | Directory for file operations and commands. Defaults to the home directory. |
| `--label` | Computer name in Grok Bot. Saved for later runs; defaults to the host name. |
| `--permission` | `ask` (default) asks in the terminal before each action; `always` allows everything; `never` refuses everything. |

At the `ask` prompt, `y` allows the action, `a` allows all actions until the end
of the session, anything else refuses it. Without a terminal, `ask` refuses
every request, so use `--permission always` when running without a terminal,
e.g. under systemd. If Grok Bot manages permissions itself, the tunnel
does not prompt and runs only the actions Grok Bot approved.

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
