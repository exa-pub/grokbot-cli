// Package permission decides whether a local action may run, prompting the
// user on the terminal when needed.
package permission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Modes accepted by Policy.Mode.
const (
	ModeAsk    = "ask"
	ModeAlways = "always"
	ModeNever  = "never"
)

// The error texts are shown to the agent as complete sentences explaining what
// the user can do, hence the capitalization and punctuation.
//
//nolint:revive,staticcheck // user-facing sentences
var (
	ErrRefused = errors.New("That action was not approved on the user's computer, so nothing ran. " +
		"Ask the user to approve it (or to set Settings → Bot → Execution on Local Computer to \"Always allow\"), " +
		"and use your own computer (Shell, Read, AwaitShell) in the meantime.")
	ErrDisabled = errors.New("Local tools are turned off. " +
		"The user has set local tool access to \"Never\", so Shell, Read, and AwaitShell with machineId, " +
		"plus CopyToBox and CopyFromBox, cannot run on their computer. " +
		"Do not retry them while this setting remains \"Never\". " +
		"Use your own computer instead by omitting machineId, " +
		"or ask the user to change the setting in Settings → Bot → Execution on Local Computer. " +
		"If they change it away from \"Never\", you may try again.")
)

const maxAnswer = 4096

// Policy holds the standing permission mode and the terminal used for prompts.
type Policy struct {
	mu     sync.Mutex    // guards Mode and prompt
	prompt chan struct{} // one-slot semaphore serializing terminal prompts
	Mode   string
	In     *os.File
	Out    io.Writer
}

// Standing returns the current permission mode.
func (p *Policy) Standing() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Mode
}

func (p *Policy) setStanding(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Mode = mode
}

// decide resolves the request without asking the user. ok is false when the
// user has to be prompted.
func (p *Policy) decide(server, approved bool) (ok bool, err error) {
	switch {
	case p.Standing() == ModeNever:
		return true, ErrDisabled
	case server && approved:
		return true, nil
	case server:
		return true, ErrRefused
	case p.Standing() == ModeAlways:
		return true, nil
	}
	return false, nil
}

// Allow returns nil if action may run. When server is set, the server's
// approval flag is authoritative and the user is never prompted.
func (p *Policy) Allow(ctx context.Context, action string, server, approved bool) error {
	if ok, err := p.decide(server, approved); ok {
		return err
	}
	if p.In == nil || !term.IsTerminal(int(p.In.Fd())) {
		return ErrRefused
	}
	// Wait for the terminal without ignoring cancellation, so a queued request
	// canceled by the server does not hold its worker slot.
	slot := p.promptSlot()
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-slot }()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Another prompt may have switched the mode to "always" while we waited.
	if ok, err := p.decide(server, approved); ok {
		return err
	}
	out := p.Out
	if out == nil {
		out = os.Stderr
	}
	// Discard keystrokes typed before the prompt appeared so they cannot
	// answer a question the user has not seen.
	if err := flushInput(p.In); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s? [y/N/a] ", action)
	answer, err := p.readLine(ctx)
	if err != nil {
		fmt.Fprintln(out)
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "a":
		p.setStanding(ModeAlways)
		return nil
	case "y", "yes":
		return nil
	}
	return ErrRefused
}

func (p *Policy) promptSlot() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prompt == nil {
		p.prompt = make(chan struct{}, 1)
	}
	return p.prompt
}

// flushInput discards pending terminal input; it is a no-op for non-terminals.
func flushInput(f *os.File) error {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	if err := unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH); err != nil {
		return fmt.Errorf("discard terminal input: %w", err)
	}
	return nil
}

// readLine polls canonical terminal input so cancellation does not strand a
// blocked reader. It returns the first line without the newline.
func (p *Policy) readLine(ctx context.Context) (string, error) {
	fd := int(p.In.Fd())
	if fd < 0 || fd > math.MaxInt32 {
		return "", fmt.Errorf("invalid terminal descriptor %d", fd)
	}
	var answer []byte
	buf := make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR || n == 0 {
			continue
		}
		if err != nil {
			return "", err
		}
		n, err = unix.Read(fd, buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", ErrRefused
		}
		answer = append(answer, buf[:n]...)
		if line, _, found := strings.Cut(string(answer), "\n"); found {
			return line, nil
		}
		if len(answer) > maxAnswer {
			return "", ErrRefused
		}
	}
}
