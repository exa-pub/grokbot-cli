// Package computer manages the pull queue and its bounded execution lifecycle.
package computer

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
	"github.com/exa-pub/grokbot-tunnel/internal/executor"
	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/permission"
)

const (
	rpcTimeout = 20 * time.Second

	maxActive   = 8  // requests executing concurrently
	maxBacklog  = 64 // requests waiting for a free slot
	pollLimit   = 64
	dedupWindow = 4096 // queue IDs remembered for deduplication

	submitBatch    = 32  // pending frames that trigger an immediate submit
	maxPending     = 256 // pending frames that abort the session
	submitAttempts = 3

	// Credentials are renewed this long before they expire.
	credentialMargin = 5 * time.Minute
	// A watch stream without events for this long is reconnected.
	watchStale          = 15 * time.Second
	watchConnectTimeout = 35 * time.Second
	staleRedial         = 2 * time.Second
	minBackoff          = time.Second
	maxBackoff          = 60 * time.Second

	tickInterval      = 100 * time.Millisecond
	pollInterval      = 30 * time.Second
	keepAliveInterval = 2 * time.Second
	shutdownGrace     = 8 * time.Second
	// The presence is withdrawn on shutdown within this time, as the desktop's
	// user-computer-presence-withdraw deadline.
	withdrawTimeout = 750 * time.Millisecond
	// Part of shutdownGrace kept for the final submit after waiting for workers.
	finalFlushGrace = 2 * time.Second

	// Failed credential issues are retried like the desktop's gJe:
	// 30 s · 2^(n−1) up to 10 min, or Retry-After up to 15 min, plus jitter.
	issueBackoff    = 30 * time.Second
	maxIssueBackoff = 10 * time.Minute
	maxRetryAfter   = 15 * time.Minute
)

// channelRetry is how long the watch stays down after the server rejected the
// credential, as the desktop daemon's channelRetry. A variable so tests can
// shorten it.
var channelRetry = 10 * time.Second

var errBacklogFull = errors.New("response backlog full")

// Reasons given to the agent for requests that do not run, in the desktop's
// words.
//
//nolint:revive,staticcheck // user-facing sentences
var (
	errBusy    = errors.New("The user's computer is busy with other requests right now; try again shortly.")
	errStopped = errors.New("The user's computer stopped serving requests before this one started. Retry it once the computer is back.")
)

type (
	requestFrame  = aiserverv1.GrokBotUserComputerRequestFrame
	responseFrame = aiserverv1.GrokBotUserComputerResponseFrame
)

// Provider connects this machine to the server and executes its requests.
type Provider struct {
	User         rpc.GrokBotServiceClient // authenticated with the user's token
	Channel      rpc.GrokBotServiceClient // authenticated with the computer credential
	Store        *config.Store
	Machine      config.Machine
	Executor     *executor.Executor
	Policy       *permission.Policy
	Debug        bool
	Backend      string
	AccountScope string
	Now          func() time.Time
	Jitter       func(time.Duration) time.Duration
}

// Run serves requests until ctx is canceled or a fatal error occurs.
func (pv *Provider) Run(ctx context.Context) error {
	s := newSession(ctx, pv)
	defer s.close()
	return s.run(ctx)
}

func rpcContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, rpcTimeout)
}

func (pv *Provider) issue(ctx context.Context) (config.Computer, error) {
	ctx, cancel := rpcContext(ctx)
	defer cancel()
	req := &aiserverv1.IssueGrokBotUserComputerCredentialRequest{MachineId: pv.Machine.MachineID}
	r, err := pv.User.IssueGrokBotUserComputerCredential(ctx, connect.NewRequest(req))
	if err != nil {
		return config.Computer{}, err
	}
	c := config.Computer{
		AccountScope: pv.AccountScope, Backend: pv.Backend, MachineID: pv.Machine.MachineID,
		Credential:          r.Msg.Credential,
		ExpiresAtMS:         r.Msg.ExpiresAtMs,
		ServerAuthoritative: r.Msg.ServerAuthoritative,
	}
	if c.Credential == "" || c.ExpiresAtMS <= pv.now().UnixMilli() {
		return c, errors.New("server returned invalid computer credential")
	}
	// The file only caches the credential across restarts.
	if err := pv.Store.Save("computer", c); err != nil {
		log.Printf("cache computer credential: %v", err)
	}
	return c, nil
}

