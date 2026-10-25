package executor

import (
	"log"
	"os"
	"os/exec"
	"syscall"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
)

// background starts cmd in its own session with output going to a terminal
// file. It replays the foreground state but neither waits for running
// foreground commands nor changes the state. Descriptors are those of a
// foreground shell, except that 4 is a gate opened once the terminal header is
// written; the footer is appended when the process exits.
func (e *Executor) background(cmd, path string) (_ *agentv1.BackgroundShellSpawnSuccess, err error) {
	e.initShell()
	st := e.shellState()
	cwd, _, err := e.shellCwd(path, st)
	if err != nil {
		return nil, err
	}
	n, f, err := e.terminal()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			closeLogged(f)
		}
	}()
	gate, release, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	defer release.Close()
	replay, command, err := inputFiles(st, cmd)
	if err != nil {
		return nil, err
	}
	defer closeLogged(replay) // the child keeps its own descriptors
	defer closeLogged(command)
	sh := userShell()
	c := shellCommand(sh, st, cwd, sh.args("-c", sh.backgroundScript(), "grok-background")...)
	c.ExtraFiles = []*os.File{replay, gate, command}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdout = f
	c.Stderr = f
	if err = c.Start(); err != nil {
		return nil, err
	}
	if err = writeTerminalHeader(f, cwd, cmd, c.Process.Pid); err == nil {
		_, err = release.WriteString("\n")
	}
	if err != nil {
		kill(c)
		wait(c)
		return nil, err
	}
	go closeTerminal(n, f, c)
	return &agentv1.BackgroundShellSpawnSuccess{ShellId: n, Command: cmd, WorkingDirectory: cwd, Pid: pid(c)}, nil
}

// closeTerminal appends the footer to terminal file n once c has exited.
func closeTerminal(n uint32, f *os.File, c *exec.Cmd) {
	wait(c)
	if err := appendTerminalExit(f, c.ProcessState); err != nil {
		log.Printf("terminal %d: %v", n, err)
	}
	closeLogged(f)
}
