// Package executor implements the Linux user-computer tools. Shell commands run
// with the user's privileges; Root confines file tools and shell cwd, not bash.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-cli/internal/permission"
)

// Size limits for files and for a single exec result message.
const (
	MaxFile = 100 << 20
	MaxJSON = 8 << 20
)

const (
	downloadChunk      = 1 << 20
	streamChunk        = 16 << 10
	defaultListTimeout = 5 * time.Second
	terminalsDir       = "terminals"
	// waitDelay bounds how long Wait blocks on pipes held open by grandchildren.
	waitDelay = time.Second
)

var (
	errFileTooLarge = errors.New("file exceeds 100 MiB")
	errOutsideRoot  = errors.New("path is outside the allowed local-exec root")
)

// Sink delivers response frames to the server.
type Sink func(*aiserverv1.GrokBotUserComputerResponseFrame) error

// reply sends one exec result message of the given kind, e.g. "readResult".
type reply func(*agentv1.ExecClientMessage) error

// Executor runs user-computer requests below Root.
type Executor struct {
	Root   string
	Files  *os.Root
	Policy *permission.Policy
	// Discovery locations; empty means $HOME, $CODEX_HOME and $CLAUDE_CONFIG_DIR.
	DiscoveryHome   string
	CodexHome       string
	ClaudeConfigDir string

	// lastTerminal numbers background output files in Root/terminals.
	lastTerminal atomic.Uint32
	nextExec     atomic.Uint32 // exec IDs
	// foreground holds a token while a foreground shell runs, so each sees
	// the state left by the previous one. Waiting for it can be canceled.
	foreground chan struct{}
	stateMu    sync.Mutex // guards state
	state      shellState
}

// New opens an executor rooted at root, resolving symlinks in root itself.
func New(root string, policy *permission.Policy) (*Executor, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	files, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &Executor{Root: abs, Files: files, Policy: policy, foreground: make(chan struct{}, 1)}, nil
}

// Close releases the root directory handle.
func (e *Executor) Close() error { return e.Files.Close() }

// relative converts path, absolute or relative to Root, to a path relative to
// Root. It rejects lexical escapes; os.Root rejects symlink escapes.
func (e *Executor) relative(path string) (string, error) {
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.Root, path)
	}
	rel, err := filepath.Rel(e.Root, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errOutsideRoot
	}
	return rel, nil
}

// cwd resolves a shell working directory. A missing directory falls back to
// Root with a warning for the agent instead of failing the command.
func (e *Executor) cwd(path string) (dir, warning string, err error) {
	rel, err := e.relative(path)
	if err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(e.Root, rel))
	if errors.Is(err, os.ErrNotExist) {
		return e.Root, fmt.Sprintf("working directory %s does not exist on this machine; running in %s instead\n", path, e.Root), nil
	}
	if err != nil {
		return "", "", err
	}
	if _, err = e.relative(resolved); err != nil {
		return "", "", err
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", "", err
	}
	if !st.IsDir() {
		return "", "", errors.New("working directory is not a directory")
	}
	return resolved, "", nil
}

// Reject answers f with err in the form the request type expects.
func Reject(f *aiserverv1.GrokBotUserComputerRequestFrame, err error, sink Sink) error {
	return failure(f, 0, err, sink)
}

func failure(f *aiserverv1.GrokBotUserComputerRequestFrame, execID uint32, err error, sink Sink) error {
	resp := &aiserverv1.GrokBotUserComputerResponseFrame{RequestId: f.RequestId}
	switch {
	case f.GetExec() != nil:
		return sendControl(f.RequestId, throwMessage(execID, err), nil, sink)
	case f.GetMessagesOp() != nil:
		resp.Frame = &aiserverv1.GrokBotUserComputerResponseFrame_MessagesError{
			MessagesError: &aiserverv1.GrokBotUserComputerMessagesError{Error: err.Error()},
		}
	case f.GetLocalTool() != nil:
		resp.Frame = &aiserverv1.GrokBotUserComputerResponseFrame_LocalToolResult{
			LocalToolResult: &aiserverv1.GrokBotUserComputerLocalToolResult{Status: "failed", Error: err.Error()},
		}
	default:
		resp.Frame = &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
			FileError: &aiserverv1.GrokBotUserComputerFileError{Error: err.Error()},
		}
	}
	return sink(resp)
}

func fileFrame(requestID string, data []byte, seq uint32, last bool) *aiserverv1.GrokBotUserComputerResponseFrame {
	return &aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: requestID,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{
			File: &aiserverv1.GrokBotUserComputerFile{Data: data, Seq: seq, Last: last},
		},
	}
}

