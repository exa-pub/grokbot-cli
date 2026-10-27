package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// eachShell runs f with $SHELL set to bash and, if available, zsh
// ($GROK_TEST_ZSH or zsh on PATH). home is an empty directory for setupHome
// that profile files can be added to first.
func eachShell(t *testing.T, f func(t *testing.T, home string)) {
	shells := map[string]string{"bash": "/bin/bash"}
	if zsh := os.Getenv("GROK_TEST_ZSH"); zsh != "" {
		shells["zsh"] = zsh
	} else if zsh, err := exec.LookPath("zsh"); err == nil {
		shells["zsh"] = zsh
	}
	for name, path := range shells {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SHELL", path)
			f(t, t.TempDir())
		})
	}
}

type shellResult struct {
	stdout, stderr string
	exit           *agentv1.ShellStreamExit
}

func runShell(t *testing.T, e *Executor, command string) shellResult {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": command}})
	var r shellResult
	for _, m := range clientMessages(t, collect(t, e, execFrame(string(b)))) {
		s := m.GetShellStream()
		r.stdout += s.GetStdout().GetData()
		r.stderr += s.GetStderr().GetData()
		if s.GetExit() != nil {
			r.exit = s.GetExit()
		}
	}
	if r.exit == nil {
		t.Fatalf("%q: no exit event", command)
	}
	return r
}

func expectStdout(t *testing.T, e *Executor, command, want string) shellResult {
	t.Helper()
	r := runShell(t, e, command)
	if r.stdout != want {
		t.Fatalf("%q: stdout %q, want %q (stderr %q)", command, r.stdout, want, r.stderr)
	}
	return r
}

// A command must not be able to redirect the state dump into its output.
func TestShellStateDoesNotLeakIntoOutput(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		r := expectStdout(t, e, "export SECRET_TOKEN=abc; exec 3>&1 4>&1 5>&1; echo hi", "hi\n")
		if r.exit.Code != 0 {
			t.Fatal(r.exit)
		}
		expectStdout(t, e, `printf %s "$SECRET_TOKEN"`, "abc")
		for _, command := range []string{
			"exec 4>&1; f() { exit 3; }; f",
			"exec 4>&1; kill -TERM $$",
			"exec 4>&1; exec true",
			// The fallback snapshot's file name cannot be redirected.
			"__grok_fallback=/dev/stdout; exit 0",
		} {
			r := expectStdout(t, e, command, "")
			if strings.Contains(r.stderr, "SECRET_TOKEN") {
				t.Fatalf("%q: stderr %q", command, r.stderr)
			}
		}
		expectStdout(t, e, `printf %s "$SECRET_TOKEN"`, "abc")
	})
}

func TestShellStateSurvivesCommandTrapsAndDescriptors(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "trap 'echo mine' EXIT; export A=1", "mine\n")
		expectStdout(t, e, "exec 4>&-; export B=2", "")
		expectStdout(t, e, "exec 4>/dev/null; export C=3", "")
		expectStdout(t, e, `printf %s "$A$B$C"`, "123")
	})
}

func TestShellChildrenInheritNoStateDescriptors(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs /proc")
	}
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, `(for fd in 3 4 5; do [ -e /proc/self/fd/$fd ] && echo "open $fd"; done; true) & wait`, "")
	})
}

func TestShellStateDropsVolatileSettings(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "set -euo pipefail")
		expectStdout(t, e, `false | true; printf %s $?; false; printf after; printf %s "${UNSET_VAR_X}"`, "0after")
		first := runShell(t, e, "printf %s $SHLVL").stdout
		expectStdout(t, e, "printf %s $SHLVL", first)
	})
}

// The profile is read once, by an interactive login shell, so functions from
// the rc file are available and profile edits to PATH, EDITOR and SHLVL do not
// accumulate or override the agent's. Aliases are not expanded: an
// interactive alias such as rm -i would silently keep files.
func TestShellProfileReadOnce(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		profile := `export PATH="/grok-profile:$PATH"; export EDITOR=vim; echo run >> "$HOME/count"` + "\n"
		rc := "alias grok_alias='printf alias-ok'\nalias rm='rm -i'\ngrok_rc_fn() { printf fn-ok; }\n"
		files := map[string]string{".bash_profile": profile + ". \"$HOME/.bashrc\"\n", ".bashrc": rc, ".zprofile": profile, ".zshrc": rc}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e := setupHome(t, home)
		runShell(t, e, "export EDITOR=nano")
		level := runShell(t, e, "printf %s $SHLVL").stdout
		for range 3 {
			expectStdout(t, e, `printf %s "$PATH" | tr : '\n' | grep -c grok-profile; printf '%s|%s' "$EDITOR" "$SHLVL"`, "1\nnano|"+level)
		}
		expectStdout(t, e, "grok_rc_fn; grok_alias 2>/dev/null || printf '|no-alias'", "fn-ok|no-alias")
		expectStdout(t, e, "echo x > victim; rm victim </dev/null; [ -e victim ] || printf removed", "removed")
		if b, _ := os.ReadFile(filepath.Join(home, "count")); string(b) != "run\n" {
			t.Fatalf("profile ran %q", b)
		}
	})
}

