package computer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/config"
	"github.com/exa-pub/grokbot-tunnel/internal/executor"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/permission"
)

type fakeCloud struct {
	rpc.UnimplementedGrokBotServiceHandler
	mu            sync.Mutex
	polls, issues int
	acked         bool
	submitted     []*aiserverv1.GrokBotUserComputerResponseFrame
	finished      chan struct{}
	once          sync.Once
	t             *testing.T
}

func (s *fakeCloud) IssueGrokBotUserComputerCredential(ctx context.Context, r *connect.Request[aiserverv1.IssueGrokBotUserComputerCredentialRequest]) (*connect.Response[aiserverv1.IssueGrokBotUserComputerCredentialResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues++
	if r.Header().Get("Authorization") != "Bearer test" {
		s.t.Error("missing user bearer")
	}
	return connect.NewResponse(&aiserverv1.IssueGrokBotUserComputerCredentialResponse{Credential: fmt.Sprint(s.issues), ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()}), nil
}
func (s *fakeCloud) WatchGrokBotUserComputerRequests(ctx context.Context, r *connect.Request[aiserverv1.WatchGrokBotUserComputerRequestsRequest], stream *connect.ServerStream[aiserverv1.WatchGrokBotUserComputerRequestsEvent]) error {
	if r.Header().Get("Authorization") != "" {
		s.t.Error("credential channel used bearer")
	}
	if !r.Msg.Hello.Capabilities.GetAiToolSetup() || r.Msg.Hello.Capabilities.GetMessagesOp() || r.Msg.Hello.Capabilities.GetMessagesOpGeneration() != 2 || r.Msg.Hello.Capabilities.Shell != nil {
		s.t.Error("unsupported capability advertised")
	}
	if err := stream.Send(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}}); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}
func (s *fakeCloud) PollGrokBotUserComputerRequests(ctx context.Context, r *connect.Request[aiserverv1.PollGrokBotUserComputerRequestsRequest]) (*connect.Response[aiserverv1.PollGrokBotUserComputerRequestsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	for _, id := range r.Msg.AckIds {
		if id == "queue-1" {
			s.acked = true
		}
	}
	for _, f := range s.submitted {
		if s.acked && f.GetClient() != nil {
			s.once.Do(func() { close(s.finished) })
		}
	}
	out := &aiserverv1.PollGrokBotUserComputerRequestsResponse{}
	if s.polls <= 2 {
		out.Requests = []*aiserverv1.GrokBotUserComputerQueuedRequest{{Id: "queue-1", Frame: &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "read-1", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: `{"id":1,"readArgs":{"path":"hello"}}`}}}}}
	}
	return connect.NewResponse(out), nil
}
func (s *fakeCloud) SubmitGrokBotUserComputerResponses(ctx context.Context, r *connect.Request[aiserverv1.SubmitGrokBotUserComputerResponsesRequest]) (*connect.Response[aiserverv1.SubmitGrokBotUserComputerResponsesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header().Get("Authorization") != "" {
		s.t.Error("submit used bearer")
	}
	if r.Msg.Credential == "1" {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("expired"))
	}
	s.submitted = append(s.submitted, r.Msg.Frames...)
	for _, f := range r.Msg.Frames {
		if s.acked && f.GetClient() != nil && strings.Contains(f.GetClient().MessageJson, "world") {
			s.once.Do(func() { close(s.finished) })
		}
	}
	return connect.NewResponse(&aiserverv1.SubmitGrokBotUserComputerResponsesResponse{AcceptedCount: uint32(len(r.Msg.Frames))}), nil
}
func TestQueueAckDedupCredentialRecoveryAndShutdown(t *testing.T) {
	cloud := &fakeCloud{t: t, finished: make(chan struct{})}
	path, handler := rpc.NewGrokBotServiceHandler(cloud)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	policy := &permission.Policy{Mode: "always"}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello"), []byte("world"), 0600); err != nil {
		t.Fatal(err)
	}
	ex, err := executor.New(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	userInterceptor := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			r.Header().Set("Authorization", "Bearer test")
			return next(ctx, r)
		}
	})
	pv := &Provider{User: rpc.NewGrokBotServiceClient(server.Client(), server.URL, connect.WithInterceptors(userInterceptor)), Channel: rpc.NewGrokBotServiceClient(server.Client(), server.URL), Store: &config.Store{Dir: t.TempDir()}, Machine: config.Machine{MachineID: "m", Label: "test"}, Executor: ex, Policy: policy}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pv.Run(ctx) }()
	select {
	case <-cloud.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("request was not delivered")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stalled")
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if !cloud.acked || cloud.issues != 2 {
		t.Fatalf("ack=%v issues=%d", cloud.acked, cloud.issues)
	}
	count := 0
	for _, f := range cloud.submitted {
		if f.GetClient() != nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate result: %d", count)
	}
}
