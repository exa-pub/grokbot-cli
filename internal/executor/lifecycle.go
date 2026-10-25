package executor

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
)

// errUnsupportedExec matches the desktop daemon's message for unknown requests.
var errUnsupportedExec = errors.New("unsupported exec request: No handler found for server message of type undefined")

const heartbeatInterval = 3 * time.Second

func throwMessage(execID uint32, err error) *agentv1.ExecClientControlMessage {
	return &agentv1.ExecClientControlMessage{Message: &agentv1.ExecClientControlMessage_Throw{
		Throw: &agentv1.ExecClientThrow{Id: execID, Error: err.Error()},
	}}
}

func closeMessage(execID uint32) *agentv1.ExecClientControlMessage {
	return &agentv1.ExecClientControlMessage{Message: &agentv1.ExecClientControlMessage_StreamClose{
		StreamClose: &agentv1.ExecClientStreamClose{Id: execID},
	}}
}

func heartbeatMessage(execID uint32) *agentv1.ExecClientControlMessage {
	return &agentv1.ExecClientControlMessage{Message: &agentv1.ExecClientControlMessage_Heartbeat{
		Heartbeat: &agentv1.ExecClientHeartbeat{Id: execID},
	}}
}

// sendControl sends an exec control message; cwd optionally reports the state
// of the requested working directory.
func sendControl(requestID string, msg *agentv1.ExecClientControlMessage, cwd *string, sink Sink) error {
	b, err := protojson.Marshal(msg)
	if err != nil {
		return err
	}
	return sink(&aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: requestID,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
			Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: string(b), CwdState: cwd},
		},
	})
}

// Canceled closes work canceled before an exec ID was assigned.
func Canceled(f *aiserverv1.GrokBotUserComputerRequestFrame, sink Sink) error {
	if f.GetExec() == nil {
		return Reject(f, context.Canceled, sink)
	}
	return sendControl(f.RequestId, closeMessage(0), nil, sink)
}

// execStream serializes the messages of one exec request: results,
// heartbeats and the final throw or close.
type execStream struct {
	mu        sync.Mutex
	requestID string
	msg       *agentv1.ExecServerMessage
	start     time.Time
	sink      Sink
	err       error // first delivery failure; nothing is sent after it
}

// emit delivers a frame; s.mu must be held.
func (s *execStream) emit(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
	if s.err == nil {
		s.err = s.sink(f)
	}
	return s.err
}

// control sends a control message; s.mu must be held.
func (s *execStream) control(msg *agentv1.ExecClientControlMessage, cwd *string) error {
	return sendControl(s.requestID, msg, cwd, s.emit)
}

// send is the reply for the request's handler.
func (s *execStream) send(result *agentv1.ExecClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result.Id = s.msg.Id
	result.ExecId = s.msg.ExecId
	result.LocalExecutionTimeMs = elapsedMs(s.start)
	b, err := marshalWithinLimit(result)
	if err != nil {
		return err
	}
	return s.emit(&aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: s.requestID,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
			Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: string(b)},
		},
	})
}

// heartbeats tells the server the request is alive until stop is closed.
func (s *execStream) heartbeats(stop <-chan struct{}) {
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			s.mu.Lock()
			err := s.control(heartbeatMessage(s.msg.Id), nil)
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// finish ends the stream with a throw for err, or a close on success.
func (s *execStream) finish(err error, cwd *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if errors.Is(err, errUnsupportedExec) {
		// Like the desktop, throw and then still close the stream.
		if err := s.control(throwMessage(s.msg.Id, err), nil); err != nil {
			return err
		}
		err = nil
	}
	if err != nil {
		return s.control(throwMessage(s.msg.Id, err), cwd)
	}
	return s.control(closeMessage(s.msg.Id), nil)
}

func elapsedMs(start time.Time) *int32 {
	ms := min(time.Since(start).Milliseconds(), math.MaxInt32)
	return proto.Int32(int32(ms)) //nolint:gosec // clamped above
}

// marshalWithinLimit encodes result, truncating read content until the JSON
// fits in MaxJSON. Other results that are too large fail.
func marshalWithinLimit(result *agentv1.ExecClientMessage) ([]byte, error) {
	errTooLarge := errors.New("result exceeds 8 MiB")
	for {
		b, err := protojson.Marshal(result)
		if err != nil || len(b) <= MaxJSON {
			return b, err
		}
		read := result.GetReadResult().GetSuccess()
		content, ok := read.GetOutput().(*agentv1.ReadSuccess_Content)
		if !ok || content.Content == "" {
			return nil, errTooLarge
		}
		// Escaping only grows the text, so dropping the excess bytes of the
		// encoding from the raw content converges quickly.
		content.Content = truncateUTF8(content.Content, max(0, len(content.Content)-max(1, len(b)-MaxJSON)))
		read.Truncated = true
	}
}

// controlled runs an exec request as a stream with an exec ID, heartbeats and
// a final throw or close.
func (e *Executor) controlled(
	ctx context.Context, f *aiserverv1.GrokBotUserComputerRequestFrame, msg *agentv1.ExecServerMessage, sink Sink,
) error {
	msg.Id = e.nextExec.Add(1)
	s := &execStream{requestID: f.RequestId, msg: msg, start: time.Now(), sink: sink}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.heartbeats(stop)
	}()
	err := e.exec(ctx, msg, s.send)
	close(stop)
	<-stopped
	var cwd *string
	if err != nil && !errors.Is(err, errUnsupportedExec) {
		cwd = e.cwdState(msg)
	}
	return s.finish(err, cwd)
}

// cwdState describes the requested shell working directory for a failed
// request: "exists", "missing" or "unset"; nil if it cannot be determined.
func (e *Executor) cwdState(msg *agentv1.ExecServerMessage) *string {
	var path string
	switch {
	case msg.GetShellStreamArgs() != nil:
		path = msg.GetShellStreamArgs().WorkingDirectory
	case msg.GetBackgroundShellSpawnArgs() != nil:
		path = msg.GetBackgroundShellSpawnArgs().WorkingDirectory
	default:
		return proto.String("unset")
	}
	rel, err := e.relative(path)
	if err != nil {
		return nil
	}
	st, err := e.Files.Stat(rel)
	switch {
	case err == nil && st.IsDir():
		return proto.String("exists")
	case errors.Is(err, os.ErrNotExist):
		return proto.String("missing")
	}
	return nil
}
