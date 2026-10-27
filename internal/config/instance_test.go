package config

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// holdEnv makes the test binary hold the connect lock of the store in that
// directory until its stdin closes.
const holdEnv = "GROK_TEST_HOLD_LOCK"

func TestMain(m *testing.M) {
	if dir := os.Getenv(holdEnv); dir != "" {
		release, err := (&Store{Dir: dir}).Claim("connect", Holder{PID: os.Getpid()})
		if err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("ready\n")
		io.Copy(io.Discard, os.Stdin)
		release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestClaimExcludesOtherHolders(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "store")}
	self := Holder{PID: 42, Version: "v1.2.3", StartedAt: time.Unix(1700000000, 0).UTC()}
	release, err := s.Claim("connect", self)
	if err != nil {
		t.Fatal(err)
	}
	// flock locks belong to the open file, so a second Claim in the same
	// process contends like another process would.
	var busy *BusyError
	if _, err = s.Claim("connect", Holder{}); !errors.As(err, &busy) || busy.Holder != self {
		t.Fatalf("got %v, want holder %+v", err, self)
	}
	if h, held, err := s.Probe("connect"); err != nil || !held || h != self {
		t.Fatal(h, held, err)
	}
	if _, err = s.Claim("update", Holder{}); err != nil {
		t.Fatal("locks with different names conflict:", err)
	}
	release()
	if _, held, err := s.Probe("connect"); err != nil || held {
		t.Fatal("still held after release", err)
	}
	again, err := s.Claim("connect", Holder{})
	if err != nil {
		t.Fatal(err)
	}
	again()
	if st, err := os.Stat(s.instancePath("connect")); err != nil || st.Size() != 0 {
		t.Fatal("lock file removed or holder left behind:", err)
	}
}

func TestProbeWithoutLockFile(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "missing")}
	if _, held, err := s.Probe("connect"); err != nil || held {
		t.Fatal(held, err)
	}
}

func TestProbeBeforeHolderIsWritten(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	f, err := os.OpenFile(s.instancePath("connect"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if ok, err := tryLock(f); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if h, held, err := s.Probe("connect"); err != nil || !held || h != (Holder{}) {
		t.Fatal(h, held, err)
	}
}

func TestClaimReleasedWhenHolderDies(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), holdEnv+"="+s.Dir)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal(line, err)
	}
	var busy *BusyError
	if _, err = s.Claim("connect", Holder{}); !errors.As(err, &busy) || busy.Holder.PID != child.Process.Pid {
		t.Fatal(err)
	}
	child.Process.Kill()
	child.Wait()
	release, err := s.Claim("connect", Holder{})
	if err != nil {
		t.Fatal("lock survived its holder:", err)
	}
	release()
}

func TestClaimNotInheritedByChildren(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	release, err := s.Claim("connect", Holder{})
	if err != nil {
		t.Fatal(err)
	}
	// Like a background command started by connect that outlives it.
	sleep := exec.CommandContext(t.Context(), "sleep", "30")
	if err = sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sleep.Process.Kill()
		sleep.Wait()
	}()
	release()
	again, err := s.Claim("connect", Holder{})
	if err != nil {
		t.Fatal("child process inherited the lock:", err)
	}
	again()
}