func (pv *Provider) hello(c config.Computer, standing string) *aiserverv1.GrokBotUserComputerHello {
	if c.ServerAuthoritative {
		standing = ""
	}
	setup := true
	generation := int32(4)
	return &aiserverv1.GrokBotUserComputerHello{
		Label:               pv.Machine.Label,
		LocalRoot:           pv.Executor.Root,
		TerminalsFolder:     pv.Executor.TerminalsFolder(),
		Standing:            standing,
		Variant:             "sand",
		ServerAuthoritative: c.ServerAuthoritative,
		Capabilities:        &aiserverv1.GrokBotUserComputerCapabilities{AiToolSetup: &setup, MessagesOpGeneration: &generation},
	}
}

type task struct {
	frame  *requestFrame
	cancel context.CancelFunc // nil until the task starts
	// server is the credential's server-authoritative mode when the request
	// arrived; the request's approval flags are only meaningful under it.
	server bool
}

// submitted is the outcome of a submit RPC run off the Run goroutine.
type submitted struct {
	credential string
	frames     []*responseFrame
	accepted   int
	err        error
}

// polled is the outcome of a poll RPC run off the Run goroutine.
type polled struct {
	credential string
	server     bool // the credential's ServerAuthoritative
	requests   []*aiserverv1.GrokBotUserComputerQueuedRequest
	err        error
}

// issued is the outcome of a credential issue run off the Run goroutine.
type issued struct {
	credential config.Computer
	err        error
}

// watched is an event or the terminal error of a watch stream.
type watched struct {
	event      *aiserverv1.WatchGrokBotUserComputerRequestsEvent
	err        error
	generation int
}

// dedup remembers the most recent queue IDs.
type dedup struct {
	seen  map[string]bool
	order []string
}

func newDedup() dedup { return dedup{seen: map[string]bool{}} }

func (d *dedup) has(id string) bool { return d.seen[id] }

