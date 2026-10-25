package executor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/exa-pub/grokbot-cli/internal/gen/agent/v1"
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
		} {
			r := expectStdout(t, e, command, "")
			if r.stderr != "" {
				t.Fatalf("%q: stderr %q", command, r.stderr)
			}
		}
		// Commands that ended the shell themselves keep the previous state.
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

// The profile is read once, by an interactive login shell, so aliases and
// functions from the rc file are available and profile edits to PATH do not
// accumulate.
func TestShellProfileReadOnce(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		profile := `export PATH="/grok-profile:$PATH"; echo run >> "$HOME/count"` + "\n"
		rc := "alias grok_alias='printf alias-ok'\ngrok_rc_fn() { printf fn-ok; }\n"
		files := map[string]string{".bash_profile": profile + ". \"$HOME/.bashrc\"\n", ".bashrc": rc, ".zprofile": profile, ".zshrc": rc}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e := setupHome(t, home)
		for range 3 {
			expectStdout(t, e, `printf %s "$PATH" | tr : '\n' | grep -c grok-profile`, "1\n")
		}
		expectStdout(t, e, "grok_alias; printf '|'; grok_rc_fn", "alias-ok|fn-ok")
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
		if r.exit.Cwd != e.Root {
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
		sectionAliases + "\x00\x00" + sectionFunctions + "\x00f () { :; }\n\x00" + sectionEnd + "\x00"
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

func TestShellOptionsAliasesFunctionsPersist(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "set -o noclobber; alias grok_a='printf a'; grok_f() { printf f; }", "")
		r := expectStdout(t, e, "[[ -o noclobber ]] && grok_a; grok_f", "af")
		if r.stderr != "" {
			t.Fatal(r.stderr)
		}
	})
}
