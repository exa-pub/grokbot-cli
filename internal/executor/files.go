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
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

const (
	// envelopeReserve leaves room in MaxJSON for the rest of a result message.
	envelopeReserve = 4096
	maxTextRead     = MaxJSON - envelopeReserve
	// Binary data is base64-encoded in JSON.
	maxBinaryRead = maxTextRead * 3 / 4
	// maxSymlinks bounds a chain of symlinks, like Linux's MAXSYMLINKS.
	maxSymlinks = 40
	// listBudget is how many UTF-16 code units of names a listing may hold.
	listBudget = 2500
	// mostlyCRLF is the share of CRLF line endings, in percent, above which
	// they are normalized to LF.
	mostlyCRLF = 5
	// readChunk is the size of the chunks the desktop reads text files in;
	// content beyond max_chars may run up to one chunk further.
	readChunk = 64 << 10
	// sniffSize is how much of a file that is not UTF-8 decides between lossy
	// UTF-8 and Latin-1.
	sniffSize = 1 << 20
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
		return fileTooLarge(int64(len(data)))
	}
	if rel, err = e.uploadTarget(rel); err != nil {
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

// uploadTarget follows symlinks in rel, including dangling ones, so that an
// upload writes through a symlink to its target instead of replacing it. The
// target must stay inside Root.
func (e *Executor) uploadTarget(rel string) (string, error) {
	for range maxSymlinks {
		path := filepath.Join(e.Root, rel)
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return e.relative(resolved)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		// Something is missing: either the file, which is created, or the
		// target of a dangling symlink, which is followed.
		target, err := os.Readlink(path)
		if err != nil {
			return rel, nil //nolint:nilerr // not a symlink: create the file
		}
		if !filepath.IsAbs(target) {
			dir, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			target = filepath.Join(dir, target)
		}
		if rel, err = e.relative(target); err != nil {
			return "", err
		}
	}
	return "", syscall.ELOOP
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

// open opens a regular file of at most MaxFile bytes below Root. O_NONBLOCK
// keeps a FIFO from blocking until a writer appears; it does not affect
// regular files.
func (e *Executor) open(path string) (*os.File, error) {
	rel, err := e.relative(path)
	if err != nil {
		return nil, err
	}
	f, err := e.Files.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	switch {
	case err != nil:
	case !st.Mode().IsRegular():
		err = errors.New("expected regular file")
	case st.Size() > MaxFile:
		err = fileTooLarge(st.Size())
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
		err = fileTooLarge(int64(len(b)))
	}
	return b, err
}

// fileText is a decoded text file.
type fileText struct {
	text     string
	terminal bool // a terminal file, read whole up to max_chars
	// base is the file offset from which text holds the file's bytes as they
	// are, or -1 if it was converted.
	base int
}

// decodeFile decodes text, keeping the envelope of terminal files as UTF-8.
func decodeFile(rel string, b []byte, hint string) (fileText, error) {
	if filepath.Dir(rel) == terminalsDir && terminalHeaderEnd(b) >= 0 {
		text, err := decodeTerminalText(b, hint)
		return fileText{text: text, terminal: true, base: -1}, err
	}
	text, base, err := decodeText(b, hint)
	return fileText{text: text, base: base}, err
}

// setTextRange stores the requested lines of text in r.
func setTextRange(r *agentv1.ReadSuccess, f fileText, x *agentv1.ReadArgs) {
	text := f.text
	normalize := strings.Count(text, "\r\n")*100 >= max(1, strings.Count(text, "\n"))*mostlyCRLF
	lines := splitLines(text)
	start, end := lineRange(len(lines), x.Offset, x.Limit)
	ranged := x.Offset != nil || x.Limit != nil
	r.TotalLines = int32(min(len(lines), math.MaxInt32)) //nolint:gosec // clamped
	r.RangeApplied = ranged && text != "" && (x.Offset == nil || int(x.GetOffset()) <= len(lines))
	selected := lines[start:end]
	units, byLines := maxCharsLimit(f, x, r.RangeApplied)
	if byLines {
		selected, r.Truncated = limitChars(lines, start, end, int(x.GetMaxChars()), f.base)
	}
	if normalize {
		selected = stripCR(lines, start, selected)
	}
	content := strings.Join(selected, "\n")
	if units >= 0 && utf16Len(content) > units {
		content, r.Truncated = truncateUTF16(content, units), true
	}
	if len(content) > maxTextRead {
		content = truncateUTF8(content, maxTextRead)
		r.Truncated = true
	}
	r.Output = &agentv1.ReadSuccess_Content{Content: content}
}

// maxCharsLimit returns how many UTF-16 code units of content max_chars
// leaves, or -1, and whether the lines are limited first as the desktop's
// streaming read does. Like the desktop, max_chars does not limit reads from
// the end, nor an offset past the end, which returns the whole text. A
// terminal file is read whole up to two characters more than asked, so a
// longer one shows as truncated, and its ranges are not limited.
func maxCharsLimit(f fileText, x *agentv1.ReadArgs, rangeApplied bool) (int, bool) {
	ranged := x.Offset != nil || x.Limit != nil
	switch {
	case x.MaxChars == nil:
		return -1, false
	case f.terminal && ranged:
		return -1, false
	case f.terminal:
		return int(x.GetMaxChars()) + 2, false
	case x.GetOffset() < 0 || ranged && !rangeApplied:
		return -1, false
	}
	return int(x.GetMaxChars()) + readChunk, true
}

// stripCR removes the CR of the CRLF endings of selected, the lines of
// lines from first on. A line that max_chars cut short has no ending.
func stripCR(lines []string, first int, selected []string) []string {
	out := slices.Clone(selected)
	for i, line := range out {
		if n := first + i; n < len(lines)-1 && len(line) == len(lines[n]) {
			out[i] = strings.TrimSuffix(line, "\r")
		}
	}
	return out
}

// limitChars returns lines[first:last] up to the line that takes their
// UTF-16 length past maxChars, as the desktop's streaming read does; the
// newlines between lines count, CRs of line endings do not. It reports
// whether more of the range was left out. When the lines hold the file's
// bytes from offset base on, the desktop's read chunks are followed: a line
// that went past maxChars by a chunk boundary inside it ends there.
func limitChars(lines []string, first, last, maxChars, base int) ([]string, bool) {
	at := base
	if base >= 0 {
		for _, line := range lines[:first] {
			at += len(line) + 1
		}
	}
	count := 0
	for i := first; i < last; i++ {
		line := lines[i]
		sep := min(i-first, 1)
		for b := (at/readChunk + 1) * readChunk; base >= 0 && b < at+len(line); b += readChunk {
			n := b - at
			for n > 0 && !utf8.RuneStart(line[n]) {
				n-- // a character the chunk cuts belongs to the next one
			}
			if n > 0 && count+sep+utf16Len(line[:n])-1 > maxChars {
				return append(lines[first:i:i], line[:n]), true
			}
		}
		count += sep + utf16Len(line)
		if i < len(lines)-1 && strings.HasSuffix(line, "\r") {
			count--
		}
		if count > maxChars {
			return lines[first : i+1], i+1 < last
		}
		at += len(line) + 1
	}
	return lines[first:last], false
}

// utf16Len returns the length of s in UTF-16 code units, as JavaScript
// counts characters.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// truncateUTF16 cuts s to at most n UTF-16 code units without splitting a
// character.
func truncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		if units += utf16.RuneLen(r); units > n {
			return s[:i]
		}
	}
	return s
}

