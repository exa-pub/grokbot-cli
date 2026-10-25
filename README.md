# grokbot-tunnel

grokbot-tunnel connects a Linux computer to Grok Bot so that the cloud agent can
run commands and work with files on it. It speaks the protocol of the Grok Bot
0.75.8 desktop client.

## Installation

```sh
curl -fsSL https://raw.githubusercontent.com/exa-pub/grokbot-tunnel/main/install.sh | bash
```

The script installs the latest release to `~/.local/bin` and does not need
sudo. To install a specific release, pass its tag:
`... | bash -s v0.1.0`. Set `GROKBOT_INSTALL_DIR` to change the target
directory. You can also download the binary from
[Releases](https://github.com/exa-pub/grokbot-tunnel/releases) or
[build from source](CONTRIBUTING.md).

Requirements:

- bash. If `$SHELL` points to zsh, the agent's commands run in zsh.
- `rg` (ripgrep), optional. With it, directory listings respect `.gitignore`.

## Login

```sh
grokbot-tunnel login
```

`login` opens the sign-in page in the browser.

- `--no-browser` — print the link instead of opening the browser, so you can
  open it on another device.
- `--team ID` — select a team.

## Connecting

```sh
grokbot-tunnel connect --root "$HOME/projects"
```

`connect` runs the agent's requests until you press Ctrl-C.

| Flag | Meaning |
|------|---------|
| `--root` | Directory for file operations and commands. Defaults to the home directory. |
| `--label` | Computer name in Grok Bot. It is saved for later runs; on the first run it is the host name. |
| `--permission` | Permission mode: `ask` (default), `always` or `never`. |

On Ctrl-C the CLI stops running commands. Background commands keep running.

### Permissions

| Mode | What happens |
|------|--------------|
| `ask` | The CLI asks in the terminal before each action. Without a terminal, all requests are refused. |
| `always` | All actions are allowed. |
| `never` | All actions are refused. |

Answers to the `ask` prompt:

- `y` — allow this action;
- `a` — allow all actions until the end of the session;
- anything else — refuse.

If Grok Bot manages permissions itself, the CLI does not prompt and runs only
the actions Grok Bot approved. The `never` mode still refuses all actions.

## What the agent can do

- **Commands.** Commands run one at a time. Each one continues from the state
  the previous one left: working directory, exported variables, aliases,
  functions and shell options.
- **Background commands.** Their output goes to `<root>/terminals/<number>.txt`.
- **Files.** The agent reads whole files or line ranges, lists directory trees,
  and uploads and downloads files up to 100 MiB.
- **Codex and Claude Code setup.** The agent gets the names of skills, MCP
  servers, plugins and commands, plus recent requests. Keys, links, emails and
  paths are removed from them before sending.

## Security

- `--root` limits only file operations and the working directory of commands.
  The commands themselves run with your privileges and can access any file.
- Codex and Claude Code settings are read from the home directory, even if it
  is outside `--root`.
- Tokens are stored unencrypted in `~/.grok/bot-tunnel/`. Only the owner can
  access that directory.
- `connect --debug` prints the agent's requests in full, including commands
  and file contents.
