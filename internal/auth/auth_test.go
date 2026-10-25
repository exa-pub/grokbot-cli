package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/config"
)

func TestPKCE(t *testing.T) {
	l := NewLogin("https://cursor.com")
	u, _ := url.Parse(l.URL)
	h := sha256.Sum256([]byte(l.Verifier))
	if u.Query().Get("challenge") != base64.RawURLEncoding.EncodeToString(h[:]) || len(l.Verifier) != 43 || u.Query().Get("redirectTarget") != "sand" {
		t.Fatal("bad PKCE")
	}
}
func TestPoll(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("verifier") != "v" {
			t.Error("missing verifier")
		}
		fmt.Fprint(w, `{"accessToken":"a","refreshToken":"r","selectedTeamId":12}`)
	}))
	defer s.Close()
	a, e := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"})
	if e != nil || a.TeamID != "12" || a.AccessToken != "a" {
		t.Fatal(a, e)
	}
}
func TestRefreshRotationSingleFlight(t *testing.T) {
	var calls atomic.Int32
	token := jwt(time.Now().Add(time.Hour))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "old" {
			t.Errorf("unexpected refresh: %v", body)
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": token, "refresh_token": "new"})
	}))
	defer s.Close()
	store := &config.Store{Dir: t.TempDir()}
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{AccessToken: jwt(time.Now().Add(-time.Minute)), RefreshToken: "old"}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := source.Token(context.Background(), false); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	var saved config.Auth
	store.Load("auth", &saved)
	if calls.Load() != 1 || saved.RefreshToken != "new" {
		t.Fatal(calls.Load(), saved.RefreshToken)
	}
}

func TestPollInvalidBaseURL(t *testing.T) {
	if _, err := Poll(context.Background(), http.DefaultClient, "http://bad host", Login{UUID: "u", Verifier: "v"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRefreshReportsHTTPStatus(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
	}))
	defer s.Close()
	source := &TokenSource{Store: &config.Store{Dir: t.TempDir()}, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{RefreshToken: "r"}}
	if _, err := source.Token(context.Background(), true); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatal(err)
	}
}

func jwt(exp time.Time) string {
	return "x." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".x"
}

// rotatingServer accepts each refresh token once and answers reuse with
// shouldLogout, like the real server. extra is merged into every response.
func rotatingServer(t *testing.T, extra map[string]any) (*httptest.Server, *atomic.Int32) {
	var mu sync.Mutex
	var calls atomic.Int32
	used := map[string]bool{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		old := body["refresh_token"]
		if used[old] {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]bool{"shouldLogout": true})
			return
		}
		used[old] = true
		resp := map[string]any{"access_token": "access-" + old + "'", "refresh_token": old + "'"}
		for k, v := range extra {
			resp[k] = v
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

func TestRefreshKeepsRotatedTokenWhenSaveFails(t *testing.T) {
	s, _ := rotatingServer(t, nil)
	// A store whose directory is a regular file cannot be locked or saved.
	dir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(dir, nil, 0o600)
	source := &TokenSource{Store: &config.Store{Dir: dir}, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{RefreshToken: "r"}}
	for range 2 {
		if _, err := source.Token(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	if got := source.Snapshot().RefreshToken; got != "r''" {
		t.Fatal(got)
	}
}

func TestRefreshAdoptsTokensRotatedByAnotherProcess(t *testing.T) {
	s, calls := rotatingServer(t, nil)
	store := &config.Store{Dir: t.TempDir()}
	start := config.Auth{AccessToken: "a", RefreshToken: "r", AuthID: "acc"}
	store.Save("auth", start)
	first := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: start}
	second := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: start}
	if _, err := first.Token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	tok, err := second.Token(context.Background(), true)
	if err != nil || tok != "access-r'" || calls.Load() != 1 {
		t.Fatal(tok, err, calls.Load())
	}
	var saved config.Auth
	store.Load("auth", &saved)
	if saved.RefreshToken != "r'" || second.Snapshot().RefreshToken != "r'" {
		t.Fatal(saved, second.Snapshot())
	}
}

func TestRefreshLeavesOtherAccountOnDisk(t *testing.T) {
	s, _ := rotatingServer(t, nil)
	store := &config.Store{Dir: t.TempDir()}
	other := config.Auth{AccessToken: "b", RefreshToken: "rb", AuthID: "account-b"}
	store.Save("auth", other)
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{RefreshToken: "ra", AuthID: "account-a"}}
	if tok, err := source.Token(context.Background(), true); err != nil || tok != "access-ra'" {
		t.Fatal(tok, err)
	}
	// Reusing the stale token is rejected, but must not log account B out.
	source.Identity.RefreshToken = "ra"
	if _, err := source.Token(context.Background(), true); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatal(err)
	}
	var saved config.Auth
	store.Load("auth", &saved)
	if saved != other {
		t.Fatal(saved)
	}
}

