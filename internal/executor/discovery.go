package executor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

// AI tools whose setup can be discovered.
const (
	sourceCodex      = "codex"
	sourceClaudeCode = "claude_code"
)

const (
	discoveryTimeout = 10 * time.Second
	historyBytes     = 256 << 10 // read from the end of history.jsonl
	maxConfigBytes   = MaxFile
	maxDirEntries    = 200
	maxSessionFiles  = 500
	maxSetupItems    = 24
	maxItems         = 40
	maxTitleUnits    = 160 // UTF-16 code units, like the desktop
)

var errUnsupportedSource = errors.New("unsupported AI tool source")

type discoveryItem struct {
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Excerpt      string `json:"excerpt,omitempty"`
	OccurredAtMS int64  `json:"occurredAtMs,omitempty"`
}

type discoveryResult struct {
	Source         string          `json:"source"`
	Found          bool            `json:"found"`
	SessionCount   int             `json:"sessionCount"`
	LastActiveAtMS int64           `json:"lastActiveAtMs,omitempty"`
	Items          []discoveryItem `json:"items"`
}

// secretPatterns match API keys and tokens of common providers, bearer
// tokens, basic credentials, JWTs and long hex strings.
var secretPatterns = []string{
	`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}\b`,
	`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{10,}\b`,
	`\bglpat-[A-Za-z0-9_-]{20,}\b`,
	`\bhf_[A-Za-z0-9]{30,}\b`,
	`\bnpm_[A-Za-z0-9]{36}\b`,
	`(?i:Authorization:\s*Basic\s+[A-Za-z0-9+/=]{8,})`,
	`\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{16,}\b`,
	`\bxox[abpors]-[A-Za-z0-9-]{10,}\b`,
	`\bAKIA[0-9A-Z]{16}\b`,
	`\bAIza[0-9A-Za-z_-]{30,}\b`,
	`(?i:Bearer\s+[A-Za-z0-9._~+/=-]{16,})`,
	`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`,
	`\b[A-Fa-f0-9]{32,}\b`,
}

// redactors strip secrets, links, emails and paths from discovered titles.
var redactors = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(strings.Join(secretPatterns, "|")), "[redacted]"},
	{regexp.MustCompile(`([=:]\s*["']?)[A-Za-z0-9+/_-]{32,}={0,2}`), "${1}[redacted]"},
	// AWS secret access keys: 40 characters of base64, often with slashes.
	{regexp.MustCompile(`(^|[\s"'])[A-Za-z0-9+][A-Za-z0-9+/]{39}($|[\s"',;.])`), "${1}[redacted]${2}"},
	{regexp.MustCompile("(?i)\\b[a-z][a-z0-9+.-]*://[^\\s<>\"'`]+"), "[link]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
	{regexp.MustCompile("[A-Za-z]:\\\\[^\\s<>\"'`,;]+|(?:~|\\.{1,2})?(?:/[^\\s/<>\"'`,;)]+){2,}/?"), "[path]"},
}

var (
	slashCommand = regexp.MustCompile(`^/[A-Za-z][\w:-]*(?:\s|$)`)
	pastedText   = regexp.MustCompile(`\[(?:Pasted text|Image) #\d+[^\]]*\]`)
	mcpSection   = regexp.MustCompile(`^\s*\[\s*mcp_servers\.(?:"([^"]+)"|([A-Za-z0-9_-]+))\s*(\.[^\]]*)?\]\s*$`)
	mcpCommand   = regexp.MustCompile(`^\s*command\s*=\s*"([^"]*)"`)
	mcpURL       = regexp.MustCompile(`^\s*url\s*=\s*"`)
)

// discoveryTitle normalizes whitespace, redacts and shortens text.
func discoveryTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for _, r := range redactors {
		text = r.pattern.ReplaceAllString(text, r.replacement)
	}
	if units := utf16.Encode([]rune(text)); len(units) > maxTitleUnits {
		n := maxTitleUnits - 1
		if utf16.IsSurrogate(rune(units[n-1])) && units[n-1] < 0xdc00 {
			n-- // do not split a surrogate pair
		}
		text = string(utf16.Decode(units[:n])) + "…"
	}
	return text
}

// The discovery paths come from the user's own home directory and
// environment, outside Root by design.

// absent reports errors that mean "not there", like the desktop does for
// ENOENT, ENOTDIR and ELOOP.
func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

func statPath(path string) (os.FileInfo, error) {
	return os.Stat(path) //nolint:gosec // see above
}

