// Package update downloads grokbot-tunnel releases from GitHub and replaces
// the running binary with them.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Repo is the GitHub repository releases come from.
const Repo = "https://github.com/exa-pub/grokbot-tunnel"

const (
	maxArchive   = 100 << 20
	maxBinary    = 100 << 20
	maxChecksums = 64 << 10
	fetchTimeout = 5 * time.Minute
)

var (
	tag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
	// devSuffix marks versions from git describe between or after tags.
	devSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+(-dirty)?$|-dirty$`)
)

// IsRelease reports whether version is a release tag rather than a
// development build.
func IsRelease(version string) bool {
	return tag.MatchString(version) && !devSuffix.MatchString(version) && semver.IsValid(version)
}

// Normalize turns "latest" or a version, with or without the leading v, into
// "latest" or a tag.
func Normalize(version string) (string, error) {
	if version == "latest" {
		return version, nil
	}
	v := version
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !tag.MatchString(v) {
		return "", fmt.Errorf("invalid version %q: want latest or a release such as v1.2.3", version)
	}
	return v, nil
}

// Compare returns -1, 0 or +1 as release a is older than, equal to or newer
// than b.
func Compare(a, b string) int {
	return semver.Compare(a, b)
}

// Client fetches releases.
type Client struct {
	Base      string // repository URL, Repo by default
	HTTP      *http.Client
	Arch      string // GOARCH of the binary to fetch, runtime.GOARCH by default
	UserAgent string
}

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return Repo
}

func (c *Client) arch() string {
	if c.Arch != "" {
		return c.Arch
	}
	return runtime.GOARCH
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Latest returns the tag of the latest release. GitHub redirects
// /releases/latest to the release page, which avoids the rate-limited API.
func (c *Client) Latest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/releases/latest", http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	noFollow := *c.httpClient()
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", fmt.Errorf("find the latest release: %w", err)
	}
	resp.Body.Close()
	latest := path.Base(resp.Header.Get("Location"))
	if resp.StatusCode/100 != 3 || !tag.MatchString(latest) {
		return "", fmt.Errorf("could not determine the latest release of %s (HTTP %d)", c.base(), resp.StatusCode)
	}
	return latest, nil
}

// Fetch downloads release version for Linux, verifies it against the
// release's checksums.txt and returns the binary.
func (c *Client) Fetch(ctx context.Context, version string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	name := "grokbot-tunnel-linux-" + c.arch()
	archive := name + ".tar.gz"
	base := c.base() + "/releases/download/" + version + "/"
	sums, err := c.get(ctx, base+"checksums.txt", maxChecksums)
	if err != nil {
		return nil, err
	}
	want, err := checksum(sums, archive)
	if err != nil {
		return nil, err
	}
	data, err := c.get(ctx, base+archive, maxArchive)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", archive, want, got)
	}
	return extract(data, name)
}

func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// checksum finds the SHA-256 of file in sha256sum output.
func checksum(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == file {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != 64 {
				return "", fmt.Errorf("invalid checksum for %s", file)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s is missing from checksums.txt", file)
}

// extract returns the regular file name from a .tar.gz archive. Nothing from
// the archive is written to disk under its own name.
func extract(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is missing from the release archive", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Name != name {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size > maxBinary {
			return nil, fmt.Errorf("%s in the release archive is not a regular file of at most %d bytes", name, maxBinary)
		}
		return io.ReadAll(tr)
	}
}