func TestRefreshLogoutWipesOnlyRejectedSession(t *testing.T) {
	s, _ := rotatingServer(t, nil)
	store := &config.Store{Dir: t.TempDir()}
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{RefreshToken: "r"}}
	if _, err := source.Token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// Another process still holds "r", and so does the disk.
	store.Save("auth", config.Auth{RefreshToken: "r"})
	stale := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{RefreshToken: "r"}}
	if _, err := stale.Token(context.Background(), true); err == nil {
		t.Fatal("expected revocation")
	}
	var saved config.Auth
	store.Load("auth", &saved)
	if saved != (config.Auth{}) {
		t.Fatal("rejected session kept", saved)
	}
}

func TestTokenWithoutExpiry(t *testing.T) {
	s, calls := rotatingServer(t, map[string]any{"expires_in": 3600})
	store := &config.Store{Dir: t.TempDir()}
	// An opaque token of unknown lifetime is used until the server rejects it.
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{AccessToken: "opaque", RefreshToken: "r"}}
	if tok, err := source.Token(context.Background(), false); err != nil || tok != "opaque" || calls.Load() != 0 {
		t.Fatal(tok, err, calls.Load())
	}
	// After a forced refresh, expires_in keeps the new token in use.
	if _, err := source.Token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if tok, err := source.Token(context.Background(), false); err != nil || tok != "access-r'" {
			t.Fatal(tok, err)
		}
	}
	if calls.Load() != 1 || source.Snapshot().ExpiresAtMS <= time.Now().UnixMilli() {
		t.Fatal(calls.Load(), source.Snapshot())
	}
	// A reported expiry inside the refresh margin triggers a refresh.
	source.Identity.ExpiresAtMS = time.Now().Add(time.Minute).UnixMilli()
	if tok, err := source.Token(context.Background(), false); err != nil || tok != "access-r''" {
		t.Fatal(tok, err)
	}
}

func TestAccountScopeIgnoresRotatingTokens(t *testing.T) {
	if AccountScope(config.Auth{AccessToken: "a"}) != "" {
		t.Fatal("scope derived from access token")
	}
	a := AccountScope(config.Auth{AccessToken: "a", AuthID: "acc", TeamID: "1"})
	if a == "" || a != AccountScope(config.Auth{AccessToken: "b", AuthID: "acc", TeamID: "1"}) {
		t.Fatal("unstable scope")
	}
}

func TestPollRidesOutTransientErrors(t *testing.T) {
	defer func(d time.Duration) { pollFirstDelay = d }(pollFirstDelay)
	pollFirstDelay = time.Millisecond
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1, 2, 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 4, 5:
			w.WriteHeader(http.StatusTooManyRequests)
		case 6:
			w.WriteHeader(http.StatusNotFound)
		default:
			fmt.Fprint(w, `{"accessToken":"a","refreshToken":"r"}`)
		}
	}))
	defer s.Close()
	if a, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); err != nil || a.AccessToken != "a" {
		t.Fatal(a, err)
	}
}

func TestPollTimeoutMessage(t *testing.T) {
	defer func(d, timeout time.Duration) { pollFirstDelay, pollTimeout = d, timeout }(pollFirstDelay, pollTimeout)
	pollFirstDelay, pollTimeout = time.Millisecond, 50*time.Millisecond
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer s.Close()
	if _, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); err == nil || err.Error() != "login polling timed out" {
		t.Fatal(err)
	}
}