// exists reports whether path is a file (wantDir false) or a directory,
// following symlinks.
func exists(path string, wantDir bool) (bool, error) {
	st, err := statPath(path)
	if absent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if wantDir {
		return st.IsDir(), nil
	}
	return st.Mode().IsRegular(), nil
}

// entryIs reports whether a directory entry is a file or directory itself or
// through a symlink.
func entryIs(path string, entry os.DirEntry, wantDir bool) (bool, error) {
	switch {
	case wantDir && entry.IsDir(), !wantDir && entry.Type().IsRegular():
		return true, nil
	case entry.Type()&os.ModeSymlink != 0:
		return exists(path, wantDir)
	}
	return false, nil
}

// openDiscovery opens a regular file. O_NONBLOCK keeps a FIFO from blocking
// until a writer appears.
func openDiscovery(ctx context.Context, path string) (*os.File, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // see above
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = errors.New("expected regular file: " + path)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// readConfig reads a whole configuration file; a missing file reads as
// empty. maxConfigBytes only guards against absurd files.
func readConfig(ctx context.Context, path string) ([]byte, error) {
	f, _, err := openDiscovery(ctx, path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err == nil && len(b) > maxConfigBytes {
		err = fmt.Errorf("AI tool configuration exceeds %d bytes", maxConfigBytes)
	}
	return b, err
}

// readTail reads the last historyBytes of a file, dropping the partial first
// line if the file was cut; a missing file reads as empty.
func readTail(ctx context.Context, path string) ([]byte, error) {
	f, st, err := openDiscovery(ctx, path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n := min(st.Size(), historyBytes)
	b, err := readAtMost(f, n, st.Size()-n)
	if err != nil {
		return nil, err
	}
	if n < st.Size() {
		if _, rest, found := bytes.Cut(b, []byte("\n")); found {
			b = rest
		} else {
			b = nil
		}
	}
	return b, nil
}

// discoveryEntries lists at most maxDirEntries entries of a directory sorted
// by name; a missing directory is empty.
func discoveryEntries(ctx context.Context, path string, descending bool) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if descending {
		slices.Reverse(entries)
	}
	return entries[:min(len(entries), maxDirEntries)], nil
}

// decodeLenient decodes JSON configuration; malformed files count as empty.
func decodeLenient(path string, b []byte, v any) {
	if len(b) == 0 {
		return
	}
	if err := json.Unmarshal(b, v); err != nil {
		log.Printf("discovery: ignoring malformed %s: %v", path, err)
	}
}

// discoveryLayout locates one tool's files.
type discoveryLayout struct {
	root         string // configuration directory
	global       string // global configuration file, if any
	promptDir    string // custom commands, relative to root
	promptDepth  int
	sessionDir   string // session logs, relative to root
	sessionDepth int
}

func (e *Executor) discoveryLayout(source string) (discoveryLayout, error) {
	home := e.DiscoveryHome
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return discoveryLayout{}, err
		}
	}
	switch source {
	case sourceCodex:
		l := discoveryLayout{root: filepath.Join(home, ".codex"), promptDir: "prompts", sessionDir: "sessions", sessionDepth: 4}
		if dir := envOverride(e.CodexHome, "CODEX_HOME"); dir != "" {
			l.root = dir
		}
		return l, nil
	case sourceClaudeCode:
		l := discoveryLayout{
			root:         filepath.Join(home, ".claude"),
			global:       filepath.Join(home, ".claude.json"),
			promptDir:    "commands",
			promptDepth:  2,
			sessionDir:   "projects",
			sessionDepth: 2,
		}
		if dir := envOverride(e.ClaudeConfigDir, "CLAUDE_CONFIG_DIR"); dir != "" {
			l.root = dir
			l.global = filepath.Join(dir, ".claude.json")
		}
		return l, nil
	}
	return discoveryLayout{}, errUnsupportedSource
}

func envOverride(value, key string) string {
	if value == "" {
		value = os.Getenv(key)
	}
	return strings.TrimSpace(value)
}

// found reports whether the tool's configuration exists.
func (l discoveryLayout) found() (bool, error) {
	if found, err := exists(l.root, true); found || err != nil {
		return found, err
	}
	if l.global == "" {
		return false, nil
	}
	return exists(l.global, false)
}

