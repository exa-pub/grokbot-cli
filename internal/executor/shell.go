package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
)

// agentEnv marks processes started on behalf of the agent.
var agentEnv = []string{"CURSOR_AGENT=1", "SAND_AGENT=1"}

const (
	maxSnapshot = 1 << 20
	// sectionPrefix tags the non-environment parts of a state snapshot.
	sectionPrefix    = "GROK_SHELL_"
	sectionOptions   = sectionPrefix + "OPTIONS"
	sectionAliases   = sectionPrefix + "ALIASES"
	sectionFunctions = sectionPrefix + "FUNCTIONS"
)

// saveStateTrap writes the shell state to descriptor 3 on exit: NUL-separated
// cwd and environment, then tagged option, alias and function sections.
// Command output cannot be mistaken for state, and state is replayed from
// arguments, not parsed back from output.
var saveStateTrap = strings.Join([]string{
	`__grok_exit=$?`,
	`builtin printf "%s\0" "$PWD" >&3`,
	`/usr/bin/env -0 >&3`,
	`builtin printf "\0` + sectionOptions + `\0" >&3`,
	`builtin set +o >&3`,
	`if [ -n "${BASH_VERSION-}" ]; then builtin shopt -p >&3; fi`,
	`builtin printf "\0` + sectionAliases + `\0" >&3`,
	`if [ -n "${BASH_VERSION-}" ]; then builtin alias -p >&3; else builtin alias -L >&3; fi`,
	`builtin printf "\0` + sectionFunctions + `\0" >&3`,
	`builtin typeset -f >&3`,
	`exit "$__grok_exit"`,
}, "; ")

// The saved state and the command reach the shell through files on
// descriptors, not arguments: Linux limits a single argument to 128 KiB, and
// functions from a login profile or a command with a heredoc can exceed that.

// foregroundScript replays the saved options, functions and aliases from
// descriptor 4, then runs the command from descriptor 5.
var foregroundScript = strings.Join([]string{
	`builtin . /dev/fd/4`,
	`trap '` + saveStateTrap + `' EXIT`,
	`eval -- "$(</dev/fd/5)"`,
}, "; ")

// backgroundScript runs the command from descriptor 4 and appends the
// terminal footer.
const backgroundScript = `trap '__grok_exit=$?; ` +
	`builtin printf "\n---\nexit_code: %s\nended_at: %s\n---\n" "$__grok_exit" "$(date -u +%Y-%m-%dT%H:%M:%S.000Z)"; ` +
	`exit "$__grok_exit"' EXIT; eval -- "$(</dev/fd/4)"`

// gateScript waits for a line on descriptor 3 before exec'ing the login shell
// ($1) with the background script ($2), so the terminal header is written
// before any output.
const gateScript = `IFS= read -r __grok_start <&3; exec "$1" -lc "$2" grok-background`

// shellState is carried over between foreground commands.
type shellState struct {
	cwd       string
	env       []string // nil means the CLI's own environment
	options   []string // "set -o x" / "shopt -s x" lines
	aliases   string
	functions string
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

// shellBinary returns the user's shell if it is bash or zsh, else bash.
func shellBinary() string {
	shell := os.Getenv("SHELL")
	if name := filepath.Base(shell); name != "bash" && name != "zsh" {
		shell = "bash"
	}
	if path, err := exec.LookPath(shell); err == nil {
		return path
	}
	return "/bin/bash"
}

// replayScript returns the shell code that restores st.
func (st shellState) replayScript() string {
	return strings.Join(st.options, "\n") + "\n" + st.functions + "\n" + st.aliases + "\n"
}

// tempFile returns an unlinked temporary file holding content; only open
// descriptors keep it alive.
func tempFile(prefix, content string) (*os.File, error) {
	f, err := os.CreateTemp("", prefix)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		log.Printf("remove %s: %v", f.Name(), err)
	}
	if _, err := f.WriteString(content); err != nil {
		closeLogged(f)
		return nil, err
	}
	return f, nil
}

// shellCommand runs a script in the user's shell. Running agent commands is
// the purpose of this tool; their lifetime is managed by killing the process
// group, not through a context.
func shellCommand(st shellState, cwd string, args ...string) *exec.Cmd {
	c := exec.Command(shellBinary(), args...) //nolint:gosec,noctx // see above
	c.Dir = cwd
	c.Env = st.environ()
	return c
}

