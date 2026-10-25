package executor

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Foreground commands share one shell state: cwd, exported environment,
// options, aliases and functions. The user's profile is read once, by an
// interactive login shell that only dumps its state; every command then runs
// in a fresh non-login shell that replays the state, runs the command and
// dumps the state again.
//
// Descriptors of a foreground shell:
//
//	3  replay script (options, functions, aliases), read and closed first
//	4  state snapshot, closed while the command runs
//	5  command text, read and closed first
//
// The dump runs after the command, not from an EXIT trap: a trap can be
// replaced by the command, and when the command exits from a function or is
// killed by a signal, bash runs it with the command's descriptors, so a
// command that ran "exec 4>&1" would print the whole environment. The command
// runs as "eval ... 4>&-"; bash and zsh restore descriptor 4 after eval even if
// the command reassigned it, and children never inherit it. A command that
// ends the shell itself (exit, exec, set -e, a signal) leaves no snapshot and
// the previous state stays in effect.
//
// The state and the command reach the shell through files, not arguments:
// Linux limits a single argument to 128 KiB, and functions from a profile or
// a command with a heredoc can exceed that.

const (
	maxSnapshot = 1 << 20
	initTimeout = 10 * time.Second
	// sectionPrefix tags the non-environment parts of a state snapshot.
	sectionPrefix    = "GROK_SHELL_"
	sectionOptions   = sectionPrefix + "OPTIONS"
	sectionAliases   = sectionPrefix + "ALIASES"
	sectionFunctions = sectionPrefix + "FUNCTIONS"
	sectionEnd       = sectionPrefix + "END"
)

// agentEnv marks processes started on behalf of the agent.
var agentEnv = []string{"CURSOR_AGENT=1", "SAND_AGENT=1"}

// volatileEnv is set by the shell itself and must not be carried over: bash
// increments an inherited SHLVL, so a saved one would grow with every command.
// environ passes the CLI's SHLVL instead; without one, bash -c ignores
// $BASH_ENV.
var volatileEnv = []string{"SHLVL", "_", "PWD"}

// volatileOptions are not carried over. errexit, nounset and pipefail would
// change how every later command and the state dump behave; the others are
// set by the interactive shell that reads the profile, and job control
// (monitor) would move background jobs out of the command's process group.
var volatileOptions = map[string]bool{
	"errexit": true, "errreturn": true, "unset": true, "nounset": true, "pipefail": true,
	"monitor": true, "interactive": true, "shinstdin": true, "zle": true, "login": true,
	"privileged": true, "restricted": true, "singlecommand": true, "onecmd": true, "exec": true,
	"history": true, "histexpand": true, "emacs": true, "vi": true,
	"login_shell": true, "restricted_shell": true,
}

// shellState is carried over between foreground commands.
type shellState struct {
	cwd       string
	env       []string // nil means the CLI's own environment
	options   []string // "set -o x" / "shopt -s x" lines
	aliases   string
	functions string
}

// shell is the user's shell if it is bash or zsh, else bash.
type shell struct {
	path string
	zsh  bool
}

func userShell() shell {
	name := os.Getenv("SHELL")
	if base := filepath.Base(name); base != "bash" && base != "zsh" {
		name = "bash"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return shell{path: "/bin/bash"}
	}
	return shell{path: path, zsh: filepath.Base(name) == "zsh"}
}

// args runs script with the glob extensions the desktop enables; name is $0.
func (s shell) args(flags, script, name string) []string {
	if s.zsh {
		return []string{"-o", "extendedglob", flags, script, name}
	}
	return []string{"-O", "extglob", flags, script, name}
}

// dump writes the state as NUL-separated cwd and environment, then tagged
// option, alias and function sections and an end tag. Command output cannot be
// mistaken for state, and state is replayed from a file, not parsed back from
// output.
func (s shell) dump() string {
	options, aliases := `builtin set +o; builtin shopt -p`, `builtin alias -p`
	if s.zsh {
		options, aliases = `builtin set +o`, `builtin alias -L; builtin alias -gL; builtin alias -sL`
	}
	return strings.Join([]string{
		`builtin printf '%s\0' "$PWD"`,
		`/usr/bin/env -0`,
		`builtin printf '\0` + sectionOptions + `\0'`,
		options,
		`builtin printf '\0` + sectionAliases + `\0'`,
		aliases,
		`builtin printf '\0` + sectionFunctions + `\0'`,
		`builtin typeset -f`,
		`builtin printf '\0` + sectionEnd + `\0'`,
	}, "; ")
}

