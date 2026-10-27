package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Holder describes the process holding an instance lock.
type Holder struct {
	PID       int       `json:"pid"`
	Exe       string    `json:"exe,omitempty"`
	Version   string    `json:"version,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// BusyError reports that another process holds an instance lock.
type BusyError struct {
	Name   string
	Holder Holder // zero if the holder has not written it yet
}

func (e *BusyError) Error() string {
	if e.Holder.PID == 0 {
		return fmt.Sprintf("another %s is running", e.Name)
	}
	return fmt.Sprintf("another %s is running (pid %d)", e.Name, e.Holder.PID)
}

// claimWait is how long Claim retries. It covers a Probe that holds the lock
// for a moment, not a holder that is shutting down.
const claimWait = time.Second

// maxHolder bounds how much of a lock file is read as Holder.
const maxHolder = 4096

func (s *Store) instancePath(name string) string {
	return filepath.Join(s.Dir, name+".lock")
}

// Claim takes the instance lock name until release is called or the process
// exits, and records self in the lock file. It fails with *BusyError if
// another process holds the lock.
//
// The lock file is never removed: a process that opens a fresh file after the
// old one was unlinked would hold a lock of its own next to the old holder.
func (s *Store) Claim(name string, self Holder) (release func(), err error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	// os.OpenFile sets O_CLOEXEC. Shells and background commands started by
	// the holder must not inherit the descriptor, or the lock would outlive it.
	f, err := os.OpenFile(s.instancePath(name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(claimWait)
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			busy := &BusyError{Name: name, Holder: readHolder(f)}
			f.Close()
			return nil, busy
		}
		time.Sleep(lockPoll)
	}
	if err := writeHolder(f, self); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		// Leave no stale holder for Probe to report while the next holder
		// has taken the lock but not yet recorded itself.
		f.Truncate(0) //nolint:errcheck // only diagnostics
		f.Close()
	}, nil
}

// Probe reports the holder of the instance lock name; held is false if no
// process holds it. Probe takes the lock for a moment to find out.
func (s *Store) Probe(name string) (h Holder, held bool, err error) {
	// Opened for writing like in Claim: NFS emulates flock with POSIX locks,
	// and an exclusive one needs a writable descriptor.
	f, err := os.OpenFile(s.instancePath(name), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Holder{}, false, nil
	}
	if err != nil {
		return Holder{}, false, err
	}
	defer f.Close()
	ok, err := tryLock(f)
	if err != nil || ok {
		return Holder{}, false, err
	}
	return readHolder(f), true, nil
}

func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // fds fit in int
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EINTR):
		return false, nil
	}
	return false, err
}

func writeHolder(f *os.File, h Holder) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(b, 0)
	return err
}

// readHolder returns the recorded holder, or zero if the holder has not
// written it yet.
func readHolder(f *os.File) Holder {
	var h Holder
	b, err := io.ReadAll(io.NewSectionReader(f, 0, maxHolder))
	if err != nil || json.Unmarshal(b, &h) != nil {
		return Holder{}
	}
	return h
}
