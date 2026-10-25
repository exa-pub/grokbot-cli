package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-cli/internal/auth"
	"github.com/exa-pub/grokbot-cli/internal/config"
	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
)

// LoginOptions configure Login.
type LoginOptions struct {
	NoBrowser     bool   // only print the login URL
	TeamID        string // selected team, overrides the one chosen in the browser
	ClientVersion string
}

func (o LoginOptions) validate() error {
	if o.TeamID == "" {
		return nil
	}
	if n, err := strconv.ParseInt(o.TeamID, 10, 32); err != nil || n <= 0 {
		return errors.New("team must be a positive integer")
	}
	return nil
}

// Login signs the user in through the browser, saves the tokens and verifies
// them by fetching the profile. Progress is printed to out.
func Login(ctx context.Context, opts LoginOptions, out io.Writer) error {
	if err := opts.validate(); err != nil {
		return err
	}
	e, err := newEnv(opts.ClientVersion)
	if err != nil {
		return err
	}
	l := auth.NewLogin(websiteURL())
	fmt.Fprintln(out, "Open this URL to sign in:\n"+l.URL)
	if !opts.NoBrowser {
		openBrowser(ctx, l.URL)
	}
	identity, err := auth.Poll(ctx, e.rest, e.base, l)
	if err != nil {
		return err
	}
	if opts.TeamID != "" {
		identity.TeamID = opts.TeamID
	}
	if err = e.store.Save("auth", identity); err != nil {
		return err
	}
	// Computer credentials belong to the previous account.
	if err = e.store.Save("computer", config.Computer{}); err != nil {
		return err
	}

	u := e.user(identity)
	ctx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	me, err := u.dashboard.GetMe(ctx, connect.NewRequest(&aiserverv1.GetMeRequest{}))
	if err != nil {
		return fmt.Errorf("tokens saved; profile verification failed: %w", err)
	}
	identity = u.tokens.Snapshot()
	identity.Email = me.Msg.GetEmail()
	identity.AuthID = me.Msg.GetAuthId()
	if err = e.store.Save("auth", identity); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in as %s\n", identity.Email)
	return nil
}

// openBrowser opens url in the desktop browser without waiting for it.
func openBrowser(ctx context.Context, url string) {
	opener := exec.CommandContext(ctx, "xdg-open", url) //nolint:gosec // url is our own login page
	if err := opener.Start(); err != nil {
		log.Printf("browser: %v", err)
		return
	}
	go func() {
		if err := opener.Wait(); err != nil {
			log.Printf("browser: %v", err)
		}
	}()
}
