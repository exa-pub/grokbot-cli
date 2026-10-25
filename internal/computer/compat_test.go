package computer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/exa-pub/grokbot-cli/internal/config"
	"github.com/exa-pub/grokbot-cli/internal/executor"
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
	s.accept(frame, false)
	if len(s.backlog) != 1 {
		t.Fatal("missing backlog")
	}
	s.queue(&responseFrame{RequestId: "canceled", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: "old"}}})
	s.accept(&requestFrame{RequestId: "canceled", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Cancel{Cancel: &aiserverv1.GrokBotUserComputerCancel{}}}, false)
	if len(s.backlog) != 0 || s.tasks["canceled"] != nil || len(s.pending) != 1 || s.pending[0].GetControl() == nil {
		t.Fatal("cancel did not discard backlog/replies")
	}
	s.accept(frame, false)
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
		s.accept(&requestFrame{RequestId: fmt.Sprint(i), Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: `{"readArgs":{"path":"x"}}`}}}, false)
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

// stubCloud is a configurable server for session-level tests.
type stubCloud struct {
	rpc.UnimplementedGrokBotServiceHandler
	mu                     sync.Mutex
	issues, submits, polls int
	ttl                    time.Duration
	failIssue, failSubmit  bool
	failPoll               bool
	submitted              []*responseFrame
	block                  chan struct{} // holds submits until closed
	unauthenticated        bool          // submits are rejected
	rejectWatch            bool          // watches are rejected
	connect                bool          // watches report connected
	blockIssue             chan struct{} // holds issues until closed
}

