package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/config"
	"github.com/exa-pub/grokbot-tunnel/internal/service"
	"github.com/exa-pub/grokbot-tunnel/internal/update"
)

// UpdateOptions configure Update.
type UpdateOptions struct {
	Target    string // "latest" or a release
	Check     bool   // only report the versions
	Force     bool   // reinstall, downgrade or replace a development build
	NoRestart bool   // leave the service alone
	// Auto marks a run by the update timer: what would need the user's
	// decision is skipped with a message instead of failing the unit.
	Auto     bool
	Version  string // version of this binary
	Packager string // package manager that installed this build, if any
	// Client fetches releases; nil means GitHub.
	Client *update.Client
	// Exe is the binary to replace; empty means this one.
	Exe string
}

// Update replaces this binary with a release and restarts the service.
func Update(ctx context.Context, opts UpdateOptions, out io.Writer) error {
	target, err := update.Normalize(opts.Target)
	if err != nil {
		return err
	}
	if opts.Packager != "" {
		return opts.skip(out, fmt.Errorf("grokbot-tunnel was installed by %s; update it the same way", opts.Packager))
	}
	store, err := config.Default()
	if err != nil {
		return err
	}
	release, err := store.Claim("update", config.Holder{PID: os.Getpid(), Version: opts.Version, StartedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	defer release()
	client := opts.Client
	if client == nil {
		client = &update.Client{}
	}
	client.UserAgent = "grokbot-tunnel/" + opts.Version
	if target == "latest" {
		if target, err = client.Latest(ctx); err != nil {
			return err
		}
	}
	if done, err := opts.decide(target, out); done || err != nil {
		return err
	}
	exe := opts.Exe
	if exe == "" {
		if exe, err = update.Executable(); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Downloading grokbot-tunnel %s\n", target)
	if err = replaceWith(ctx, client, exe, target); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated grokbot-tunnel %s -> %s (%s)\nRelease notes: %s/releases/tag/%s\n",
		opts.Version, target, exe, update.Repo, target)
	if opts.NoRestart {
		return nil
	}
	return restartUpdated(ctx, store, exe, target, out)
}

// replaceWith replaces exe with release target.
func replaceWith(ctx context.Context, client *update.Client, exe, target string) error {
	bin, err := client.Fetch(ctx, target)
	if err != nil {
		return err
	}
	err = update.Replace(ctx, exe, bin, target)
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w\n\nYou cannot write to %s. Reinstall grokbot-tunnel into your home directory with install.sh, "+
			"or run update as the owner of %s", err, filepath.Dir(exe), exe)
	}
	return err
}

// skip ends a run that needs the user's decision: an error for the user, a
// message for the timer.
func (o UpdateOptions) skip(out io.Writer, err error) error {
	if !o.Auto {
		return err
	}
	fmt.Fprintf(out, "Not updating: %v\n", err)
	return nil
}

// decide reports whether Update is done before downloading target.
func (o UpdateOptions) decide(target string, out io.Writer) (done bool, err error) {
	release := update.IsRelease(o.Version)
	if o.Check {
		status := "an update is available"
		switch {
		case !release:
			status = "this is a development build"
		case update.Compare(target, o.Version) == 0:
			status = "up to date"
		case update.Compare(target, o.Version) < 0:
			status = "older than the installed version"
		}
		fmt.Fprintf(out, "Installed: %s\nAvailable: %s (%s)\n", o.Version, target, status)
		return true, nil
	}
	switch {
	case o.Force:
		return false, nil
	case !release:
		return true, o.skip(out, fmt.Errorf("this is a development build (%s); use --force to replace it with %s", o.Version, target))
	case update.Compare(target, o.Version) == 0:
		fmt.Fprintf(out, "grokbot-tunnel %s is up to date\n", o.Version)
		return true, nil
	case update.Compare(target, o.Version) < 0:
		return true, o.skip(out, fmt.Errorf("%s is older than the installed %s; use --force to downgrade", target, o.Version))
	}
	return false, nil
}

// restartUpdated restarts the service if it runs the updated binary, or
// tells the user about a connect that still runs the old one.
func restartUpdated(ctx context.Context, store *config.Store, exe, target string, out io.Writer) error {
	if service.UnderSudo() {
		fmt.Fprintln(out, "Restart grokbot-tunnel as its user to use the new version.")
		return nil
	}
	if service.Booted() == nil {
		if scope, err := service.DetectScope(); err == nil {
			if m := service.New(scope, out); m.Active(ctx) {
				return restartService(ctx, m, exe)
			}
		}
	}
	if h, held, err := store.Probe("connect"); err == nil && held {
		fmt.Fprintf(out, "grokbot-tunnel connect (pid %d) still runs %s; restart it to use %s\n", h.PID, h.Version, target)
	}
	return nil
}

func restartService(ctx context.Context, m *service.Manager, exe string) error {
	path, err := m.ExecPath(ctx)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if path != exe {
		fmt.Fprintf(m.Out, "Warning: the service runs %s, not %s, so it was not restarted\n", path, exe)
		return nil
	}
	// Print first: if the agent runs this update inside the service, the
	// restart ends this process too.
	fmt.Fprintln(m.Out, "Restarting the service")
	return m.Restart(ctx)
}
