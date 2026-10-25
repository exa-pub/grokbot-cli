// Command compat-tls compares the ClientHello of Go's transport with that of
// a Node https.Agent on loopback. It does not launch the desktop app or
// contact the production service.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/exa-pub/grokbot-cli/internal/client"
)

const probeTimeout = 20 * time.Second

// nodeProbe makes one keep-alive POST to argv[1] and prints process.versions.
const nodeProbe = `const https = require('node:https');
const agent = new https.Agent({keepAlive: true, timeout: 5000});
const req = https.request(process.argv[1], {method: 'POST', agent, rejectUnauthorized: false}, r => {
  r.resume();
  r.on('end', () => { console.log(JSON.stringify(process.versions)); agent.destroy(); });
});
req.on('error', e => { console.error(e); process.exitCode = 1; });
req.end();`

type hello struct {
	CipherSuites []uint16              `json:"cipherSuites"`
	Curves       []tls.CurveID         `json:"curves"`
	Versions     []uint16              `json:"versions"`
	ALPN         []string              `json:"alpn"`
	Signatures   []tls.SignatureScheme `json:"signatures"`
}

type report struct {
	Method    string            `json:"method"`
	Caveat    string            `json:"caveat"`
	Runtime   map[string]string `json:"referenceRuntime"`
	Go        hello             `json:"go"`
	Node      hello             `json:"node"`
	GoVersion string            `json:"goVersion"`
}

func main() {
	if err := newCommand().Execute(); err != nil {
		log.Fatal(err)
	}
}

func newCommand() *cobra.Command {
	var node string
	cmd := &cobra.Command{
		Use:           "compat-tls",
		Short:         "Compare Go and Node TLS ClientHellos on loopback",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), probeTimeout)
			defer cancel()
			r, err := probe(ctx, node)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(r)
		},
	}
	cmd.Flags().StringVar(&node, "node", "node", "Node-mode runtime executable; do not pass an ordinary Electron launcher")
	return cmd
}

// recorder captures ClientHellos received by a TLS server.
type recorder struct {
	mu     sync.Mutex
	hellos []hello
}

func (r *recorder) record(c *tls.ClientHelloInfo) (*tls.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hellos = append(r.hellos, hello{
		CipherSuites: list(c.CipherSuites),
		Curves:       list(c.SupportedCurves),
		Versions:     list(c.SupportedVersions),
		ALPN:         list(c.SupportedProtos),
		Signatures:   list(c.SignatureSchemes),
	})
	return nil, nil //nolint:nilnil // nil keeps the server's configuration
}

// list copies s, keeping an empty list non-nil so it encodes as [] rather
// than null.
func list[T any](s []T) []T { return append([]T{}, s...) }

// take returns the oldest recorded hello.
func (r *recorder) take() (hello, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.hellos) == 0 {
		return hello{}, errors.New("no ClientHello received")
	}
	h := r.hellos[0]
	r.hellos = r.hellos[1:]
	return h, nil
}

func probe(ctx context.Context, node string) (*report, error) {
	rec := &recorder{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "ok"); err != nil {
			log.Printf("respond: %v", err)
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: rec.record}
	server.StartTLS()
	defer server.Close()

	if err := goRequest(ctx, server); err != nil {
		return nil, fmt.Errorf("go request: %w", err)
	}
	goHello, err := rec.take()
	if err != nil {
		return nil, err
	}
	versions, err := nodeRequest(ctx, node, server.URL)
	if err != nil {
		return nil, fmt.Errorf("node request: %w", err)
	}
	nodeHello, err := rec.take()
	if err != nil {
		return nil, err
	}
	return &report{
		Method: "Loopback TLS; Go ComputerRPC transport vs Node https.Agent keepAlive=true timeout=5000",
		Caveat: "Node-mode probe, not a desktop-app traffic capture. " +
			"The supplied runtime may be a temporary copy with RunAsNode fuse enabled. " +
			"No account, commands or API requests.",
		Runtime:   versions,
		Go:        goHello,
		Node:      nodeHello,
		GoVersion: runtime.Version(),
	}, nil
}

// goRequest sends one request through the ComputerRPC transport.
func goRequest(ctx context.Context, server *httptest.Server) error {
	base, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		return errors.New("test server transport is not an *http.Transport")
	}
	transport := base.Clone()
	transport.ForceAttemptHTTP2 = false
	h := &http.Client{Transport: client.Transport{Base: transport, Profile: client.ComputerRPC}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
	if err != nil {
		return err
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// nodeRequest runs the Node probe and returns the runtime's versions.
func nodeRequest(ctx context.Context, node, url string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, node, "-e", nodeProbe, url) //nolint:gosec // the runtime is chosen by the operator
	cmd.Stderr = os.Stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var versions map[string]string
	if err = json.Unmarshal(b, &versions); err != nil {
		return nil, err
	}
	return versions, nil
}
