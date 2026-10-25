package computer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

// tickWatch is tick that also applies the end of a watch stream that fails
// right away, as Run would.
func tickWatch(t *testing.T, s *session) error {
	t.Helper()
	if err := tick(t, s); err != nil {
		return err
	}
	if !s.watchAlive {
		return nil
	}
	select {
	case ev := <-s.events:
		s.onEvent(ev)
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not end")
	}
	return nil
}

// After the session is revoked no credential can be issued; Run ends instead
// of retrying forever. The client returns the interceptor's error unwrapped.
func TestLogoutEndsRun(t *testing.T) {
	for _, want := range []error{auth.ErrLogout, auth.ErrNotLoggedIn} {
		var now time.Time
		c := &stubCloud{}
		s := stubSession(t, c, &now)
		srv := stubServer(t, c)
		logout := connect.UnaryInterceptorFunc(func(connect.UnaryFunc) connect.UnaryFunc {
			return func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) { return nil, want }
		})
		s.pv.User = rpc.NewGrokBotServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(logout))
		if err := tick(t, s); !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
}

// A server that rejects every fresh credential is not hammered: the issues
// back off like the desktop's gJe and the watch waits channelRetry.
func TestRejectedCredentialBacksOff(t *testing.T) {
	var now time.Time
	c := &stubCloud{rejectWatch: true, ttl: time.Hour}
	s := stubSession(t, c, &now)
	end := now.Add(5 * time.Minute)
	for now.Before(end) {
		if err := tickWatch(t, s); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	// Issued at 0:00, 0:31, 1:32 and 3:33; each is rejected a second later.
	if issues, _, _ := c.counts(); issues != 4 {
		t.Fatalf("%d issues in 5 minutes", issues)
	}
}

// A proven credential that the server rejects later is replaced right away,
// but the watch only reconnects after channelRetry.
func TestProvenCredentialRejected(t *testing.T) {
	var now time.Time
	c := &stubCloud{connect: true, ttl: time.Hour}
	s := stubSession(t, c, &now)
	tick(t, s) // issue
	tick(t, s) // watch
	select {
	case ev := <-s.events:
		s.onEvent(ev)
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	if !s.proven {
		t.Fatal("connected credential not proven")
	}
	s.invalidate(connect.NewError(connect.CodeUnauthenticated, errors.New("expired")))
	if !s.nextRenew.IsZero() || s.nextDial != now.Add(channelRetry) {
		t.Fatal("proven credential treated as a failed issue", s.nextRenew, s.nextDial)
	}
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 2 || s.watchAlive {
		t.Fatal("not reissued, or dialed before channelRetry", issues, s.watchAlive)
	}
	now = now.Add(channelRetry)
	tick(t, s)
	if !s.watchAlive {
		t.Fatal("watch not redialed after channelRetry")
	}
}

// A slow issue does not hold up the session: the old credential keeps
// submitting until the new one arrives.
func TestSlowIssueKeepsSessionWorking(t *testing.T) {
	var now time.Time
	c := &stubCloud{blockIssue: make(chan struct{}), ttl: time.Hour}
	s := stubSession(t, c, &now)
	s.credential = config.Computer{Credential: "old", ExpiresAtMS: now.Add(4 * time.Minute).UnixMilli()}
	start := time.Now()
	if err := s.onTick(context.Background()); err != nil || !s.issuing {
		t.Fatal("renewal not started", err)
	}
	s.queue(closeFrame("a"))
	s.onTick(context.Background())
	if time.Since(start) > time.Second || !s.submitting {
		t.Fatal("issue blocked the session")
	}
	r := <-s.submitted
	s.onSubmitted(r)
	if len(s.pending) != 0 {
		t.Fatal("submit with the old credential failed")
	}
	close(c.blockIssue)
	if err := settle(t, s); err != nil || s.credential.Credential != "1" {
		t.Fatal("new credential not adopted", err, s.credential.Credential)
	}
}

// computer.json only caches the credential; failing to write it must not
// throw away a valid credential.
func TestUnwritableStoreKeepsCredential(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	var now time.Time
	s := stubSession(t, &stubCloud{ttl: time.Hour}, &now)
	dir := s.pv.Store.Dir
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck,gosec // best effort for TempDir cleanup
	tick(t, s)
	tick(t, s)
	if s.credential.Credential == "" || !s.watchAlive {
		t.Fatal("credential dropped because it could not be cached")
	}
}

// A request polled under server-authoritative mode carries the server's
// approval. If the credential is dropped before it starts, it must not fall
// back to the local policy, which might allow what the server refused.
func TestServerModeFixedWhenAccepted(t *testing.T) {
	var now time.Time
	s := stubSession(t, &stubCloud{}, &now)
	s.credential = config.Computer{Credential: "c", ExpiresAtMS: now.Add(time.Hour).UnixMilli(), ServerAuthoritative: true}
	marker := filepath.Join(s.pv.Executor.Root, "ran")
	s.active = maxActive
	s.accept(&requestFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{
		ServerMessageJson: `{"id":1,"shellStreamArgs":{"command":"touch ran"}}`,
	}}}, true)
	s.invalidate(connect.NewError(connect.CodeUnauthenticated, errors.New("expired")))
	s.active = 0
	s.startJobs()
	if s.active != 0 || len(s.tasks) != 0 || len(s.pending) != 1 || !strings.Contains(s.pending[0].GetControl().GetMessageJson(), "stopped serving") {
		t.Fatal("request not rejected", s.pending)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("request ran under the local policy")
	}
}