// truncateUTF8 cuts s to at most n bytes at a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// decodeText decodes b using the encoding hint. Without a hint it honors a
// UTF-16 BOM and strips a UTF-8 BOM; invalid UTF-8 is decoded as lossy UTF-8
// or, if it does not look like UTF-8, as Latin-1. It also returns the offset
// in b from which the text holds b's bytes as they are, or -1.
func decodeText(b []byte, hint string) (string, int, error) {
	if decoder := textDecoder(b, hint); decoder != nil {
		out, _, err := transform.Bytes(decoder, b)
		return string(out), -1, err
	}
	base := 0
	if bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) {
		b, base = b[3:], 3
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return "", -1, errBinaryFile
	}
	if utf8.Valid(b) {
		return string(b), base, nil
	}
	if mostlyUTF8(lossyUTF8(sniff(b))) {
		return lossyUTF8(b), -1, nil
	}
	out, _, err := transform.Bytes(charmap.ISO8859_1.NewDecoder(), b)
	return string(out), -1, err
}

// sniff returns the start of b that decides how invalid UTF-8 is decoded,
// without a character that the cut leaves incomplete.
func sniff(b []byte) []byte {
	if len(b) <= sniffSize {
		return b
	}
	b = b[:sniffSize]
	for i := len(b) - 1; i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				b = b[:i]
			}
			break
		}
	}
	return b
}

// lossyUTF8 decodes b, replacing each maximal invalid subsequence with
// U+FFFD as a WHATWG decoder does.
func lossyUTF8(b []byte) string {
	out, _, err := transform.Bytes(unicode.UTF8.NewDecoder(), b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "�") // not expected: the decoder replaces
	}
	return string(out)
}

// mostlyUTF8 reports whether text decoded from invalid UTF-8 has more
// characters beyond ASCII than replacement characters, as the desktop
// decides: a run of them loses its last two to a replacement character that
// follows, and the two after one do not count.
func mostlyUTF8(text string) bool {
	const skip = 2
	replaced, valid, run, skipped := 0, 0, 0, 0
	for _, r := range text {
		for range utf16.RuneLen(r) {
			switch {
			case r < utf8.RuneSelf:
				valid, run, skipped = valid+run, 0, 0
			case r == utf8.RuneError:
				replaced++
				valid, run, skipped = valid+max(0, run-skip), 0, skip
			case skipped > 0:
				skipped--
			default:
				run++
			}
		}
	}
	return valid+run > replaced
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
	validUTF8(tree)
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
