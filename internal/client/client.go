// Package client provides the HTTP transport and auth interceptor shared by
// all RPC clients.
package client

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"runtime"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
)

// Version is the Sand desktop version whose protocol this client speaks.
const Version = "0.77.0"

// AuthUserAgent is the bundled Undici default: n5t selects "undici" when
// neither __UNDICI_IS_NODE__ nor esbuildDetection is defined.
const AuthUserAgent = "undici"

// Profile selects which set of headers a Transport sends.
type Profile int

// Profiles keep auth REST, user RPC and credential RPC headers separate.
const (
	UserRPC     Profile = iota // dashboard and credential issuing, with the user's bearer token
	ComputerRPC                // watch/poll/submit, authenticated by the computer credential
	AuthREST                   // login polling and token refresh
)

// Checksum computes the X-Cursor-Checksum header: an obfuscated coarse
// timestamp followed by the machine ID.
func Checksum(machine string, now time.Time) string {
	// The protocol keeps only the low 32 bits of the timestamp.
	t := binary.BigEndian.AppendUint32(nil, uint32(now.UnixMilli()/1_000_000)) //nolint:gosec // truncation is intended
	b := []byte{t[2], t[3], t[0], t[1], t[2], t[3]}
	prev := byte(0xa5)
	for i := range b {
		b[i] = (b[i] ^ prev) + byte(i)
		prev = b[i]
	}
	return base64.RawURLEncoding.EncodeToString(b) + machine
}

// Transport adds the desktop client headers for its Profile to every request.
type Transport struct {
	Base    http.RoundTripper
	Machine string
	Version string
	Profile Profile
	Privacy *Privacy // sets X-Ghost-Mode for UserRPC; nil means always private
}

// computerStripped are user-scoped headers that must never reach the
// credential-authenticated computer channel.
var computerStripped = []string{"Authorization", "X-Cursor-Team-Id", "X-Cursor-Checksum", "X-Ghost-Mode", "X-Request-Id"}

// RoundTrip implements http.RoundTripper.
func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.Profile == AuthREST {
		return t.roundTripREST(r)
	}
	v := t.Version
	if v == "" {
		v = Version
	}
	h := r.Header
	h.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Sand/"+v+" Safari/537.36")
	h.Set("X-Cursor-Client-Type", "sand")
	h.Set("X-Cursor-Client-Version", v)
	h.Set("X-Cursor-Client-OS", runtime.GOOS)
	h.Set("X-Cursor-Client-Source", "sand-desktop")
	h.Set("X-Sand-Box-Namespace", "prod")
	if t.Profile == ComputerRPC {
		for _, key := range computerStripped {
			h.Del(key)
		}
		return t.Base.RoundTrip(r)
	}
	h.Set("X-Cursor-Checksum", Checksum(t.Machine, time.Now()))
	h.Set("X-Ghost-Mode", t.ghostMode(r))
	h.Set("X-Request-Id", config.UUID())
	return t.Base.RoundTrip(r)
}

func (t Transport) ghostMode(r *http.Request) string {
	// The privacy lookup itself goes through PrivacyHTTP, which has no Privacy.
	if t.Privacy == nil {
		return ghostOn
	}
	return t.Privacy.Ghost(r.Context())
}

// roundTripREST mimics Node's fetch: it negotiates compression itself, so the
// response is decoded here rather than by net/http.
func (t Transport) roundTripREST(r *http.Request) (*http.Response, error) {
	h := r.Header
	h.Set("User-Agent", AuthUserAgent)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "*")
	h.Set("Sec-Fetch-Mode", "cors")
	if r.URL.Scheme == "https" {
		h.Set("Accept-Encoding", "br, gzip, deflate")
	} else {
		h.Set("Accept-Encoding", "gzip, deflate")
	}
	resp, err := t.Base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	return decodeBody(resp)
}

// Bearer authenticates unary calls with the user's access token and retries
// once with a refreshed token if the server rejects it.
func Bearer(s *auth.TokenSource) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			tok, err := s.Token(ctx, false)
			if err != nil {
				return nil, err
			}
			r.Header().Set("Authorization", "Bearer "+tok)
			if team := s.Snapshot().TeamID; team != "" {
				r.Header().Set("X-Cursor-Team-Id", team)
			}
			resp, err := next(ctx, r)
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				return resp, err
			}
			if tok, err = s.Token(ctx, true); err != nil {
				return nil, err
			}
			r.Header().Set("Authorization", "Bearer "+tok)
			return next(ctx, r)
		}
	})
}