// environ returns the saved environment with the agent markers set once.
func (st shellState) environ() []string {
	env := st.env
	if env == nil {
		env = os.Environ()
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

// shellCwd resolves the working directory, defaulting to the one the previous
// foreground command ended in.
func (e *Executor) shellCwd(path string, st shellState) (dir, warning string, err error) {
	if path == "" && st.cwd != "" {
		path = st.cwd
	}
	return e.cwd(path)
}

func kill(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("kill process group %d: %v", c.Process.Pid, err)
	}
}

// wait reaps c, logging anything but a non-zero exit, which is reported
// through ProcessState.
func wait(c *exec.Cmd) {
	var exitErr *exec.ExitError
	if err := c.Wait(); err != nil && !errors.As(err, &exitErr) {
		log.Printf("shell: %v", err)
	}
}

func closeLogged(f *os.File) {
	if err := f.Close(); err != nil {
		log.Printf("close %s: %v", f.Name(), err)
	}
}

func pid(c *exec.Cmd) *uint32 {
	return proto.Uint32(uint32(c.Process.Pid)) //nolint:gosec // PIDs are positive
}

func timer(ms int32) (<-chan time.Time, func() bool) {
	if ms <= 0 {
		return nil, func() bool { return false }
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	return t.C, t.Stop
}

// exitCode follows the shell convention of 128+signal for killed processes.
func exitCode(state *os.ProcessState) int {
	if state == nil {
		return 128 + int(syscall.SIGKILL)
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

func shellMessage(s *agentv1.ShellStream) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: s}}
}

// background starts cmd in its own session with output going to a terminal
// file. It does not wait for running foreground commands.
func (e *Executor) background(cmd, path string) (*agentv1.BackgroundShellSpawnSuccess, error) {
	st := e.shellState()
	cwd, _, err := e.shellCwd(path, st)
	if err != nil {
		return nil, err
	}
	n, f, err := e.terminal()
	if err != nil {
		return nil, err
	}
	gate, release, err := os.Pipe()
	if err != nil {
		closeLogged(f)
		return nil, err
	}
	defer gate.Close()
	defer release.Close()
	command, err := tempFile("grok-command-", cmd)
	if err != nil {
		closeLogged(f)
		return nil, err
	}
	defer closeLogged(command) // the child keeps its own descriptor
	c := shellCommand(st, cwd, "-c", gateScript, "grok-background", shellBinary(), backgroundScript)
	c.ExtraFiles = []*os.File{gate, command}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdout = f
	c.Stderr = f
	if err = c.Start(); err != nil {
		closeLogged(f)
		return nil, err
	}
	if err = writeTerminalHeader(f, cwd, cmd, c.Process.Pid); err == nil {
		_, err = release.WriteString("\n")
	}
	if err != nil {
		kill(c)
		wait(c)
		closeLogged(f)
		return nil, err
	}
	go func() {
		wait(c)
		if err := appendTerminalExit(f, c.ProcessState); err != nil {
			log.Printf("terminal %d: %v", n, err)
		}
		closeLogged(f)
	}()
	return &agentv1.BackgroundShellSpawnSuccess{ShellId: n, Command: cmd, WorkingDirectory: cwd, Pid: pid(c)}, nil
}

// shellOutput streams stdout and stderr to the agent and, optionally, to a
// terminal file. A single mutex preserves frame ordering across both streams.
type shellOutput struct {
	mu       sync.Mutex
	send     reply
	file     *os.File
	detached bool  // the command was backgrounded; stop streaming
	err      error // first send failure
}

func (o *shellOutput) emit(kind, data string) error {
	stream := &agentv1.ShellStream{}
	if kind == "stdout" {
		stream.Event = &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: data}}
	} else {
		stream.Event = &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: data}}
	}
	return o.send(shellMessage(stream))
}

func (o *shellOutput) sendErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// streamWriter forwards one stream, holding back a trailing incomplete UTF-8
// sequence until the rest of it arrives.
type streamWriter struct {
	out     *shellOutput
	kind    string // "stdout" or "stderr"
	partial []byte
}

// Write never fails: the process must keep running even if the agent or the
// terminal file stops accepting output, otherwise it would block on a full pipe.
func (w *streamWriter) Write(b []byte) (int, error) {
	o := w.out
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file != nil {
		if _, err := o.file.Write(b); err != nil {
			log.Printf("terminal output: %v", err)
			o.file = nil
		}
	}
	if o.detached || o.err != nil {
		return len(b), nil
	}
	data := append(w.partial, b...)
	n := completeLen(data)
	w.partial = append([]byte(nil), data[n:]...)
	for data = data[:n]; len(data) > 0; {
		n := chunkLen(data)
		if err := o.emit(w.kind, strings.ToValidUTF8(string(data[:n]), "�")); err != nil {
			o.err = err
			break
		}
		data = data[n:]
	}
	return len(b), nil
}

// flush sends the held-back bytes once the stream has ended.
func (w *streamWriter) flush() {
	w.out.mu.Lock()
	defer w.out.mu.Unlock()
	if len(w.partial) > 0 && !w.out.detached && w.out.err == nil {
		w.out.err = w.out.emit(w.kind, strings.ToValidUTF8(string(w.partial), "�"))
	}
	w.partial = nil
}

