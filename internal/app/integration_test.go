package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

type localCloud struct {
	rpc.UnimplementedDashboardServiceHandler
	rpc.UnimplementedGrokBotServiceHandler
	mu                     sync.Mutex
	queued                 bool
	clients, closed, files int
	read, shell            bool
	complete               chan struct{}
	once                   sync.Once
	t                      *testing.T
}

func (c *localCloud) GetMe(ctx context.Context, r *connect.Request[aiserverv1.GetMeRequest]) (*connect.Response[aiserverv1.GetMeResponse], error) {
	if !strings.HasPrefix(r.Header().Get("Authorization"), "Bearer ") {
		c.t.Error("profile bearer missing")
	}
	email := "test@example.invalid"
	return connect.NewResponse(&aiserverv1.GetMeResponse{AuthId: "fixture-account", Email: &email}), nil
}
func (c *localCloud) GetUserPrivacyMode(ctx context.Context, r *connect.Request[aiserverv1.GetUserPrivacyModeRequest]) (*connect.Response[aiserverv1.GetUserPrivacyModeResponse], error) {
	return connect.NewResponse(&aiserverv1.GetUserPrivacyModeResponse{PrivacyMode: aiserverv1.PrivacyMode_PRIVACY_MODE_NO_STORAGE}), nil
}
func (c *localCloud) RegisterSandMachine(ctx context.Context, r *connect.Request[aiserverv1.RegisterSandMachineRequest]) (*connect.Response[aiserverv1.RegisterSandMachineResponse], error) {
	if r.Header().Get("X-Cursor-Checksum") == "" {
		c.t.Error("registration checksum missing")
	}
	return connect.NewResponse(&aiserverv1.RegisterSandMachineResponse{}), nil
}
func (c *localCloud) IssueGrokBotUserComputerCredential(ctx context.Context, r *connect.Request[aiserverv1.IssueGrokBotUserComputerCredentialRequest]) (*connect.Response[aiserverv1.IssueGrokBotUserComputerCredentialResponse], error) {
	return connect.NewResponse(&aiserverv1.IssueGrokBotUserComputerCredentialResponse{Credential: "fixture-credential", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()}), nil
}
func (c *localCloud) WatchGrokBotUserComputerRequests(ctx context.Context, r *connect.Request[aiserverv1.WatchGrokBotUserComputerRequestsRequest], stream *connect.ServerStream[aiserverv1.WatchGrokBotUserComputerRequestsEvent]) error {
	for _, key := range []string{"Authorization", "X-Cursor-Checksum", "X-Ghost-Mode", "X-Request-Id"} {
		if r.Header().Get(key) != "" {
			c.t.Error("credential channel header", key)
		}
	}
	if r.Header().Get("X-Sand-Box-Namespace") != "prod" {
		c.t.Error("namespace missing")
	}
	if err := stream.Send(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}}); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}
func (c *localCloud) PollGrokBotUserComputerRequests(ctx context.Context, r *connect.Request[aiserverv1.PollGrokBotUserComputerRequestsRequest]) (*connect.Response[aiserverv1.PollGrokBotUserComputerRequestsResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := &aiserverv1.PollGrokBotUserComputerRequestsResponse{}
	if !c.queued {
		c.queued = true
		for i, raw := range []string{`{"id":900,"shellStreamArgs":{"command":"printf shell-ok"}}`, `{"id":900,"readArgs":{"path":"hello"}}`, `{"id":900,"lsArgs":{"path":"."}}`} {
			out.Requests = append(out.Requests, &aiserverv1.GrokBotUserComputerQueuedRequest{Id: fmt.Sprint(i), Frame: &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: fmt.Sprint(i), Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: raw}}}})
		}
		out.Requests = append(out.Requests, &aiserverv1.GrokBotUserComputerQueuedRequest{Id: "u", Frame: &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "u", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "uploaded", Data: []byte("upload-ok")}}}}, &aiserverv1.GrokBotUserComputerQueuedRequest{Id: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{Download: &aiserverv1.GrokBotUserComputerDownload{Path: "hello"}}}})
	}
	return connect.NewResponse(out), nil
}
func (c *localCloud) SubmitGrokBotUserComputerResponses(ctx context.Context, r *connect.Request[aiserverv1.SubmitGrokBotUserComputerResponsesRequest]) (*connect.Response[aiserverv1.SubmitGrokBotUserComputerResponsesResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range r.Msg.Frames {
		if v := f.GetClient(); v != nil {
			c.clients++
			var msg agentv1.ExecClientMessage
			if err := protojson.Unmarshal([]byte(v.MessageJson), &msg); err != nil {
				c.t.Error(err)
			}
			c.read = c.read || msg.GetReadResult().GetSuccess().GetContent() == "world"
			c.shell = c.shell || msg.GetShellStream().GetStdout().GetData() == "shell-ok"
		}
		if v := f.GetControl(); v != nil {
			var msg agentv1.ExecClientControlMessage
			_ = protojson.Unmarshal([]byte(v.MessageJson), &msg)
			if msg.GetStreamClose() != nil {
				c.closed++
			}
			if msg.GetThrow() != nil {
				c.t.Error(msg.GetThrow())
			}
		}
		if f.GetFile() != nil {
			c.files++
		}
	}
	if c.read && c.shell && c.closed == 3 && c.files == 2 {
		c.once.Do(func() { close(c.complete) })
	}
	return connect.NewResponse(&aiserverv1.SubmitGrokBotUserComputerResponsesResponse{AcceptedCount: uint32(len(r.Msg.Frames))}), nil
}

// listenNotify points NOTIFY_SOCKET at a socket like systemd's for
// Type=notify services.
func listenNotify(t *testing.T) *net.UnixConn {
	t.Helper()
	addr := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", addr)
	return conn
}

func readNotify(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestLoginConnectEndToEndLocal(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(root, "hello"), []byte("world"), 0600)
	cloud := &localCloud{t: t, complete: make(chan struct{})}
	mux := http.NewServeMux()
	path, handler := rpc.NewDashboardServiceHandler(cloud)
	mux.Handle(path, handler)
	path, handler = rpc.NewGrokBotServiceHandler(cloud)
	mux.Handle(path, handler)
	token := "fixture." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"sub":"fixture-account"}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "undici" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("auth header profile", r.Header)
		}
		json.NewEncoder(w).Encode(map[string]string{"accessToken": token, "refreshToken": "fixture-refresh", "authId": "fixture-account"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("SAND_BACKEND_URL", server.URL)
	t.Setenv("SAND_CURSOR_WEBSITE_URL", "https://example.invalid")
	var out bytes.Buffer
	if err := Login(context.Background(), LoginOptions{NoBrowser: true}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Logged in as test@example.invalid") {
		t.Fatal(out.String())
	}
	notifications := listenNotify(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Connect(ctx, ConnectOptions{Root: root, Permission: "always"}) }()
	select {
	case <-cloud.complete:
	case <-time.After(5 * time.Second):
		t.Fatal("local cloud requests did not complete")
	}
	if got := readNotify(t, notifications); got != "READY=1" {
		t.Fatal("systemd notification", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("graceful shutdown failed")
	}
	if got := readNotify(t, notifications); got != "STOPPING=1" {
		t.Fatal("systemd notification", got)
	}
	b, err := os.ReadFile(filepath.Join(root, "uploaded"))
	if err != nil || string(b) != "upload-ok" {
		t.Fatal(string(b), err)
	}
}
