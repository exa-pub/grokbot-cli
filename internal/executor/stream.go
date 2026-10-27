package executor

import (
	"log"
	"os"
	"sync"
	"unicode/utf8"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

func shellMessage(s *agentv1.ShellStream) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: s}}
}

func stdoutEvent(data string) *agentv1.ShellStream {
	return &agentv1.ShellStream{Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: data}}}
}

func stderrEvent(data string) *agentv1.ShellStream {
	return &agentv1.ShellStream{Event: &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: data}}}
}

// shellOutput streams stdout and stderr to the agent and, optionally, to a
// terminal file. A single mutex preserves frame ordering across both streams.
type shellOutput struct {
	mu       sync.Mutex
	send     reply
	file     *os.File
	detached bool  // the command was backgrounded; stop streaming
	finished bool  // the command has exited; discard further output
	err      error // first send failure
}

func (o *shellOutput) emit(event func(string) *agentv1.ShellStream, data string) error {
	return o.send(shellMessage(event(data)))
}

// finish discards all further output, so that nothing reaches the agent after
// the command's exit or the terminal file after its footer.
func (o *shellOutput) finish() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.finished = true
}

func (o *shellOutput) sendErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// maxStreamOutput is how many bytes of a stream the desktop forwards; the
// agent gets nothing of a stream after the write that would exceed it.
const maxStreamOutput = 1 << 20

// streamWriter forwards one stream, holding back a trailing incomplete UTF-8
// sequence until the rest of it arrives.
type streamWriter struct {
	out     *shellOutput
	event   func(string) *agentv1.ShellStream // stdoutEvent or stderrEvent
	partial []byte
	sent    int  // bytes of the stream forwarded so far
	trimmed bool // maxStreamOutput was reached
}

// Write never fails: the process must keep running even if the agent or the
// terminal file stops accepting output, otherwise it would block on a full pipe.
func (w *streamWriter) Write(b []byte) (int, error) {
	o := w.out
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return len(b), nil
	}
	if o.file != nil {
		if _, err := o.file.Write(b); err != nil {
			log.Printf("terminal output: %v", err)
			o.file = nil
		}
	}
	if o.detached || o.err != nil || w.trimmed {
		return len(b), nil
	}
	if w.sent+len(b) > maxStreamOutput {
		// The held-back bytes still go out, as the desktop flushes its decoder.
		w.trimmed = true
		if len(w.partial) > 0 {
			o.err = o.emit(w.event, lossyUTF8(w.partial))
		}
		w.partial = nil
		return len(b), nil
	}
	w.sent += len(b)
	data := append(w.partial, b...)
	n := completeLen(data)
	w.partial = append([]byte(nil), data[n:]...)
	for data = data[:n]; len(data) > 0; {
		n := chunkLen(data)
		if err := o.emit(w.event, lossyUTF8(data[:n])); err != nil {
			o.err = err
			break
		}
		data = data[n:]
	}
	return len(b), nil
}

// flush sends the held-back bytes once the stream has ended.
func (w *streamWriter) flush() {
	w.out.mu.Lock()
	defer w.out.mu.Unlock()
	if len(w.partial) > 0 && !w.out.detached && !w.out.finished && w.out.err == nil {
		w.out.err = w.out.emit(w.event, lossyUTF8(w.partial))
	}
	w.partial = nil
}

// completeLen returns the length of b without a trailing incomplete UTF-8
// sequence.
func completeLen(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

// chunkLen returns at most streamChunk bytes, cut at a rune boundary.
func chunkLen(b []byte) int {
	if len(b) <= streamChunk {
		return len(b)
	}
	n := streamChunk
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	if n == 0 {
		return streamChunk
	}
	return n
}
