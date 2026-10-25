package executor

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
)

// shellCwd resolves the working directory, defaulting to the one the previous
// foreground command ended in.
func (e *Executor) shellCwd(path string, st shellState) (dir, warning string, err error) {
	if path == "" && st.cwd != "" {
		path = st.cwd
	}
	return e.cwd(path)
}

// shell runs a foreground command in its own process group and streams its
// output. Foreground commands run one at a time and share cwd, environment,
// options and functions. On soft timeout the command is either killed or, with
// TIMEOUT_BEHAVIOR_BACKGROUND, left running with output going to a terminal
// file.
func (e *Executor) shell(ctx context.Context, x *agentv1.ShellArgs, send reply) error {
	if x.IsBackground {
		v, err := e.background(ctx, x.Command, x.WorkingDirectory)
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
	if err := e.initShell(ctx); err != nil {
		return err
	}
	st := e.shellState()
	cwd, warning, err := e.shellCwd(x.WorkingDirectory, st)
	if err != nil {
		return err
	}
	if warning != "" {
		if err = send(shellMessage(stderrEvent(warning))); err != nil {
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
	if sh.final != nil {
		e.setShellState(*sh.final)
	}
	// A backgrounded command's state is discarded: the next command starts
	// from the current state.
	go func() {
		<-sh.done
		closeLogged(sh.snapshot)
		closeLogged(sh.fallback)
	}()
	return err
}

// runningShell is a started foreground command.
//
// Its stdout and stderr are pipes read by drainers that outlive the shell:
// background jobs the command started may keep writing to them, and closing
// the pipes would kill those jobs with SIGPIPE. Once the shell has exited and
// the drainers have hit EOF or waitDelay has passed, the output is finished:
// nothing more reaches the agent or the terminal file, but the drainers keep
// reading until the last writer is gone.
type runningShell struct {
	e        *Executor
	args     *agentv1.ShellArgs
	cwd      string
	cmd      *exec.Cmd
	out      *shellOutput
	stdout   *streamWriter
	stderr   *streamWriter
	drained  sync.WaitGroup // the drainers have hit EOF
	done     chan struct{}  // closed once the process has exited and its output is finished
	shellID  uint32         // terminal file number, if the command may be backgrounded
	file     *os.File       // terminal file, if the command may be backgrounded
	statusAt int64          // offset of the status in the terminal file
	snapshot *os.File       // receives the state on exit
	fallback *os.File       // receives the state from the EXIT trap; linked until the shell exits
	final    *shellState    // the state the command left, once it has exited
}

func (sh *runningShell) canBackground() bool {
	return sh.args.TimeoutBehavior == agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND
}

func (e *Executor) startShell(x *agentv1.ShellArgs, cwd string, st shellState, send reply) (_ *runningShell, err error) {
	sh := &runningShell{e: e, args: x, cwd: cwd, out: &shellOutput{send: send}, done: make(chan struct{})}
	defer func() {
		if err != nil {
			sh.closeFiles()
		}
	}()
	if sh.canBackground() {
		if sh.shellID, sh.file, err = e.terminal(); err != nil {
			return nil, err
		}
		sh.out.file = sh.file
	}
	if sh.snapshot, err = tempFile("grok-shell-state-", ""); err != nil {
		return nil, err
	}
	if sh.fallback, err = os.CreateTemp("", "grok-shell-state-"); err != nil {
		return nil, err
	}
	replay, command, err := inputFiles(st, x.Command)
	if err != nil {
		return nil, err
	}
	defer closeLogged(replay) // the child keeps its own descriptors
	defer closeLogged(command)
	user := userShell()
	sh.cmd = shellCommand(user, st, cwd, user.args("-c", user.foregroundScript(), "grokbot-shell", sh.fallback.Name())...)
	sh.cmd.ExtraFiles = []*os.File{replay, sh.snapshot, command}
	// A new session detaches the command from the CLI's controlling terminal,
	// as the desktop does: sudo, ssh or git fail instead of stopping on
	// SIGTTIN or writing over a permission prompt.
	sh.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	sh.stdout = &streamWriter{out: sh.out, event: stdoutEvent}
	sh.stderr = &streamWriter{out: sh.out, event: stderrEvent}
	if err = sh.start(); err != nil {
		return nil, err
	}
	go sh.reap()
	return sh, nil
}

// start launches the command with its output pipes and their drainers. The
// output lock is held until the terminal header is written so that the header
// precedes any output.
func (sh *runningShell) start() error {
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		closeLogged(stdout)
		closeLogged(stdoutW)
		return err
	}
	sh.cmd.Stdout, sh.cmd.Stderr = stdoutW, stderrW
	sh.out.mu.Lock()
	err = sh.cmd.Start()
	// Only the command keeps the write ends, so the drainers see EOF once it
	// and its children are gone.
	closeLogged(stdoutW)
	closeLogged(stderrW)
	if err == nil && sh.file != nil {
		sh.statusAt, err = writeTerminalHeader(sh.file, sh.cwd, sh.args.Command, sh.cmd.Process.Pid, "")
	}
	sh.out.mu.Unlock()
	if err != nil && sh.cmd.Process != nil {
		kill(sh.cmd)
		wait(sh.cmd)
	}
	if err != nil {
		closeLogged(stdout)
		closeLogged(stderr)
		return err
	}
	sh.drained.Add(2)
	go sh.drain(stdout, sh.stdout)
	go sh.drain(stderr, sh.stderr)
	return nil
}

// drain copies r to w until EOF; w discards what arrives after the output is
// finished.
func (sh *runningShell) drain(r *os.File, w *streamWriter) {
	defer sh.drained.Done()
	defer closeLogged(r)
	if _, err := io.Copy(w, r); err != nil {
		log.Printf("shell output: %v", err)
	}
}

func (sh *runningShell) closeFiles() {
	if sh.file != nil {
		closeLogged(sh.file)
	}
	if sh.snapshot != nil {
		closeLogged(sh.snapshot)
	}
	if sh.fallback != nil {
		removeLogged(sh.fallback.Name())
		closeLogged(sh.fallback)
	}
}

// reap waits for the process and its output and finalizes the output.
func (sh *runningShell) reap() {
	wait(sh.cmd)
	removeLogged(sh.fallback.Name())
	drained := make(chan struct{})
	go func() {
		sh.drained.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(waitDelay):
	}
	sh.stdout.flush()
	sh.stderr.flush()
	sh.out.finish()
	if sh.file != nil {
		if err := appendTerminalExit(sh.file, sh.cmd.ProcessState, sh.statusAt); err != nil {
			log.Printf("terminal %d: %v", sh.shellID, err)
		}
		closeLogged(sh.file)
	}
	close(sh.done)
}

// exited reports whether the command has exited and its output is finished.
func (sh *runningShell) exited() bool {
	select {
	case <-sh.done:
		return true
	default:
		return false
	}
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
		if sh.canBackground() && !sh.exited() {
			stopHard()
			return sh.detach()
		}
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT
	}
	// select picks at random when several are ready: a command that has
	// already finished is reported as such.
	if sh.exited() {
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
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
// with the directory the next command starts in. An aborted command's state is
// discarded.
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
		st, notice := sh.e.adoptState(sh.snapshot, sh.fallback, sh.cwd)
		if st != nil {
			sh.final, cwd = st, st.cwd
		}
		if notice != "" {
			if err := sh.out.send(shellMessage(stderrEvent(notice))); err != nil {
				return err
			}
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
