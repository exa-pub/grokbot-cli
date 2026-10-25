package executor

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"
)

// shellCommand runs the user's shell with st's environment. Running agent
// commands is the purpose of this tool; their lifetime is managed by killing
// the process group, not through a context.
func shellCommand(sh shell, st shellState, cwd string, args ...string) *exec.Cmd {
	c := exec.Command(sh.path, args...) //nolint:gosec,noctx // see above
	c.Dir = cwd
	c.Env = st.environ()
	return c
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

// inputFiles returns the replay script for st and the command, for
// descriptors 3 and 5 of a shell.
func inputFiles(st shellState, cmd string) (replay, command *os.File, err error) {
	if replay, err = tempFile("grok-shell-replay-", st.replayScript()); err != nil {
		return nil, nil, err
	}
	if command, err = tempFile("grok-command-", cmd); err != nil {
		closeLogged(replay)
		return nil, nil, err
	}
	return replay, command, nil
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
