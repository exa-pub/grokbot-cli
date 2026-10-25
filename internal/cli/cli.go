// Package cli defines the grokbot-cli command line.
package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/exa-pub/grokbot-cli/internal/app"
	"github.com/exa-pub/grokbot-cli/internal/client"
	"github.com/exa-pub/grokbot-cli/internal/permission"
)

// version is the release version, set at build time with
// -ldflags "-X github.com/exa-pub/grokbot-cli/internal/cli.version=v1.2.3".
var version = "dev"

// Execute parses args and runs the selected command.
func Execute(ctx context.Context, args []string) error {
	root := newRootCommand()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func newRootCommand() *cobra.Command {
	var clientVersion string
	root := &cobra.Command{
		Use:           "grokbot-cli",
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
	root.SetVersionTemplate("grokbot-cli {{.Version}} (Sand protocol " + client.Version + ")\n")
	root.PersistentFlags().StringVar(&clientVersion, "client-version",
		app.Env("SAND_CLIENT_APP_VERSION", client.Version), "Sand client version header")
	root.AddCommand(newLoginCommand(&clientVersion), newConnectCommand(&clientVersion))
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
			return app.Connect(cmd.Context(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.Root, "root", "", "root for file tools and shell working directories (default: home directory)")
	f.StringVar(&opts.Permission, "permission", permission.ModeAsk, "ask, always, or never")
	f.StringVar(&opts.Label, "label", "", "computer label")
	f.BoolVar(&opts.Debug, "debug", false, "log raw request JSON (may contain sensitive data)")
	return cmd
}
