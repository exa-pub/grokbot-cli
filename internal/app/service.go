package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
	"github.com/exa-pub/grokbot-tunnel/internal/permission"
	"github.com/exa-pub/grokbot-tunnel/internal/service"
)

// InstallOptions configure Install.
type InstallOptions struct {
	// Connect holds the connect flags for the service. ClientVersion is set
	// only if the user chose one.
	Connect    ConnectOptions
	AutoUpdate bool   // install the daily update timer
	Version    string // version of this binary
}

// errPermissionRequired explains the choice instead of defaulting to "ask",
// which refuses nearly everything without a terminal, or to "always".
var errPermissionRequired = errors.New("--permission is required: " +
	"\"always\" runs every action the agent requests; " +
	"\"ask\" runs only actions approved in Grok Bot, since a service has no terminal to ask in; " +
	"\"never\" refuses all actions")

// serviceEnv lists the variables the service keeps from the environment of
// install: the user manager starts services with a minimal PATH, and the
// others select the server.
var serviceEnv = []string{
	"PATH", "SHELL",
	"SAND_BACKEND_URL", "SAND_CURSOR_WEBSITE_URL", "SAND_CLIENT_APP_VERSION",
	"CURSOR_API_BASE_URL", "CURSOR_WEBSITE_URL",
}

// Install runs connect as a systemd service that starts at boot.
func Install(ctx context.Context, opts InstallOptions, out io.Writer) error {
	args, err := opts.connectArgs()
	if err != nil {
		return err
	}
	if service.UnderSudo() {
		return service.ErrSudo
	}
	m, err := serviceManager(ctx, out)
	if err != nil {
		return err
	}
	store, err := config.Default()
	if err != nil {
		return err
	}
	if err = checkInstallable(store); err != nil {
		return err
	}
	exe, err := serviceExecutable()
	if err != nil {
		return err
	}
	c := service.Config{Exe: exe, Args: args, Version: opts.Version, AutoUpdate: opts.AutoUpdate}
	for _, k := range serviceEnv {
		if v := os.Getenv(k); v != "" {
			c.Env = append(c.Env, k+"="+v)
			fmt.Fprintf(out, "The service runs with %s=%s\n", k, v)
		}
	}
	if opts.Connect.Permission == permission.ModeAsk {
		fmt.Fprintln(out, "Note: with --permission ask the service has no terminal to ask in, "+
			"so it runs only actions approved in Grok Bot and refuses the rest.")
	}
	if err = m.Install(ctx, c); err != nil {
		return err
	}
	flag := m.Scope.Flag()
	fmt.Fprintf(out, "\nThe service is connected and starts at boot.\n"+
		"    systemctl %sstatus %s\n    journalctl %s-u %s -f\n    grokbot-tunnel uninstall\n",
		flag, service.Name, flag, service.Name)
	if opts.AutoUpdate {
		fmt.Fprintln(out, "It updates grokbot-tunnel daily; re-run install with --no-auto-update to turn that off.")
	}
	return nil
}

// connectArgs validates the connect flags and renders them for ExecStart.
func (o InstallOptions) connectArgs() ([]string, error) {
	c := o.Connect
	if c.Permission == "" {
		return nil, errPermissionRequired
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	args := []string{"--permission", c.Permission}
	if c.Root != "" {
		root, err := filepath.Abs(c.Root)
		if err != nil {
			return nil, err
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("--root %s is not a directory", root)
		}
		args = append(args, "--root", root)
	}
	if c.Label != "" {
		args = append(args, "--label", c.Label)
	}
	if c.Debug {
		args = append(args, "--debug")
	}
	if c.ClientVersion != "" {
		args = append(args, "--client-version", c.ClientVersion)
	}
	return args, nil
}

// serviceManager returns the manager for the current user's scope after
// checking that systemd is there to talk to.
func serviceManager(ctx context.Context, out io.Writer) (*service.Manager, error) {
	if err := service.Booted(); err != nil {
		return nil, err
	}
	scope, err := service.DetectScope()
	if err != nil {
		return nil, err
	}
	m := service.New(scope, out)
	return m, m.Reachable(ctx)
}

// checkInstallable fails if the service could not connect: without a login,
// or while a connect outside the service holds the instance lock.
func checkInstallable(store *config.Store) error {
	if _, err := loadIdentity(store); errors.Is(err, auth.ErrNotLoggedIn) {
		return errors.New("not logged in; run \"grokbot-tunnel login\" first")
	} else if err != nil {
		return err
	}
	h, held, err := store.Probe("connect")
	if err != nil || !held {
		return err
	}
	if _, ok := service.Owner(h.PID); ok {
		return nil // the service itself; installing restarts it
	}
	return fmt.Errorf("grokbot-tunnel connect is already running (pid %d); stop it first, since only one can run per user", h.PID)
}

// serviceExecutable returns the binary the service runs, refusing temporary
// builds that would disappear.
func serviceExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	if strings.HasPrefix(exe, tmp) || strings.Contains(exe, "/go-build") {
		return "", fmt.Errorf("%s is a temporary build; install grokbot-tunnel and run install from the installed binary", exe)
	}
	return exe, nil
}

// Uninstall stops and removes the service. The binary and the login stay.
func Uninstall(ctx context.Context, out io.Writer) error {
	m, err := serviceManager(ctx, out)
	if err != nil {
		return err
	}
	removed, err := m.Uninstall(ctx)
	if err != nil {
		return err
	}
	if !removed {
		if _, err := os.Stat(filepath.Join(service.SystemDir, service.TunnelUnit)); err == nil && m.Scope.User {
			fmt.Fprintln(out, "The service is installed for root; run \"grokbot-tunnel uninstall\" as root to remove it.")
			return nil
		}
		fmt.Fprintln(out, "The service is not installed.")
		return nil
	}
	fmt.Fprintln(out, "Uninstalled the service. The grokbot-tunnel binary and your login in ~/.grok/bot-tunnel were kept.")
	if m.Scope.User {
		fmt.Fprintln(out, "Lingering stays enabled for other user services; turn it off with: loginctl disable-linger")
	}
	return nil
}