// discover summarizes an AI tool's setup and recent activity: names of MCP
// servers, plugins, skills and commands plus redacted recent requests. File
// contents beyond these names are never included.
func (e *Executor) discover(ctx context.Context, source string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	layout, err := e.discoveryLayout(source)
	if err != nil {
		return nil, err
	}
	result := discoveryResult{Source: source, Items: []discoveryItem{}}
	if result.Found, err = layout.found(); err != nil {
		return nil, err
	}
	if !result.Found {
		return json.Marshal(result)
	}
	d := &discoverer{ctx: ctx, source: source, layout: layout}
	if err = d.collect(); err != nil {
		return nil, err
	}
	setup := dedupItems(d.setup)
	setup = setup[:min(len(setup), maxSetupItems)]
	// Newest first, so that a repeated request keeps its latest time.
	slices.SortStableFunc(d.sessions, func(a, b discoveryItem) int { return cmp.Compare(b.OccurredAtMS, a.OccurredAtMS) })
	sessions := dedupItems(d.sessions)
	sessions = sessions[:min(len(sessions), maxItems-len(setup))]
	result.Items = append(setup, sessions...)
	result.SessionCount = max(d.sessionFiles, len(sessions))
	result.LastActiveAtMS = d.lastActive
	if len(sessions) > 0 {
		result.LastActiveAtMS = max(result.LastActiveAtMS, sessions[0].OccurredAtMS)
	}
	return json.Marshal(result)
}

// discoverer collects items for one tool.
type discoverer struct {
	ctx          context.Context
	source       string
	layout       discoveryLayout
	setup        []discoveryItem
	sessions     []discoveryItem
	sessionFiles int
	lastActive   int64 // newest session file, in Unix milliseconds
}

