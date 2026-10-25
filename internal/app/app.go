// Package app wires configuration, authentication and RPC clients into the
// login and connect workflows.
package app

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/client"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

const (
	restTimeout = 20 * time.Second
	maxMessage  = 128 << 20
)

// Env returns the environment variable key without a trailing slash, or
// fallback if it is unset.
func Env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimRight(v, "/")
	}
	return fallback
}

func backendURL() string {
	return Env("SAND_BACKEND_URL", Env("CURSOR_API_BASE_URL", "https://api2.cursor.sh"))
}

func websiteURL() string {
	return Env("SAND_CURSOR_WEBSITE_URL", Env("CURSOR_WEBSITE_URL", "https://cursor.com"))
}

// env holds what both workflows need.
type env struct {
	store    *config.Store
	machine  config.Machine
	base     string
	rest     *http.Client // plain REST calls: login polling and token refresh
	computer *http.Client // Connect RPC authenticated by the computer credential
	rpc      *http.Client // Connect RPC authenticated as the user
	lookup   *http.Client // privacy-mode lookups, as the user but always private
	privacy  *client.Privacy
}

func newEnv(clientVersion string) (*env, error) {
	store, err := config.Default()
	if err != nil {
		return nil, err
	}
	machine, err := store.Machine()
	if err != nil {
		return nil, err
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}
	http1 := defaultTransport.Clone()
	http1.ForceAttemptHTTP2 = false
	privacy := &client.Privacy{}
	userRPC := client.Transport{
		Base:    http1,
		Machine: machine.MachineID,
		Version: clientVersion,
		Profile: client.UserRPC,
		Privacy: privacy,
	}
	return &env{
		store:   store,
		machine: machine,
		base:    backendURL(),
		rest: &http.Client{Timeout: restTimeout, Transport: client.Transport{
			Base:    defaultTransport.Clone(),
			Profile: client.AuthREST,
		}},
		computer: &http.Client{Transport: client.Transport{
			Base:    http1.Clone(),
			Version: clientVersion,
			Profile: client.ComputerRPC,
		}},
		rpc:     &http.Client{Transport: userRPC},
		lookup:  client.PrivacyHTTP(userRPC),
		privacy: privacy,
	}, nil
}

// user holds the RPC clients authenticated as the signed-in user.
type user struct {
	tokens    *auth.TokenSource
	dashboard rpc.DashboardServiceClient
	grokbot   rpc.GrokBotServiceClient
}

func (e *env) user(identity config.Auth) *user {
	tokens := &auth.TokenSource{Store: e.store, HTTP: e.rest, Base: e.base, Identity: identity}
	u := &user{
		tokens:    tokens,
		dashboard: rpc.NewDashboardServiceClient(e.rpc, e.base, userOptions(tokens)...),
		grokbot:   rpc.NewGrokBotServiceClient(e.rpc, e.base, userOptions(tokens)...),
	}
	// Ghost mode depends on the account, so look it up as this user.
	e.privacy.Client = rpc.NewDashboardServiceClient(e.lookup, e.base, userOptions(tokens)...)
	return u
}

// computerChannel returns the client for the credential-authenticated channel.
func (e *env) computerChannel() rpc.GrokBotServiceClient {
	return rpc.NewGrokBotServiceClient(e.computer, e.base, connect.WithReadMaxBytes(maxMessage), client.Compression())
}

// userOptions authenticate calls with the user's bearer token.
func userOptions(tokens *auth.TokenSource) []connect.ClientOption {
	return []connect.ClientOption{
		connect.WithInterceptors(client.Bearer(tokens)),
		connect.WithReadMaxBytes(maxMessage),
		client.Compression(),
	}
}
