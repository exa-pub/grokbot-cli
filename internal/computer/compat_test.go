package computer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/exa-pub/grokbot-cli/internal/config"
	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-cli/internal/permission"
)

func testSession(t *testing.T) *session {
	t.Helper()
	pv := &Provider{Policy: &permission.Policy{Mode: "always"}, Store: &config.Store{Dir: t.TempDir()}, Now: func() time.Time { return time.Unix(1000, 0) }, Jitter: func(time.Duration) time.Duration { return 0 }}
	s := newSession(context.Background(), pv)
	t.Cleanup(s.close)
	return s
}
func TestWatchEventsAndDeadlines(t *testing.T) {
	s := testSession(t)
	s.generation = 4
	s.onEvent(watched{generation: 3, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{Notify: &aiserverv1.WatchGrokBotUserComputerNotify{}}}})
	if s.needPoll {
		t.Fatal("stale event used")
	}
	s.onEvent(watched{generation: 4, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{}}}})
	if s.needPoll {
		t.Fatal("empty heartbeat caused poll")
	}
	s.needPoll = true
	s.onEvent(watched{generation: 4, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{}}}})
	if !s.needPoll {
		t.Fatal("empty heartbeat lost scheduled poll")
	}
	if s.watchTimeout() != 35*time.Second {
		t.Fatal(s.watchTimeout())
	}
	s.onEvent(watched{generation: 4, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}}})
	if s.watchTimeout() != 15*time.Second {
		t.Fatal(s.watchTimeout())
	}
	s.needPoll = false
	s.onEvent(watched{generation: 4, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{PendingRequestCount: 1}}}})
	if !s.needPoll {
		t.Fatal("nonempty heartbeat ignored")
	}
	s.onEvent(watched{generation: 4, err: errors.New("network")})
	if s.watchAlive || s.nextDial != s.now().Add(time.Second) || s.backoff != 2*time.Second {
		t.Fatal(s.nextDial, s.backoff)
	}
}
func TestBatchFairnessAndByteBudget(t *testing.T) {
	s := testSession(t)
	for _, id := range []string{"a", "b"} {
		for i := 0; i < 3; i++ {
			s.queue(&responseFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{File: &aiserverv1.GrokBotUserComputerFile{Data: make([]byte, 1<<20), Seq: uint32(i)}}})
		}
	}
	batch := s.batch()
	if len(batch) != 1 || batch[0].RequestId != "a" {
		t.Fatal("wrong first file batch")
	}
	s.pending = s.pending[1:]
	batch = s.batch()
	if len(batch) != 1 {
		t.Fatal("file bytes budget ignored")
	}
	s.queue(&responseFrame{RequestId: "c", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: `{"streamClose":{}}`}}})
	batch = s.batch()
	if batch[0].RequestId != "c" {
		t.Fatal("metadata starved by file transfer")
	}
	var data int
	for _, f := range batch {
		data += len(f.GetFile().GetData())
	}
	if data > 1<<20 {
		t.Fatal(data)
	}
	for i := range 300 {
		s.queue(&responseFrame{RequestId: fmt.Sprint(i), Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesAccepted{MessagesAccepted: &aiserverv1.GrokBotUserComputerMessagesAccepted{}}})
	}
	batch = s.batch()
	if len(batch) > 256 {
		t.Fatal("frame budget ignored")
	}
}
func TestCancelBacklogTombstoneAndPendingKeepalive(t *testing.T) {
	s := testSession(t)
	s.credential.Credential = "c"
	s.active = maxActive
	frame := &requestFrame{RequestId: "canceled", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: `{"readArgs":{"path":"x"}}`}}}
	s.accept(frame)
	if len(s.backlog) != 1 {
		t.Fatal("missing backlog")
	}
	s.queue(&responseFrame{RequestId: "canceled", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: "old"}}})
	s.accept(&requestFrame{RequestId: "canceled", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Cancel{Cancel: &aiserverv1.GrokBotUserComputerCancel{}}})
	if len(s.backlog) != 0 || s.tasks["canceled"] != nil || len(s.pending) != 1 || s.pending[0].GetControl() == nil {
		t.Fatal("cancel did not discard backlog/replies")
	}
	s.accept(frame)
	if len(s.backlog) != 0 {
		t.Fatal("canceled work restarted")
	}
	s.queue(&responseFrame{RequestId: "unsent", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: "result"}}})
	s.keepAlive()
	found := false
	for _, f := range s.pending {
		found = found || (f.RequestId == "unsent" && f.GetMessagesAccepted() != nil)
	}
	if !found {
		t.Fatal("unsent response not kept alive")
	}
}
func TestCredentialCacheScopeAndInvalidation(t *testing.T) {
	store := &config.Store{Dir: t.TempDir()}
	now := time.Now()
	valid := config.Computer{Credential: "cached", AccountScope: "account", Backend: "https://api.example", MachineID: "machine", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	if err := store.Save("computer", valid); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"account", "other"} {
		pv := &Provider{Store: store, AccountScope: scope, Backend: valid.Backend, Machine: config.Machine{MachineID: valid.MachineID}}
		s := newSession(context.Background(), pv)
		if (s.credential.Credential != "") != (scope == "account") {
			t.Fatal("cross-account cache use")
		}
		if scope == "account" {
			s.invalidate(connect.NewError(connect.CodeUnauthenticated, errors.New("expired")))
			var c config.Computer
			store.Load("computer", &c)
			if c.Credential != "" {
				t.Fatal("rejected credential persisted")
			}
			store.Save("computer", valid)
		}
		s.close()
	}
	legacy := valid
	legacy.AccountScope = ""
	store.Save("computer", legacy)
	s := newSession(context.Background(), &Provider{Store: store, AccountScope: "account", Backend: valid.Backend, Machine: config.Machine{MachineID: valid.MachineID}})
	defer s.close()
	if s.credential.Credential != "" {
		t.Fatal("legacy cache accepted")
	}
}
func TestGenericFailedPreconditionIsNotRemoval(t *testing.T) {
	err := connect.NewError(connect.CodeFailedPrecondition, errors.New("temporary registration state"))
	if removedComputer(err) {
		t.Fatal("generic precondition treated as removed")
	}
}

