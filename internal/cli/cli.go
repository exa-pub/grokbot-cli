// Package cli defines the grokbot-tunnel command line.
package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/exa-pub/grokbot-tunnel/internal/app"
	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/client"
	"github.com/exa-pub/grokbot-tunnel/internal/permission"
	"github.com/exa-pub/grokbot-tunnel/internal/service"
)

// version is the release version, set at build time with
// -ldflags "-X github.com/exa-pub/grokbot-tunnel/internal/cli.version=v1.2.3".
var version = "dev"

// packager names the package manager that installed this build, set at
// build time like version. Such builds leave updates to it.
var packager = ""

// Execute parses args and runs the selected command.
func Execute(ctx context.Context, args []string) error {
	root := newRootCommand()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

// ExitCode returns the exit status for an error from Execute:
// service.ExitLogin when the user has to log in again, which tells systemd
// that restarting cannot help, and 1 otherwise.
func ExitCode(err error) int {
	if errors.Is(err, auth.ErrLogout) || errors.Is(err, auth.ErrNotLoggedIn) {
		return service.ExitLogin
	}
	return 1
}

func newRootCommand() *cobra.Command {
	var clientVersion string
	root := &cobra.Command{
		Use:           "grokbot-tunnel",
		Short:         "Run Grok Bot agent requests on this computer",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Runnable so that cobra.NoArgs rejects unknown subcommands.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("missing command\n\n%s", cmd.UsageString())
		},
	}
	root.SetVersionTemplate("grokbot-tunnel {{.Version}} (Sand protocol " + client.Version + ")\n")
	root.PersistentFlags().StringVar(&clientVersion, "client-version",
		app.Env("SAND_CLIENT_APP_VERSION", client.Version), "Sand client version header")
	root.AddCommand(
		newLoginCommand(&clientVersion),
		newConnectCommand(&clientVersion),
		newInstallCommand(&clientVersion),
		newUninstallCommand(),
		newUpdateCommand(),
	)
	return root
}

func newLoginCommand(clientVersion *string) *cobra.Command {
	var opts app.LoginOptions
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in through the browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.ClientVersion = *clientVersion
			return app.Login(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&opts.NoBrowser, "no-browser", false, "print login URL without opening browser")
	cmd.Flags().StringVar(&opts.TeamID, "team", "", "selected team ID")
	return cmd
}

func newConnectCommand(clientVersion *string) *cobra.Command {
	var opts app.ConnectOptions
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Execute agent requests on this computer until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.ClientVersion = *clientVersion
			opts.Version = version
			return app.Connect(cmd.Context(), opts)
		},
	}
	connectFlags(cmd.Flags(), &opts, permission.ModeAsk)
	return cmd
}

// connectFlags defines the flags that configure connect, shared by connect
// and install.
func connectFlags(f *pflag.FlagSet, opts *app.ConnectOptions, defaultPermission string) {
	f.StringVar(&opts.Root, "root", "", "root for file tools and shell working directories (default: home directory)")
	f.StringVar(&opts.Permission, "permission", defaultPermission, "ask, always, or never")
	f.StringVar(&opts.Label, "label", "", "computer label")
	f.BoolVar(&opts.Debug, "debug", false, "log raw request JSON (may contain sensitive data)")
}

func newInstallCommand(clientVersion *string) *cobra.Command {
	var opts app.InstallOptions
	var noAutoUpdate bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Run connect as a systemd service that starts at boot",
		Long: `Installs a systemd service that runs "grokbot-tunnel connect" with the given
flags, starts it at boot and restarts it on failure. Root gets a system
service; other users get a service of their own systemd user manager, with
lingering enabled so that it runs without a login session.

Unless --no-auto-update is given, a daily timer updates grokbot-tunnel and
restarts the service. Run install again to change the flags.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("client-version") {
				opts.Connect.ClientVersion = *clientVersion
			}
			opts.Version = version
			opts.AutoUpdate = !noAutoUpdate && packager == ""
			return app.Install(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}
	connectFlags(cmd.Flags(), &opts.Connect, "")
	cmd.Flags().Lookup("permission").Usage = "ask, always, or never (required)"
	cmd.Flags().BoolVar(&noAutoUpdate, "no-auto-update", false, "do not update grokbot-tunnel daily")
	return cmd
}

func newUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the systemd service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.Uninstall(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func newUpdateCommand() *cobra.Command {
	var opts app.UpdateOptions
	cmd := &cobra.Command{
		Use:   "update [VERSION]",
		Short: "Update grokbot-tunnel to the latest or the given release",
		Long: `Downloads a release from GitHub, verifies its checksum, replaces this binary
and restarts the service if it runs this binary. VERSION is "latest" (the
default) or a release such as v1.2.3.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Target = "latest"
			if len(args) == 1 {
				opts.Target = args[0]
			}
			opts.Version = version
			opts.Packager = packager
			return app.Update(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.Check, "check", false, "only show the installed and the available version")
	f.BoolVar(&opts.Force, "force", false, "reinstall the same version, downgrade, or replace a development build")
	f.BoolVar(&opts.NoRestart, "no-restart", false, "do not restart the service")
	f.BoolVar(&opts.Auto, "auto", false, "run by the update timer: skip instead of failing when a decision is needed")
	if err := f.MarkHidden("auto"); err != nil {
		panic(err)
	}
	return cmd
}
