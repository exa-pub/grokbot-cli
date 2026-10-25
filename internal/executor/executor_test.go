package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-cli/internal/permission"
)

func setup(t *testing.T) *Executor {
	t.Helper()
	return setupHome(t, t.TempDir())
}

// setupHome isolates shells from the user's profile: HOME and ZDOTDIR point to
// home.
func setupHome(t *testing.T, home string) *Executor {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)
	e, err := New(t.TempDir(), &permission.Policy{Mode: "always"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}
func execFrame(value string) *aiserverv1.GrokBotUserComputerRequestFrame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: value}}}
}
func collect(t *testing.T, e *Executor, f *aiserverv1.GrokBotUserComputerRequestFrame) []*aiserverv1.GrokBotUserComputerResponseFrame {
	t.Helper()
	var frames []*aiserverv1.GrokBotUserComputerResponseFrame
	err := e.Handle(context.Background(), f, false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { frames = append(frames, f); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return frames
}
func TestRootConfinement(t *testing.T) {
	e := setup(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.Root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", secret, "escape/secret"} {
		b, _ := json.Marshal(map[string]any{"id": 7, "readArgs": map[string]any{"path": path}})
		frames := collect(t, e, execFrame(string(b)))
		if len(frames) != 1 || frames[0].GetControl() == nil {
			t.Fatalf("escape accepted: %s %#v", path, frames)
		}
	}
	f := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "w", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "escape/new", Data: []byte("bad")}}}
	frames := collect(t, e, f)
	if frames[0].GetFileError() == nil {
		t.Fatal("symlink write accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatal("outside file created")
	}
}
func TestReadRangeAndDigest(t *testing.T) {
	e := setup(t)
	os.WriteFile(filepath.Join(e.Root, "f"), []byte("one\ntwo\nthree\n"), 0600)
	frames := collect(t, e, execFrame(`{"id":42,"readArgs":{"path":"f","offset":2,"limit":1}}`))
	if len(frames) != 2 || frames[0].GetClient() == nil {
		t.Fatal(frames)
	}
	var v struct {
		ID         int
		ReadResult struct {
			Success struct {
				Content      string
				TotalLines   int
				RangeApplied bool
				Sha256       string
			}
		}
	}
	if err := json.Unmarshal([]byte(frames[0].GetClient().MessageJson), &v); err != nil {
		t.Fatal(err)
	}
	if v.ID != 1 || v.ReadResult.Success.Content != "two" || v.ReadResult.Success.TotalLines != 4 || !v.ReadResult.Success.RangeApplied || v.ReadResult.Success.Sha256 != "" {
		t.Fatalf("%+v", v)
	}
}
func TestUploadDownload(t *testing.T) {
	e := setup(t)
	data := []byte(strings.Repeat("x", (1<<20)+19))
	collect(t, e, &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "u", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "new", Data: data}}})
	frames := collect(t, e, &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{Download: &aiserverv1.GrokBotUserComputerDownload{Path: "new"}}})
	var got []byte
	for i, f := range frames {
		if f.GetFile() == nil || f.GetFile().Seq != uint32(i) {
			t.Fatal(f)
		}
		got = append(got, f.GetFile().Data...)
	}
	if string(got) != string(data) || !frames[len(frames)-1].GetFile().Last {
		t.Fatal("bad download")
	}
}
func TestShellTimeoutAndCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[canceled], func(t *testing.T) {
			e := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			var mu sync.Mutex
			var output strings.Builder
			start := time.Now()
			err := e.Handle(ctx, execFrame(`{"id":8,"shellStreamArgs":{"command":"printf hello; printf problem >&2; sleep 20","timeout":200}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
				mu.Lock()
				defer mu.Unlock()
				if f.GetClient() != nil {
					output.WriteString(f.GetClient().MessageJson)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("did not kill process group promptly")
			}
			s := output.String()
			if !strings.Contains(s, "hello") || !strings.Contains(s, "problem") || !strings.Contains(s, `"aborted":true`) {
				t.Fatal(s)
			}
		})
	}
}
func TestAskWithoutTTYAndServerAuthorization(t *testing.T) {
	e := setup(t)
	e.Policy.Mode = "ask"
	f := execFrame(`{"id":9,"readArgs":{"path":"missing"}}`)
	frames := collect(t, e, f)
	if !strings.Contains(frames[0].GetControl().MessageJson, "not approved") {
		t.Fatal(frames)
	}
	e.Policy.Mode = "always"
	var got []*aiserverv1.GrokBotUserComputerResponseFrame
	e.Handle(context.Background(), f, true, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { got = append(got, f); return nil })
	if !strings.Contains(got[0].GetControl().MessageJson, "not approved") {
		t.Fatal("server authorization ignored")
	}
}

func TestListAndUnsupported(t *testing.T) {
	e := setup(t)
	if err := os.Mkdir(filepath.Join(e.Root, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.Root, "child", "note.txt"), []byte("hi"), 0600)
	os.WriteFile(filepath.Join(e.Root, "skip.log"), nil, 0600)
	frames := collect(t, e, execFrame(`{"id":2,"lsArgs":{"path":".","ignore":["*.log"]}}`))
	if frames[0].GetClient() == nil {
		t.Fatal(frames)
	}
	s := frames[0].GetClient().MessageJson
	if !strings.Contains(s, "note.txt") || strings.Contains(s, "skip.log") {
		t.Fatal(s)
	}
	frames = collect(t, e, execFrame(`{"id":3,"writeArgs":{"path":"x"}}`))
	if frames[0].GetControl() == nil || !strings.Contains(frames[0].GetControl().MessageJson, "unsupported") {
		t.Fatal("unsupported request accepted")
	}
}
func TestBackgroundCommands(t *testing.T) {
	for _, kind := range []string{"backgroundShellSpawnArgs", "shellStreamArgs"} {
		t.Run(kind, func(t *testing.T) {
			e := setup(t)
			args := map[string]any{"command": "sleep 0.15; printf finished"}
			if kind == "shellStreamArgs" {
				args["timeout"] = 30
				args["timeoutBehavior"] = "TIMEOUT_BEHAVIOR_BACKGROUND"
			}
			b, _ := json.Marshal(map[string]any{"id": 10, kind: args})
			start := time.Now()
			frames := collect(t, e, execFrame(string(b)))
			if time.Since(start) > time.Second {
				t.Fatal("background spawn blocked")
			}
			if len(frames) == 0 || frames[len(frames)-2].GetClient() == nil {
				t.Fatal("missing background result")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				entries, _ := os.ReadDir(filepath.Join(e.Root, "terminals"))
				found := false
				for _, entry := range entries {
					b, _ := os.ReadFile(filepath.Join(e.Root, "terminals", entry.Name()))
					found = found || strings.Contains(string(b), "finished")
				}
				if found {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("background output not captured")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestLineRange(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	u32 := func(v uint32) *uint32 { return &v }
	for _, tc := range []struct {
		offset     *int32
		limit      *uint32
		start, end int
	}{
		{nil, nil, 0, 5},
		{i32(2), u32(2), 1, 3},
		{i32(-2), nil, 3, 5},
		{i32(-10), u32(1), 0, 1},
		{i32(9), nil, 0, 5},
	} {
		if start, end := lineRange(5, tc.offset, tc.limit); start != tc.start || end != tc.end {
			t.Errorf("lineRange(5, %v, %v) = %d, %d; want %d, %d", tc.offset, tc.limit, start, end, tc.start, tc.end)
		}
	}
}

func TestListBudgetCountsOverflow(t *testing.T) {
	e := setup(t)
	for i := range 500 {
		if err := os.WriteFile(filepath.Join(e.Root, "file-"+strconv.Itoa(i)+".txt"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	frames := collect(t, e, execFrame(`{"lsArgs":{"path":"."}}`))
	tree := clientMessages(t, frames)[0].GetLsResult().GetSuccess().DirectoryTreeRoot
	if tree.ChildrenWereProcessed || len(tree.ChildrenFiles) == 0 || len(tree.ChildrenFiles)+int(tree.NumFiles) != 500 || tree.FullSubtreeExtensionCounts[".txt"] != tree.NumFiles {
		t.Fatal(tree)
	}
}

func TestShellExitCode(t *testing.T) {
	e := setup(t)
	frames := collect(t, e, execFrame(`{"id":5,"shellStreamArgs":{"command":"exit 3"}}`))
	last := frames[len(frames)-2].GetClient()
	if last == nil || !strings.Contains(last.MessageJson, `"code":3`) {
		t.Fatal(frames)
	}
}

func TestBackgroundDoesNotWaitForForeground(t *testing.T) {
	e := setup(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 1"}}`))
	}()
	time.Sleep(100 * time.Millisecond) // let the foreground command start
	start := time.Now()
	collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"true"}}`))
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("background spawn waited for the foreground command")
	}
	<-done
}

func TestShellEnvironmentMarkersOnce(t *testing.T) {
	t.Setenv("SHLVL", "2")
	st := shellState{env: []string{"A=1", "CURSOR_AGENT=0", "SAND_AGENT=1", "CURSOR_AGENT_X=1"}}
	got := strings.Join(st.environ(), " ")
	if got != "A=1 CURSOR_AGENT_X=1 SHLVL=2 CURSOR_AGENT=1 SAND_AGENT=1" {
		t.Fatal(got)
	}
}

func TestTerminalExitCodeOverflow(t *testing.T) {
	header := terminalHeader("/w", "x", 1, time.Unix(0, 0))
	m := terminalMetadata([]byte(header + "\n---\nexit_code: 4294967297\nended_at: x\n---\n"))
	if m == nil || len(m.LastCommands) != 0 || m.CurrentCommand == nil {
		t.Fatal(m)
	}
}

func TestQueuedShellCancelsWhileWaiting(t *testing.T) {
	e := setup(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 1"}}`))
	}()
	time.Sleep(100 * time.Millisecond) // let the first command take the shell
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.Handle(ctx, execFrame(`{"shellStreamArgs":{"command":"true"}}`), false,
		func(*aiserverv1.GrokBotUserComputerResponseFrame) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("canceled command waited for the running one")
	}
	<-done
}

