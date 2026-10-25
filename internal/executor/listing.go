package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

type treeNode = agentv1.LsDirectoryTreeNode

// lister builds a directory tree. Names are listed until their total length
// exceeds budget; further files are only counted per extension.
type lister struct {
	root   string
	files  *os.Root
	ignore []string // glob patterns
	budget int      // UTF-16 code units, like the desktop
}

func newNode(path string) *treeNode {
	return &treeNode{AbsPath: path, ChildrenWereProcessed: true, FullSubtreeExtensionCounts: map[string]int32{}}
}

// nameCost is the length of name in UTF-16 code units.
func nameCost(name string) int {
	n := 0
	for _, r := range name {
		n += utf16.RuneLen(r)
	}
	return n
}

// extension is the key for extension counts. Like Node's path.extname, a
// dotfile such as ".bashrc" has none.
func extension(name string) string {
	if strings.HasPrefix(name, ".") && strings.Count(name, ".") == 1 {
		return ""
	}
	return filepath.Ext(name)
}

// charge spends the cost of name if the budget allows. Once a name does not
// fit, nothing further is listed.
func (l *lister) charge(name string) bool {
	if cost := nameCost(name); l.budget >= cost {
		l.budget -= cost
		return true
	}
	l.budget = -1
	return false
}

// addFile lists name in node if the budget allows.
func (l *lister) addFile(node *treeNode, name string) bool {
	if !l.charge(name) {
		return false
	}
	node.ChildrenFiles = append(node.ChildrenFiles, &agentv1.LsDirectoryTreeNode_File{Name: name})
	return true
}

// countOmitted records a file that was not listed.
func countOmitted(node *treeNode, name string) {
	node.ChildrenWereProcessed = false
	node.FullSubtreeExtensionCounts[extension(name)]++
	node.NumFiles++
}

// list uses the same ripgrep enumeration as the desktop when rg is
// available. The Go fallback keeps file tools usable on hosts without
// ripgrep, but matches ignore patterns against names only.
func (l *lister) list(ctx context.Context, dir string) (*treeNode, error) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		return l.walk(ctx, dir)
	}
	return l.ripgrep(ctx, rg, dir)
}

func (l *lister) ignored(name string) bool {
	return slices.ContainsFunc(l.ignore, func(pattern string) bool {
		match, err := filepath.Match(pattern, name)
		return err == nil && match
	})
}

// walk lists dir recursively without following symlinks. Directories without
// listed files are left out, and so are directories reached after the budget
// ran out: their files are only counted in the nearest listed ancestor.
func (l *lister) walk(ctx context.Context, dir string) (*treeNode, error) {
	node := newNode(filepath.Join(l.root, dir))
	if err := ctx.Err(); err != nil {
		node.ChildrenWereProcessed = false
		return node, err
	}
	entries, err := l.readDir(dir)
	if err != nil {
		return node, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			node.ChildrenWereProcessed = false
			return node, err
		}
		name := entry.Name()
		switch {
		case l.ignored(name):
		case entry.IsDir():
			if err := l.walkChild(ctx, node, filepath.Join(dir, name)); err != nil {
				return node, err
			}
		case entry.Type()&os.ModeSymlink != 0:
		case !l.addFile(node, name):
			countOmitted(node, name)
		}
	}
	return node, nil
}

// walkChild lists a subdirectory and merges its counts into parent. Like
// file names, directory names are charged to the budget.
func (l *lister) walkChild(ctx context.Context, parent *treeNode, dir string) error {
	before := l.budget
	listed := l.charge(filepath.Base(dir))
	child, err := l.walk(ctx, dir)
	if err != nil && ctx.Err() == nil {
		// Like rg, skip a directory that cannot be read instead of failing
		// the whole listing; the agent sees it as not fully processed.
		log.Printf("ls: %v", err)
		child.ChildrenWereProcessed = false
		err = nil
	}
	switch {
	case !listed:
	case len(child.ChildrenFiles) > 0 || len(child.ChildrenDirs) > 0 || !child.ChildrenWereProcessed:
		parent.ChildrenDirs = append(parent.ChildrenDirs, child)
	default:
		l.budget = before // an empty directory is left out for free
	}
	if !child.ChildrenWereProcessed {
		parent.ChildrenWereProcessed = false
	}
	for ext, n := range child.FullSubtreeExtensionCounts {
		parent.FullSubtreeExtensionCounts[ext] += n
		parent.NumFiles += n
	}
	return err
}

