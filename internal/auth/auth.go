// Package auth implements the PKCE browser login and access-token refresh.
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/config"
)

// ClientID is the OAuth client used for token refresh.
const ClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"

const (
	maxBody      = 1 << 20
	pollMaxDelay = 10 * time.Second
	// pollMaxOutage bounds how long login polling rides out network errors,
	// rate limiting and server errors in a row.
	pollMaxOutage = 2 * time.Minute
	// refreshMargin refreshes access tokens shortly before they expire.
	refreshMargin = 5 * time.Minute
)

// Variables so tests can shorten them.
var (
	pollTimeout    = 20 * time.Minute
	pollFirstDelay = time.Second
)

// Login is a pending browser login.
type Login struct {
	UUID     string
	Verifier string // PKCE verifier, kept secret until polling
	URL      string // page the user opens to sign in
}

// NewLogin starts a login on the given website.
func NewLogin(website string) Login {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	challenge := sha256.Sum256([]byte(verifier))
	id := config.UUID()
	q := url.Values{
		"challenge":                 {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"uuid":                      {id},
		"mode":                      {"login"},
		"redirectTarget":            {"sand"},
		"supportsSelectedTeamLogin": {"true"},
	}
	return Login{
		UUID:     id,
		Verifier: verifier,
		URL:      strings.TrimRight(website, "/") + "/loginDeepControl?" + q.Encode(),
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Poll waits until the user completes login in the browser. Network errors,
// rate limiting and server errors are retried for up to pollMaxOutage in a
// row; other unexpected responses end the login.
func Poll(ctx context.Context, h *http.Client, base string, l Login) (config.Auth, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	q := url.Values{"uuid": {l.UUID}, "verifier": {l.Verifier}}
	endpoint := base + "/auth/poll?" + q.Encode()
	delay := pollFirstDelay
	var outage time.Time // start of the current run of transient failures
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return config.Auth{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		status, body, err := do(h, req)
		switch {
		case transient(status, err):
			if outage.IsZero() {
				outage = time.Now()
			} else if time.Since(outage) > pollMaxOutage && ctx.Err() == nil {
				if err == nil {
					err = fmt.Errorf("HTTP %d", status)
				}
				return config.Auth{}, fmt.Errorf("login polling failed: %w", err)
			}
		case status == http.StatusOK:
			return parseLogin(body)
		case status == http.StatusNotFound: // not signed in yet
			outage = time.Time{}
		case status == http.StatusForbidden:
			return config.Auth{}, errors.New("sign-in policy refused login")
		default:
			return config.Auth{}, fmt.Errorf("login polling failed: HTTP %d", status)
		}
		if err := sleep(ctx, delay); err != nil {
			// Our own deadline, not the caller's, means the user never finished.
			if parent.Err() == nil {
				err = errors.New("login polling timed out")
			}
			return config.Auth{}, err
		}
		delay = min(delay*6/5, pollMaxDelay)
	}
}

// transient reports whether a failed poll is worth retrying.
func transient(status int, err error) bool {
	return err != nil || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func do(h *http.Client, req *http.Request) (int, []byte, error) {
	r, err := h.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	return r.StatusCode, b, err
}

func parseLogin(body []byte) (config.Auth, error) {
	var v struct {
		AccessToken    string      `json:"accessToken"`
		RefreshToken   string      `json:"refreshToken"`
		AuthID         string      `json:"authId"`
		SelectedTeamID json.Number `json:"selectedTeamId"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return config.Auth{}, err
	}
	if v.AccessToken == "" || v.RefreshToken == "" {
		return config.Auth{}, errors.New("login returned empty tokens")
	}
	return config.Auth{
		AccessToken:  v.AccessToken,
		RefreshToken: v.RefreshToken,
		AuthID:       v.AuthID,
		TeamID:       string(v.SelectedTeamID),
	}, nil
}

// TokenSource hands out access tokens, refreshing and persisting them as
// needed. Concurrent callers share a single refresh.
type TokenSource struct {
	mu       sync.Mutex
	Store    *config.Store
	HTTP     *http.Client
	Base     string
	Identity config.Auth
}

// jwtClaims holds the JWT claims this client uses.
type jwtClaims struct {
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
}

// claims decodes the payload of a JWT without verifying it. Malformed tokens
// yield zero claims.
func claims(token string) jwtClaims {
	var c jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return jwtClaims{}
	}
	return c
}

// expiry returns when the access token expires: the exp claim of a JWT,
// else the expiry the server reported with it, else zero if unknown.
func expiry(id config.Auth) time.Time {
	if exp := claims(id.AccessToken).Exp; exp != 0 {
		return time.Unix(exp, 0)
	}
	if id.ExpiresAtMS != 0 {
		return time.UnixMilli(id.ExpiresAtMS)
	}
	return time.Time{}
}

// fresh reports whether the access token can be used without a refresh. A
// token of unknown lifetime is used until the server rejects it, and the
// Bearer interceptor then forces a refresh.
func fresh(id config.Auth) bool {
	if id.AccessToken == "" {
		return false
	}
	exp := expiry(id)
	return exp.IsZero() || time.Until(exp) > refreshMargin
}

// sameAccount reports whether two identities may belong to the same account
// and team. Identities without a known account are assumed to match.
func sameAccount(a, b config.Auth) bool {
	sa, sb := AccountScope(a), AccountScope(b)
	return sa == "" || sb == "" || sa == sb
}

// Token returns a valid access token. force refreshes even if the current
// token looks valid, e.g. after the server rejected it.
//
// Several processes share auth.json and the server rotates the refresh token
// on every refresh, so refreshes are serialized by the store lock and start
// from the tokens on disk: if another process already rotated them, its
// tokens are adopted instead of refreshing with the stale refresh token, which
// the server would answer with shouldLogout.
func (s *TokenSource) Token(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.Identity
	if !force && fresh(id) {
		return id.AccessToken, nil
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	onDisk, persist := s.reload()
	if s.Identity.RefreshToken != id.RefreshToken {
		id = s.Identity
		// The adopted access token is not the one the server rejected.
		if fresh(id) {
			return id.AccessToken, nil
		}
	}
	if id.RefreshToken == "" {
		return "", ErrNotLoggedIn
	}
	next, err := s.refresh(ctx, id.RefreshToken)
	if errors.Is(err, ErrLogout) {
		s.Identity = config.Auth{}
		// Only wipe auth.json if it still holds the rejected session.
		if onDisk == id.RefreshToken {
			if err := s.Store.Save("auth", config.Auth{}); err != nil {
				return "", err
			}
		}
		return "", err
	}
	if err != nil {
		return "", err
	}
	id.AccessToken, id.ExpiresAtMS = next.AccessToken, next.ExpiresAtMS
	if next.RefreshToken != "" {
		id.RefreshToken = next.RefreshToken
	}
	// The server already invalidated the old refresh token, so keep the new
	// one in memory even if it cannot be persisted.
	s.Identity = id
	if persist {
		if err := s.Store.Save("auth", id); err != nil {
			log.Printf("save refreshed tokens: %v", err)
		}
	}
	return id.AccessToken, nil
}

// lock takes the store lock, failing only if ctx ends. An unwritable store
// must not stop the refresh itself, so other errors leave it unlocked.
func (s *TokenSource) lock(ctx context.Context) (func(), error) {
	unlock, err := s.Store.Lock(ctx)
	switch {
	case err == nil:
		return unlock, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	log.Printf("lock auth store: %v", err)
	return func() {}, nil
}

// reload adopts the tokens on disk if another process rotated them since this
// one loaded them. It returns the refresh token on disk, empty if none, and
// whether this process may save its tokens there: not if the disk belongs to a
// different account, which means the user logged in again elsewhere. This
// process then keeps using its own tokens but leaves the new login alone.
// The caller holds the store lock.
func (s *TokenSource) reload() (onDisk string, persist bool) {
	var disk config.Auth
	if err := s.Store.Load("auth", &disk); err != nil || disk.RefreshToken == "" {
		return "", true
	}
	if !sameAccount(s.Identity, disk) {
		return disk.RefreshToken, false
	}
	if disk.RefreshToken != s.Identity.RefreshToken {
		s.Identity = disk
	}
	return disk.RefreshToken, true
}

// Errors after which no token can be obtained until the user logs in again.
var (
	ErrLogout      = errors.New("session revoked; run grokbot-tunnel login")
	ErrNotLoggedIn = errors.New("not logged in; run grokbot-tunnel login")
)

// refresh exchanges a refresh token for new tokens. The refresh token in the
// result is empty if the server did not rotate it.
func (s *TokenSource) refresh(ctx context.Context, refreshToken string) (config.Auth, error) {
	b, err := json.Marshal(map[string]string{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return config.Auth{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base+"/oauth/token", bytes.NewReader(b))
	if err != nil {
		return config.Auth{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, body, err := do(s.HTTP, req)
	if err != nil {
		return config.Auth{}, err
	}
	var v struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"` // seconds; optional
		ShouldLogout bool        `json:"shouldLogout"`
	}
	decodeErr := json.Unmarshal(body, &v)
	switch {
	case v.ShouldLogout:
		return config.Auth{}, ErrLogout
	case status != http.StatusOK:
		return config.Auth{}, fmt.Errorf("refresh failed: HTTP %d", status)
	case decodeErr != nil:
		return config.Auth{}, fmt.Errorf("refresh failed: %w", decodeErr)
	case v.AccessToken == "":
		return config.Auth{}, errors.New("refresh failed: empty access token")
	}
	next := config.Auth{AccessToken: v.AccessToken, RefreshToken: v.RefreshToken}
	if secs, err := v.ExpiresIn.Float64(); err == nil && secs > 0 {
		next.ExpiresAtMS = time.Now().Add(time.Duration(secs * float64(time.Second))).UnixMilli()
	}
	return next, nil
}

// Snapshot returns the identity without racing token rotation.
func (s *TokenSource) Snapshot() config.Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Identity
}

// AccountScope identifies the account and team that cached computer
// credentials belong to. It is stable across token rotation; emails are not
// used because they can be reassigned. It is empty if the account is unknown.
func AccountScope(id config.Auth) string {
	sub := id.AuthID
	if sub == "" {
		sub = claims(id.AccessToken).Sub
	}
	if sub == "" {
		// Nothing stable identifies the account (access and refresh tokens
		// rotate), and an empty scope disables credential caching.
		return ""
	}
	sum := sha256.Sum256([]byte(sub + "\x00" + id.TeamID))
	return hex.EncodeToString(sum[:])
}
