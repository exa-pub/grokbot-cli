package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	aiserverv1 "github.com/exa-pub/grokbot-cli/internal/gen/aiserver/v1"
)

func discoverySetup(t *testing.T) *Executor {
	e := setup(t)
	e.DiscoveryHome = t.TempDir()
	e.CodexHome = filepath.Join(e.DiscoveryHome, ".codex")
	e.ClaudeConfigDir = filepath.Join(e.DiscoveryHome, ".claude")
	return e
}
func put(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestDiscoveryMissingAndCancellation(t *testing.T) {
	e := discoverySetup(t)
	b, err := e.discover(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	var result discoveryResult
	json.Unmarshal(b, &result)
	if result.Found || result.Source != "codex" || result.Items == nil {
		t.Fatal(string(b))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = e.discover(ctx, "codex"); err == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestDiscoveryCodexRedactionAndLimits(t *testing.T) {
	e := discoverySetup(t)
	put(t, filepath.Join(e.CodexHome, "config.toml"), `[mcp_servers.safe]
command = "/usr/bin/tool"
[mcp_servers.remote]
url = "https://secret.example/token"
`)
	put(t, filepath.Join(e.CodexHome, "skills", "skill", "SKILL.md"), "private body must not be sent")
	put(t, filepath.Join(e.CodexHome, "prompts", "review.md"), "private body")
	put(t, filepath.Join(e.CodexHome, "sessions", "2026", "a.jsonl"), "{}")
	var history strings.Builder
	for i := 0; i < 50; i++ {
		json.NewEncoder(&history).Encode(map[string]any{"text": "request " + strings.Repeat("g", i+1) + " user@example.com https://private.example/path sk-123456789012345678901234567890 /home/private/file", "ts": 1700000000 + i})
	}
	put(t, filepath.Join(e.CodexHome, "history.jsonl"), history.String())
	b, err := e.discover(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	var r discoveryResult
	json.Unmarshal(b, &r)
	if !r.Found || len(r.Items) != 40 || r.SessionCount != 36 {
		t.Fatal(string(b))
	}
	for _, secret := range []string{"user@example.com", "private.example", "sk-123", "/home/private", "private body"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("disclosure %s", secret)
		}
	}
	if r.Items[0].Kind != "mcp_server" || r.Items[0].Excerpt != "stdio · tool" {
		t.Fatal(r.Items)
	}
	for _, item := range r.Items {
		if len([]rune(item.Title)) > 160 {
			t.Fatal(item)
		}
	}
}
func TestDiscoveryClaudeMalformedAndAuthorization(t *testing.T) {
	e := discoverySetup(t)
	put(t, filepath.Join(e.ClaudeConfigDir, ".claude.json"), `{"mcpServers":{"one":{"type":"http","url":"https://example.invalid"}},"projects":{"x":{"mcpServers":{"one":{"command":"duplicate"}}}}}`)
	put(t, filepath.Join(e.ClaudeConfigDir, "commands", "ns", "review.md"), "ignored")
	put(t, filepath.Join(e.ClaudeConfigDir, "plugins", "installed_plugins.json"), `{malformed`)
	put(t, filepath.Join(e.ClaudeConfigDir, "history.jsonl"), "not-json\n"+`{"display":"/clear","timestamp":1}`+"\n"+`{"display":"[Image #1] fix tests","timestamp":2}`+"\n")
	b, err := e.discover(context.Background(), "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	var r discoveryResult
	json.Unmarshal(b, &r)
	if !r.Found || len(r.Items) != 3 || r.Items[1].Title != "ns:review" || r.Items[2].Title != "fix tests" {
		t.Fatal(string(b))
	}
	f := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool{DiscoverAiTool: &aiserverv1.GrokBotUserComputerDiscoverAiTool{Source: "codex"}}}
	e.Policy.Mode = "ask"
	frames := collect(t, e, f)
	if frames[0].GetFileError() == nil {
		t.Fatal("non-TTY discovery allowed")
	}
	if _, err = e.discover(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown source accepted")
	}
}

func TestDiscoverySkillsSkipFilesAndFollowSymlinks(t *testing.T) {
	e := discoverySetup(t)
	put(t, filepath.Join(e.CodexHome, "skills", "README.md"), "not a skill")
	put(t, filepath.Join(e.CodexHome, "skills", "real", "SKILL.md"), "body")
	put(t, filepath.Join(e.DiscoveryHome, "elsewhere", "linked", "SKILL.md"), "body")
	if err := os.Symlink(filepath.Join(e.DiscoveryHome, "elsewhere", "linked"), filepath.Join(e.CodexHome, "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	b, err := e.discover(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	var r discoveryResult
	json.Unmarshal(b, &r)
	titles := make([]string, 0, len(r.Items))
	for _, item := range r.Items {
		titles = append(titles, item.Title)
	}
	if strings.Join(titles, ",") != "linked,real" {
		t.Fatal(string(b))
	}
}

func TestDiscoveryReadsLargeClaudeConfig(t *testing.T) {
	e := discoverySetup(t)
	config := `{"mcpServers":{"big":{"command":"tool"}},"padding":"` + strings.Repeat("x", 300<<10) + `"}`
	put(t, filepath.Join(e.ClaudeConfigDir, ".claude.json"), config)
	b, err := e.discover(context.Background(), "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	var r discoveryResult
	json.Unmarshal(b, &r)
	if len(r.Items) != 1 || r.Items[0].Title != "big" {
		t.Fatal(string(b))
	}
}

// A repeated request keeps its latest time.
func TestDiscoveryDedupKeepsNewest(t *testing.T) {
	e := discoverySetup(t)
	put(t, filepath.Join(e.CodexHome, "history.jsonl"), `{"text":"same","ts":1700000000}`+"\n"+`{"text":"same","ts":1700000100}`+"\n")
	b, err := e.discover(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	var r discoveryResult
	json.Unmarshal(b, &r)
	if len(r.Items) != 1 || r.Items[0].OccurredAtMS != 1700000100000 {
		t.Fatal(string(b))
	}
}

// A session log removed after its directory was read is skipped.
func TestDiscoverySessionRemovedWhileCounting(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "a.jsonl"), "{}")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "a.jsonl")); err != nil {
		t.Fatal(err)
	}
	d := &discoverer{}
	if err = d.countSession(entries[0]); err != nil || d.sessionFiles != 0 {
		t.Fatal(err, d.sessionFiles)
	}
}

func TestDiscoveryRedactsMoreSecrets(t *testing.T) {
	for _, secret := range []string{
		"sk_live_" + strings.Repeat("a1", 12),
		"rk_test_" + strings.Repeat("b2", 12),
		"glpat-" + strings.Repeat("c3", 10),
		"hf_" + strings.Repeat("d4", 17),
		"npm_" + strings.Repeat("e5", 18),
		"Authorization: Basic dXNlcjpwYXNzd29yZA==",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		`aws_secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEYX"`,
	} {
		title := discoveryTitle("use " + secret + " now")
		if !strings.Contains(title, "[redacted]") || strings.Contains(title, "EXAMPLE") || strings.Contains(title, secret[len(secret)-6:]) {
			t.Errorf("%q -> %q", secret, title)
		}
	}
	if title := discoveryTitle("basic configuration of /usr/local/bin/tool"); title != "basic configuration of [path]" {
		t.Error(title)
	}
}

// Shortening a title does not split a surrogate pair.
func TestDiscoveryTitleKeepsSurrogatePairs(t *testing.T) {
	title := discoveryTitle(strings.Repeat("x", maxTitleUnits-2) + "\U0001F30D tail")
	if strings.ContainsRune(title, utf8.RuneError) || !strings.HasSuffix(title, "x…") {
		t.Fatalf("%q", title)
	}
}