func (c *stubCloud) IssueGrokBotUserComputerCredential(ctx context.Context, r *connect.Request[aiserverv1.IssueGrokBotUserComputerCredentialRequest]) (*connect.Response[aiserverv1.IssueGrokBotUserComputerCredentialResponse], error) {
	if c.blockIssue != nil {
		select {
		case <-c.blockIssue:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issues++
	if c.failIssue {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	return connect.NewResponse(&aiserverv1.IssueGrokBotUserComputerCredentialResponse{Credential: fmt.Sprint(c.issues), ExpiresAtMs: time.Now().Add(c.ttl).UnixMilli()}), nil
}
func (c *stubCloud) SubmitGrokBotUserComputerResponses(ctx context.Context, r *connect.Request[aiserverv1.SubmitGrokBotUserComputerResponsesRequest]) (*connect.Response[aiserverv1.SubmitGrokBotUserComputerResponsesResponse], error) {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submits++
	if c.unauthenticated {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("expired"))
	}
	if c.failSubmit {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	c.submitted = append(c.submitted, r.Msg.Frames...)
	return connect.NewResponse(&aiserverv1.SubmitGrokBotUserComputerResponsesResponse{AcceptedCount: uint32(len(r.Msg.Frames))}), nil
}
func (c *stubCloud) PollGrokBotUserComputerRequests(ctx context.Context, r *connect.Request[aiserverv1.PollGrokBotUserComputerRequestsRequest]) (*connect.Response[aiserverv1.PollGrokBotUserComputerRequestsResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polls++
	if c.failPoll {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	return connect.NewResponse(&aiserverv1.PollGrokBotUserComputerRequestsResponse{}), nil
}
func (c *stubCloud) WatchGrokBotUserComputerRequests(ctx context.Context, r *connect.Request[aiserverv1.WatchGrokBotUserComputerRequestsRequest], stream *connect.ServerStream[aiserverv1.WatchGrokBotUserComputerRequestsEvent]) error {
	if c.rejectWatch {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("rejected"))
	}
	if c.connect {
		if err := stream.Send(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}
func (c *stubCloud) counts() (issues, submits, polls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issues, c.submits, c.polls
}

// stubServer serves c over HTTP until the test ends.
func stubServer(t *testing.T, c *stubCloud) *httptest.Server {
	t.Helper()
	path, h := rpc.NewGrokBotServiceHandler(c)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// stubSession returns a session talking to c whose clock is controlled by *now.
func stubSession(t *testing.T, c *stubCloud, now *time.Time) *session {
	t.Helper()
	srv := stubServer(t, c)
	s := testSession(t)
	*now = time.Now()
	s.pv.Now = func() time.Time { return *now }
	cl := rpc.NewGrokBotServiceClient(srv.Client(), srv.URL)
	s.pv.User, s.pv.Channel = cl, cl
	ex, err := executor.New(t.TempDir(), s.pv.Policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	s.pv.Executor = ex
	return s
}

// settle waits for the issue, submit and poll in flight, applying their results like
// Run does, and returns the first error.
func settle(t *testing.T, s *session) error {
	t.Helper()
	var first error
	for s.submitting || s.polling || s.issuing {
		select {
		case r := <-s.issued:
			if err := s.onIssued(r); err != nil && first == nil {
				first = err
			}
		case r := <-s.submitted:
			if err := s.onSubmitted(r); err != nil && first == nil {
				first = err
			}
		case r := <-s.polled:
			s.onPolled(r)
		case <-time.After(5 * time.Second):
			t.Fatal("RPC did not finish")
		}
	}
	return first
}

// tick runs onTick and settles the RPCs it started.
func tick(t *testing.T, s *session) error {
	t.Helper()
	if err := s.onTick(context.Background()); err != nil {
		return err
	}
	return settle(t, s)
}

func closeFrame(id string) *responseFrame {
	return &responseFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: `{"streamClose":{"id":3}}`}}}
}

func TestKeepAliveIsBounded(t *testing.T) {
	var now time.Time
	c := &stubCloud{failSubmit: true}
	s := stubSession(t, c, &now)
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	for i := range 72 {
		s.tasks[fmt.Sprint(i)] = &task{cancel: func() {}}
	}
	s.tasks["queued"] = &task{}
	s.queue(closeFrame("closed"))
	for range 10 {
		s.keepAlive()
	}
	// A full round of failed submits must not end the session.
	for range submitAttempts {
		s.nextSubmit = time.Time{}
		s.startSubmit()
		if err := settle(t, s); errors.Is(err, errBacklogFull) {
			t.Fatal(err)
		}
	}
	if _, submits, _ := c.counts(); submits != submitAttempts {
		t.Fatal(submits)
	}
	alive := map[string]int{}
	for _, f := range s.pending {
		if f.GetMessagesAccepted() != nil {
			alive[f.RequestId]++
		}
	}
	if len(alive) != 72 || alive["queued"] != 0 || alive["closed"] != 0 {
		t.Fatal(len(alive), alive["queued"], alive["closed"])
	}
	for id, n := range alive {
		if n != 1 {
			t.Fatalf("%s has %d keepalives", id, n)
		}
	}
	// Keepalives alone never exhaust the backlog.
	for i := range maxPending {
		s.queue(&responseFrame{RequestId: fmt.Sprint(i), Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesAccepted{MessagesAccepted: &aiserverv1.GrokBotUserComputerMessagesAccepted{}}})
	}
	if s.stuck() != 1 {
		t.Fatal(s.stuck())
	}
}

func TestCancelKeepsCloseOfFinishedRequest(t *testing.T) {
	s := testSession(t)
	s.queue(&responseFrame{RequestId: "fin", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: "result"}}})
	s.queue(closeFrame("fin"))
	s.accept(&requestFrame{RequestId: "fin", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Cancel{Cancel: &aiserverv1.GrokBotUserComputerCancel{}}}, false)
	if len(s.pending) != 1 || s.pending[0].GetControl() == nil {
		t.Fatal("cancel purged the stream close", s.pending)
	}
}

func TestCanceledRunningRequestReportsTerminalFrame(t *testing.T) {
	s := testSession(t)
	download := &aiserverv1.GrokBotUserComputerRequestFrame_Download{Download: &aiserverv1.GrokBotUserComputerDownload{Path: "x"}}
	s.canceled.add("run")
	if err := executor.Reject(&requestFrame{RequestId: "run", Frame: download}, context.Canceled, func(f *responseFrame) error { s.take(f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 1 || s.pending[0].GetFileError() == nil {
		t.Fatal("terminal error of canceled request dropped", s.pending)
	}
	s.pending = nil
	s.canceled.add("dl")
	s.take(&responseFrame{RequestId: "dl", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{File: &aiserverv1.GrokBotUserComputerFile{Data: []byte("a")}}})
	s.take(&responseFrame{RequestId: "dl", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{File: &aiserverv1.GrokBotUserComputerFile{Data: []byte("b"), Seq: 1, Last: true}}})
	if len(s.pending) != 1 || s.pending[0].GetFileError() == nil {
		t.Fatal("truncated transfer reported as complete", s.pending)
	}
	// A last chunk already pending when the cancel arrives is replaced too.
	s.pending = nil
	s.tasks["late"] = &task{cancel: func() {}}
	s.queue(&responseFrame{RequestId: "late", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{File: &aiserverv1.GrokBotUserComputerFile{Data: []byte("a")}}})
	s.queue(&responseFrame{RequestId: "late", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{File: &aiserverv1.GrokBotUserComputerFile{Data: []byte("b"), Seq: 1, Last: true}}})
	s.cancel("late")
	if len(s.pending) != 1 || s.pending[0].GetFileError() == nil {
		t.Fatal("pending terminal frame of canceled request lost", s.pending)
	}
}

func TestShortCredentialRenewedHalfway(t *testing.T) {
	var now time.Time
	c := &stubCloud{ttl: 4 * time.Minute}
	s := stubSession(t, c, &now)
	for range 5 {
		if err := tick(t, s); err != nil {
			t.Fatal(err)
		}
	}
	if issues, _, _ := c.counts(); issues != 1 {
		t.Fatalf("credential reissued %d times", issues)
	}
	now = now.Add(time.Minute)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 1 {
		t.Fatal("renewed too early")
	}
	now = now.Add(61 * time.Second)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 2 {
		t.Fatal("not renewed halfway", issues)
	}
}

func TestFailedRenewalKeepsSessionWorking(t *testing.T) {
	var now time.Time
	c := &stubCloud{failIssue: true}
	s := stubSession(t, c, &now)
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(4 * time.Minute).UnixMilli()}
	s.watchAlive, s.lastStanding, s.lastEvent = true, "always", now
	s.queue(&responseFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesError{MessagesError: &aiserverv1.GrokBotUserComputerMessagesError{}}})
	for range 3 {
		if err := tick(t, s); err != nil {
			t.Fatal(err)
		}
	}
	issues, submits, polls := c.counts()
	if issues != 1 || submits != 1 || len(s.pending) != 0 {
		t.Fatalf("issues=%d submits=%d pending=%d", issues, submits, len(s.pending))
	}
	if !s.nextDial.IsZero() || !s.watchAlive {
		t.Fatal("renewal failure delayed the watch", s.nextDial)
	}
	s.needPoll = true
	tick(t, s)
	if _, _, p := c.counts(); p != polls+1 {
		t.Fatal("poll blocked by failed renewal")
	}
	now = now.Add(issueBackoff - time.Second)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 1 {
		t.Fatal("renewal retried before backoff", issues)
	}
	now = now.Add(time.Second)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 2 {
		t.Fatal("renewal not retried after backoff", issues)
	}
	// Once the old credential expires, nothing is sent with it.
	now = now.Add(5 * time.Minute)
	s.queue(&responseFrame{RequestId: "r2"})
	tick(t, s)
	if _, submits, _ := c.counts(); submits != 1 || len(s.pending) != 1 {
		t.Fatal("expired credential used")
	}
}

func TestQueuedEventPreventsStaleDrop(t *testing.T) {
	s := testSession(t)
	now := s.now()
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	s.watchAlive, s.watchConnected, s.lastStanding = true, true, "always"
	s.lastEvent = now.Add(-16 * time.Second)
	s.events <- watched{generation: s.generation, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{}}}}
	if err := s.onTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.watchAlive {
		t.Fatal("live stream dropped while its event was queued")
	}
}

func TestShutdownFlushesDespiteSlowWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the worker grace period")
	}
	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	s.active = 1 // never finishes
	s.queue(closeFrame("done"))
	start := time.Now()
	s.shutdown()
	if _, submits, _ := c.counts(); submits != 1 || len(s.pending) != 0 {
		t.Fatal("final submit skipped", submits)
	}
	if d := time.Since(start); d > shutdownGrace {
		t.Fatal("shutdown overran its grace", d)
	}
}

