package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestVersion(t *testing.T) {
	out, err := run(t, "--version")
	if err != nil || out != "grokbot-cli dev (Sand protocol 0.75.8)\n" {
		t.Fatal(out, err)
	}
}

func TestInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"bogus"}, "unknown command"},
		{[]string{"connect", "--permission", "sometimes"}, `invalid permission "sometimes"`},
		{[]string{"connect", "extra"}, "unknown command"},
		{[]string{"login", "--team", "-1"}, "team must be a positive integer"},
	} {
		if _, err := run(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestMissingCommand(t *testing.T) {
	if _, err := run(t); err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatal(err)
	}
}
