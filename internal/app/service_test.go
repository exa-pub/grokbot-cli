package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
)

func testStore(t *testing.T) *config.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestConnectArgs(t *testing.T) {
	root := t.TempDir()
	t.Chdir(filepath.Dir(root))
	opts := InstallOptions{Connect: ConnectOptions{
		Root: filepath.Base(root), Permission: "always", Label: "my box", Debug: true, ClientVersion: "0.1.0",
	}}
	args, err := opts.connectArgs()
	want := []string{"--permission", "always", "--root", root, "--label", "my box", "--debug", "--client-version", "0.1.0"}
	if err != nil || !slices.Equal(args, want) {
		t.Fatal(args, err)
	}
	if args, err = (InstallOptions{Connect: ConnectOptions{Permission: "ask"}}).connectArgs(); err != nil || len(args) != 2 {
		t.Fatal(args, err)
	}
	for _, c := range []ConnectOptions{
		{},
		{Permission: "sometimes"},
		{Permission: "always", Root: filepath.Join(root, "missing")},
	} {
		if _, err := (InstallOptions{Connect: c}).connectArgs(); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestCheckInstallable(t *testing.T) {
	store := testStore(t)
	if err := checkInstallable(store); err == nil || !strings.Contains(err.Error(), "grokbot-tunnel login") {
		t.Fatal(err)
	}
	if err := store.Save("auth", config.Auth{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := checkInstallable(store); err != nil {
		t.Fatal(err)
	}
	release, err := store.Claim("connect", config.Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := checkInstallable(store); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatal(err)
	}
}

func TestConnectRefusesSecondInstance(t *testing.T) {
	store := testStore(t)
	release, err := store.Claim("connect", config.Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = Connect(context.Background(), ConnectOptions{Permission: "always", Label: "other"})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatal(err)
	}
	// The second instance must not touch the store, e.g. save its label.
	if _, err := os.Stat(filepath.Join(store.Dir, "machine.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("machine.json written:", err)
	}
}

func TestConnectWithoutLogin(t *testing.T) {
	store := testStore(t)
	err := Connect(context.Background(), ConnectOptions{Permission: "always"})
	if !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatal(err)
	}
	// A login wiped after the server revoked the session.
	if err = store.Save("auth", config.Auth{}); err != nil {
		t.Fatal(err)
	}
	if err = Connect(context.Background(), ConnectOptions{Permission: "always"}); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatal(err)
	}
	// The lock is released on failure.
	if _, held, err := store.Probe("connect"); held || err != nil {
		t.Fatal("lock held after Connect failed", err)
	}
}

func TestWarnStaleConnect(t *testing.T) {
	store := testStore(t)
	alice := config.Auth{AuthID: "alice", Email: "alice@example.invalid"}
	bob := config.Auth{AuthID: "bob"}
	var out bytes.Buffer
	warnStaleConnect(store, alice, bob, &out)
	if out.Len() != 0 {
		t.Fatal("warned without a running connect:", out.String())
	}
	release, err := store.Claim("connect", config.Holder{PID: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	warnStaleConnect(store, alice, alice, &out)
	warnStaleConnect(store, config.Auth{}, bob, &out)
	if out.Len() != 0 {
		t.Fatal("warned without an account change:", out.String())
	}
	warnStaleConnect(store, alice, bob, &out)
	if !strings.Contains(out.String(), "(pid 7) keeps serving alice@example.invalid") {
		t.Fatal(out.String())
	}
}
