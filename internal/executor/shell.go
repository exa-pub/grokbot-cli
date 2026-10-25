package executor

import (
	"context"
	"log"
	"os"
	"os/exec"
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
	e.initShell()
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
	}()
	return err
}

// runningShell is a started foreground command.
type runningShell struct {
	e        *Executor
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
	final    *shellState   // the state the command left, once it has exited
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
	replay, command, err := inputFiles(st, x.Command)
	if err != nil {
		return nil, err
	}
	defer closeLogged(replay) // the child keeps its own descriptors
	defer closeLogged(command)
	user := userShell()
	sh.cmd = shellCommand(user, st, cwd, user.args("-c", user.foregroundScript(), "grokbot-shell")...)
	sh.cmd.ExtraFiles = []*os.File{replay, sh.snapshot, command}
	sh.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sh.stdout = &streamWriter{out: sh.out, event: stdoutEvent}
	sh.stderr = &streamWriter{out: sh.out, event: stderrEvent}
	sh.cmd.Stdout = sh.stdout
	sh.cmd.Stderr = sh.stderr
	sh.cmd.WaitDelay = waitDelay
	if err = sh.start(); err != nil {
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
		if st, ok := sh.e.adoptState(sh.snapshot, sh.cwd); ok {
			sh.final, cwd = &st, st.cwd
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
