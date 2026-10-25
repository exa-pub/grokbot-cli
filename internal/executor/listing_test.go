package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLister(t *testing.T) *lister {
	t.Helper()
	e := setup(t)
	return &lister{root: e.Root, files: e.Files, budget: listBudget}
}

// fakeRG writes an executable that stands in for ripgrep.
func fakeRG(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // test helper
		t.Fatal(err)
	}
	return path
}

// An unreadable subdirectory is skipped, not fatal for the whole listing.
func TestWalkSkipsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	l := testLister(t)
	locked := filepath.Join(l.root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.root, "note.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) }) //nolint:errcheck,gosec // best effort for TempDir cleanup
	tree, err := l.walk(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.ChildrenFiles) != 1 || tree.ChildrenWereProcessed || len(tree.ChildrenDirs) != 1 || tree.ChildrenDirs[0].ChildrenWereProcessed {
		t.Fatal(tree)
	}
	if _, err := l.walk(context.Background(), "locked"); err == nil {
		t.Fatal("unreadable root listed")
	}
}

// rg exits with 2 when some directories cannot be read; its output is still
// a usable tree. With no output the status is an error, as in the desktop.
func TestRipgrepExitStatus(t *testing.T) {
	l := testLister(t)
	tree, err := l.ripgrep(context.Background(), fakeRG(t, `printf 'a.txt\0d/b.txt\0'; echo denied >&2; exit 2`), ".")
	if err != nil || len(tree.ChildrenFiles) != 1 || len(tree.ChildrenDirs) != 1 {
		t.Fatal(tree, err)
	}
	_, err = l.ripgrep(context.Background(), fakeRG(t, `echo denied >&2; exit 2`), ".")
	if err == nil || err.Error() != "Ripgrep failed (exit 2) with no results: denied" {
		t.Fatal(err)
	}
	if _, err = l.ripgrep(context.Background(), fakeRG(t, `exit 1`), "."); err != nil {
		t.Fatal(err)
	}
}

// When the scanner gives up on an overlong path, rg is killed instead of
// blocking on a full pipe until the deadline.
func TestRipgrepScanErrorKills(t *testing.T) {
	l := testLister(t)
	rg := fakeRG(t, `exec yes`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := l.ripgrep(ctx, rg, ".")
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("ripgrep not killed")
	}
}
