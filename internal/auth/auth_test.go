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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-cli/internal/config"
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
	token := "x." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".x"
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
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: config.Auth{AccessToken: "expired", RefreshToken: "old"}}
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
