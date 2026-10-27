package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func script(version string) []byte {
	return []byte("#!/bin/sh\necho 'grokbot-tunnel " + version + " (Sand protocol 0.77.0)'\n")
}

type entry struct {
	name string
	typ  byte
	body []byte
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			h.Linkname, h.Size = "/bin/sh", 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write(e.body)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// release serves a fake GitHub repository whose latest release is v1.2.3.
type release struct {
	archive []byte
	sums    string
	latest  string
}

func (r *release) serve(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, r.latest, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, r.sums)
	})
	mux.HandleFunc("/releases/download/v1.2.3/grokbot-tunnel-linux-amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(r.archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, Arch: "amd64", UserAgent: "test"}
}

func goodRelease(t *testing.T, body []byte) *release {
	t.Helper()
	archive := tarball(t, entry{"README", tar.TypeReg, []byte("x")}, entry{"grokbot-tunnel-linux-amd64", tar.TypeReg, body})
	sum := sha256.Sum256(archive)
	return &release{
		archive: archive,
		sums:    hex.EncodeToString(sum[:]) + "  grokbot-tunnel-linux-amd64.tar.gz\n",
		latest:  "/exa-pub/grokbot-tunnel/releases/tag/v1.2.3",
	}
}

func TestLatest(t *testing.T) {
	r := goodRelease(t, script("v1.2.3"))
	c := r.serve(t)
	if v, err := c.Latest(context.Background()); err != nil || v != "v1.2.3" {
		t.Fatal(v, err)
	}
	// Without releases GitHub redirects elsewhere.
	r.latest = "/exa-pub/grokbot-tunnel/releases"
	if v, err := c.Latest(context.Background()); err == nil {
		t.Fatal("no error for", v)
	}
}

func TestFetchAndReplace(t *testing.T) {
	c := goodRelease(t, script("v1.2.3")).serve(t)
	bin, err := c.Fetch(context.Background(), "v1.2.3")
	if err != nil || !bytes.Equal(bin, script("v1.2.3")) {
		t.Fatal(string(bin), err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "grokbot-tunnel")
	os.WriteFile(exe, script("v1.0.0"), 0o755)
	link := filepath.Join(dir, "link")
	os.Symlink(exe, link)
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if err = Replace(context.Background(), target, bin, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	st, _ := os.Stat(exe)
	if !bytes.Equal(got, bin) || st.Mode().Perm() != 0o755 {
		t.Fatal(string(got), st.Mode())
	}
	if l, err := os.Lstat(link); err != nil || l.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatal("temporary files left behind", entries)
	}
}

func TestFetchRejectsBadReleases(t *testing.T) {
	good := goodRelease(t, script("v1.2.3"))
	for name, r := range map[string]*release{
		"checksum mismatch": {archive: good.archive, sums: strings.Repeat("0", 64) + "  grokbot-tunnel-linux-amd64.tar.gz\n"},
		"missing checksum":  {archive: good.archive, sums: strings.Repeat("0", 64) + "  other.tar.gz\n"},
		"missing binary":    signed(tarball(t, entry{"grokbot-tunnel-linux-arm64", tar.TypeReg, []byte("x")})),
		"symlink":           signed(tarball(t, entry{"grokbot-tunnel-linux-amd64", tar.TypeSymlink, nil})),
		"not gzip":          signed([]byte("not an archive")),
	} {
		if _, err := r.serve(t).Fetch(context.Background(), "v1.2.3"); err == nil {
			t.Error(name, "accepted")
		}
	}
	if _, err := good.serve(t).Fetch(context.Background(), "v9.9.9"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Error("missing release:", err)
	}
}

func signed(archive []byte) *release {
	sum := sha256.Sum256(archive)
	return &release{archive: archive, sums: hex.EncodeToString(sum[:]) + " *grokbot-tunnel-linux-amd64.tar.gz\n"}
}

func TestReplaceKeepsBinaryIfNewOneFails(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "grokbot-tunnel")
	old := script("v1.0.0")
	os.WriteFile(exe, old, 0o755)
	for name, bin := range map[string][]byte{
		"wrong version": script("v1.2.4"),
		"exits 1":       []byte("#!/bin/sh\nexit 1\n"),
		"not runnable":  []byte("\x7fELF garbage"),
	} {
		if err := Replace(context.Background(), exe, bin, "v1.2.3"); err == nil {
			t.Error(name, "accepted")
		}
		if got, _ := os.ReadFile(exe); !bytes.Equal(got, old) {
			t.Fatal(name, "replaced the binary")
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatal("temporary files left behind", entries)
	}
}

func TestVersions(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3": true, "v0.2.0-rc1": true,
		"dev": false, "dev-abc1234": false, "v0.77.0-3-gabc1234": false, "v0.77.0-3-gabc1234-dirty": false,
		"v0.77.0-dirty": false, "1.2.3": false, "abc1234": false,
	} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%q) = %v", v, !want)
		}
	}
	for in, want := range map[string]string{"latest": "latest", "1.2.3": "v1.2.3", "v1.2.3-rc.1": "v1.2.3-rc.1"} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "v1", "1.2", "newest", "v1.2.3 x"} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) accepted", bad)
		}
	}
	if Compare("v1.2.3", "v1.10.0") >= 0 || Compare("v1.2.3-rc1", "v1.2.3") >= 0 {
		t.Error("Compare")
	}
}
