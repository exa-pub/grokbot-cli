package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/config"
	"github.com/exa-pub/grokbot-tunnel/internal/update"
)

func fakeBinary(version string) []byte {
	return []byte("#!/bin/sh\necho 'grokbot-tunnel " + version + " (Sand protocol 0.78.1)'\n")
}

// fakeReleases serves a repository whose latest release is v1.2.3.
func fakeReleases(t *testing.T) *update.Client {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	bin := fakeBinary("v1.2.3")
	tw.WriteHeader(&tar.Header{Name: "grokbot-tunnel-linux-amd64", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bin))})
	tw.Write(bin)
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v1.2.3", http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(hex.EncodeToString(sum[:]) + "  grokbot-tunnel-linux-amd64.tar.gz\n"))
	})
	mux.HandleFunc("/releases/download/v1.2.3/grokbot-tunnel-linux-amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &update.Client{Base: srv.URL, Arch: "amd64"}
}

type updateCase struct {
	opts     UpdateOptions
	replaced bool
	fails    bool
	output   string
}

func TestUpdate(t *testing.T) {
	for name, tc := range map[string]updateCase{
		"newer":             {opts: UpdateOptions{Version: "v1.0.0"}, replaced: true, output: "Updated grokbot-tunnel v1.0.0 -> v1.2.3"},
		"explicit":          {opts: UpdateOptions{Version: "v1.0.0", Target: "1.2.3"}, replaced: true},
		"same":              {opts: UpdateOptions{Version: "v1.2.3"}, output: "v1.2.3 is up to date"},
		"same forced":       {opts: UpdateOptions{Version: "v1.2.3", Force: true}, replaced: true},
		"downgrade":         {opts: UpdateOptions{Version: "v2.0.0"}, fails: true},
		"downgrade auto":    {opts: UpdateOptions{Version: "v2.0.0", Auto: true}, output: "Not updating: v1.2.3 is older"},
		"downgrade forced":  {opts: UpdateOptions{Version: "v2.0.0", Force: true}, replaced: true},
		"dev":               {opts: UpdateOptions{Version: "v1.0.0-3-gabc1234"}, fails: true},
		"dev auto":          {opts: UpdateOptions{Version: "dev", Auto: true}, output: "development build"},
		"dev forced":        {opts: UpdateOptions{Version: "dev", Force: true}, replaced: true},
		"check":             {opts: UpdateOptions{Version: "v1.0.0", Check: true}, output: "Available: v1.2.3 (an update is available)"},
		"packaged":          {opts: UpdateOptions{Version: "v1.0.0", Packager: "apt"}, fails: true},
		"packaged auto":     {opts: UpdateOptions{Version: "v1.0.0", Packager: "apt", Auto: true}, output: "installed by apt"},
		"missing release":   {opts: UpdateOptions{Version: "v1.0.0", Target: "v9.9.9"}, fails: true},
		"invalid version":   {opts: UpdateOptions{Version: "v1.0.0", Target: "newest"}, fails: true},
		"unchanged on test": {opts: UpdateOptions{Version: "v1.2.3", Check: true}, output: "(up to date)"},
	} {
		t.Run(name, func(t *testing.T) { runUpdate(t, tc) })
	}
}

func runUpdate(t *testing.T, tc updateCase) {
	testStore(t)
	exe := filepath.Join(t.TempDir(), "grokbot-tunnel")
	old := fakeBinary("v0")
	os.WriteFile(exe, old, 0o755)
	tc.opts.Exe, tc.opts.NoRestart, tc.opts.Client = exe, true, fakeReleases(t)
	if tc.opts.Target == "" {
		tc.opts.Target = "latest"
	}
	var out bytes.Buffer
	err := Update(context.Background(), tc.opts, &out)
	if (err != nil) != tc.fails {
		t.Fatalf("err = %v, output:\n%s", err, out.String())
	}
	got, _ := os.ReadFile(exe)
	if replaced := !bytes.Equal(got, old); replaced != tc.replaced {
		t.Fatalf("replaced = %v, output:\n%s", replaced, out.String())
	}
	if !strings.Contains(out.String(), tc.output) {
		t.Fatalf("output:\n%s\nwant %q", out.String(), tc.output)
	}
}

func TestUpdateRunsOnce(t *testing.T) {
	store := testStore(t)
	release, err := store.Claim("update", config.Holder{PID: 9})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	opts := UpdateOptions{Target: "latest", Version: "v1.0.0", Client: fakeReleases(t), Exe: "/nonexistent", NoRestart: true}
	if err = Update(context.Background(), opts, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "another update is running (pid 9)") {
		t.Fatal(err)
	}
}
