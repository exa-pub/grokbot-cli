package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
)

// Terminal files look like the desktop's: a metadata header, the raw output
// and, once the command exits, a footer.
//
//	---
//	pid: 42
//	cwd: "/workspace"
//	command: "make"
//	status: running
//	started_at: 2026-01-01T00:00:00.000Z
//	running_for_ms: 0
//	---
//	<output>
//	---
//	exit_code: 0
//	ended_at: 2026-01-01T00:00:01.000Z
//	---
const (
	timestampFormat = "2006-01-02T15:04:05.000Z"
	envelopeStart   = "---\n"
	envelopeEnd     = "\n---\n"
	exitMarker      = "\n---\nexit_code: "
	errorMarker     = "\n---\nerror: "
	// Only this much of the file's head and tail is inspected.
	envelopeWindow = 4096
)

// terminal creates the next free Root/terminals/<n>.txt output file.
func (e *Executor) terminal() (uint32, *os.File, error) {
	if err := e.Files.MkdirAll(terminalsDir, 0o700); err != nil {
		return 0, nil, err
	}
	for {
		n := e.lastTerminal.Add(1)
		f, err := e.Files.OpenFile(fmt.Sprintf("%s/%d.txt", terminalsDir, n), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return n, f, err
	}
}

// statusField pads the status so it can be rewritten in place.
func statusField(status string) string { return fmt.Sprintf("status: %-9s", status) }

func terminalHeader(cwd, command string, pid int, started time.Time) string {
	var b strings.Builder
	b.WriteString(envelopeStart)
	if pid > 0 {
		fmt.Fprintf(&b, "pid: %d\n", pid)
	}
	fmt.Fprintf(&b, "cwd: %s\n", strconv.Quote(cwd))
	fmt.Fprintf(&b, "command: %s\n", strconv.Quote(command))
	b.WriteString(statusField("running") + "\n")
	fmt.Fprintf(&b, "started_at: %s\n", started.UTC().Format(timestampFormat))
	fmt.Fprintf(&b, "running_for_ms: %-9d\n", 0)
	b.WriteString("---\n")
	return b.String()
}

func writeTerminalHeader(f *os.File, cwd, command string, pid int) error {
	_, err := f.WriteString(terminalHeader(cwd, command, pid, time.Now()))
	return err
}

// readAtMost reads up to n bytes at off; a short read at the end of the file
// is not an error.
func readAtMost(f *os.File, n, off int64) ([]byte, error) {
	b := make([]byte, n)
	read, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:read], nil
}

// appendTerminalExit adds the footer, unless the shell already wrote it, and
// updates the status in the header.
func appendTerminalExit(f *os.File, state *os.ProcessState) error {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	tail, err := readAtMost(f, min(end, envelopeWindow), max(0, end-envelopeWindow))
	if err != nil {
		return err
	}
	code := exitCode(state)
	if !bytes.Contains(tail, []byte(exitMarker)) {
		footer := fmt.Sprintf("%s%d\nended_at: %s%s", exitMarker, code, time.Now().UTC().Format(timestampFormat), envelopeEnd)
		if _, err := f.WriteString(footer); err != nil {
			return err
		}
	}
	head, err := readAtMost(f, envelopeWindow, 0)
	if err != nil {
		return err
	}
	headerEnd := bytes.Index(head, []byte(envelopeEnd))
	if headerEnd < 0 {
		return nil
	}
	status := "completed"
	if code != 0 {
		status = "failed"
	}
	if i := bytes.Index(head[:headerEnd], []byte(statusField("running"))); i >= 0 {
		if _, err := f.WriteAt([]byte(statusField(status)), int64(i)); err != nil {
			return err
		}
	}
	return nil
}

// terminalMetadata parses the envelope of a terminal file, or returns nil if b
// is not one.
func terminalMetadata(b []byte) *agentv1.TerminalMetadata {
	header, ok := terminalHeaderBody(b)
	if !ok {
		return nil
	}
	m := &agentv1.TerminalMetadata{}
	c := &agentv1.TerminalMetadata_Command{}
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "cwd":
			if v, err := strconv.Unquote(value); err == nil {
				m.Cwd = &v
			}
		case "command":
			if v, err := strconv.Unquote(value); err == nil {
				c.Command = v
			}
		case "started_at":
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
				c.TimestampMs = proto.Int64(t.UnixMilli())
			}
		}
	}
	if code, ok := terminalExitCode(b); ok {
		c.ExitCode = proto.Int32(code)
		m.LastCommands = []*agentv1.TerminalMetadata_Command{c}
	} else {
		m.CurrentCommand = c
	}
	return m
}

func terminalHeaderBody(b []byte) (string, bool) {
	end := terminalHeaderEnd(b)
	if end < 0 {
		return "", false
	}
	return string(b[len(envelopeStart) : end-len(envelopeEnd)]), true
}

// terminalHeaderEnd returns the offset just past the header of a terminal
// file, or -1 if b has none.
func terminalHeaderEnd(b []byte) int {
	if !bytes.HasPrefix(b, []byte(envelopeStart)) {
		return -1
	}
	i := bytes.Index(b[len(envelopeStart):], []byte(envelopeEnd))
	if i < 0 {
		return -1
	}
	return len(envelopeStart) + i + len(envelopeEnd)
}

func terminalExitCode(b []byte) (int32, bool) {
	footer := bytes.LastIndex(b, []byte(exitMarker))
	if footer < 0 {
		return 0, false
	}
	line, _, _ := strings.Cut(string(b[footer+len(exitMarker):]), "\n")
	code, err := strconv.ParseInt(line, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(code), true
}

// decodeTerminalText decodes the output of a terminal file with hint while
// keeping the UTF-8 header and footer intact.
func decodeTerminalText(b []byte, hint string) (string, error) {
	start := max(terminalHeaderEnd(b), 0)
	end := len(b)
	i := max(bytes.LastIndex(b, []byte(exitMarker)), bytes.LastIndex(b, []byte(errorMarker)))
	if i >= start && bytes.HasSuffix(b, []byte(envelopeEnd)) {
		tail := string(b[i:])
		if strings.Contains(tail, "ended_at: ") && (strings.Contains(tail, "exit_code: ") || strings.Contains(tail, "error: ")) {
			end = i
		}
	}
	body, err := decodeText(b[start:end], hint)
	if err != nil {
		return "", err
	}
	return string(b[:start]) + body + string(b[end:]), nil
}
