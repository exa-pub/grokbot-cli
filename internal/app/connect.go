package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/computer"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
	"github.com/exa-pub/grokbot-tunnel/internal/executor"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/permission"
	"github.com/exa-pub/grokbot-tunnel/internal/service"
)

// ConnectOptions configure Connect.
type ConnectOptions struct {
	Root          string // root for file tools and shell cwd; empty means home
	Permission    string // one of the permission.Mode* values
	Label         string // new computer label; empty keeps the saved one
	Debug         bool   // log raw requests
	ClientVersion string
	Version       string // grokbot-tunnel version, recorded in the instance lock
}

// Validate checks the options without touching the system.
func (o ConnectOptions) Validate() error {
	switch o.Permission {
	case permission.ModeAsk, permission.ModeAlways, permission.ModeNever:
		return nil
	}
	return fmt.Errorf("invalid permission %q", o.Permission)
}

// Connect registers this computer and executes the agent's requests until ctx
// is canceled. Only one Connect runs per store, since they would share the
// computer's identity.
func Connect(ctx context.Context, opts ConnectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	notifier := service.NewNotifier()
	if opts.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		opts.Root = home
	}
	e, identity, release, err := openConnect(opts)
	if err != nil {
		return err
	}
	defer release()
	policy := &permission.Policy{Mode: opts.Permission, In: os.Stdin, Out: os.Stderr}
	ex, err := executor.New(opts.Root, policy)
	if err != nil {
		return err
	}
	defer ex.Close()

	u := e.user(identity)
	if err = register(ctx, u.dashboard, e.machine.Label, opts.Permission); err != nil {
		if ctx.Err() != nil {
			return nil // stopped while starting, e.g. by systemctl stop
		}
		return err
	}
	log.Printf("root=%s permission=%s", ex.Root, opts.Permission)
	notify(notifier, "READY=1")
	stopNotify := context.AfterFunc(ctx, func() { notify(notifier, "STOPPING=1") })
	defer stopNotify()
	pv := computer.Provider{
		User:         u.grokbot,
		Channel:      e.computerChannel(),
		Store:        e.store,
		Machine:      e.machine,
		Executor:     ex,
		Policy:       policy,
		Debug:        opts.Debug,
		Backend:      e.base,
		AccountScope: auth.AccountScope(u.tokens.Snapshot()),
	}
	return pv.Run(ctx)
}

// openConnect takes the instance lock and loads what Connect needs. The
// caller releases the lock.
func openConnect(opts ConnectOptions) (*env, config.Auth, func(), error) {
	store, err := config.Default()
	if err != nil {
		return nil, config.Auth{}, nil, err
	}
	// Before anything writes to the store, such as the label below.
	release, err := claimConnect(store, opts.Version)
	if err != nil {
		return nil, config.Auth{}, nil, err
	}
	e, identity, err := loadConnect(opts)
	if err != nil {
		release()
		return nil, identity, nil, err
	}
	return e, identity, release, nil
}

func loadConnect(opts ConnectOptions) (*env, config.Auth, error) {
	e, err := newEnv(opts.ClientVersion)
	if err != nil {
		return nil, config.Auth{}, err
	}
	warnCopiedStore(e)
	identity, err := loadIdentity(e.store)
	if err != nil {
		return nil, identity, err
	}
	if opts.Label != "" {
		e.machine.Label = opts.Label
		if err = e.store.Save("machine", e.machine); err != nil {
			return nil, identity, err
		}
	}
	return e, identity, nil
}

// loadIdentity returns the saved login, or auth.ErrNotLoggedIn without one.
func loadIdentity(store *config.Store) (config.Auth, error) {
	var identity config.Auth
	err := store.Load("auth", &identity)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && identity.RefreshToken == "":
		return identity, auth.ErrNotLoggedIn
	case err != nil:
		return identity, fmt.Errorf("load authentication (run grokbot-tunnel login): %w", err)
	}
	return identity, nil
}

// claimConnect takes the instance lock that keeps a second connect from
// serving the same computer.
func claimConnect(store *config.Store, version string) (func(), error) {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	self := config.Holder{PID: os.Getpid(), Exe: exe, Version: version, StartedAt: time.Now().UTC()}
	release, err := store.Claim("connect", self)
	var busy *config.BusyError
	if errors.As(err, &busy) {
		return nil, busyConnect(busy.Holder)
	}
	return release, err
}

func busyConnect(h config.Holder) error {
	pid := "pid unknown"
	if h.PID != 0 {
		pid = fmt.Sprintf("pid %d", h.PID)
	}
	if scope, ok := service.Owner(h.PID); ok {
		return fmt.Errorf("the grokbot-tunnel service is already connected (%s); "+
			"stop it with \"systemctl %sstop %s\" or follow its log with \"journalctl %s-u %s -f\"",
			pid, scope.Flag(), service.Name, scope.Flag(), service.Name)
	}
	if !h.StartedAt.IsZero() {
		pid += ", started " + h.StartedAt.Local().Format(time.DateTime)
	}
	return fmt.Errorf("another grokbot-tunnel connect is already running (%s); only one can run per user", pid)
}

// warnCopiedStore warns if the store was created on another host: a copy
// connecting there would fight over the same computer, which the instance
// lock cannot see.
func warnCopiedStore(e *env) {
	if host := config.HostID(); e.machine.HostID != "" && host != "" && e.machine.HostID != host {
		log.Printf("warning: %s was created on another host. If grokbot-tunnel connects there too, "+
			"both fight over the same computer in Grok Bot. Delete machine.json and computer.json "+
			"in it to register this host as a new computer.", e.store.Dir)
	}
}

func notify(n *service.Notifier, state string) {
	if err := n.Notify(state); err != nil {
		log.Printf("notify systemd: %v", err)
	}
}

func register(ctx context.Context, dashboard rpc.DashboardServiceClient, label, mode string) error {
	ctx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req := &aiserverv1.RegisterSandMachineRequest{Label: label, LocalToolPermission: &mode}
	if _, err := dashboard.RegisterSandMachine(ctx, connect.NewRequest(req)); err != nil {
		return fmt.Errorf("register computer: %w", err)
	}
	return nil
}