// add reports whether id is new.
func (d *dedup) add(id string) bool {
	if d.seen[id] {
		return false
	}
	d.seen[id] = true
	d.order = append(d.order, id)
	if len(d.order) > dedupWindow {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

// session holds the state of Run. All fields are owned by the Run goroutine;
// workers and the submit and poll RPCs talk to it only through the channels.
type session struct {
	pv *Provider

	// presence identifies this session's watch streams to the server, which
	// forgets the computer's presence when it is withdrawn.
	presence string

	// work is canceled on shutdown and parents watch and task contexts.
	work     context.Context
	stopWork context.CancelFunc
	// life outlives work so canceled tasks can still report their results.
	life     context.Context
	stopLife context.CancelFunc

	replies chan *responseFrame
	done    chan string
	events  chan watched
	// At most one submit and one poll are in flight; their results come back
	// on these channels, buffered so an abandoned RPC never blocks.
	submitted chan submitted
	polled    chan polled
	issued    chan issued

	tasks    map[string]*task
	backlog  []string
	active   int
	seen     dedup            // queue IDs
	requests dedup            // request IDs ever accepted; redeliveries are ignored
	canceled dedup            // request IDs canceled by the server; their results are dropped
	pending  []*responseFrame // responses not yet accepted by the server
	acks     []string         // queue IDs to acknowledge on the next poll
	// previousFile alternates file chunks with metadata between batches.
	previousFile bool
	// Cached credentials are reused only for the same account, backend and machine.
	credential config.Computer
	// issuedAt is the local time the credential was issued; zero if unknown.
	issuedAt time.Time
	// proven is set once the watch stream connected with the credential, as in
	// the desktop; until then a rejection counts as a failed issue.
	proven bool
	// Failed issues, and credentials rejected before they were proven, are
	// retried after nextRenew.
	issuing       bool
	issueFailures int
	nextRenew     time.Time

	watchCancel    context.CancelFunc
	generation     int
	watchAlive     bool
	watchConnected bool
	lastEvent      time.Time
	lastStanding   string
	nextDial       time.Time
	backoff        time.Duration
	needPoll       bool
	nextPoll       time.Time
	pollBackoff    time.Duration
	polling        bool

	submitting     bool
	submitFailures int // consecutive submits without progress
	nextSubmit     time.Time
}

func newSession(ctx context.Context, pv *Provider) *session {
	s := &session{
		pv:          pv,
		presence:    newUUID(),
		replies:     make(chan *responseFrame, submitBatch),
		done:        make(chan string, maxActive),
		events:      make(chan watched, 16),
		submitted:   make(chan submitted, 1),
		polled:      make(chan polled, 1),
		issued:      make(chan issued, 1),
		tasks:       map[string]*task{},
		seen:        newDedup(),
		requests:    newDedup(),
		canceled:    newDedup(),
		backoff:     minBackoff,
		pollBackoff: minBackoff,
		credential:  pv.cachedCredential(),
	}
	s.life, s.stopLife = context.WithCancel(context.Background())
	s.work, s.stopWork = context.WithCancel(ctx)
	return s
}

// cachedCredential returns the saved credential if it was issued for this
// account, backend and machine and is not about to expire.
func (pv *Provider) cachedCredential() config.Computer {
	if pv.Store == nil || pv.Backend == "" || pv.AccountScope == "" {
		return config.Computer{}
	}
	var c config.Computer
	if err := pv.Store.Load("computer", &c); err != nil {
		return config.Computer{}
	}
	sameScope := c.Backend == pv.Backend && c.AccountScope == pv.AccountScope && c.MachineID == pv.Machine.MachineID
	if !sameScope || time.UnixMilli(c.ExpiresAtMS).Sub(pv.now()) <= credentialMargin {
		return config.Computer{}
	}
	return c
}

func (s *session) close() {
	if s.watchCancel != nil {
		s.watchCancel()
	}
	s.stopWork()
	s.stopLife()
}

// sink is the executor's output; it is safe to call from any goroutine.
func (s *session) sink(f *responseFrame) error {
	select {
	case s.replies <- f:
		return nil
	case <-s.life.Done():
		return s.life.Err()
	}
}

// queue adds a frame to pending; only the Run goroutine may call it.
func (s *session) queue(f *responseFrame) {
	s.pending = append(s.pending, f)
}

// queueSink adapts queue to executor.Sink.
func (s *session) queueSink(f *responseFrame) error {
	s.queue(f)
	return nil
}

// take queues a reply unless its request was canceled. It reports whether
// a full batch is pending.
func (s *session) take(f *responseFrame) bool {
	if s.dropped(f) {
		return false
	}
	if s.canceled.has(f.RequestId) {
		f = canceledFile(f)
	}
	s.queue(f)
	return len(s.pending) >= submitBatch
}

// reject answers a request that will not run.
func (s *session) reject(f *requestFrame, reason error) {
	if err := executor.Reject(f, reason, s.queueSink); err != nil {
		log.Printf("reject %s: %v", f.RequestId, err)
	}
}

// closeCanceled answers a request canceled before it started.
func (s *session) closeCanceled(f *requestFrame) {
	if err := executor.Canceled(f, s.queueSink); err != nil {
		log.Printf("cancel %s: %v", f.RequestId, err)
	}
}

// dropped reports whether a reply belongs to a canceled request. Control
// and terminal frames still go through so the request's stream is closed.
func (s *session) dropped(f *responseFrame) bool {
	return s.canceled.has(f.RequestId) && f.GetControl() == nil && !terminal(f)
}

// terminal reports whether f is the final frame of a non-exec request.
func terminal(f *responseFrame) bool {
	return f.GetFileError() != nil || f.GetMessagesError() != nil || f.GetLocalToolResult() != nil || f.GetFile().GetLast()
}

// closes reports whether f ends its request's stream.
func closes(f *responseFrame) bool {
	if terminal(f) {
		return true
	}
	if f.GetControl() == nil {
		return false
	}
	var msg agentv1.ExecClientControlMessage
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(f.GetControl().MessageJson), &msg); err != nil {
		return false
	}
	return msg.GetStreamClose() != nil || msg.GetThrow() != nil
}

// canceledFile replaces the last chunk of a canceled transfer, whose earlier
// chunks were dropped, with the error a canceled request reports.
func canceledFile(f *responseFrame) *responseFrame {
	if f.GetFile() == nil {
		return f
	}
	return &responseFrame{
		RequestId: f.RequestId,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
			FileError: &aiserverv1.GrokBotUserComputerFileError{Error: context.Canceled.Error()},
		},
	}
}

func (s *session) run(ctx context.Context) error {
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	pollTick := time.NewTicker(pollInterval)
	defer pollTick.Stop()
	aliveTick := time.NewTicker(keepAliveInterval)
	defer aliveTick.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			withdrawn := s.withdrawPresence()
			s.shutdown()
			<-withdrawn
			return nil
		case f := <-s.acceptReplies():
			s.onReply(f)
		case r := <-s.submitted:
			err = s.onSubmitted(r)
		case r := <-s.polled:
			s.onPolled(r)
		case r := <-s.issued:
			err = s.onIssued(r)
		case id := <-s.done:
			s.finish(id)
			s.startJobs()
		case ev := <-s.events:
			s.onEvent(ev)
		case <-aliveTick.C:
			s.keepAlive()
		case <-pollTick.C:
			s.needPoll = true
		case <-tick.C:
			err = s.onTick(ctx)
		}
		if err != nil {
			s.shutdown()
			return err
		}
	}
}

// acceptReplies returns the replies channel, or nil to make workers wait
// while enough frames are pending, as a blocking submit used to.
func (s *session) acceptReplies() chan *responseFrame {
	if s.stuck() >= maxPending || (s.submitting && len(s.pending) >= submitBatch) {
		return nil
	}
	return s.replies
}