// initScript dumps the state of the shell that read the profile.
func (s shell) initScript() string {
	return `{ ` + s.dump() + `; } >&4 2>/dev/null`
}

// prepare reads the replay script from descriptor 3 and the command from
// descriptor 5, closes both and replays the state, leaving the command in $1.
// The descriptors are closed with a bare exec before the replay, which could
// define a function named exec; "builtin exec" would only close them for the
// duration of the builtin.
func (s shell) prepare() string {
	unalias, aliases := "", `builtin shopt -s expand_aliases 2>/dev/null`
	if s.zsh {
		// Aliases from .zshenv must not change how the replayed functions parse.
		unalias = `builtin unsetopt aliases 2>/dev/null; builtin unalias -m '*' 2>/dev/null; `
		aliases = `builtin setopt aliases 2>/dev/null`
	}
	return `__grok_cmd=$(</dev/fd/5) && __grok_replay=$(</dev/fd/3) || { ` +
		`builtin printf '%s\n' 'The command did not run: a shell startup file ($BASH_ENV or .zshenv) closed descriptor 3 or 5.' >&2; ` +
		`builtin exit 126; }; ` +
		`exec 3<&- 5<&-; ` +
		unalias + `builtin eval -- "$__grok_replay"; builtin unset __grok_replay; ` +
		aliases + `; ` +
		`builtin set -- "$__grok_cmd"; builtin unset __grok_cmd; `
}

// foregroundScript runs the command with descriptor 4 closed and then dumps
// the state to it.
func (s shell) foregroundScript() string {
	return s.prepare() +
		`builtin eval -- "$1" 4>&-; __grok_exit=$?; ` +
		`{ ` + s.dump() + `; } >&4 2>/dev/null; ` +
		`builtin exit "$__grok_exit"`
}

// backgroundScript waits for a line on descriptor 4 before running the
// command, so the terminal header is written before any output. It replays the
// state but does not save it.
func (s shell) backgroundScript() string {
	return `IFS= builtin read -r __grok_start <&4; exec 4<&-; builtin unset __grok_start; ` +
		s.prepare() + `builtin eval -- "$1"`
}

// replayScript returns the shell code that restores st. Functions come first,
// so that options such as posix cannot break their definitions; option lines
// that cannot be changed in a non-interactive shell fail quietly.
func (st shellState) replayScript() string {
	var b strings.Builder
	b.WriteString(st.functions)
	b.WriteString("\n")
	for _, option := range st.options {
		b.WriteString(option + " 2>/dev/null\n")
	}
	b.WriteString(st.aliases)
	b.WriteString("\n")
	return b.String()
}

// environ returns the saved environment with the CLI's SHLVL and the agent
// markers set once.
func (st shellState) environ() []string {
	env := st.env
	if env == nil {
		env = os.Environ()
	} else if level, ok := os.LookupEnv("SHLVL"); ok {
		env = append(env[:len(env):len(env)], "SHLVL="+level)
	}
	out := make([]string, 0, len(env)+len(agentEnv))
	for _, kv := range env {
		if !isAgentVar(kv) {
			out = append(out, kv)
		}
	}
	return append(out, agentEnv...)
}

func isAgentVar(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	for _, marker := range agentEnv {
		if name, _, _ := strings.Cut(marker, "="); name == key {
			return true
		}
	}
	return false
}

func (e *Executor) shellState() shellState {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.state
}

func (e *Executor) setShellState(st shellState) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.state = st
}

// initShell captures the state of the user's profile once, before the first
// command. A profile that fails or hangs leaves the CLI's environment.
func (e *Executor) initShell() {
	e.initMu.Lock()
	defer e.initMu.Unlock()
	if e.initialized {
		return
	}
	e.initialized = true
	st, err := e.profileState()
	if err != nil {
		log.Printf("shell profile: %v; using the CLI environment", err)
		return
	}
	if !e.validCwd(st.cwd) {
		st.cwd = ""
	}
	e.setShellState(st)
}

