package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
)

const (
	// envelopeReserve leaves room in MaxJSON for the rest of a result message.
	envelopeReserve = 4096
	maxTextRead     = MaxJSON - envelopeReserve
	// Binary data is base64-encoded in JSON.
	maxBinaryRead = maxTextRead * 3 / 4
	// listBudget is how many UTF-16 code units of names a listing may hold.
	listBudget = 2500
	// mostlyCRLF is the share of CRLF line endings, in percent, above which
	// they are normalized to LF.
	mostlyCRLF = 5
)

// binaryExtensions are returned as data instead of text.
var binaryExtensions = []string{".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp"}

// Messages shown to the agent, matching the desktop.
const (
	msgBinaryFile = "File is binary and cannot be read as text"
	msgDirectory  = "Path is a directory, not a file"
)

var errBinaryFile = errors.New("file is binary")

func (e *Executor) upload(path string, data []byte) error {
	rel, err := e.relative(path)
	if err != nil {
		return err
	}
	if len(data) > MaxFile {
		return errFileTooLarge
	}
	// Write through a symlink to its target, as long as it stays inside Root.
	resolved, err := filepath.EvalSymlinks(filepath.Join(e.Root, rel))
	switch {
	case err == nil:
		if rel, err = e.relative(resolved); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	dir := filepath.Dir(rel)
	if err = e.Files.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode, existed, err := e.uploadMode(rel)
	if err != nil {
		return err
	}
	// Write a temporary file and rename it so readers never see partial data.
	temp := filepath.Join(dir, fmt.Sprintf(".grok-upload-%d", time.Now().UnixNano()))
	f, err := e.Files.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer e.removeTemp(temp)
	if existed {
		// Bypass the umask to keep the original permissions.
		err = f.Chmod(mode)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return e.Files.Rename(temp, rel)
}

// uploadMode returns the permissions of an existing target, or the default.
func (e *Executor) uploadMode(rel string) (os.FileMode, bool, error) {
	st, err := e.Files.Stat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return 0o644, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !st.Mode().IsRegular() {
		return 0, false, errors.New("target is not a regular file")
	}
	return st.Mode().Perm(), true, nil
}

func (e *Executor) removeTemp(rel string) {
	if err := e.Files.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("remove %s: %v", rel, err)
	}
}

// open opens a regular file of at most MaxFile bytes below Root.
func (e *Executor) open(path string) (*os.File, error) {
	rel, err := e.relative(path)
	if err != nil {
		return nil, err
	}
	f, err := e.Files.Open(rel)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && (!st.Mode().IsRegular() || st.Size() > MaxFile) {
		err = errors.New("expected regular file no larger than 100 MiB")
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// download streams the file in chunks. The size is fixed when the download
// starts, so a growing file is cut and a shrinking one ends early.
func (e *Executor) download(ctx context.Context, requestID, path string, sink Sink) error {
	f, err := e.open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	remaining := st.Size()
	for seq := uint32(0); ; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A fresh buffer per chunk: the sink may hold on to the frame.
		b := make([]byte, min(downloadChunk, remaining))
		n, err := io.ReadFull(f, b)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		remaining -= int64(n)
		last := remaining == 0 || err != nil
		if err := sink(fileFrame(requestID, b[:n], seq, last)); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

func readMessage(r *agentv1.ReadResult) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: r}}
}

func readError(path string, err error) *agentv1.ReadResult {
	r := &agentv1.ReadResult{}
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.Result = &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: path}}
	case errors.Is(err, os.ErrPermission):
		r.Result = &agentv1.ReadResult_PermissionDenied{PermissionDenied: &agentv1.ReadPermissionDenied{Path: path}}
	default:
		r.Result = &agentv1.ReadResult_Error{Error: &agentv1.ReadError{Path: path, Error: err.Error()}}
	}
	return r
}

func invalidFile(path, reason string) *agentv1.ReadResult {
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_InvalidFile{
		InvalidFile: &agentv1.ReadInvalidFile{Path: path, Reason: reason},
	}}
}

// rootEscape reports whether os.Root refused a path leading outside Root.
// Such requests fail instead of producing a typed result.
func rootEscape(err error) bool { return err != nil && strings.Contains(err.Error(), "path escapes") }

func (e *Executor) read(ctx context.Context, x *agentv1.ReadArgs, send reply) error {
	rel, err := e.relative(x.Path)
	if err != nil {
		return err
	}
	result, err := e.readResult(ctx, rel, x)
	if err != nil {
		return err
	}
	return send(readMessage(result))
}

// readResult reads a file into a typed result. Only failures that the
// desktop reports as errors rather than results are returned as errors.
func (e *Executor) readResult(ctx context.Context, rel string, x *agentv1.ReadArgs) (*agentv1.ReadResult, error) {
	path := filepath.Join(e.Root, rel)
	st, err := e.Files.Stat(rel)
	if rootEscape(err) {
		return nil, err
	}
	if err != nil {
		return readError(path, err), nil
	}
	if st.IsDir() {
		return invalidFile(path, msgDirectory), nil
	}
	b, err := e.readAll(rel)
	if rootEscape(err) {
		return nil, err
	}
	if err != nil {
		return readError(path, err), nil
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	success := &agentv1.ReadSuccess{Path: path, FileSize: int64(len(b))}
	if slices.Contains(binaryExtensions, strings.ToLower(filepath.Ext(path))) {
		if len(b) > maxBinaryRead {
			return readError(path, errors.New("binary read exceeds JSON limit; use download")), nil
		}
		success.Output = &agentv1.ReadSuccess_Data{Data: b}
	} else {
		text, err := decodeFile(rel, b, x.GetEncodingHint())
		if errors.Is(err, errBinaryFile) {
			return invalidFile(path, msgBinaryFile), nil
		}
		if err != nil {
			return invalidFile(path, err.Error()), nil
		}
		setTextRange(success, text, x)
	}
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: success}}, nil
}

