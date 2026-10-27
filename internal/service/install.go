package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Install writes the units for c, enables them and restarts the tunnel. The
// restart waits until the tunnel reports that it is connected, so an error
// means the service does not work; it stays installed and keeps retrying.
func (m *Manager) Install(ctx context.Context, c Config) error {
	units, err := Units(m.Scope, c)
	if err != nil {
		return err
	}
	if !c.AutoUpdate {
		if err = m.removeTimer(ctx); err != nil {
			return err
		}
	}
	for _, name := range []string{TunnelUnit, UpdateUnit, UpdateTimer} {
		if content, ok := units[name]; ok {
			if err = m.writeUnit(name, content); err != nil {
				return err
			}
		}
	}
	if _, err = m.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err = m.systemctl(ctx, "enable", TunnelUnit); err != nil {
		return err
	}
	if c.AutoUpdate {
		if _, err = m.systemctl(ctx, "enable", "--now", UpdateTimer); err != nil {
			return err
		}
	}
	if m.Scope.User {
		m.enableLinger(ctx)
	}
	fmt.Fprintln(m.Out, "Starting the service...")
	if _, err = m.systemctl(ctx, "restart", TunnelUnit); err != nil {
		if journal := m.Journal(ctx); journal != "" {
			err = fmt.Errorf("%w\n\nService log:\n%s", err, journal)
		}
		return fmt.Errorf("the service did not start: %w", err)
	}
	return nil
}

// removeTimer removes update units left by an install with auto-update.
func (m *Manager) removeTimer(ctx context.Context) error {
	timer := filepath.Join(m.Scope.Dir, UpdateTimer)
	if _, err := os.Stat(timer); err == nil {
		if _, err = m.systemctl(ctx, "disable", "--now", UpdateTimer); err != nil {
			return err
		}
	}
	for _, name := range []string{UpdateTimer, UpdateUnit} {
		err := os.Remove(filepath.Join(m.Scope.Dir, name))
		if err == nil {
			fmt.Fprintf(m.Out, "Removed %s\n", filepath.Join(m.Scope.Dir, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writeUnit atomically replaces the unit file name with content unless it
// already has that content.
func (m *Manager) writeUnit(name, content string) error {
	path := filepath.Join(m.Scope.Dir, name)
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, []byte(content)):
		fmt.Fprintf(m.Out, "%s is up to date\n", path)
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(m.Scope.Dir, 0o755); err != nil { //nolint:gosec // unit directories are world-readable
		return err
	}
	f, err := os.CreateTemp(m.Scope.Dir, ".tmp-"+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(content); err == nil {
		err = f.Chmod(0o644)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if old == nil {
		fmt.Fprintf(m.Out, "Installed %s\n", path)
	} else {
		fmt.Fprintf(m.Out, "Updated %s\n", path)
	}
	return nil
}

// Uninstall disables, removes and stops the units that exist. It reports
// whether there were any. Stopping comes last: run from inside the service,
// e.g. by the agent, it ends this process too.
func (m *Manager) Uninstall(ctx context.Context) (bool, error) {
	found, err := m.Installed()
	if err != nil || len(found) == 0 {
		return false, err
	}
	var units, running []string
	for _, name := range found {
		// The update service has no [Install] section; its timer starts it.
		if name != UpdateUnit {
			units = append(units, name)
		}
		if m.active(ctx, name) {
			running = append(running, name)
		}
	}
	if len(units) > 0 {
		if _, err = m.systemctl(ctx, append([]string{"disable"}, units...)...); err != nil {
			return true, err
		}
	}
	for _, name := range found {
		path := filepath.Join(m.Scope.Dir, name)
		if err = os.Remove(path); err != nil {
			return true, err
		}
		fmt.Fprintf(m.Out, "Removed %s\n", path)
	}
	if _, err = m.systemctl(ctx, "daemon-reload"); err != nil {
		return true, err
	}
	// Units removed while running stay loaded until they stop; stopping
	// the others would fail, since they are gone.
	if len(running) > 0 {
		if _, err = m.systemctl(ctx, append([]string{"stop"}, running...)...); err != nil {
			return true, err
		}
	}
	// Forget a failed state, so the unit does not linger in systemctl --failed.
	_, _ = m.systemctl(ctx, "reset-failed", TunnelUnit) //nolint:errcheck // fails if the unit never failed
	return true, nil
}
