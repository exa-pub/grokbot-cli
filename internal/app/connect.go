package app

import (
	"context"
	"fmt"
	"log"
	"os"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-cli/internal/auth"
	"github.com/exa-pub/grokbot-cli/internal/computer"
	"github.com/exa-pub/grokbot-cli/internal/config"
	"github.com/exa-pub/grokbot-cli/internal/executor"
	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-cli/internal/permission"
)

// ConnectOptions configure Connect.
type ConnectOptions struct {
	Root          string // root for file tools and shell cwd; empty means home
	Permission    string // one of the permission.Mode* values
	Label         string // new computer label; empty keeps the saved one
	Debug         bool   // log raw requests
	ClientVersion string
}

func (o ConnectOptions) validate() error {
	switch o.Permission {
	case permission.ModeAsk, permission.ModeAlways, permission.ModeNever:
		return nil
	}
	return fmt.Errorf("invalid permission %q", o.Permission)
}

// Connect registers this computer and executes the agent's requests until ctx
// is canceled.
func Connect(ctx context.Context, opts ConnectOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if opts.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		opts.Root = home
	}
	e, err := newEnv(opts.ClientVersion)
	if err != nil {
		return err
	}
	var identity config.Auth
	if err = e.store.Load("auth", &identity); err != nil {
		return fmt.Errorf("load authentication (run grokbot-cli login): %w", err)
	}
	if opts.Label != "" {
		e.machine.Label = opts.Label
		if err = e.store.Save("machine", e.machine); err != nil {
			return err
		}
	}
	policy := &permission.Policy{Mode: opts.Permission, In: os.Stdin, Out: os.Stderr}
	ex, err := executor.New(opts.Root, policy)
	if err != nil {
		return err
	}
	defer ex.Close()

	u := e.user(identity)
	if err = register(ctx, u.dashboard, e.machine.Label, opts.Permission); err != nil {
		return err
	}
	log.Printf("root=%s permission=%s", ex.Root, opts.Permission)
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

func register(ctx context.Context, dashboard rpc.DashboardServiceClient, label, mode string) error {
	ctx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req := &aiserverv1.RegisterSandMachineRequest{Label: label, LocalToolPermission: &mode}
	if _, err := dashboard.RegisterSandMachine(ctx, connect.NewRequest(req)); err != nil {
		return fmt.Errorf("register computer: %w", err)
	}
	return nil
}