func (e *Executor) readAll(rel string) ([]byte, error) {
	f, err := e.open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err == nil && len(b) > MaxFile {
		err = errFileTooLarge
	}
	return b, err
}

// decodeFile decodes text, keeping the envelope of terminal files as UTF-8.
func decodeFile(rel string, b []byte, hint string) (string, error) {
	if filepath.Dir(rel) == terminalsDir && terminalMetadata(b) != nil {
		return decodeTerminalText(b, hint)
	}
	return decodeText(b, hint)
}

// setTextRange stores the requested lines of text in r.
func setTextRange(r *agentv1.ReadSuccess, text string, x *agentv1.ReadArgs) {
	if strings.Count(text, "\r\n")*100 >= max(1, strings.Count(text, "\n"))*mostlyCRLF {
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	lines := splitLines(text)
	start, end := lineRange(len(lines), x.Offset, x.Limit)
	content := strings.Join(lines[start:end], "\n")
	r.TotalLines = int32(min(len(lines), math.MaxInt32)) //nolint:gosec // clamped
	r.RangeApplied = (x.Offset != nil || x.Limit != nil) && text != "" && (x.Offset == nil || int(x.GetOffset()) <= len(lines))
	if len(content) > maxTextRead {
		content = content[:maxTextRead]
		for !utf8.ValidString(content) {
			content = content[:len(content)-1]
		}
		r.Truncated = true
	}
	r.Output = &agentv1.ReadSuccess_Content{Content: content}
}

// decodeText decodes b using the encoding hint. Without a hint it honors a
// UTF-16 BOM, strips a UTF-8 BOM and falls back to Latin-1 for invalid UTF-8.
func decodeText(b []byte, hint string) (string, error) {
	if decoder := textDecoder(b, hint); decoder != nil {
		out, _, err := transform.Bytes(decoder, b)
		return string(out), err
	}
	b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	if bytes.IndexByte(b, 0) >= 0 {
		return "", errBinaryFile
	}
	if !utf8.Valid(b) {
		out, _, err := transform.Bytes(charmap.ISO8859_1.NewDecoder(), b)
		return string(out), err
	}
	return string(b), nil
}

// textDecoder returns the decoder for hint, or nil to treat b as UTF-8.
func textDecoder(b []byte, hint string) transform.Transformer {
	switch strings.ToLower(hint) {
	case "utf-16", "utf-16le", "utf16le":
		return unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()
	case "utf-16be", "utf16be":
		return unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder()
	case "latin1", "latin-1", "iso-8859-1":
		return charmap.ISO8859_1.NewDecoder()
	case "windows-1252":
		return charmap.Windows1252.NewDecoder()
	case "", "utf8", "utf-8", "utf-8-bom", "utf8bom", "utf-8 bom":
		switch {
		case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
			return unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()
		case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
			return unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder()
		}
		return nil
	}
	if encoding, err := ianaindex.MIME.Encoding(hint); err == nil && encoding != nil {
		return encoding.NewDecoder()
	}
	return nil
}

// splitLines splits like the desktop: a trailing newline yields an empty
// last line.
func splitLines(s string) []string { return strings.Split(s, "\n") }

// lineRange converts a 1-based offset (negative counts from the end) and a
// line limit into slice bounds. Like the desktop, an offset past the end
// returns the whole text.
func lineRange(total int, offset *int32, limit *uint32) (start, end int) {
	if offset != nil {
		if o := int(*offset); o > 0 {
			start = o - 1
		} else if o < 0 {
			start = max(0, total+o)
		}
	}
	if start >= total {
		return 0, total
	}
	end = total
	if limit != nil {
		end = min(total, start+int(*limit))
	}
	return start, end
}

func lsMessage(r *agentv1.LsResult) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_LsResult{LsResult: r}}
}

func (e *Executor) ls(ctx context.Context, x *agentv1.LsArgs, send reply) error {
	rel, err := e.relative(x.Path)
	if err != nil {
		return err
	}
	path := filepath.Join(e.Root, rel)
	fail := func(msg string) error {
		return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Error{
			Error: &agentv1.LsError{Path: path, Error: msg},
		}}))
	}
	st, err := e.Files.Stat(rel)
	switch {
	case rootEscape(err):
		return err
	case errors.Is(err, os.ErrNotExist):
		return fail("Path does not exist: " + path)
	case err != nil:
		return fail(err.Error())
	case !st.IsDir():
		return fail("Path is not a directory: " + path)
	}
	timeout := defaultListTimeout
	if x.TimeoutMs != nil {
		timeout = time.Duration(x.GetTimeoutMs()) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	l := lister{root: e.Root, files: e.Files, ignore: x.Ignore, budget: listBudget - nameCost(path)}
	tree, err := l.list(ctx, rel)
	if errors.Is(err, context.DeadlineExceeded) {
		return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Timeout{
			Timeout: &agentv1.LsTimeout{DirectoryTreeRoot: tree},
		}}))
	}
	if err != nil {
		return fail(err.Error())
	}
	return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Success{
		Success: &agentv1.LsSuccess{DirectoryTreeRoot: tree},
	}}))
}