func TestShellStartupFileClosingCommandDescriptor(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		startup := filepath.Join(home, "startup.sh")
		if err := os.WriteFile(startup, []byte("exec 5<&-\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BASH_ENV", startup)
		if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(". "+startup+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e := setupHome(t, home)
		r := runShell(t, e, "printf ran")
		if r.stdout != "" || r.exit.Code != 126 || !strings.Contains(r.stderr, "did not run") {
			t.Fatal(r)
		}
	})
}

// Leaving Root with cd keeps the rest of the state; the next command starts
// where the previous one did.
func TestShellCwdOutsideRootKeepsState(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		r := runShell(t, e, "export KEPT=1; cd /")
		if r.exit.Cwd != e.Root || !strings.Contains(r.stderr, "Working directory / is outside") {
			t.Fatal(r.exit)
		}
		expectStdout(t, e, `printf '%s|' "$KEPT"; pwd`, "1|"+e.Root+"\n")
	})
}

func TestBackgroundReplaysForegroundState(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "grok_bg_fn() { printf bg-fn-ok; }; export BG_VAR=1")
		collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"grok_bg_fn; export BG_VAR=2"}}`))
		path := filepath.Join(e.Root, "terminals", "1.txt")
		deadline := time.Now().Add(2 * time.Second)
		var data []byte
		for !strings.Contains(string(data), "exit_code:") {
			if time.Now().After(deadline) {
				t.Fatalf("background did not finish: %q", data)
			}
			time.Sleep(10 * time.Millisecond)
			data, _ = os.ReadFile(path)
		}
		if !strings.Contains(string(data), "bg-fn-ok") || strings.Count(string(data), "exit_code:") != 1 {
			t.Fatalf("%q", data)
		}
		// Background commands do not change the foreground state.
		expectStdout(t, e, `printf %s "$BG_VAR"`, "1")
	})
}

func TestParseSnapshotRejectsTruncated(t *testing.T) {
	full := "/w\x00A=1\x00SHLVL=3\x00_=/usr/bin/env\x00\x00" + sectionOptions + "\x00set -o noclobber\nset -o errexit\nset -o monitor\nset +o nounset\nshopt -s login_shell\nshopt -s nullglob\n\x00" +
		sectionFunctions + "\x00f () { :; }\n\x00" + sectionEnd + "\x00"
	st, ok := parseSnapshot([]byte(full))
	if !ok || strings.Join(st.env, " ") != "A=1" || strings.Join(st.options, "|") != "set -o noclobber|shopt -s nullglob" || st.functions != "f () { :; }\n" {
		t.Fatal(ok, st)
	}
	if _, ok := parseSnapshot([]byte(full[:len(full)-len(sectionEnd)-2])); ok {
		t.Fatal("truncated snapshot accepted")
	}
	if _, ok := parseSnapshot([]byte("relative\x00A=1\x00" + sectionEnd + "\x00")); ok {
		t.Fatal("relative cwd accepted")
	}
}

func TestShellOptionsAndFunctionsPersist(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "set -o noclobber; alias grok_a='printf a'; grok_f() { printf f; }", "")
		r := expectStdout(t, e, "[[ -o noclobber ]] && grok_f; grok_a 2>/dev/null || printf '|no-alias'", "f|no-alias")
		if r.stderr != "" {
			t.Fatal(r.stderr)
		}
	})
}

// The command text is not left in the positional parameters.
func TestShellCommandHasNoPositionalParameters(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, `echo "$#|${1:-none}"; for arg; do echo "arg $arg"; done`, "0|none\n")
		expectStdout(t, e, "set -u; echo \"${1-unset}\"", "unset\n")
	})
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	// eval reports the command's own line numbers.
	expectStdout(t, e, "echo $LINENO\necho $LINENO", "1\n2\n")
}

// xtrace and verbose would trace the replay of every later command.
func TestShellTracingNotCarriedOver(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "grok_f() { :; }; set -xv")
		r := expectStdout(t, e, "echo a", "a\n")
		if r.stderr != "" {
			t.Fatalf("stderr %q", r.stderr)
		}
	})
}

// A command that ends the shell itself, as with set -e, still keeps its
// state through the EXIT trap. Without any snapshot the agent is told that the
// state was not kept.
func TestShellStateSavedWhenCommandExits(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		sub := filepath.Join(e.Root, "sub")
		r := runShell(t, e, "mkdir sub; cd sub; export Z=1; set -e; false; echo unreachable")
		if r.stdout != "" || r.stderr != "" || r.exit.Code != 1 || r.exit.Cwd != sub {
			t.Fatal(r)
		}
		r = runShell(t, e, "export Y=2; f() { exit 4; }; f")
		if r.exit.Code != 4 || r.stderr != "" {
			t.Fatal(r)
		}
		expectStdout(t, e, `printf '%s%s|' "$Z" "$Y"; pwd`, "12|"+sub+"\n")
		r = runShell(t, e, "export X=3; cd ..; trap - EXIT; exit 5")
		if r.exit.Code != 5 || r.exit.Cwd != sub || !strings.Contains(r.stderr, "were not kept") {
			t.Fatal(r)
		}
		expectStdout(t, e, `printf '%s|' "$X"; pwd`, "|"+sub+"\n")
		// Subshells do not save their state.
		expectStdout(t, e, `(cd /; export W=1; exit 0); printf '%s|' "$W"; exit 0`, "|")
		expectStdout(t, e, `printf '%s|' "$W"; pwd`, "|"+sub+"\n")
	})
}

// Background jobs of a foreground command outlive it: its pipes are drained,
// not closed. Output that arrives shortly after the command exits is still
// forwarded, before the exit.
func TestShellBackgroundJobSurvivesCommand(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "(sleep 0.2; echo late) &", "late\n")
		start := time.Now()
		runShell(t, e, "(sleep 1.5; echo later; echo later >&2; echo alive > marker) &")
		if time.Since(start) > 1400*time.Millisecond {
			t.Fatal("command waited for its background job")
		}
		marker := filepath.Join(e.Root, "marker")
		deadline := time.Now().Add(4 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("background job was killed")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// A command that finished is not reported as aborted, even when its context
// ends at the same time.
func TestShellFinishedCommandNotAborted(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 20 {
		var exit *agentv1.ShellStreamExit
		send := func(m *agentv1.ExecClientMessage) error {
			if m.GetShellStream().GetExit() != nil {
				exit = m.GetShellStream().GetExit()
			}
			return nil
		}
		sh, err := e.startShell(&agentv1.ShellArgs{Command: "true", Timeout: 1}, e.Root, shellState{}, send)
		if err != nil {
			t.Fatal(err)
		}
		<-sh.done
		if err := sh.wait(ctx); err != nil {
			t.Fatal(err)
		}
		closeLogged(sh.snapshot)
		closeLogged(sh.fallback)
		if exit == nil || exit.Aborted {
			t.Fatal(exit)
		}
	}
}

// Reading the profile does not hold up a canceled command; the profile still
// applies to the next one.
func TestShellInitHonorsCancellation(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		profile := "sleep 1; export FROM_PROFILE=1\n"
		for _, name := range []string{".bash_profile", ".zprofile"} {
			if err := os.WriteFile(filepath.Join(home, name), []byte(profile), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e := setupHome(t, home)
		for _, command := range []string{
			`{"shellStreamArgs":{"command":"true"}}`,
			`{"backgroundShellSpawnArgs":{"command":"true"}}`,
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			start := time.Now()
			err := e.Handle(ctx, execFrame(command), false, func(*aiserverv1.GrokBotUserComputerResponseFrame) error { return nil })
			cancel()
			if err != nil || time.Since(start) > 500*time.Millisecond {
				t.Fatal(err, time.Since(start))
			}
		}
		expectStdout(t, e, `printf %s "$FROM_PROFILE"`, "1")
	})
}

// The footer is always the one Go writes, whatever the output contains, and
// the status is updated however long the header is.
func TestTerminalFooterAndStatus(t *testing.T) {
	fake := `printf '\n---\nexit_code: 0\nended_at: x\n---\n'; exit 7`
	long := "true #" + strings.Repeat("x", 5000)
	for _, tc := range []struct{ command, status, code string }{{fake, "failed", "7"}, {long, "completed", "0"}} {
		for _, kind := range []string{"backgroundShellSpawnArgs", "shellStreamArgs"} {
			e := setup(t)
			args := map[string]any{"command": tc.command}
			if kind == "shellStreamArgs" {
				args["timeoutBehavior"] = "TIMEOUT_BEHAVIOR_BACKGROUND"
				args["timeout"] = 5000
			}
			b, _ := json.Marshal(map[string]any{kind: args})
			collect(t, e, execFrame(string(b)))
			data := waitTerminal(t, e, "1.txt")
			if !strings.Contains(data[len(data)-60:], "\n---\nexit_code: "+tc.code+"\nended_at: ") {
				t.Fatalf("%s %q: %q", kind, tc.command, data[max(0, len(data)-120):])
			}
			if strings.Count(data, "status: ") != 1 || !strings.Contains(data, statusField(tc.status)+"\n") {
				t.Fatalf("%s %q: status not %s", kind, tc.command, tc.status)
			}
		}
	}
}

// waitTerminal returns terminal file name once its footer is written.
func waitTerminal(t *testing.T, e *Executor, name string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(e.Root, "terminals", name))
		if strings.HasSuffix(string(data), envelopeEnd) && strings.Contains(string(data[max(0, len(data)-100):]), "ended_at: ") {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal %s not finished: %q", name, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackgroundReportsMissingCwd(t *testing.T) {
	e := setup(t)
	collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf ran","workingDirectory":"missing"}}`))
	data := waitTerminal(t, e, "1.txt")
	if !strings.Contains(data, "---\nworking directory missing does not exist") || !strings.Contains(data, "instead\nran") {
		t.Fatalf("%q", data)
	}
}