func (s *session) onReply(f *responseFrame) {
	if s.take(f) {
		s.startSubmit()
	}
}

// overflow fails the session once a failed submit leaves too many frames stuck.
func (s *session) overflow(err error) error {
	if s.stuck() < maxPending {
		return nil
	}
	return fmt.Errorf("%w: %w", errBacklogFull, err)
}

// stuck counts the pending frames other than keepalives, which are bounded
// by the number of requests and must not end the session on their own.
func (s *session) stuck() int {
	n := 0
	for _, f := range s.pending {
		if f.GetMessagesAccepted() == nil {
			n++
		}
	}
	return n
}

func (s *session) finish(id string) {
	delete(s.tasks, id)
	s.active--
}

func (s *session) onEvent(ev watched) {
	if ev.generation != s.generation {
		return // from a replaced stream
	}
	s.lastEvent = s.now()
	if ev.event != nil {
		s.backoff = minBackoff
		s.needPoll = s.needPoll || hasWork(ev.event)
		if ev.event.GetConnected() != nil {
			s.watchConnected = true
			s.proven = true
			s.issueFailures = 0
			log.Printf("connected as %s (%s)", s.pv.Machine.Label, s.pv.Machine.MachineID)
		}
		return
	}
	s.watchAlive = false
	s.invalidate(ev.err)
	s.delayDial(s.backoff + s.jitter(s.backoff))
	s.increaseBackoff()
	if ev.err != nil {
		log.Printf("watch: %v; reconnecting", ev.err)
	}
}

// hasWork reports whether a watch event says requests may be waiting.
func hasWork(ev *aiserverv1.WatchGrokBotUserComputerRequestsEvent) bool {
	return ev.GetConnected() != nil || ev.GetNotify() != nil || ev.GetHeartbeat().GetPendingRequestCount() > 0
}

// keepAlive tells the server that accepted requests are still being handled.
// A request gets at most one pending keepalive, and none while it waits in the
// backlog or once its stream is closed.
func (s *session) keepAlive() {
	if s.credential.Credential == "" {
		return // could not be delivered anyway
	}
	ids := map[string]bool{}
	for id, t := range s.tasks {
		ids[id] = t.cancel != nil // started
	}
	for _, f := range s.pending {
		if _, ok := s.tasks[f.RequestId]; !ok {
			ids[f.RequestId] = true // finished, results not yet delivered
		}
	}
	for _, f := range s.pending {
		if f.GetMessagesAccepted() != nil || closes(f) {
			ids[f.RequestId] = false
		}
	}
	for id, alive := range ids {
		if !alive || s.canceled.has(id) {
			continue
		}
		s.queue(&responseFrame{
			RequestId: id,
			Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesAccepted{
				MessagesAccepted: &aiserverv1.GrokBotUserComputerMessagesAccepted{},
			},
		})
	}
}

func (s *session) increaseBackoff() {
	s.backoff = min(maxBackoff, s.backoff*2)
}

// delayDial postpones the next watch dial by at least d.
func (s *session) delayDial(d time.Duration) {
	if at := s.now().Add(d); at.After(s.nextDial) {
		s.nextDial = at
	}
}

func (s *session) onTick(context.Context) error {
	if !s.ensureCredential() {
		return nil
	}
	// Events that queued up while Run was busy prove the stream is alive.
	s.drainEvents()
	switch {
	case !s.watchAlive && !s.now().Before(s.nextDial):
		s.startWatch()
		s.needPoll = true
	case s.watchAlive && s.lastStanding != s.standing():
		// Reconnect to announce the new standing mode.
		s.startWatch()
	case s.watchAlive && s.now().Sub(s.lastEvent) > s.watchTimeout():
		s.dropStaleWatch()
	}
	s.startSubmit()
	s.startPoll()
	return nil
}

// drainEvents applies the watch events already buffered in the channel.
func (s *session) drainEvents() {
	for range cap(s.events) {
		select {
		case ev := <-s.events:
			s.onEvent(ev)
		default:
			return
		}
	}
}

// standing is the standing mode announced in hello; the server decides
// in server-authoritative mode, so none is sent.
func (s *session) standing() string {
	if s.credential.ServerAuthoritative {
		return ""
	}
	return s.pv.Policy.Standing()
}

// stopWatch cancels the watch stream and ignores its cancellation event.
func (s *session) stopWatch() {
	if s.watchCancel != nil {
		s.watchCancel()
	}
	s.generation++
	s.watchAlive = false
}

