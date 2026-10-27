// Package service installs grokbot-tunnel as a systemd service and talks to
// the service manager.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// systemdDir exists if systemd is the init system, as sd_booted(3) checks.
var systemdDir = "/run/systemd/system"

// SystemDir holds the units of the system manager installed by root.
const SystemDir = "/etc/systemd/system"

// ErrNoSystemd means the host does not run systemd, e.g. in a container.
var ErrNoSystemd = errors.New("systemd is not running on this host; " +
	"run \"grokbot-tunnel connect\" directly or under your container's supervisor")

// ErrSudo means a user ran the command with sudo, which would install the
// service for root instead of them.
var ErrSudo = errors.New("do not run this command with sudo: " +
	"grokbot-tunnel runs as your user, with the login stored in your home directory; " +
	"re-run it without sudo, or as root itself to install it for root")

// Booted reports whether systemd manages this host.
func Booted() error {
	if _, err := os.Stat(systemdDir); err != nil {
		return ErrNoSystemd
	}
	return nil
}

// UnderSudo reports whether a user other than root runs this process as root
// through sudo.
func UnderSudo() bool {
	u := os.Getenv("SUDO_USER")
	return os.Geteuid() == 0 && u != "" && u != "root"
}

// Scope is the service manager the service belongs to: root installs a system
// unit, other users a unit of their own user manager.
type Scope struct {
	User bool   // per-user manager, controlled with systemctl --user
	Dir  string // directory of the unit files
}

// DetectScope returns the scope for the current user.
func DetectScope() (Scope, error) {
	if os.Geteuid() == 0 {
		return Scope{Dir: SystemDir}, nil
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		home, err := os.UserHomeDir()
		if err != nil {
			return Scope{}, err
		}
		config = filepath.Join(home, ".config")
	}
	return Scope{User: true, Dir: filepath.Join(config, "systemd", "user")}, nil
}

// Flag returns the systemctl and journalctl flag selecting the scope, with a
// trailing space, or nothing for the system manager. It is meant for hints.
func (s Scope) Flag() string {
	if s.User {
		return "--user "
	}
	return ""
}

// Runner runs a command and returns its output. Errors include the output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec runs commands on this host.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // systemctl, journalctl and loginctl with our arguments
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// Manager controls the service in one scope.
type Manager struct {
	Scope Scope
	Run   Runner
	Out   io.Writer // progress and hints
}

// New returns a manager for scope that runs commands on this host.
func New(scope Scope, out io.Writer) *Manager {
	return &Manager{Scope: scope, Run: Exec, Out: out}
}

func (m *Manager) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	if m.Scope.User {
		args = append([]string{"--user"}, args...)
	}
	return m.Run(ctx, "systemctl", args...)
}

// Reachable checks that systemctl can talk to the service manager. The user
// manager is out of reach e.g. after su, which keeps the old user's session.
func (m *Manager) Reachable(ctx context.Context) error {
	if !m.Scope.User {
		return nil
	}
	if _, err := m.systemctl(ctx, "show-environment"); err != nil {
		return fmt.Errorf("%w\n\nThe systemd user manager is not reachable. Log in as this user directly (e.g. over SSH) "+
			"instead of with su, or run: export XDG_RUNTIME_DIR=/run/user/%d", err, os.Getuid())
	}
	return nil
}

// Installed reports which of our units exist in the scope.
func (m *Manager) Installed() ([]string, error) {
	var found []string
	for _, name := range []string{TunnelUnit, UpdateUnit, UpdateTimer} {
		_, err := os.Stat(filepath.Join(m.Scope.Dir, name))
		switch {
		case err == nil:
			found = append(found, name)
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	return found, nil
}

// Active reports whether the tunnel service is running or starting.
func (m *Manager) Active(ctx context.Context) bool {
	out, _ := m.systemctl(ctx, "is-active", TunnelUnit) //nolint:errcheck // inactive units exit non-zero
	state := strings.TrimSpace(string(out))
	return state == "active" || state == "activating" || state == "reloading"
}

// ExecPath returns the binary the tunnel service runs.
func (m *Manager) ExecPath(ctx context.Context) (string, error) {
	out, err := m.systemctl(ctx, "show", "--property=ExecStart", "--value", TunnelUnit)
	if err != nil {
		return "", err
	}
	// The value looks like "{ path=/usr/bin/x ; argv[]=/usr/bin/x a b ; ... }".
	_, rest, ok := strings.Cut(string(out), "path=")
	if !ok {
		return "", fmt.Errorf("unexpected ExecStart %q", strings.TrimSpace(string(out)))
	}
	path, _, _ := strings.Cut(rest, " ;")
	return strings.TrimSpace(path), nil
}

// Restart restarts the tunnel service without waiting for it, so that it
// works even from a process inside the service, which the restart kills.
func (m *Manager) Restart(ctx context.Context) error {
	_, err := m.systemctl(ctx, "restart", "--no-block", TunnelUnit)
	return err
}

// Journal returns the last lines the tunnel service logged, or nothing if
// the journal cannot be read.
func (m *Manager) Journal(ctx context.Context) string {
	args := []string{"-u", TunnelUnit, "-n", "20", "--no-pager", "-o", "cat"}
	if m.Scope.User {
		args = append([]string{"--user"}, args...)
	}
	out, err := m.Run(ctx, "journalctl", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// enableLinger lets the user manager run without a login session, so the
// service starts at boot and survives logging out.
func (m *Manager) enableLinger(ctx context.Context) {
	u, err := user.Current()
	if err != nil {
		fmt.Fprintf(m.Out, "Warning: cannot determine the current user: %v\n", err)
		return
	}
	out, err := m.Run(ctx, "loginctl", "show-user", u.Username, "--property=Linger", "--value")
	if err == nil && strings.TrimSpace(string(out)) == "yes" {
		return
	}
	if _, err = m.Run(ctx, "loginctl", "enable-linger"); err != nil {
		fmt.Fprintf(m.Out, "Warning: %v\n"+
			"The service stops when you log out and does not start at boot until you run:\n"+
			"    sudo loginctl enable-linger %s\n", err, u.Username)
		return
	}
	fmt.Fprintln(m.Out, "Enabled lingering, so the service runs without a login session")
}