func TestPollFailureRetriesWithBackoff(t *testing.T) {
	var now time.Time
	c := &stubCloud{failPoll: true}
	s := stubSession(t, c, &now)
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	s.watchAlive, s.lastStanding, s.lastEvent = true, "always", now
	s.needPoll = true
	tick(t, s)
	tick(t, s)
	if _, _, polls := c.counts(); polls != 1 || !s.needPoll {
		t.Fatal("failed poll not rescheduled with backoff", polls)
	}
	now = now.Add(time.Second)
	tick(t, s)
	if _, _, polls := c.counts(); polls != 2 {
		t.Fatal("failed poll not retried", polls)
	}
}

func TestServerAuthoritativeStandingChangeKeepsWatch(t *testing.T) {
	var now time.Time
	s := stubSession(t, &stubCloud{}, &now)
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli(), ServerAuthoritative: true}
	s.startWatch()
	gen := s.generation
	s.pv.Policy.Mode = permission.ModeAsk
	if err := s.onTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.generation != gen {
		t.Fatal("reconnected for a standing the server ignores")
	}
	s.credential.ServerAuthoritative = false
	s.onTick(context.Background())
	if s.generation == gen {
		t.Fatal("standing change not announced")
	}
}

func TestSubmitRunsOffTheRunGoroutine(t *testing.T) {
	var now time.Time
	c := &stubCloud{block: make(chan struct{})}
	s := stubSession(t, c, &now)
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	s.queue(closeFrame("a"))
	start := time.Now()
	s.startSubmit()
	if !s.submitting || time.Since(start) > time.Second {
		t.Fatal("submit blocked the caller")
	}
	for i := range submitBatch {
		s.queue(closeFrame(fmt.Sprint(i)))
	}
	s.startSubmit()
	if s.acceptReplies() != nil {
		t.Fatal("replies not throttled while a full batch waits")
	}
	close(c.block)
	if err := settle(t, s); err != nil {
		t.Fatal(err)
	}
	if _, submits, _ := c.counts(); submits != 2 || len(s.pending) != 0 || len(c.submitted) != submitBatch+1 {
		t.Fatal("pending not drained one submit at a time", submits, len(s.pending))
	}
	if s.acceptReplies() == nil {
		t.Fatal("replies still throttled")
	}
}

func TestStaleSubmitResultKeepsNewCredential(t *testing.T) {
	var now time.Time
	c := &stubCloud{block: make(chan struct{}), unauthenticated: true}
	s := stubSession(t, c, &now)
	s.credential = config.Computer{Credential: "old", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	s.queue(closeFrame("a"))
	s.startSubmit()
	s.credential = config.Computer{Credential: "new", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	close(c.block)
	r := <-s.submitted
	if err := s.onSubmitted(r); err != nil {
		t.Fatal(err)
	}
	if s.credential.Credential != "new" || len(s.pending) != 1 {
		t.Fatal("rejection of the old credential dropped the new one")
	}
}