// dropStaleWatch abandons a silent stream and schedules a reconnect.
func (s *session) dropStaleWatch() {
	s.stopWatch()
	s.nextDial = s.now().Add(staleRedial + s.jitter(s.backoff))
	s.increaseBackoff()
}

// renewAt returns when the credential should be renewed: credentialMargin
// before it expires, or halfway through its lifetime if that is shorter.
func (s *session) renewAt() time.Time {
	expiresAt := time.UnixMilli(s.credential.ExpiresAtMS)
	margin := credentialMargin
	if !s.issuedAt.IsZero() {
		margin = min(margin, expiresAt.Sub(s.issuedAt)/2)
	}
	return expiresAt.Add(-margin)
}

// ensureCredential starts issuing a new credential when the current one is
// missing or due for renewal. It reports whether a usable credential is
// available; while an issue is in flight or backing off, the old credential
// keeps serving until it expires.
func (s *session) ensureCredential() bool {
	now := s.now()
	if s.credential.Credential != "" && now.Before(s.renewAt()) {
		return true
	}
	usable := s.credential.Credential != "" && now.Before(time.UnixMilli(s.credential.ExpiresAtMS))
	if !s.issuing && s.work.Err() == nil && !now.Before(s.nextRenew) {
		s.startIssue()
	}
	return usable
}

// startIssue issues a credential in the background. Shutdown abandons it.
func (s *session) startIssue() {
	s.issuing = true
	go func() {
		c, err := s.pv.issue(s.work)
		s.issued <- issued{credential: c, err: err}
	}()
}

// onIssued applies an issue result. Without a session or a registration no
// credential can ever be issued, so those errors end Run.
func (s *session) onIssued(r issued) error {
	s.issuing = false
	switch err := r.err; {
	case removedComputer(err):
		return fmt.Errorf("computer registration refused: %w", err)
	case errors.Is(err, auth.ErrLogout), errors.Is(err, auth.ErrNotLoggedIn):
		return err
	case err != nil:
		if s.work.Err() == nil {
			log.Printf("credential: %v", err)
		}
		s.failIssue(retryAfter(err))
		return nil
	}
	s.credential = r.credential
	s.issuedAt = s.now()
	s.proven = false
	s.nextRenew = time.Time{}
	// The next tick dials with the new credential once nextDial allows.
	s.stopWatch()
	s.needPoll = true
	return nil
}

// failIssue schedules the next issue after a failure, like the desktop's gJe.
func (s *session) failIssue(retryAfter time.Duration) {
	s.issueFailures++
	d := min(maxIssueBackoff, issueBackoff<<min(s.issueFailures-1, 10))
	d = max(d, min(retryAfter, maxRetryAfter))
	s.nextRenew = s.now().Add(d + s.jitter(d/2))
}

// retryAfter returns the delay the server asked for in a Retry-After header
// of err, in seconds; zero if none.
func retryAfter(err error) time.Duration {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return 0
	}
	n, perr := strconv.Atoi(ce.Meta().Get("Retry-After"))
	if perr != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// startWatch replaces the watch stream with a new one for the current credential.
func (s *session) startWatch() {
	s.stopWatch()
	gen := s.generation
	ctx, cancel := context.WithCancel(s.work)
	s.watchCancel = cancel
	s.lastStanding = s.standing()
	s.watchAlive = true
	s.watchConnected = false
	s.lastEvent = s.now()
	req := &aiserverv1.WatchGrokBotUserComputerRequestsRequest{
		MachineId:     s.pv.Machine.MachineID,
		Credential:    s.credential.Credential,
		Hello:         s.pv.hello(s.credential, s.lastStanding),
		PresenceToken: s.presence,
	}
	go func() {
		err := s.watch(ctx, req, gen)
		select {
		case s.events <- watched{err: err, generation: gen}:
		case <-ctx.Done():
		}
	}()
}