// describe returns a human-readable action for the permission prompt, or ""
// if the exec request is not supported.
func describe(msg *agentv1.ExecServerMessage) string {
	switch {
	case msg.GetShellStreamArgs() != nil:
		x := msg.GetShellStreamArgs()
		return fmt.Sprintf("Run %q in %s", x.Command, x.WorkingDirectory)
	case msg.GetBackgroundShellSpawnArgs() != nil:
		x := msg.GetBackgroundShellSpawnArgs()
		return fmt.Sprintf("Run background %q in %s", x.Command, x.WorkingDirectory)
	case msg.GetReadArgs() != nil:
		return "Read " + msg.GetReadArgs().Path
	case msg.GetLsArgs() != nil:
		return "List " + msg.GetLsArgs().Path
	}
	return ""
}

// request is a parsed request frame.
type request struct {
	msg      *agentv1.ExecServerMessage // empty unless the frame is an exec
	action   string                     // shown in the permission prompt
	approved bool                       // authorized by the server
}

func parse(f *aiserverv1.GrokBotUserComputerRequestFrame) (request, error) {
	r := request{msg: &agentv1.ExecServerMessage{}}
	switch v := f.Frame.(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Exec:
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(v.Exec.ServerMessageJson), r.msg); err != nil {
			return r, err
		}
		if r.action = describe(r.msg); r.action == "" {
			r.action = "Execute unsupported local tool"
		}
		r.approved = v.Exec.AuthorizedByApproval || v.Exec.AuthorizedByStanding
	case *aiserverv1.GrokBotUserComputerRequestFrame_Upload:
		r.action = "Write " + v.Upload.Path
		r.approved = v.Upload.AuthorizedByApproval || v.Upload.AuthorizedByStanding
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		source := v.DiscoverAiTool.Source
		if source != sourceCodex && source != sourceClaudeCode {
			return r, errUnsupportedSource
		}
		r.action = "Read AI tool setup and recent history for " + source + " in the home/config directories (may be outside root)"
		r.approved = v.DiscoverAiTool.AuthorizedByApproval || v.DiscoverAiTool.AuthorizedByStanding
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		r.action = "Read " + v.Download.Path
		r.approved = v.Download.AuthorizedByApproval || v.Download.AuthorizedByStanding
	default:
		return r, errors.New("unsupported on Linux")
	}
	return r, nil
}

// Handle checks permission for f, runs it and streams the results to sink.
// Failures are reported to sink; the returned error means sink itself failed.
// server selects server-authoritative permission checks.
func (e *Executor) Handle(ctx context.Context, f *aiserverv1.GrokBotUserComputerRequestFrame, server bool, sink Sink) error {
	switch f.Frame.(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Cancel, *aiserverv1.GrokBotUserComputerRequestFrame_RetireApproval:
		return nil // handled by the scheduler
	}
	defer log.Printf("request %s finished", f.RequestId)
	req, err := parse(f)
	if err == nil {
		log.Printf("request %s: %s", f.RequestId, req.action)
		err = e.Policy.Allow(ctx, req.action, server, req.approved)
	}
	if err != nil {
		return failure(f, 0, err, sink)
	}
	if ctx.Err() != nil {
		return Canceled(f, sink)
	}
	if f.GetExec() != nil {
		return e.controlled(ctx, f, req.msg, sink)
	}
	if err = e.run(ctx, f, req.msg, sink); err != nil {
		return failure(f, 0, err, sink)
	}
	return nil
}

func (e *Executor) run(
	ctx context.Context, f *aiserverv1.GrokBotUserComputerRequestFrame, msg *agentv1.ExecServerMessage, sink Sink,
) error {
	switch v := f.Frame.(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Upload:
		if err := e.upload(v.Upload.Path, v.Upload.Data); err != nil {
			return err
		}
		return sink(fileFrame(f.RequestId, nil, 0, true))
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		return e.download(ctx, f.RequestId, v.Download.Path, sink)
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		data, err := e.discover(ctx, v.DiscoverAiTool.Source)
		if err != nil {
			return err
		}
		return sink(fileFrame(f.RequestId, data, 0, true))
	}
	return nil
}

func (e *Executor) exec(ctx context.Context, msg *agentv1.ExecServerMessage, send reply) error {
	switch {
	case msg.GetReadArgs() != nil:
		return e.read(ctx, msg.GetReadArgs(), send)
	case msg.GetLsArgs() != nil:
		return e.ls(ctx, msg.GetLsArgs(), send)
	case msg.GetShellStreamArgs() != nil:
		return e.shell(ctx, msg.GetShellStreamArgs(), send)
	case msg.GetBackgroundShellSpawnArgs() != nil:
		x := msg.GetBackgroundShellSpawnArgs()
		result, err := e.background(x.Command, x.WorkingDirectory)
		if err != nil {
			return err
		}
		return send(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_BackgroundShellSpawnResult{
			BackgroundShellSpawnResult: &agentv1.BackgroundShellSpawnResult{
				Result: &agentv1.BackgroundShellSpawnResult_Success{Success: result},
			},
		}})
	}
	return errUnsupportedExec
}