// Linux limits a single exec argument to 128 KiB; state and commands must not
// be passed as arguments.
func TestShellStateAndCommandsAboveArgumentLimit(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	body := strings.Repeat(": padding; ", 16<<10) // about 176 KiB once saved
	define, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": "big_fn() { " + body + "printf fn-ok; }"}})
	collect(t, e, execFrame(string(define)))
	stdout := func(frames []*aiserverv1.GrokBotUserComputerResponseFrame) string {
		var out strings.Builder
		for _, m := range clientMessages(t, frames) {
			out.WriteString(m.GetShellStream().GetStdout().GetData())
		}
		return out.String()
	}
	if got := stdout(collect(t, e, execFrame(`{"shellStreamArgs":{"command":"big_fn"}}`))); got != "fn-ok" {
		t.Fatalf("large function not replayed: %q", got)
	}
	large := strings.Repeat("x", 200<<10)
	command, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": "v=" + large + "; printf %d ${#v}"}})
	if got := stdout(collect(t, e, execFrame(string(command)))); got != strconv.Itoa(len(large)) {
		t.Fatalf("large command failed: %q", got)
	}
	background, _ := json.Marshal(map[string]any{"backgroundShellSpawnArgs": map[string]any{"command": "v=" + large + "; printf bg-%d ${#v}"}})
	collect(t, e, execFrame(string(background)))
	want := "bg-" + strconv.Itoa(len(large))
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(e.Root, "terminals"))
		for _, entry := range entries {
			if b, _ := os.ReadFile(filepath.Join(e.Root, "terminals", entry.Name())); strings.Contains(string(b), want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("large background command produced no output")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