func (s *session) watch(ctx context.Context, req *aiserverv1.WatchGrokBotUserComputerRequestsRequest, gen int) error {
	stream, err := s.pv.Channel.WatchGrokBotUserComputerRequests(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		select {
		case s.events <- watched{event: stream.Msg(), generation: gen}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return stream.Err()
}

// startJobs starts backlogged tasks while there are free slots.
func (s *session) startJobs() {
	for s.active < maxActive && len(s.backlog) > 0 {
		id := s.backlog[0]
		s.backlog = s.backlog[1:]
		t := s.tasks[id]
		if t == nil {
			continue // canceled while waiting
		}
		if s.credential.Credential == "" || s.credential.ServerAuthoritative != t.server {
			// The permission mode the request was made under is gone; like the
			// desktop, which recreates its provider then, do not run it.
			delete(s.tasks, id)
			s.reject(t.frame, errStopped)
			continue
		}
		ctx, cancel := context.WithCancel(s.work)
		t.cancel = cancel
		s.active++
		go func() {
			defer cancel()
			if err := s.pv.Executor.Handle(ctx, t.frame, t.server, s.sink); err != nil {
				log.Printf("request %s: %v", id, err)
			}
			select {
			case s.done <- id:
			case <-s.life.Done():
			}
		}()
	}
}

// invalidateFor is invalidate for an RPC made with credential; a result for a
// credential that has since been replaced says nothing about the current one.
func (s *session) invalidateFor(credential string, err error) {
	if credential == s.credential.Credential {
		s.invalidate(err)
	}
}

// invalidate drops the credential if err shows the server rejected it.
func (s *session) invalidate(err error) {
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		return
	}
	if !s.proven {
		// A credential rejected before it ever connected counts as a failed
		// issue, so a server that keeps rejecting it is not hammered.
		s.failIssue(retryAfter(err))
	}
	s.credential = config.Computer{}
	s.issuedAt = time.Time{}
	s.proven = false
	if s.pv.Store != nil {
		if err := s.pv.Store.Save("computer", s.credential); err != nil {
			log.Printf("forget computer credential: %v", err)
		}
	}
	s.stopWatch()
	s.delayDial(channelRetry)
}

// submit delivers pending frames, retrying briefly on failure.
func (s *session) submit(ctx context.Context) error {
	failures := 0
	for len(s.pending) > 0 && s.credential.Credential != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := len(s.pending)
		err := s.submitOnce(ctx)
		if err != nil {
			s.invalidate(err)
			if s.credential.Credential == "" {
				return err
			}
		}
		if len(s.pending) < before {
			failures = 0
			continue
		}
		failures++
		if failures >= submitAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(failures) * 200 * time.Millisecond):
		}
	}
	return nil
}

func (s *session) submitOnce(ctx context.Context) error {
	frames := s.batch()
	n, err := s.pv.submitFrames(ctx, s.credential.Credential, frames)
	s.accepted(frames, n)
	return err
}

