// Package computer manages the pull queue and its bounded execution lifecycle.
package computer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/exa-pub/grokbot-cli/internal/config"
	"github.com/exa-pub/grokbot-cli/internal/executor"
	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-cli/internal/permission"
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
)

var errBacklogFull = errors.New("response backlog full")

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
	return c, pv.Store.Save("computer", c)
}

func (pv *Provider) hello(c config.Computer, standing string) *aiserverv1.GrokBotUserComputerHello {
	if c.ServerAuthoritative {
		standing = ""
	}
	setup := true
	generation := int32(2)
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
// workers talk to it only through the channels.
type session struct {
	pv *Provider

	// work is canceled on shutdown and parents watch and task contexts.
	work     context.Context
	stopWork context.CancelFunc
	// life outlives work so canceled tasks can still report their results.
	life     context.Context
	stopLife context.CancelFunc

	replies chan *responseFrame
	done    chan string
	events  chan watched

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

	watchCancel    context.CancelFunc
	generation     int
	watchAlive     bool
	watchConnected bool
	lastEvent      time.Time
	lastStanding   string
	nextDial       time.Time
	backoff        time.Duration
	needPoll       bool
}

func newSession(ctx context.Context, pv *Provider) *session {
	s := &session{
		pv:         pv,
		replies:    make(chan *responseFrame, submitBatch),
		done:       make(chan string, maxActive),
		events:     make(chan watched, 16),
		tasks:      map[string]*task{},
		seen:       newDedup(),
		requests:   newDedup(),
		canceled:   newDedup(),
		backoff:    minBackoff,
		credential: pv.cachedCredential(),
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
// frames still go through so the request's stream is closed.
func (s *session) dropped(f *responseFrame) bool {
	return s.canceled.has(f.RequestId) && f.GetControl() == nil
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
			s.shutdown()
			return nil
		case f := <-s.replies:
			err = s.onReply(ctx, f)
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

func (s *session) onReply(ctx context.Context, f *responseFrame) error {
	if !s.take(f) {
		return nil
	}
	return s.flush(ctx)
}

// flush submits pending frames and fails once too many are stuck.
func (s *session) flush(ctx context.Context) error {
	err := s.submit(ctx)
	if err != nil {
		log.Printf("submit: %v", err)
	}
	if len(s.pending) < maxPending {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errBacklogFull, err)
	}
	return errBacklogFull
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
			log.Printf("connected as %s (%s)", s.pv.Machine.Label, s.pv.Machine.MachineID)
		}
		return
	}
	s.watchAlive = false
	s.invalidate(ev.err)
	s.nextDial = s.now().Add(s.backoff + s.jitter(s.backoff))
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
func (s *session) keepAlive() {
	if s.credential.Credential == "" {
		return // could not be delivered anyway
	}
	ids := map[string]bool{}
	for id := range s.tasks {
		ids[id] = true
	}
	for _, f := range s.pending {
		ids[f.RequestId] = true
	}
	for id := range ids {
		if s.canceled.has(id) {
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

func (s *session) onTick(ctx context.Context) error {
	if ready, err := s.ensureCredential(ctx); !ready {
		return err
	}
	switch {
	case !s.watchAlive && !s.now().Before(s.nextDial):
		s.startWatch()
		s.needPoll = true
	case s.watchAlive && s.lastStanding != s.pv.Policy.Standing():
		// Reconnect to announce the new standing mode.
		s.startWatch()
	case s.watchAlive && s.now().Sub(s.lastEvent) > s.watchTimeout():
		s.dropStaleWatch()
	}
	if len(s.pending) > 0 {
		if err := s.flush(ctx); err != nil {
			return err
		}
	}
	if s.needPoll && s.credential.Credential != "" {
		s.needPoll = false
		s.poll(ctx)
	}
	return nil
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

// ensureCredential issues a new credential when the current one is missing or
// about to expire. It reports whether a usable credential is available.
func (s *session) ensureCredential(ctx context.Context) (bool, error) {
	expiresAt := time.UnixMilli(s.credential.ExpiresAtMS)
	if s.credential.Credential != "" && expiresAt.Sub(s.now()) > credentialMargin {
		return true, nil
	}
	select {
	case <-ctx.Done():
		return false, nil // shutting down
	default:
	}
	if s.now().Before(s.nextDial) {
		return false, nil
	}
	c, err := s.pv.issue(ctx)
	switch {
	case removedComputer(err):
		return false, fmt.Errorf("computer registration refused: %w", err)
	case err != nil:
		log.Printf("credential: %v", err)
		s.nextDial = s.now().Add(s.backoff)
		s.increaseBackoff()
		return false, nil
	}
	s.credential = c
	s.startWatch()
	s.needPoll = true
	return true, nil
}

// startWatch replaces the watch stream with a new one for the current credential.
func (s *session) startWatch() {
	s.stopWatch()
	gen := s.generation
	ctx, cancel := context.WithCancel(s.work)
	s.watchCancel = cancel
	s.lastStanding = s.pv.Policy.Standing()
	s.watchAlive = true
	s.watchConnected = false
	s.lastEvent = s.now()
	req := &aiserverv1.WatchGrokBotUserComputerRequestsRequest{
		MachineId:  s.pv.Machine.MachineID,
		Credential: s.credential.Credential,
		Hello:      s.pv.hello(s.credential, s.lastStanding),
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
		ctx, cancel := context.WithCancel(s.work)
		t.cancel = cancel
		s.active++
		server := s.credential.ServerAuthoritative
		go func() {
			defer cancel()
			if err := s.pv.Executor.Handle(ctx, t.frame, server, s.sink); err != nil {
				log.Printf("request %s: %v", id, err)
			}
			select {
			case s.done <- id:
			case <-s.life.Done():
			}
		}()
	}
}

// invalidate drops the credential if err shows the server rejected it.
func (s *session) invalidate(err error) {
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		return
	}
	s.credential = config.Computer{}
	if s.pv.Store != nil {
		if err := s.pv.Store.Save("computer", s.credential); err != nil {
			log.Printf("forget computer credential: %v", err)
		}
	}
	s.stopWatch()
	s.nextDial = s.now()
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
	ctx, cancel := rpcContext(ctx)
	defer cancel()
	req := &aiserverv1.SubmitGrokBotUserComputerResponsesRequest{
		MachineId:  s.pv.Machine.MachineID,
		Credential: s.credential.Credential,
		Frames:     s.batch(),
	}
	r, err := s.pv.Channel.SubmitGrokBotUserComputerResponses(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	n := int(r.Msg.AcceptedCount)
	if n > len(req.Frames) {
		return fmt.Errorf("invalid accepted_count %d", n)
	}
	accepted := map[*responseFrame]bool{}
	for _, f := range req.Frames[:n] {
		accepted[f] = true
	}
	s.pending = slices.DeleteFunc(s.pending, func(f *responseFrame) bool { return accepted[f] })
	if n < len(req.Frames) {
		return fmt.Errorf("server accepted only %d response frames", n)
	}
	return nil
}

// poll fetches queued requests and acknowledges the previous batch.
func (s *session) poll(ctx context.Context) {
	call, cancel := rpcContext(ctx)
	defer cancel()
	req := &aiserverv1.PollGrokBotUserComputerRequestsRequest{
		MachineId:  s.pv.Machine.MachineID,
		Credential: s.credential.Credential,
		AckIds:     s.acks,
		Limit:      pollLimit,
	}
	r, err := s.pv.Channel.PollGrokBotUserComputerRequests(call, connect.NewRequest(req))
	if err != nil {
		s.invalidate(err)
		if !errors.Is(err, context.Canceled) {
			log.Printf("poll: %v", err)
		}
		return
	}
	s.acks = nil
	for _, q := range r.Msg.Requests {
		s.acks = append(s.acks, q.Id)
		if s.seen.add(q.Id) && q.Frame != nil {
			s.accept(q.Frame)
		}
	}
	// Poll again right away to deliver the acks and drain the queue.
	if len(s.acks) > 0 {
		s.needPoll = true
	}
}

func (s *session) accept(f *requestFrame) {
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
		s.reject(f, errors.New("too many in-flight requests"))
	default:
		s.requests.add(f.RequestId)
		s.tasks[f.RequestId] = &task{frame: f}
		s.backlog = append(s.backlog, f.RequestId)
		s.startJobs()
	}
}

// cancel stops a request and discards its undelivered results. The request
// is remembered so a late or repeated delivery does not start it again.
func (s *session) cancel(id string) {
	s.canceled.add(id)
	s.requests.add(id)
	s.pending = slices.DeleteFunc(s.pending, func(r *responseFrame) bool { return r.RequestId == id })
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

// shutdown cancels all work and flushes results within shutdownGrace.
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
	if s.awaitWorkers(ctx) {
		s.drainReplies()
		if err := s.submit(ctx); err != nil {
			log.Printf("final submit: %v", err)
		}
	}
}

// awaitWorkers collects results until all workers exit. It reports false if
// ctx expired first.
func (s *session) awaitWorkers(ctx context.Context) bool {
	for s.active > 0 {
		select {
		case f := <-s.replies:
			if !s.take(f) {
				continue
			}
			if err := s.submit(ctx); err != nil {
				log.Printf("submit: %v", err)
			}
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