func (l *lister) readDir(dir string) ([]os.DirEntry, error) {
	f, err := l.files.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// ripgrep lists the files rg would search. rg runs in the directory opened
// through Root, pinned by its descriptor, and does not follow symlinks.
func (l *lister) ripgrep(ctx context.Context, rg, dir string) (*treeNode, error) {
	tree := newNode(filepath.Join(l.root, dir))
	fd, err := l.files.Open(dir)
	if err != nil {
		return tree, err
	}
	defer fd.Close()
	args := []string{"--files", "--hidden", "--no-require-git", "--no-config", "--color=never", "--case-sensitive", "--null"}
	for _, pattern := range l.ignore {
		args = append(args, "--iglob", "!"+pattern)
	}
	cmd := exec.CommandContext(ctx, rg, args...) //nolint:gosec // rg from PATH; patterns are plain arguments
	cmd.Dir = fmt.Sprintf("/proc/self/fd/%d", fd.Fd())
	cmd.WaitDelay = time.Second // do not wait for descendants holding the pipes
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return tree, err
	}
	if err = cmd.Start(); err != nil {
		return tree, err
	}
	b := &treeBuilder{lister: l, root: tree, nodes: map[string]*treeNode{tree.AbsPath: tree}}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	scanner.Split(scanNUL)
	found := 0
	for scanner.Scan() {
		b.add(scanner.Text())
		found++
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// Nobody reads stdout any more; do not let rg block on a full pipe.
		if err := cmd.Process.Kill(); err != nil {
			log.Printf("ls: kill ripgrep: %v", err)
		}
	}
	waitErr := cmd.Wait()
	b.sort()
	if err := ctx.Err(); err != nil {
		tree.ChildrenWereProcessed = false
		return tree, err
	}
	if scanErr != nil {
		return tree, scanErr
	}
	return tree, ripgrepStatus(waitErr, found, strings.TrimSpace(stderr.String()))
}

// ripgrepStatus interprets how rg exited after listing found paths. Like the
// desktop, the exit status only matters when rg listed nothing: it exits with
// 1 when there are no files and with 2 when some directories could not be
// read, which still leaves a usable tree.
func ripgrepStatus(waitErr error, found int, stderr string) error {
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return fmt.Errorf("ripgrep failed: %w", waitErr)
	}
	code := exitErr.ExitCode()
	switch {
	case found > 0:
		if stderr != "" {
			log.Printf("ls: ripgrep exited with %d: %s", code, stderr)
		}
		return nil
	case code == 1:
		return nil
	case stderr != "":
		stderr = ": " + stderr
	}
	return fmt.Errorf("Ripgrep failed (exit %d) with no results%s", code, stderr) //nolint:staticcheck // desktop text
}

// scanNUL splits NUL-terminated records.
func scanNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// treeBuilder assembles a tree from a flat list of relative file paths.
type treeBuilder struct {
	lister   *lister
	root     *treeNode
	nodes    map[string]*treeNode // by absolute path
	overflow bool                 // the budget ran out
}

// dir returns the node for path, creating it and its ancestors while the
// budget allows directory names; ok is false if it returns the nearest
// existing ancestor instead. Once the budget runs out no nodes are created, so
// a large tree cannot exceed the message size through directories alone.
func (b *treeBuilder) dir(path string) (_ *treeNode, ok bool) {
	if n := b.nodes[path]; n != nil {
		return n, true
	}
	parent, ok := b.dir(filepath.Dir(path))
	if !ok || b.overflow || !b.lister.charge(filepath.Base(path)) {
		b.overflow = true
		return parent, false
	}
	n := newNode(path)
	b.nodes[path] = n
	parent.ChildrenDirs = append(parent.ChildrenDirs, n)
	return n, true
}

func (b *treeBuilder) add(rel string) {
	rel = filepath.Clean(rel)
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	path := filepath.Join(b.root.AbsPath, rel)
	parent, ok := b.dir(filepath.Dir(path))
	name := filepath.Base(path)
	if ok && !b.overflow && b.lister.addFile(parent, name) {
		return
	}
	b.overflow = true
	// Count the omitted file in every ancestor up to the root.
	for n := parent; n != nil; n = b.nodes[filepath.Dir(n.AbsPath)] {
		countOmitted(n, name)
		if n == b.root {
			break
		}
	}
}

// validUTF8 replaces invalid UTF-8 in the names and paths of tree, which
// protojson cannot encode. It runs on the finished tree, since the builder
// looks nodes up by their raw paths.
func validUTF8(tree *treeNode) {
	if tree == nil {
		return
	}
	tree.AbsPath = strings.ToValidUTF8(tree.AbsPath, "�")
	for _, f := range tree.ChildrenFiles {
		f.Name = strings.ToValidUTF8(f.Name, "�")
	}
	for ext, n := range tree.FullSubtreeExtensionCounts {
		if valid := strings.ToValidUTF8(ext, "�"); valid != ext {
			delete(tree.FullSubtreeExtensionCounts, ext)
			tree.FullSubtreeExtensionCounts[valid] += n
		}
	}
	for _, child := range tree.ChildrenDirs {
		validUTF8(child)
	}
}

func (b *treeBuilder) sort() {
	for _, n := range b.nodes {
		slices.SortFunc(n.ChildrenDirs, func(x, y *treeNode) int {
			return strings.Compare(filepath.Base(x.AbsPath), filepath.Base(y.AbsPath))
		})
		slices.SortFunc(n.ChildrenFiles, func(x, y *agentv1.LsDirectoryTreeNode_File) int {
			return strings.Compare(x.Name, y.Name)
		})
	}
}