// submitFrames sends one batch and returns how many frames the server
// accepted. It does not touch session state, so it may run on any goroutine.
func (pv *Provider) submitFrames(ctx context.Context, credential string, frames []*responseFrame) (int, error) {
	ctx, cancel := rpcContext(ctx)
	defer cancel()
	req := &aiserverv1.SubmitGrokBotUserComputerResponsesRequest{
		MachineId:  pv.Machine.MachineID,
		Credential: credential,
		Frames:     frames,
	}
	r, err := pv.Channel.SubmitGrokBotUserComputerResponses(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	n := int(r.Msg.AcceptedCount)
	if n > len(frames) {
		return 0, fmt.Errorf("invalid accepted_count %d", n)
	}
	if n < len(frames) {
		return n, fmt.Errorf("server accepted only %d response frames", n)
	}
	return n, nil
}

// accepted removes the first n frames of a submitted batch from pending.
func (s *session) accepted(frames []*responseFrame, n int) {
	done := map[*responseFrame]bool{}
	for _, f := range frames[:n] {
		done[f] = true
	}
	s.pending = slices.DeleteFunc(s.pending, func(f *responseFrame) bool { return done[f] })
}

// startSubmit sends the next batch in the background unless a submit is
// already in flight or backing off. The submit outlives shutdown, which waits
// for it: aborting a batch the server may have accepted would resend it.
func (s *session) startSubmit() {
	if s.submitting || len(s.pending) == 0 || s.credential.Credential == "" || s.now().Before(s.nextSubmit) {
		return
	}
	s.submitting = true
	credential, frames := s.credential.Credential, s.batch()
	go func() {
		n, err := s.pv.submitFrames(s.life, credential, frames)
		s.submitted <- submitted{credential: credential, frames: frames, accepted: n, err: err}
	}()
}

// onSubmitted applies a submit result and keeps draining pending, retrying
// briefly on failure like submit does.
func (s *session) onSubmitted(r submitted) error {
	s.submitting = false
	s.accepted(r.frames, r.accepted)
	if r.err != nil {
		s.invalidateFor(r.credential, r.err)
	}
	if r.accepted > 0 {
		s.submitFailures = 0
		s.nextSubmit = time.Time{}
		s.startSubmit()
		return nil
	}
	s.submitFailures++
	if s.submitFailures < submitAttempts && s.credential.Credential != "" {
		s.nextSubmit = s.now().Add(time.Duration(s.submitFailures) * 200 * time.Millisecond)
		return nil
	}
	// A full round failed; the next tick starts another one.
	log.Printf("submit: %v", r.err)
	s.submitFailures = 0
	s.nextSubmit = time.Time{}
	return s.overflow(r.err)
}

// startPoll fetches queued requests in the background and acknowledges the
// previous batch. Shutdown abandons it.
func (s *session) startPoll() {
	if s.polling || !s.needPoll || s.credential.Credential == "" || s.now().Before(s.nextPoll) {
		return
	}
	s.needPoll = false
	s.polling = true
	credential, server := s.credential.Credential, s.credential.ServerAuthoritative
	req := &aiserverv1.PollGrokBotUserComputerRequestsRequest{
		MachineId:  s.pv.Machine.MachineID,
		Credential: credential,
		AckIds:     s.acks,
		Limit:      pollLimit,
	}
	go func() {
		ctx, cancel := rpcContext(s.work)
		defer cancel()
		r, err := s.pv.Channel.PollGrokBotUserComputerRequests(ctx, connect.NewRequest(req))
		result := polled{credential: credential, server: server, err: err}
		if err == nil {
			result.requests = r.Msg.Requests
		}
		s.polled <- result
	}()
}

// onPolled applies a poll result.
func (s *session) onPolled(r polled) {
	s.polling = false
	if r.err != nil {
		s.invalidateFor(r.credential, r.err)
		if !errors.Is(r.err, context.Canceled) {
			log.Printf("poll: %v", r.err)
		}
		// Retry with backoff instead of waiting for the next poll interval.
		s.needPoll = true
		s.nextPoll = s.now().Add(s.pollBackoff)
		s.pollBackoff = min(maxBackoff, s.pollBackoff*2)
		return
	}
	s.pollBackoff = minBackoff
	s.acks = nil
	for _, q := range r.requests {
		s.acks = append(s.acks, q.Id)
		if s.seen.add(q.Id) && q.Frame != nil {
			s.accept(q.Frame, r.server)
		}
	}
	// Poll again right away to deliver the acks and drain the queue.
	if len(s.acks) > 0 {
		s.needPoll = true
	}
}

func (s *session) accept(f *requestFrame, server bool) {
	if s.pv.Debug {
		if raw, err := protojson.Marshal(f); err == nil {
			log.Printf("request JSON: %s", raw)
		}
	}
	switch {
	case f.GetCancel() != nil:
		s.cancel(f.RequestId)
	case f.GetRetireApproval() != nil:
	case s.tasks[f.RequestId] != nil || s.requests.has(f.RequestId):
		// Already queued or running.
	case s.active >= maxActive && len(s.backlog) >= maxBacklog:
		s.requests.add(f.RequestId)
		s.reject(f, errBusy)
	default:
		s.requests.add(f.RequestId)
		s.tasks[f.RequestId] = &task{frame: f, server: server}
		s.backlog = append(s.backlog, f.RequestId)
		s.startJobs()
	}
}

// keep reports whether a pending frame of a canceled request must still be
// delivered, the same frames dropped lets through.
func keep(f *responseFrame) bool {
	return f.GetControl() != nil || terminal(f)
}

// cancel stops a request and discards its undelivered results. The request
// is remembered so a late or repeated delivery does not start it again.
func (s *session) cancel(id string) {
	s.canceled.add(id)
	s.requests.add(id)
	s.pending = slices.DeleteFunc(s.pending, func(r *responseFrame) bool { return r.RequestId == id && !keep(r) })
	for i, r := range s.pending {
		if r.RequestId == id {
			s.pending[i] = canceledFile(r)
		}
	}
	s.backlog = slices.DeleteFunc(s.backlog, func(queued string) bool { return queued == id })
	t := s.tasks[id]
	switch {
	case t == nil:
	case t.cancel != nil:
		t.cancel() // the worker reports the cancellation
	default:
		s.closeCanceled(t.frame)
		delete(s.tasks, id)
	}
}

// withdrawPresence stops the watch and tells the server in the background
// that this session is gone, so the computer goes offline at once instead of
// when the stream times out. The channel is closed once the call is done.
func (s *session) withdrawPresence() <-chan struct{} {
	done := make(chan struct{})
	s.stopWatch()
	if s.credential.Credential == "" {
		close(done)
		return done
	}
	req := &aiserverv1.WithdrawGrokBotUserComputerPresenceRequest{
		MachineId:     s.pv.Machine.MachineID,
		Credential:    s.credential.Credential,
		PresenceToken: s.presence,
	}
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), withdrawTimeout)
		defer cancel()
		if _, err := s.pv.Channel.WithdrawGrokBotUserComputerPresence(ctx, connect.NewRequest(req)); err != nil {
			log.Printf("withdraw presence: %v", err)
		}
	}()
	return done
}

