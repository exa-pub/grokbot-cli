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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/exa-pub/grokbot-cli/internal/config"
)

// ClientID is the OAuth client used for token refresh.
const ClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"

const (
	maxBody         = 1 << 20
	pollTimeout     = 20 * time.Minute
	pollAttempts    = 150
	pollMaxFailures = 3
	pollMaxDelay    = 10 * time.Second
	// refreshMargin refreshes access tokens shortly before they expire.
	refreshMargin = 5 * time.Minute
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

// Poll waits until the user completes login in the browser.
func Poll(ctx context.Context, h *http.Client, base string, l Login) (config.Auth, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	q := url.Values{"uuid": {l.UUID}, "verifier": {l.Verifier}}
	endpoint := base + "/auth/poll?" + q.Encode()
	delay := time.Second
	failures := 0
	for range pollAttempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return config.Auth{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		status, body, err := do(h, req)
		switch {
		case err != nil:
			failures++
		case status == http.StatusOK:
			return parseLogin(body)
		case status == http.StatusNotFound: // not signed in yet
			failures = 0
		case status == http.StatusForbidden:
			return config.Auth{}, errors.New("sign-in policy refused login")
		default:
			failures++
		}
		if failures >= pollMaxFailures {
			return config.Auth{}, fmt.Errorf("login polling failed %d times in a row", failures)
		}
		if err := sleep(ctx, delay); err != nil {
			return config.Auth{}, err
		}
		delay = min(delay*6/5, pollMaxDelay)
	}
	return config.Auth{}, errors.New("login polling timed out")
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

// expiry returns the exp claim of a JWT, or zero if it cannot be parsed.
func expiry(token string) time.Time {
	exp := claims(token).Exp
	if exp == 0 {
		return time.Time{}
	}
	return time.Unix(exp, 0)
}

// Token returns a valid access token. force refreshes even if the current
// token looks valid, e.g. after the server rejected it.
func (s *TokenSource) Token(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.Identity
	if !force && time.Until(expiry(id.AccessToken)) > refreshMargin {
		return id.AccessToken, nil
	}
	if id.RefreshToken == "" {
		return "", errors.New("not logged in; run grokbot-cli login")
	}
	b, err := json.Marshal(map[string]string{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": id.RefreshToken,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base+"/oauth/token", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	status, body, err := do(s.HTTP, req)
	if err != nil {
		return "", err
	}
	var v struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ShouldLogout bool   `json:"shouldLogout"`
	}
	decodeErr := json.Unmarshal(body, &v)
	if v.ShouldLogout {
		s.Identity = config.Auth{}
		if err := s.Store.Save("auth", s.Identity); err != nil {
			return "", err
		}
		return "", errors.New("session revoked; run grokbot-cli login")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("refresh failed: HTTP %d", status)
	}
	if decodeErr != nil {
		return "", fmt.Errorf("refresh failed: %w", decodeErr)
	}
	if v.AccessToken == "" {
		return "", errors.New("refresh failed: empty access token")
	}
	id.AccessToken = v.AccessToken
	if v.RefreshToken != "" {
		id.RefreshToken = v.RefreshToken
	}
	if err := s.Store.Save("auth", id); err != nil {
		return "", err
	}
	s.Identity = id
	return id.AccessToken, nil
}

// Snapshot returns the identity without racing token rotation.
func (s *TokenSource) Snapshot() config.Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Identity
}

// AccountScope identifies the account and team that cached computer
// credentials belong to. It is stable across token rotation; emails are not
// used because they can be reassigned.
func AccountScope(id config.Auth) string {
	sub := id.AuthID
	if sub == "" {
		sub = claims(id.AccessToken).Sub
	}
	if sub == "" {
		sub = id.AccessToken
	}
	sum := sha256.Sum256([]byte(sub + "\x00" + id.TeamID))
	return hex.EncodeToString(sum[:])
}