func (d *discoverer) collect() error {
	steps := []func() error{d.mcpAndPlugins, d.skills, d.commands, d.history, d.countSessions}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (d *discoverer) path(elem ...string) string {
	return filepath.Join(append([]string{d.layout.root}, elem...)...)
}

func (d *discoverer) mcpAndPlugins() error {
	if d.source == sourceCodex {
		b, err := readConfig(d.ctx, d.path("config.toml"))
		if err != nil {
			return err
		}
		d.setup = append(d.setup, codexMCP(string(b))...)
		return nil
	}
	b, err := readConfig(d.ctx, d.layout.global)
	if err != nil {
		return err
	}
	var config struct {
		MCP      map[string]json.RawMessage `json:"mcpServers"`
		Projects map[string]struct {
			MCP map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	decodeLenient(d.layout.global, b, &config)
	d.addMCP(config.MCP)
	for _, name := range slices.Sorted(maps.Keys(config.Projects)) {
		d.addMCP(config.Projects[name].MCP)
	}
	pluginsPath := d.path("plugins", "installed_plugins.json")
	if b, err = readConfig(d.ctx, pluginsPath); err != nil {
		return err
	}
	var plugins struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	decodeLenient(pluginsPath, b, &plugins)
	for _, name := range slices.Sorted(maps.Keys(plugins.Plugins)) {
		d.setup = append(d.setup, discoveryItem{Kind: "plugin", Title: discoveryTitle(name)})
	}
	return nil
}

// addMCP adds Claude Code MCP servers with their transport as the excerpt.
func (d *discoverer) addMCP(servers map[string]json.RawMessage) {
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		var v struct {
			Command string `json:"command"`
			Type    string `json:"type"`
			URL     string `json:"url"`
		}
		if json.Unmarshal(servers[name], &v) != nil {
			continue
		}
		excerpt := v.Type
		switch {
		case v.Command != "":
			excerpt = "stdio · " + filepath.Base(v.Command)
		case v.URL != "" && excerpt == "":
			excerpt = "http"
		case excerpt == "":
			excerpt = "unknown"
		}
		d.setup = append(d.setup, discoveryItem{Kind: "mcp_server", Title: discoveryTitle(name), Excerpt: discoveryTitle(excerpt)})
	}
}

// skills lists skills/<name> directories, or symlinks to them, that contain
// a SKILL.md. Other entries are skipped.
func (d *discoverer) skills() error {
	entries, err := discoveryEntries(d.ctx, d.path("skills"), false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		dir := d.path("skills", entry.Name())
		ok, err := entryIs(dir, entry, true)
		if err == nil && ok {
			ok, err = exists(filepath.Join(dir, "SKILL.md"), false)
		}
		if err != nil {
			return err
		}
		if ok {
			d.setup = append(d.setup, discoveryItem{Kind: "skill", Title: discoveryTitle(entry.Name())})
		}
	}
	return nil
}

// commands lists custom commands as "namespace:name".
func (d *discoverer) commands() error {
	return d.commandDir(d.path(d.layout.promptDir), d.layout.promptDepth, nil)
}

func (d *discoverer) commandDir(dir string, depth int, namespace []string) error {
	entries, err := discoveryEntries(d.ctx, dir, false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		name, isMarkdown := strings.CutSuffix(entry.Name(), ".md")
		if isMarkdown {
			file, err := entryIs(path, entry, false)
			if err != nil {
				return err
			}
			if file {
				parts := append(slices.Clone(namespace), name)
				d.setup = append(d.setup, discoveryItem{Kind: "skill", Title: discoveryTitle(strings.Join(parts, ":"))})
				continue
			}
		}
		if depth == 0 {
			continue
		}
		subdir, err := entryIs(path, entry, true)
		if err != nil {
			return err
		}
		if subdir {
			if err = d.commandDir(path, depth-1, append(slices.Clone(namespace), entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// history adds recent requests from history.jsonl, skipping slash commands.
func (d *discoverer) history() error {
	b, err := readTail(d.ctx, d.path("history.jsonl"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		var h struct {
			Text      string  `json:"text"`      // Codex
			TS        float64 `json:"ts"`        // Codex, seconds
			Display   string  `json:"display"`   // Claude Code
			Timestamp float64 `json:"timestamp"` // Claude Code, milliseconds
		}
		if json.Unmarshal([]byte(line), &h) != nil {
			continue
		}
		text, ts := h.Text, h.TS
		if d.source == sourceClaudeCode {
			text, ts = h.Display, h.Timestamp
		}
		text = strings.TrimSpace(pastedText.ReplaceAllString(text, " "))
		if text == "" || slashCommand.MatchString(text) {
			continue
		}
		if ts > 0 && ts < 1e12 {
			ts *= 1000 // seconds
		}
		d.sessions = append(d.sessions, discoveryItem{Kind: "session", Title: discoveryTitle(text), OccurredAtMS: int64(ts)})
	}
	return nil
}

// countSessions counts the newest session logs and finds the latest activity.
func (d *discoverer) countSessions() error {
	return d.countSessionDir(d.path(d.layout.sessionDir), 0)
}

func (d *discoverer) countSessionDir(dir string, level int) error {
	entries, err := discoveryEntries(d.ctx, dir, true)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if d.sessionFiles >= maxSessionFiles {
			return nil
		}
		switch {
		case entry.IsDir():
			if level < d.layout.sessionDepth {
				if err = d.countSessionDir(filepath.Join(dir, entry.Name()), level+1); err != nil {
					return err
				}
			}
		case entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl"):
			if err = d.countSession(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// countSession counts a session log unless it was removed after the
// directory was read.
func (d *discoverer) countSession(entry os.DirEntry) error {
	st, err := entry.Info()
	if absent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	d.sessionFiles++
	d.lastActive = max(d.lastActive, st.ModTime().UnixMilli())
	return nil
}

// dedupItems drops untitled and repeated items, keeping the first.
func dedupItems(items []discoveryItem) []discoveryItem {
	seen := map[string]bool{}
	out := []discoveryItem{}
	for _, item := range items {
		key := item.Kind + "\n" + item.Title
		if item.Title != "" && !seen[key] {
			seen[key] = true
			out = append(out, item)
		}
	}
	return out
}

// codexMCP lists the MCP servers in a Codex config.toml with their transport.
// It understands just enough TOML for [mcp_servers.<name>] tables.
func codexMCP(text string) []discoveryItem {
	var order []string
	transport := map[string]string{}
	current := ""
	for _, line := range strings.Split(text, "\n") {
		if match := mcpSection.FindStringSubmatch(line); match != nil {
			name := match[1] + match[2]
			if _, ok := transport[name]; !ok {
				order = append(order, name)
				transport[name] = "unknown"
			}
			current = name
			if match[3] != "" {
				current = "" // a subtable such as [mcp_servers.x.env]
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			current = ""
		}
		if current == "" {
			continue
		}
		if match := mcpCommand.FindStringSubmatch(line); match != nil {
			transport[current] = "stdio · " + filepath.Base(match[1])
		} else if mcpURL.MatchString(line) {
			transport[current] = "http"
		}
	}
	items := make([]discoveryItem, 0, len(order))
	for _, name := range order {
		items = append(items, discoveryItem{Kind: "mcp_server", Title: discoveryTitle(name), Excerpt: discoveryTitle(transport[name])})
	}
	return items
}
