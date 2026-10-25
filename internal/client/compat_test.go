package client

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/andybalholm/brotli"
	"google.golang.org/protobuf/proto"

	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1/aiserverv1connect"
)

func TestTransportProfiles(t *testing.T) {
	for _, profile := range []Profile{UserRPC, ComputerRPC, AuthREST} {
		t.Run(string(rune('0'+profile)), func(t *testing.T) {
			var headers http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { headers = r.Header.Clone(); w.WriteHeader(204) }))
			defer server.Close()
			h := &http.Client{Transport: Transport{Base: http.DefaultTransport, Machine: "m", Profile: profile}}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			if profile != AuthREST {
				req.Header.Set("Authorization", "Bearer fake")
			}
			resp, err := h.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			switch profile {
			case UserRPC:
				if headers.Get("X-Cursor-Checksum") == "" || headers.Get("Authorization") != "Bearer fake" || headers.Get("X-Ghost-Mode") != "true" {
					t.Fatal(headers)
				}
			case ComputerRPC:
				if headers.Get("X-Sand-Box-Namespace") != "prod" || headers.Get("Authorization") != "" || headers.Get("X-Cursor-Checksum") != "" || headers.Get("X-Ghost-Mode") != "" || headers.Get("X-Request-Id") != "" {
					t.Fatal(headers)
				}
			case AuthREST:
				if headers.Get("User-Agent") != "undici" || headers.Get("X-Cursor-Client-Type") != "" {
					t.Fatal(headers)
				}
			}
		})
	}
}
func compressed(t *testing.T, kind string, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	if kind == "br" {
		w = brotli.NewWriter(&buf)
	} else {
		w = gzip.NewWriter(&buf)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func envelope(flag byte, b []byte) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{flag}, uint32(len(b))), b...)
}
func TestUnaryAndStreamingCompression(t *testing.T) {
	for _, kind := range []string{"gzip", "br"} {
		t.Run(kind, func(t *testing.T) {
			payload, _ := proto.Marshal(&aiserverv1.IssueGrokBotUserComputerCredentialResponse{Credential: strings.Repeat("credential", 100)})
			event, _ := proto.Marshal(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{PendingRequestCount: 7}}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/WatchGrokBotUserComputerRequests") {
					if !strings.Contains(r.Header.Get("Connect-Accept-Encoding"), kind) {
						t.Error("not advertised", r.Header)
					}
					w.Header().Set("Content-Type", "application/connect+proto")
					w.Header().Set("Connect-Content-Encoding", kind)
					w.Write(envelope(1, compressed(t, kind, event)))
					w.Write(envelope(2, []byte(`{}`)))
				} else {
					if !strings.Contains(r.Header.Get("Accept-Encoding"), kind) {
						t.Error("not advertised", r.Header)
					}
					w.Header().Set("Content-Type", "application/proto")
					w.Header().Set("Content-Encoding", kind)
					w.Write(compressed(t, kind, payload))
				}
			}))
			defer server.Close()
			client := rpc.NewGrokBotServiceClient(server.Client(), server.URL, Compression())
			r, err := client.IssueGrokBotUserComputerCredential(context.Background(), connect.NewRequest(&aiserverv1.IssueGrokBotUserComputerCredentialRequest{}))
			if err != nil || len(r.Msg.Credential) != 1000 {
				t.Fatal(r, err)
			}
			stream, err := client.WatchGrokBotUserComputerRequests(context.Background(), connect.NewRequest(&aiserverv1.WatchGrokBotUserComputerRequestsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if !stream.Receive() || stream.Msg().GetConnected().PendingRequestCount != 7 {
				t.Fatal(stream.Err())
			}
			if stream.Receive() || stream.Err() != nil {
				t.Fatal(stream.Err())
			}
			limited := rpc.NewGrokBotServiceClient(server.Client(), server.URL, Compression(), connect.WithReadMaxBytes(32))
			if _, err = limited.IssueGrokBotUserComputerCredential(context.Background(), connect.NewRequest(&aiserverv1.IssueGrokBotUserComputerCredentialRequest{})); err == nil {
				t.Fatal("decompression limit ignored")
			}
		})
	}
}
func TestCorruptBrotliAndStreamingError(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/connect+proto")
			w.Header().Set("Connect-Content-Encoding", "br")
			if corrupt {
				w.Write(envelope(1, []byte("invalid br")))
			} else {
				w.Write(envelope(3, compressed(t, "br", []byte(`{"error":{"code":"unauthenticated","message":"expired"}}`))))
			}
		}))
		client := rpc.NewGrokBotServiceClient(server.Client(), server.URL, Compression())
		stream, err := client.WatchGrokBotUserComputerRequests(context.Background(), connect.NewRequest(&aiserverv1.WatchGrokBotUserComputerRequestsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if stream.Receive() || stream.Err() == nil {
			t.Fatal("expected error")
		}
		if !corrupt && connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
			t.Fatal(stream.Err())
		}
		stream.Close()
		server.Close()
	}
}
func TestPrivacyLookupCacheAndFallback(t *testing.T) {
	calls := 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Ghost-Mode") != "true" {
			t.Error("privacy lookup not private")
		}
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/proto")
		b, _ := proto.Marshal(&aiserverv1.GetUserPrivacyModeResponse{PrivacyMode: aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_DATA_TRAINING_ALLOWED})
		w.Write(b)
	}))
	defer server.Close()
	privacy := &Privacy{}
	httpClient := &http.Client{Transport: Transport{Base: http.DefaultTransport, Privacy: privacy}}
	privacy.Client = rpc.NewDashboardServiceClient(httpClient, server.URL)
	for range 2 {
		if privacy.Ghost(context.Background()) != "false" {
			t.Fatal("privacy mode ignored")
		}
	}
	if calls != 1 {
		t.Fatal("cache failed", calls)
	}
	fail = true
	privacy.mu.Lock()
	privacy.expires = privacy.expires.AddDate(-1, 0, 0)
	privacy.mu.Unlock()
	if privacy.Ghost(context.Background()) != "true" {
		t.Fatal("unsafe fallback")
	}
}

func TestAuthRESTCompression(t *testing.T) {
	for _, kind := range []string{"gzip", "br", "deflate"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", kind)
				if kind == "deflate" {
					out := zlib.NewWriter(w)
					out.Write([]byte(`{"ok":true}`))
					out.Close()
				} else {
					w.Write(compressed(t, kind, []byte(`{"ok":true}`)))
				}
			}))
			defer server.Close()
			client := &http.Client{Transport: Transport{Base: http.DefaultTransport, Profile: AuthREST}}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			r, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			b, err := io.ReadAll(r.Body)
			if err != nil || string(b) != `{"ok":true}` {
				t.Fatal(string(b), err)
			}
		})
	}
}