// shutdown cancels all work and flushes results within shutdownGrace, keeping
// finalFlushGrace of it for the final submit even if workers are slow.
func (s *session) shutdown() {
	s.stopWork()
	if s.watchCancel != nil {
		s.watchCancel()
	}
	for id, t := range s.tasks {
		if t.cancel == nil {
			s.closeCanceled(t.frame)
			delete(s.tasks, id)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	workers, stopWorkers := context.WithTimeout(ctx, shutdownGrace-finalFlushGrace)
	defer stopWorkers()
	if !s.awaitWorkers(workers) {
		log.Printf("shutdown: %d requests still running", s.active)
	}
	if s.submitting {
		// The frames of a submit still in flight must not be sent twice.
		log.Printf("final submit: previous submit still running")
		return
	}
	s.drainReplies()
	if err := s.submit(ctx); err != nil {
		log.Printf("final submit: %v", err)
	}
}

// awaitWorkers collects results until all workers and the submit in flight
// finish. It reports false if ctx expired first.
func (s *session) awaitWorkers(ctx context.Context) bool {
	for s.active > 0 || s.submitting {
		select {
		case f := <-s.replies:
			if !s.take(f) || s.submitting {
				continue
			}
			if err := s.submit(ctx); err != nil {
				log.Printf("submit: %v", err)
			}
		case r := <-s.submitted:
			s.submitting = false
			s.accepted(r.frames, r.accepted)
		case id := <-s.done:
			s.finish(id)
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// drainReplies queues the replies already buffered in the channel.
func (s *session) drainReplies() {
	for {
		select {
		case f := <-s.replies:
			s.take(f)
		default:
			return
		}
	}
}

// newUUID returns a random version 4 UUID, the form of the desktop's
// presence tokens.
func newUUID() string {
	var b [16]byte
	_, _ = crand.Read(b[:]) // never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (pv *Provider) now() time.Time {
	if pv.Now != nil {
		return pv.Now()
	}
	return time.Now()
}
func (s *session) now() time.Time { return s.pv.now() }
func (s *session) jitter(d time.Duration) time.Duration {
	if s.pv.Jitter != nil {
		return s.pv.Jitter(d)
	}
	return time.Duration(rand.Int64N(int64(max(d/2, time.Nanosecond)))) //nolint:gosec // jitter needs no cryptographic randomness
}
func (s *session) watchTimeout() time.Duration {
	if s.watchConnected {
		return watchStale
	}
	return watchConnectTimeout
}
func removedComputer(err error) bool {
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		return false
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return false
	}
	for _, d := range ce.Details() {
		if d.Type() == "aiserver.v1.GrokBotUserComputerRemovedDetail" {
			return true
		}
	}
	return false
}

// Limits for a single submit batch.
const (
	maxBatchFrames = 256
	maxBatchBytes  = 8 << 20 // serialized frames
	maxBatchData   = 1 << 20 // file payload
)

// batcher fills one submit batch from per-request lanes.
type batcher struct {
	lanes map[string][]*responseFrame
	order []string // request IDs in order of first appearance
	out   []*responseFrame
	size  int
	data  int
}

func newBatcher(pending []*responseFrame) *batcher {
	b := &batcher{lanes: map[string][]*responseFrame{}}
	for _, f := range pending {
		if _, ok := b.lanes[f.RequestId]; !ok {
			b.order = append(b.order, f.RequestId)
		}
		b.lanes[f.RequestId] = append(b.lanes[f.RequestId], f)
	}
	return b
}

// take moves the head of the request's lane into the batch if it is of the
// wanted kind and fits. The first frame always fits.
func (b *batcher) take(id string, files bool) bool {
	q := b.lanes[id]
	if len(q) == 0 {
		return false
	}
	f := q[0]
	data := len(f.GetFile().GetData())
	if (data > 0) != files {
		return false
	}
	size := proto.Size(f)
	full := len(b.out) >= maxBatchFrames || b.size+size > maxBatchBytes || b.data+data > maxBatchData
	if len(b.out) > 0 && full {
		return false
	}
	b.out = append(b.out, f)
	b.size += size
	b.data += data
	b.lanes[id] = q[1:]
	return true
}

// batch fairly interleaves requests while preserving order within each one.
// Metadata goes first; file chunks, at most one per request, are skipped every
// other batch while metadata is waiting so neither kind starves the other.
func (s *session) batch() []*responseFrame {
	b := newBatcher(s.pending)
	for progress := true; progress; {
		progress = false
		for _, id := range b.order {
			progress = b.take(id, false) || progress
		}
	}
	if len(b.out) == 0 || !s.previousFile {
		for _, id := range b.order {
			b.take(id, true)
		}
	}
	s.previousFile = b.data > 0
	return b.out
}
