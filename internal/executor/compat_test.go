package executor

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

func clientMessages(t *testing.T, frames []*aiserverv1.GrokBotUserComputerResponseFrame) []*agentv1.ExecClientMessage {
	t.Helper()
	out := []*agentv1.ExecClientMessage{}
	for _, f := range frames {
		if f.GetClient() != nil {
			m := &agentv1.ExecClientMessage{}
			if err := protojson.Unmarshal([]byte(f.GetClient().MessageJson), m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	return out
}
func controlMessage(t *testing.T, f *aiserverv1.GrokBotUserComputerResponseFrame) *agentv1.ExecClientControlMessage {
	t.Helper()
	if f.GetControl() == nil {
		t.Fatalf("expected control: %v", f)
	}
	m := &agentv1.ExecClientControlMessage{}
	if err := protojson.Unmarshal([]byte(f.GetControl().MessageJson), m); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestReferenceRangesAndTerminalHeader(t *testing.T) {
	b, err := os.ReadFile("../../testdata/compat/0.78.1/pure-functions.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Ranges []struct {
			Text   string
			Args   agentv1.ReadArgs
			Result struct {
				Content      string
				RangeApplied bool
			}
		}
		TerminalHeader string
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	for i := range fixture.Ranges {
		tc := &fixture.Ranges[i]
		os.WriteFile(filepath.Join(e.Root, "range"), []byte(tc.Text), 0600)
		tc.Args.Path = "range"
		msg := &agentv1.ExecServerMessage{Id: 900, Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &tc.Args}}
		b, _ := protojson.Marshal(msg)
		frames := collect(t, e, execFrame(string(b)))
		result := clientMessages(t, frames)[0].GetReadResult().GetSuccess()
		if result == nil || result.GetContent() != tc.Result.Content || result.RangeApplied != tc.Result.RangeApplied {
			t.Fatalf("range: got %v want %+v", result, tc.Result)
		}
	}
	readStreamingReference(t, e, b)
	started, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00.000Z")
	if got := terminalHeader("/workspace", "printf hello", 42, started); got != fixture.TerminalHeader {
		t.Fatalf("header mismatch: %q vs %q", got, fixture.TerminalHeader)
	}
}

// referenceRuns is a long text stored as runs of one character.
type referenceRuns []struct {
	Char  string
	Count int
}

func (r referenceRuns) String() string {
	var b strings.Builder
	for _, run := range r {
		b.WriteString(strings.Repeat(run.Char, run.Count))
	}
	return b.String()
}

// readStreamingReference reads the texts of the desktop's streaming reader
// cases and the invalid UTF-8 cases through the executor.
func readStreamingReference(t *testing.T, e *Executor, fixtureJSON []byte) {
	t.Helper()
	var fixture struct {
		Streaming []struct {
			Text     string
			TextRuns referenceRuns
			Args     json.RawMessage
			Result   struct {
				Content      string
				ContentRuns  referenceRuns
				TotalLines   int32
				Truncated    bool
				RangeApplied bool
			}
		}
		Fallback []struct {
			Hex        string
			MostlyUtf8 bool
			Lossy      string
		}
	}
	if err := json.Unmarshal(fixtureJSON, &fixture); err != nil {
		t.Fatal(err)
	}
	read := func(text string, args json.RawMessage) *agentv1.ReadSuccess {
		t.Helper()
		os.WriteFile(filepath.Join(e.Root, "stream"), []byte(text), 0600)
		x := &agentv1.ReadArgs{}
		if err := protojson.Unmarshal(args, x); err != nil {
			t.Fatal(err)
		}
		x.Path = "stream"
		msg := &agentv1.ExecServerMessage{Id: 901, Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: x}}
		b, _ := protojson.Marshal(msg)
		return clientMessages(t, collect(t, e, execFrame(string(b))))[0].GetReadResult().GetSuccess()
	}
	if len(fixture.Streaming) == 0 || len(fixture.Fallback) == 0 {
		t.Fatal("fixture without streaming or fallback cases")
	}
	for _, tc := range fixture.Streaming {
		text, want := tc.Text, tc.Result.Content
		if tc.TextRuns != nil {
			text, want = tc.TextRuns.String(), tc.Result.ContentRuns.String()
		}
		got := read(text, tc.Args)
		if got == nil || got.GetContent() != want || got.TotalLines != tc.Result.TotalLines || got.Truncated != tc.Result.Truncated || got.RangeApplied != tc.Result.RangeApplied {
			t.Errorf("streaming %.40q %s: got %d units truncated=%v lines=%d range=%v, want %d units %+v",
				text, tc.Args, utf16Len(got.GetContent()), got.GetTruncated(), got.GetTotalLines(), got.GetRangeApplied(), utf16Len(want), tc.Result.Truncated)
		}
	}
	for _, tc := range fixture.Fallback {
		raw, err := hex.DecodeString(tc.Hex)
		if err != nil {
			t.Fatal(err)
		}
		if got := lossyUTF8(raw); got != tc.Lossy || mostlyUTF8(got) != tc.MostlyUtf8 {
			t.Errorf("fallback %s: lossy %q mostly=%v, want %q %v", tc.Hex, got, mostlyUTF8(got), tc.Lossy, tc.MostlyUtf8)
		}
		want := tc.Lossy
		if !tc.MostlyUtf8 {
			out := make([]rune, len(raw))
			for i, c := range raw {
				out[i] = rune(c) // Latin-1
			}
			want = string(out)
		}
		if got := read(string(raw), json.RawMessage(`{}`)); got.GetContent() != want {
			t.Errorf("fallback read %s: %q, want %q", tc.Hex, got.GetContent(), want)
		}
	}
}

func TestLifecycleBranches(t *testing.T) {
	e := setup(t)
	os.WriteFile(filepath.Join(e.Root, "data"), []byte("ok"), 0600)
	first := collect(t, e, execFrame(`{"id":999,"execId":"external","readArgs":{"path":"data"}}`))
	messages := clientMessages(t, first)
	if len(messages) != 1 || messages[0].Id != 1 || messages[0].ExecId != "external" || messages[0].LocalExecutionTimeMs == nil || controlMessage(t, first[len(first)-1]).GetStreamClose().Id != 1 {
		t.Fatal(first)
	}
	missing := collect(t, e, execFrame(`{"id":999,"readArgs":{"path":"missing"}}`))
	if clientMessages(t, missing)[0].GetReadResult().GetFileNotFound() == nil || controlMessage(t, missing[len(missing)-1]).GetStreamClose().Id != 2 {
		t.Fatal(missing)
	}
	unsupported := collect(t, e, execFrame(`{"id":999,"writeArgs":{"path":"x"}}`))
	if len(unsupported) != 2 || controlMessage(t, unsupported[0]).GetThrow().Id != 3 || controlMessage(t, unsupported[1]).GetStreamClose().Id != 3 {
		t.Fatal(unsupported)
	}
	e.Policy.Mode = "never"
	denied := collect(t, e, execFrame(`{"id":999,"readArgs":{"path":"data"}}`))
	if len(denied) != 1 || controlMessage(t, denied[0]).GetThrow().Id != 0 {
		t.Fatal(denied)
	}
	e.Policy.Mode = "always"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var canceled []*aiserverv1.GrokBotUserComputerResponseFrame
	_ = e.Handle(ctx, execFrame(`{"readArgs":{"path":"data"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { canceled = append(canceled, f); return nil })
	if len(canceled) != 1 || controlMessage(t, canceled[0]).GetStreamClose() == nil {
		t.Fatal(canceled)
	}
	var calls int
	err := e.Handle(context.Background(), execFrame(`{"readArgs":{"path":"data"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { calls++; return errors.New("sink failed") })
	if err == nil || calls != 1 {
		t.Fatalf("sink failure retried: %v %d", err, calls)
	}
}
func TestDownloadExactBoundariesAndMutation(t *testing.T) {
	e := setup(t)
	for _, size := range []int64{0, 1, downloadChunk - 1, downloadChunk, downloadChunk + 1, 2 * downloadChunk, MaxFile} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f, err := os.Create(filepath.Join(e.Root, "download"))
			if err != nil {
				t.Fatal(err)
			}
			f.Truncate(size)
			f.Close()
			var chunks int
			var total int
			err = e.download(context.Background(), "r", "download", func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
				chunks++
				v := f.GetFile()
				total += len(v.Data)
				if v.Seq != uint32(chunks-1) {
					t.Fatal(v)
				}
				if v.Last && total != int(size) {
					t.Fatal("bad last")
				}
				if len(v.Data) == 0 && size > 0 {
					t.Fatal("extra EOF")
				}
				return nil
			})
			if err != nil || total != int(size) || chunks != max(1, int((size+downloadChunk-1)/downloadChunk)) {
				t.Fatal(chunks, total, err)
			}
		})
	}
	os.WriteFile(filepath.Join(e.Root, "growing"), bytes.Repeat([]byte("x"), downloadChunk), 0600)
	err := e.download(context.Background(), "r", "growing", func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		out, _ := os.OpenFile(filepath.Join(e.Root, "growing"), os.O_APPEND|os.O_WRONLY, 0600)
		out.WriteString("new")
		out.Close()
		if !f.GetFile().Last {
			t.Fatal("download exceeded initial snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestShellStateCwdAndUTF8(t *testing.T) {
	e := setup(t)
	os.Mkdir(filepath.Join(e.Root, "child"), 0700)
	first := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"export GROK_TEST_PERSIST=hello; cd child"}}`))
	msgs := clientMessages(t, first)
	exit := msgs[len(msgs)-1].GetShellStream().GetExit()
	if exit.Cwd != filepath.Join(e.Root, "child") {
		t.Fatal(exit)
	}
	second := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"printf '%s|' \"$GROK_TEST_PERSIST\"; pwd"}}`))
	var stdout strings.Builder
	for _, m := range clientMessages(t, second) {
		stdout.WriteString(m.GetShellStream().GetStdout().GetData())
	}
	if stdout.String() != "hello|"+filepath.Join(e.Root, "child")+"\n" {
		t.Fatal(stdout.String())
	}
	var got strings.Builder
	o := &shellOutput{send: func(m *agentv1.ExecClientMessage) error {
		got.WriteString(m.GetShellStream().GetStdout().GetData())
		return nil
	}}
	w := &streamWriter{out: o, event: stdoutEvent}
	data := []byte("hello h\u00e9llo \u4e16\u754c \U0001F30D") // 2-, 3- and 4-byte UTF-8 sequences
	for _, b := range data {
		w.Write([]byte{b})
	}
	w.flush()
	if got.String() != string(data) {
		t.Fatalf("broken utf8: %q", got.String())
	}
}
func TestExecHeartbeatAndTerminalOrder(t *testing.T) {
	e := setup(t)
	var frames []*aiserverv1.GrokBotUserComputerResponseFrame
	var mu sync.Mutex
	err := e.Handle(context.Background(), execFrame(`{"shellStreamArgs":{"command":"sleep 3.1"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		mu.Lock()
		defer mu.Unlock()
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	beats := 0
	for _, f := range frames {
		if f.GetControl() != nil && controlMessage(t, f).GetHeartbeat() != nil {
			beats++
		}
	}
	if beats < 1 || controlMessage(t, frames[len(frames)-1]).GetStreamClose() == nil {
		t.Fatal("missing heartbeat/close", frames)
	}
}

func TestReadTypedErrorsEncodingAndEscapedLimit(t *testing.T) {
	e := setup(t)
	os.Mkdir(filepath.Join(e.Root, "dir"), 0700)
	frames := collect(t, e, execFrame(`{"readArgs":{"path":"dir"}}`))
	if clientMessages(t, frames)[0].GetReadResult().GetInvalidFile() == nil {
		t.Fatal("directory read not typed")
	}
	os.WriteFile(filepath.Join(e.Root, "utf16.txt"), []byte{0xff, 0xfe, 'h', 0, 'i', 0}, 0600)
	frames = collect(t, e, execFrame(`{"readArgs":{"path":"utf16.txt"}}`))
	if clientMessages(t, frames)[0].GetReadResult().GetSuccess().GetContent() != "hi" {
		t.Fatal(frames)
	}
	os.WriteFile(filepath.Join(e.Root, "large.txt"), bytes.Repeat([]byte("\t"), MaxJSON), 0600)
	frames = collect(t, e, execFrame(`{"readArgs":{"path":"large.txt"}}`))
	m := clientMessages(t, frames)[0]
	if !m.GetReadResult().GetSuccess().Truncated || len(frames[0].GetClient().MessageJson) > MaxJSON {
		t.Fatal("JSON escaped limit not respected")
	}
}
func TestListingTimeoutAndTerminalMetadata(t *testing.T) {
	e := setup(t)
	frames := collect(t, e, execFrame(`{"lsArgs":{"path":".","timeoutMs":0}}`))
	if clientMessages(t, frames)[0].GetLsResult().GetTimeout() == nil {
		t.Fatal("timeout did not return partial tree")
	}
	collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf terminal-ok"}}`))
	deadline := time.Now().Add(time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(filepath.Join(e.Root, "terminals", "1.txt"))
		if bytes.Contains(data, []byte("exit_code:")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	metadata := terminalMetadata(data)
	if metadata == nil || len(metadata.LastCommands) != 1 || metadata.LastCommands[0].GetExitCode() != 0 || metadata.GetCwd() != e.Root {
		t.Fatal(string(data), metadata)
	}
	frames = collect(t, e, execFrame(`{"lsArgs":{"path":"terminals"}}`))
	tree := clientMessages(t, frames)[0].GetLsResult().GetSuccess().DirectoryTreeRoot
	if len(tree.ChildrenFiles) != 1 || tree.ChildrenFiles[0].TerminalMetadata != nil {
		t.Fatal(tree)
	}
}

func TestShellOptionsPersist(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	collect(t, e, execFrame(`{"shellStreamArgs":{"command":"set -o noclobber; shopt -s nullglob"}}`))
	frames := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"if [[ -o noclobber ]] && shopt -q nullglob; then printf options-ok; fi"}}`))
	var output strings.Builder
	for _, m := range clientMessages(t, frames) {
		output.WriteString(m.GetShellStream().GetStdout().GetData())
		if stderr := m.GetShellStream().GetStderr().GetData(); stderr != "" {
			t.Errorf("state replay stderr: %s", stderr)
		}
	}
	if output.String() != "options-ok" {
		t.Fatal(output.String(), e.state.options)
	}
}

func TestTerminalMixedEncoding(t *testing.T) {
	header := terminalHeader("/workspace", "printf hi", 42, time.Unix(0, 0))
	body := []byte{0xff, 0xfe, 'h', 0, 'i', 0}
	footer := "\n---\nexit_code: 0\nended_at: 2026-01-01T00:00:00.000Z\n---\n"
	raw := append(append([]byte(header), body...), []byte(footer)...)
	text, err := decodeTerminalText(raw, "")
	if err != nil || text != header+"hi"+footer {
		t.Fatal(text, err)
	}
}

func TestShellFunctionsPersistAliasesDoNot(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	collect(t, e, execFrame(`{"shellStreamArgs":{"command":"shopt -s expand_aliases; alias grok_hi='printf alias-ok'; grok_fn() { printf function-ok; }"}}`))
	frames := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"grok_hi 2>/dev/null || printf no-alias; printf '|'; grok_fn"}}`))
	var output strings.Builder
	for _, m := range clientMessages(t, frames) {
		output.WriteString(m.GetShellStream().GetStdout().GetData())
		if stderr := m.GetShellStream().GetStderr().GetData(); stderr != "" {
			t.Error(stderr)
		}
	}
	if output.String() != "no-alias|function-ok" {
		t.Fatal(output.String())
	}
}

func TestUploadAtomicModeAndInsideSymlink(t *testing.T) {
	e := setup(t)
	target := filepath.Join(e.Root, "target")
	os.WriteFile(target, []byte("old"), 0600)
	os.Chmod(target, 0640)
	if err := os.Symlink("target", filepath.Join(e.Root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := e.upload("link", []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	st, _ := os.Stat(target)
	if string(b) != "new" || st.Mode().Perm() != 0640 {
		t.Fatal(string(b), st.Mode())
	}
	if st, _ := os.Lstat(filepath.Join(e.Root, "link")); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if err := e.upload("nested/new/file", []byte("created")); err != nil {
		t.Fatal(err)
	}
}