func TestPartialSubmitDrainsAndPreservesOrder(t *testing.T) {
	calls := 0
	var order []uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req aiserverv1.SubmitGrokBotUserComputerResponsesRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			t.Error(err)
		}
		calls++
		if len(req.Frames) == 0 {
			t.Error("empty submit")
		}
		order = append(order, req.Frames[0].GetFile().GetSeq())
		w.Header().Set("Content-Type", "application/proto")
		b, _ = proto.Marshal(&aiserverv1.SubmitGrokBotUserComputerResponsesResponse{AcceptedCount: 1})
		w.Write(b)
	}))
	defer server.Close()
	s := testSession(t)
	s.credential.Credential = "c"
	s.pv.Channel = rpc.NewGrokBotServiceClient(server.Client(), server.URL)
	for i := range 7 {
		s.queue(&responseFrame{RequestId: "a", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{File: &aiserverv1.GrokBotUserComputerFile{Seq: uint32(i), Data: []byte("x")}}})
	}
	if err := s.submit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 0 || calls != 7 {
		t.Fatal(len(s.pending), calls)
	}
	for i, v := range order {
		if v != uint32(i) {
			t.Fatal(order)
		}
	}
}

func TestInflightLimitAndShutdownCancel(t *testing.T) {
	s := testSession(t)
	s.active = 8
	for i := range 65 {
		s.accept(&requestFrame{RequestId: fmt.Sprint(i), Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: `{"readArgs":{"path":"x"}}`}}})
	}
	if len(s.backlog) != 64 || len(s.tasks) != 64 || len(s.pending) != 1 {
		t.Fatal(len(s.backlog), len(s.tasks), len(s.pending))
	}
	s.active = 0
	s.shutdown()
	if len(s.tasks) != 0 {
		t.Fatal("queued tasks not canceled")
	}
}

func TestRemovedComputerDetailAndWatchClock(t *testing.T) {
	err := connect.NewError(connect.CodeFailedPrecondition, errors.New("removed"))
	detail, _ := connect.NewErrorDetail(&anypb.Any{TypeUrl: "type.googleapis.com/aiserver.v1.GrokBotUserComputerRemovedDetail"})
	err.AddDetail(detail)
	if !removedComputer(err) {
		t.Fatal("removed detail not recognized")
	}
	s := testSession(t)
	now := s.now()
	s.pv.Now = func() time.Time { return now }
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	s.watchAlive = true
	s.lastStanding = "always"
	s.lastEvent = now
	now = now.Add(34 * time.Second)
	if e := s.onTick(context.Background()); e != nil || !s.watchAlive {
		t.Fatal("connect timeout fired early", e)
	}
	now = now.Add(2 * time.Second)
	if e := s.onTick(context.Background()); e != nil || s.watchAlive || s.nextDial != now.Add(2*time.Second) {
		t.Fatal("connect watchdog did not schedule reconnect", e)
	}
}