// completeLen returns the length of b without a trailing incomplete UTF-8
// sequence.
func completeLen(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

// chunkLen returns at most streamChunk bytes, cut at a rune boundary.
func chunkLen(b []byte) int {
	if len(b) <= streamChunk {
		return len(b)
	}
	n := streamChunk
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	if n == 0 {
		return streamChunk
	}
	return n
}

// shell runs a foreground command in its own process group and streams its
// output. Foreground commands run one at a time and share cwd, environment,
// options, aliases and functions. On soft timeout the command is either
// killed or, with TIMEOUT_BEHAVIOR_BACKGROUND, left running with output going
// to a terminal file.
func (e *Executor) shell(ctx context.Context, x *agentv1.ShellArgs, send reply) error {
	if x.IsBackground {
		v, err := e.background(x.Command, x.WorkingDirectory)
		if err != nil {
			return err
		}
		return send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{
			Backgrounded: &agentv1.ShellStreamBackgrounded{
				ShellId: v.ShellId, Command: v.Command, WorkingDirectory: v.WorkingDirectory, Pid: v.Pid,
			},
		}}))
	}
	select {
	case e.foreground <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.foreground }()
	// select picks at random when both are ready.
	if err := ctx.Err(); err != nil {
		return err
	}
	st := e.shellState()
	cwd, warning, err := e.shellCwd(x.WorkingDirectory, st)
	if err != nil {
		return err
	}
	if warning != "" {
		stderr := &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: warning}}
		if err = send(shellMessage(&agentv1.ShellStream{Event: stderr})); err != nil {
			return err
		}
	}
	start := &agentv1.ShellStream_Start{Start: &agentv1.ShellStreamStart{Command: &x.Command}}
	if err = send(shellMessage(&agentv1.ShellStream{Event: start})); err != nil {
		return err
	}
	sh, err := e.startShell(x, cwd, st, send)
	if err != nil {
		return err
	}
	err = sh.wait(ctx)
	select {
	case <-sh.done:
		e.restoreShell(sh.snapshot)
		closeLogged(sh.snapshot)
	default:
		// Backgrounded: the next command starts from the current state.
		go func() {
			<-sh.done
			closeLogged(sh.snapshot)
		}()
	}
	return err
}

// runningShell is a started foreground command.
type runningShell struct {
	args     *agentv1.ShellArgs
	cwd      string
	cmd      *exec.Cmd
	out      *shellOutput
	stdout   *streamWriter
	stderr   *streamWriter
	done     chan struct{} // closed once the process has exited
	shellID  uint32        // terminal file number, if the command may be backgrounded
	file     *os.File      // terminal file, if the command may be backgrounded
	snapshot *os.File      // receives the state on exit
}

func (sh *runningShell) canBackground() bool {
	return sh.args.TimeoutBehavior == agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND
}

func (e *Executor) startShell(x *agentv1.ShellArgs, cwd string, st shellState, send reply) (*runningShell, error) {
	sh := &runningShell{args: x, cwd: cwd, out: &shellOutput{send: send}, done: make(chan struct{})}
	var err error
	if sh.canBackground() {
		if sh.shellID, sh.file, err = e.terminal(); err != nil {
			return nil, err
		}
		sh.out.file = sh.file
	}
	if sh.snapshot, err = tempFile("grok-shell-state-", ""); err != nil {
		sh.closeFiles()
		return nil, err
	}
	replay, err := tempFile("grok-shell-replay-", st.replayScript())
	if err != nil {
		sh.closeFiles()
		return nil, err
	}
	defer closeLogged(replay) // the child keeps its own descriptor
	command, err := tempFile("grok-command-", x.Command)
	if err != nil {
		sh.closeFiles()
		return nil, err
	}
	defer closeLogged(command)
	sh.cmd = shellCommand(st, cwd, "-lc", foregroundScript, "grokbot-shell")
	sh.cmd.ExtraFiles = []*os.File{sh.snapshot, replay, command}
	sh.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sh.stdout = &streamWriter{out: sh.out, kind: "stdout"}
	sh.stderr = &streamWriter{out: sh.out, kind: "stderr"}
	sh.cmd.Stdout = sh.stdout
	sh.cmd.Stderr = sh.stderr
	sh.cmd.WaitDelay = waitDelay
	if err = sh.start(); err != nil {
		sh.closeFiles()
		return nil, err
	}
	go sh.reap()
	return sh, nil
}

// start launches the command. The output lock is held until the terminal
// header is written so that the header precedes any output.
func (sh *runningShell) start() error {
	sh.out.mu.Lock()
	err := sh.cmd.Start()
	if err == nil && sh.file != nil {
		err = writeTerminalHeader(sh.file, sh.cwd, sh.args.Command, sh.cmd.Process.Pid)
	}
	sh.out.mu.Unlock()
	if err != nil && sh.cmd.Process != nil {
		kill(sh.cmd)
		wait(sh.cmd)
	}
	return err
}

