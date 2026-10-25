package client

import (
	"context"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1/aiserverv1connect"
)

// X-Ghost-Mode header values.
const (
	ghostOn  = "true"
	ghostOff = "false"
)

const (
	privacyTimeout  = 10 * time.Second
	privacyCacheTTL = 5 * time.Minute
	// privacyRetryTTL delays the next lookup after a failure.
	privacyRetryTTL = 10 * time.Second
)

// PrivacyHTTP returns the HTTP client for privacy lookups, based on t. It
// never consults a Privacy, so lookups are always private and cannot recurse
// into Ghost, and it does not follow redirects, so a redirected lookup fails
// (falling back to ghost mode) instead of looping.
func PrivacyHTTP(t Transport) *http.Client {
	t.Privacy = nil
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Privacy caches the account's privacy mode. Lookup failures fall back to the
// safe value, ghost mode on. Client should use PrivacyHTTP.
type Privacy struct {
	mu       sync.Mutex // guards the fields below; not held during lookups
	Client   rpc.DashboardServiceClient
	expires  time.Time
	ghost    string
	inflight bool
}

// Ghost returns the X-Ghost-Mode header value: "false" only if the account
// allows its data to be used for training. While one caller looks the mode
// up, others get the last known value, or ghost mode on if there is none.
func (pv *Privacy) Ghost(ctx context.Context) string {
	pv.mu.Lock()
	if pv.ghost == "" {
		pv.ghost = ghostOn
	}
	ghost := pv.ghost
	if pv.inflight || pv.Client == nil || time.Now().Before(pv.expires) {
		pv.mu.Unlock()
		return ghost
	}
	pv.inflight = true
	client := pv.Client
	pv.mu.Unlock()

	ghost = pv.lookup(ctx, client)
	pv.mu.Lock()
	defer pv.mu.Unlock()
	pv.inflight = false
	if ghost == "" {
		pv.ghost, pv.expires = ghostOn, time.Now().Add(privacyRetryTTL)
	} else {
		pv.ghost, pv.expires = ghost, time.Now().Add(privacyCacheTTL)
	}
	return pv.ghost
}

// lookup asks the server for the privacy mode. It returns "" on failure.
func (pv *Privacy) lookup(ctx context.Context, client rpc.DashboardServiceClient) string {
	ctx, cancel := context.WithTimeout(ctx, privacyTimeout)
	defer cancel()
	req := &aiserverv1.GetUserPrivacyModeRequest{InferredPrivacyMode: aiserverv1.PrivacyMode_PRIVACY_MODE_NO_STORAGE}
	r, err := client.GetUserPrivacyMode(ctx, connect.NewRequest(req))
	if err != nil {
		return ""
	}
	switch r.Msg.PrivacyMode {
	case aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_DATA_TRAINING_ALLOWED,
		aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_CODEBASE_TRAINING_ALLOWED:
		return ghostOff
	}
	return ghostOn
}
