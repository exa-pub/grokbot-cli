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

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
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
	var previous config.Auth
	if e.store.Load("auth", &previous) != nil {
		previous = config.Auth{}
	}
	if err = saveLogin(ctx, e.store, identity); err != nil {
		return err
	}

	u := e.user(identity)
	ctx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	me, err := u.dashboard.GetMe(ctx, connect.NewRequest(&aiserverv1.GetMeRequest{}))
	if err != nil {
		return fmt.Errorf("tokens saved; profile verification failed: %w", err)
	}
	unlock, err := e.store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	identity = u.tokens.Snapshot()
	identity.Email = me.Msg.GetEmail()
	identity.AuthID = me.Msg.GetAuthId()
	if err = e.store.Save("auth", identity); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in as %s\n", identity.Email)
	warnStaleConnect(e.store, previous, identity, out)
	return nil
}

// warnStaleConnect tells the user that a running connect keeps the account it
// started with: it holds the old tokens and leaves the new login alone.
func warnStaleConnect(store *config.Store, previous, current config.Auth, out io.Writer) {
	before := auth.AccountScope(previous)
	if before == "" || before == auth.AccountScope(current) {
		return
	}
	h, held, err := store.Probe("connect")
	if err != nil || !held {
		return
	}
	account := previous.Email
	if account == "" {
		account = "the previous account"
	}
	fmt.Fprintf(out, "Note: grokbot-tunnel connect (pid %d) keeps serving %s until it is restarted\n", h.PID, account)
}

// saveLogin replaces the saved identity. It holds the store lock so that a
// concurrent token refresh in another process cannot overwrite the new login.
func saveLogin(ctx context.Context, store *config.Store, identity config.Auth) error {
	unlock, err := store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err = store.Save("auth", identity); err != nil {
		return err
	}
	// Computer credentials belong to the previous account.
	return store.Save("computer", config.Computer{})
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