func (sh *runningShell) closeFiles() {
	if sh.file != nil {
		closeLogged(sh.file)
	}
	if sh.snapshot != nil {
		closeLogged(sh.snapshot)
	}
}

// reap waits for the process and finalizes its output.
func (sh *runningShell) reap() {
	wait(sh.cmd)
	sh.stdout.flush()
	sh.stderr.flush()
	if sh.file != nil {
		if err := appendTerminalExit(sh.file, sh.cmd.ProcessState); err != nil {
			log.Printf("terminal %d: %v", sh.shellID, err)
		}
		closeLogged(sh.file)
	}
	close(sh.done)
}

func (sh *runningShell) wait(ctx context.Context) error {
	soft, stopSoft := timer(sh.args.Timeout)
	defer stopSoft()
	hard, stopHard := timer(sh.args.GetHardTimeout())
	defer stopHard()
	reason := agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
	select {
	case <-sh.done:
	case <-ctx.Done():
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT
	case <-hard:
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT
	case <-soft:
		if sh.canBackground() {
			stopHard()
			return sh.detach()
		}
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT
	}
	return sh.finish(reason)
}

// detach leaves the command running in the background, still enforcing the
// hard timeout, and tells the agent where its output goes.
func (sh *runningShell) detach() error {
	sh.out.mu.Lock()
	sh.out.detached = true
	sh.out.mu.Unlock()
	if hard := sh.args.GetHardTimeout(); hard > 0 {
		remaining := time.Duration(hard-sh.args.Timeout) * time.Millisecond
		go func() {
			select {
			case <-sh.done:
			case <-time.After(max(remaining, time.Millisecond)):
				kill(sh.cmd)
			}
		}()
	}
	return sh.out.send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{
		Backgrounded: &agentv1.ShellStreamBackgrounded{
			ShellId:          sh.shellID,
			Command:          sh.args.Command,
			WorkingDirectory: sh.cwd,
			Pid:              pid(sh.cmd),
			Reason:           agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
		},
	}}))
}

// finish kills the command if it was aborted and reports its exit together
// with the directory it ended in.
func (sh *runningShell) finish(reason agentv1.ShellAbortReason) error {
	aborted := reason != agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
	if aborted {
		kill(sh.cmd)
		<-sh.done
	}
	if err := sh.out.sendErr(); err != nil {
		return err
	}
	cwd := sh.cwd
	if !aborted {
		if final := snapshotCwd(sh.snapshot); final != "" {
			cwd = final
		}
	}
	code := max(0, exitCode(sh.cmd.ProcessState))
	return sh.out.send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{
		Code:        uint32(code), //nolint:gosec // non-negative
		Cwd:         cwd,
		Aborted:     aborted,
		AbortReason: &reason,
	}}}))
}

// snapshotCwd returns the cwd recorded in a state snapshot, or "".
func snapshotCwd(f *os.File) string {
	b := make([]byte, 4096)
	n, err := f.ReadAt(b, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	cwd, _, found := bytes.Cut(b[:n], []byte{0})
	if !found {
		return ""
	}
	return string(cwd)
}

// restoreShell adopts the state a foreground command left in its snapshot.
// Commands killed before their exit trap ran leave no state.
func (e *Executor) restoreShell(f *os.File) {
	b, err := readSnapshot(f)
	if err != nil {
		log.Printf("read shell state: %v", err)
		return
	}
	st, ok := parseSnapshot(b)
	if !ok {
		return
	}
	rel, err := e.relative(st.cwd)
	if err != nil {
		return
	}
	if info, err := e.Files.Stat(rel); err != nil || !info.IsDir() {
		return
	}
	e.setShellState(st)
}

func readSnapshot(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSnapshot {
		return nil, errors.New("shell state exceeds 1 MiB")
	}
	b := make([]byte, info.Size())
	if _, err := f.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b, nil
}

// parseSnapshot decodes the state written by saveStateTrap.
func parseSnapshot(b []byte) (shellState, bool) {
	parts := strings.Split(string(b), "\x00")
	if len(parts) < 2 {
		return shellState{}, false
	}
	st := shellState{cwd: parts[0]}
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
		if len(sections) == 0 && strings.Contains(part, "=") {
			st.env = append(st.env, part)
		}
	}
	st.options = parseOptions(sections[sectionOptions])
	st.aliases = sections[sectionAliases]
	st.functions = sections[sectionFunctions]
	return st, true
}

// parseOptions keeps only well-formed "set ±o name" and "shopt -s/-u name"
// lines, since they are evaluated by the next command.
func parseOptions(text string) []string {
	var options []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !isWord(fields[2]) {
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

func isWord(s string) bool {
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