func TestTerminalTextInvalidEnvelope(t *testing.T) {
	raw := "---\ncommand: \"\xff\"\nstatus: completed\n---\nout\n---\nexit_code: 0\xfe\nended_at: x\n---\n"
	text, err := decodeTerminalText([]byte(raw), "")
	if err != nil || !utf8.ValidString(text) || !strings.Contains(text, "out") {
		t.Fatalf("%q %v", text, err)
	}
}

// Commands get git's core.quotePath=false like the desktop's, once, also
// when the saved state already carries it.
func TestGitQuotePathForCommands(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		for range 2 {
			expectStdout(t, e, `printf '%s %s %s' "$GIT_CONFIG_COUNT" "$GIT_CONFIG_KEY_0" "$GIT_CONFIG_VALUE_0"`, "1 core.quotePath false")
		}
		if _, err := exec.LookPath("git"); err == nil {
			expectStdout(t, e, "git config core.quotePath", "false\n")
		}
	})
}

func TestWithGitConfig(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", "GIT_CONFIG_KEY_0=core.quotePath GIT_CONFIG_VALUE_0=false GIT_CONFIG_COUNT=1"},
		{"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=user.name GIT_CONFIG_KEY_1=core.quotePath GIT_CONFIG_VALUE_1=false GIT_CONFIG_COUNT=2"},
		{"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=Core.QuotePath", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=Core.QuotePath"},
		// A key past the count does not count; parseInt reads leading digits.
		{"GIT_CONFIG_COUNT=0x GIT_CONFIG_KEY_0=core.quotepath", "GIT_CONFIG_COUNT=0x GIT_CONFIG_KEY_0=core.quotepath GIT_CONFIG_KEY_0=core.quotePath GIT_CONFIG_VALUE_0=false GIT_CONFIG_COUNT=1"},
		{"GIT_CONFIG_COUNT=-1", "GIT_CONFIG_COUNT=-1 GIT_CONFIG_KEY_0=core.quotePath GIT_CONFIG_VALUE_0=false GIT_CONFIG_COUNT=1"},
		{"GIT_CONFIG_COUNT=2abc", "GIT_CONFIG_COUNT=2abc GIT_CONFIG_KEY_2=core.quotePath GIT_CONFIG_VALUE_2=false GIT_CONFIG_COUNT=3"},
	} {
		if got := strings.Join(withGitConfig(strings.Fields(tc.env)), " "); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.env, got, tc.want)
		}
	}
	for s, want := range map[string]int{" \t+7": 7, "": 0, "x1": 0, "-0": 0, "99999999999999999999": 0} {
		if got := parseCount(s); got != want {
			t.Errorf("parseCount(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestShellPlainOutputEnv(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		t.Setenv("TERM", "xterm-256color")
		e := setupHome(t, home)
		expectStdout(t, e, `printf '%s %s %s %s' "$TERM" "$NO_COLOR" "$FORCE_COLOR" "$_ZO_DOCTOR"; export TERM=vt100`, "dumb 1 0 0")
		expectStdout(t, e, `printf %s "$TERM"`, "vt100")
	})
}
