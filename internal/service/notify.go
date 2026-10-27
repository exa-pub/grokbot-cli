package service

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Notifier reports the service state to systemd, as sd_notify(3) does, in a
// unit with Type=notify. Outside such a unit it does nothing.
type Notifier struct{ addr string }

// NewNotifier takes NOTIFY_SOCKET out of the environment, so that commands
// started later, such as the agent's shells, do not inherit it.
func NewNotifier() *Notifier {
	addr := os.Getenv("NOTIFY_SOCKET")
	os.Unsetenv("NOTIFY_SOCKET")
	return &Notifier{addr: addr}
}

// Notify sends state, e.g. "READY=1".
func (n *Notifier) Notify(state string) error {
	if n == nil || n.addr == "" {
		return nil
	}
	// Go maps a leading @ to the abstract namespace, as systemd means it.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: n.addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// procDir is where Owner reads process cgroups.
var procDir = "/proc"

// Owner reports whether process pid runs as the tunnel service and in which
// scope. ok is false for other processes and for processes it cannot see,
// e.g. in another PID namespace.
func Owner(pid int) (s Scope, ok bool) {
	f, err := os.Open(fmt.Sprintf("%s/%d/cgroup", procDir, pid))
	if err != nil {
		return Scope{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// cgroup v2 has one line "0::/path", v1 one line per hierarchy.
		path := sc.Text()
		if strings.HasSuffix(path, "/"+TunnelUnit) {
			return Scope{User: strings.Contains(path, "/user@")}, true
		}
	}
	return Scope{}, false
}

// Journald reports whether stderr goes to the journal, which stamps every
// line itself. JOURNAL_STREAM alone is not enough: children of the service
// inherit it with stderr redirected elsewhere.
func Journald() bool {
	dev, ino, ok := strings.Cut(os.Getenv("JOURNAL_STREAM"), ":")
	if !ok {
		return false
	}
	var st unix.Stat_t
	if unix.Fstat(int(os.Stderr.Fd()), &st) != nil { //nolint:gosec // fds fit in int
		return false
	}
	return dev == strconv.FormatUint(st.Dev, 10) && ino == strconv.FormatUint(st.Ino, 10)
}
