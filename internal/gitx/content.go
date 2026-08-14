package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxFileBytes caps what will be shipped to the browser for a single file.
const maxFileBytes = 8 << 20

// File is the full contents of one file at some revision.
type File struct {
	Path   string   `json:"path"`
	Rev    string   `json:"rev"`
	Lines  []string `json:"lines"`
	Binary bool     `json:"binary"`
	Trunc  bool     `json:"truncated"`
}

// ReadFile returns a file's contents at the given revision.
//
// RevWorktree reads from the filesystem, RevIndex reads the staged copy, and
// anything else is treated as a git revision.
func ReadFile(ctx context.Context, r *Repo, rev, path string) (*File, error) {
	var data []byte

	if err := validRepoPath(path); err != nil {
		return nil, err
	}

	switch rev {
	case RevWorktree:
		clean, err := r.safeJoin(path)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(clean)
		if err != nil {
			if os.IsNotExist(err) {
				return &File{Path: path, Rev: rev, Lines: []string{}}, nil
			}
			return nil, err
		}
		data = b
	case RevIndex:
		data = r.runAllowFail(ctx, "cat-file", "blob", ":./"+path)
	default:
		// cat-file blob, not show: `show <rev>:<dir>` prints a tree listing,
		// which was then rendered as though it were the file's contents.
		data = r.runAllowFail(ctx, "cat-file", "blob", rev+":./"+path)
	}

	f := &File{Path: path, Rev: rev}
	if len(data) > maxFileBytes {
		data = data[:maxFileBytes]
		f.Trunc = true
	}
	if isBinary(data) {
		f.Binary = true
		f.Lines = []string{}
		return f, nil
	}
	f.Lines = splitLines(string(data))
	return f, nil
}

// validRepoPath rejects anything that is not a plain repository-relative path.
//
// An absolute path or one starting with a colon reaches git's revision parser
// through the `<rev>:<path>` syntax — `:/text` means "youngest commit whose
// message matches", which returns a whole commit rendered as a file. Paths are
// also passed with a ./ prefix at the call site to pin them to the repo root.
func validRepoPath(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, ":") {
		return fmt.Errorf("invalid path: %s", path)
	}
	if path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return fmt.Errorf("invalid path: %s", path)
	}
	return nil
}

// safeJoin resolves a repo-relative path and refuses anything that leaves the
// working tree.
//
// The lexical check alone is not enough: a symlink tracked inside the
// repository can point anywhere, so reviewing a repo containing
// `link -> /home/you/.ssh` would turn a click into a key dump. Symlinks are
// resolved before the containment check.
func (r *Repo) safeJoin(path string) (string, error) {
	if err := validRepoPath(path); err != nil {
		return "", err
	}
	clean := filepath.Join(r.Root, filepath.Clean("/"+path))

	root, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		root = r.Root
	}
	// A missing file cannot escape anything; the caller reports it as empty.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if !within(clean, r.Root) {
			return "", fmt.Errorf("path escapes repository: %s", path)
		}
		return clean, nil
	}
	if !within(resolved, root) {
		return "", fmt.Errorf("path escapes repository via a symlink: %s", path)
	}
	return resolved, nil
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

// isBinary uses git's own heuristic: a NUL byte in the first 8000 bytes, or
// content that is not valid UTF-8.
func isBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	return !utf8.Valid(head)
}

// splitLines splits on \n and drops the trailing empty element produced by a
// final newline, so a file ending in "\n" does not gain a phantom last line.
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// ListFiles returns every tracked path, for the browser that lets unrelated
// files be pulled onto the canvas.
func ListFiles(ctx context.Context, r *Repo, rev string) ([]string, error) {
	var out []byte
	var err error
	if rev == RevWorktree || rev == RevIndex {
		out, err = r.run(ctx, "ls-files", "-z")
	} else {
		out, err = r.run(ctx, "ls-tree", "-r", "--name-only", "-z", rev)
	}
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}
