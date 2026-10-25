package client

import (
	"context"
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

// Privacy caches the account's privacy mode. Lookup failures fall back to the
// safe value, ghost mode on.
type Privacy struct {
	mu      sync.Mutex // also serializes lookups
	Client  rpc.DashboardServiceClient
	expires time.Time
	ghost   string
}

// Ghost returns the X-Ghost-Mode header value: "false" only if the account
// allows its data to be used for training.
func (pv *Privacy) Ghost(ctx context.Context) string {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	if time.Now().Before(pv.expires) {
		return pv.ghost
	}
	pv.ghost = ghostOn
	pv.expires = time.Now().Add(privacyRetryTTL)
	if pv.Client == nil {
		return pv.ghost
	}
	ctx, cancel := context.WithTimeout(ctx, privacyTimeout)
	defer cancel()
	req := &aiserverv1.GetUserPrivacyModeRequest{InferredPrivacyMode: aiserverv1.PrivacyMode_PRIVACY_MODE_NO_STORAGE}
	r, err := pv.Client.GetUserPrivacyMode(ctx, connect.NewRequest(req))
	if err != nil {
		return pv.ghost
	}
	pv.expires = time.Now().Add(privacyCacheTTL)
	switch r.Msg.PrivacyMode {
	case aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_DATA_TRAINING_ALLOWED,
		aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_CODEBASE_TRAINING_ALLOWED:
		pv.ghost = ghostOff
	}
	return pv.ghost
}