func (e *Executor) profileState() (shellState, error) {
	snapshot, err := tempFile("grok-shell-state-", "")
	if err != nil {
		return shellState{}, err
	}
	defer closeLogged(snapshot)
	sh := userShell()
	c := shellCommand(sh, shellState{}, e.Root, sh.args("-ilc", sh.initScript(), "grokbot-init")...)
	// A new session keeps the interactive shell away from the CLI's terminal.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.ExtraFiles = []*os.File{nil, snapshot}
	if err = c.Start(); err != nil {
		return shellState{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			kill(c)
		case <-done:
		}
	}()
	wait(c)
	close(done)
	if ctx.Err() != nil {
		return shellState{}, errors.New("profile did not finish within 10s")
	}
	b, err := readSnapshot(snapshot)
	if err != nil {
		return shellState{}, err
	}
	st, ok := parseSnapshot(b)
	if !ok {
		return shellState{}, errors.New("profile left no state")
	}
	return st, nil
}

// adoptState returns the state a foreground command left in its snapshot.
// Commands that ended the shell themselves leave none. A cwd outside Root is
// not kept; the next command starts where this one did.
func (e *Executor) adoptState(f *os.File, cwd string) (shellState, bool) {
	b, err := readSnapshot(f)
	if err != nil {
		log.Printf("read shell state: %v", err)
		return shellState{}, false
	}
	st, ok := parseSnapshot(b)
	if !ok {
		return shellState{}, false
	}
	if !e.validCwd(st.cwd) {
		st.cwd = cwd
	}
	return st, true
}

// validCwd reports whether dir is a directory below Root.
func (e *Executor) validCwd(dir string) bool {
	rel, err := e.relative(dir)
	if err != nil {
		return false
	}
	info, err := e.Files.Stat(rel)
	return err == nil && info.IsDir()
}

func readSnapshot(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSnapshot {
		return nil, errors.New("shell state exceeds 1 MiB")
	}
	return readAtMost(f, info.Size(), 0)
}

// parseSnapshot decodes the state written by shell.dump. A snapshot without
// the end tag was cut short and is rejected.
func parseSnapshot(b []byte) (shellState, bool) {
	b, complete := bytes.CutSuffix(b, []byte("\x00"+sectionEnd+"\x00"))
	if !complete {
		return shellState{}, false
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) < 2 || !filepath.IsAbs(parts[0]) {
		return shellState{}, false
	}
	st := shellState{cwd: parts[0], env: []string{}}
	sections := map[string]string{}
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if strings.HasPrefix(part, sectionPrefix) && !strings.Contains(part, "=") {
			if i+1 < len(parts) {
				sections[part] = parts[i+1]
				i++
			}
			continue
		}
		if len(sections) == 0 && strings.Contains(part, "=") && !isVolatileVar(part) {
			st.env = append(st.env, part)
		}
	}
	st.options = parseOptions(sections[sectionOptions])
	st.aliases = sections[sectionAliases]
	st.functions = sections[sectionFunctions]
	return st, true
}

func isVolatileVar(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	for _, name := range volatileEnv {
		if name == key {
			return true
		}
	}
	return false
}

// parseOptions keeps only well-formed "set ±o name" and "shopt -s/-u name"
// lines, since they are evaluated by the next command.
func parseOptions(text string) []string {
	var options []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !isWord(fields[2]) || isVolatileOption(fields[2]) {
			continue
		}
		set := fields[0] == "set" && (fields[1] == "-o" || fields[1] == "+o")
		shopt := fields[0] == "shopt" && (fields[1] == "-s" || fields[1] == "-u")
		if set || shopt {
			options = append(options, strings.Join(fields, " "))
		}
	}
	return options
}

// isVolatileOption matches option names the way zsh does: case, underscores
// and a "no" prefix do not matter. bash's names are a subset.
func isVolatileOption(name string) bool {
	if volatileOptions[name] {
		return true
	}
	name = strings.ToLower(strings.ReplaceAll(name, "_", ""))
	return volatileOptions[name] || volatileOptions[strings.TrimPrefix(name, "no")]
}

func isWord(s string) bool {
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
