// Package gitx wraps the git command line. Everything is done by exec'ing git
// rather than linking a library, so the binary stays pure Go and cross-compiles
// without CGO.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Repo is a git working tree.
type Repo struct {
	Root string
}

// Discover finds the repository containing dir.
func Discover(ctx context.Context, dir string) (*Repo, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", dir)
	}
	return &Repo{Root: strings.TrimSpace(string(out))}, nil
}

// baseArgs are prepended to every invocation. They make output stable and
// independent of the user's global config.
var baseArgs = []string{
	"-c", "core.quotePath=false", // we use -z everywhere, but be explicit
	"-c", "diff.noprefix=false",
	"-c", "color.ui=false",
	"--no-pager",
}

// run executes git and returns stdout. Stderr is folded into the error so
// failures are diagnosable.
func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append(baseArgs, args...)...)
	cmd.Dir = r.Root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// runAllowFail is for commands where a non-zero exit is a normal outcome
// (git grep with no matches, git show on a missing path).
func (r *Repo) runAllowFail(ctx context.Context, args ...string) []byte {
	cmd := exec.CommandContext(ctx, "git", append(baseArgs, args...)...)
	cmd.Dir = r.Root
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run()
	return stdout.Bytes()
}

// emptyTree is the hash of the empty tree object, used as the "parent" of a
// root commit. Asking git for it keeps this correct under sha256 repos, where
// the well-known sha1 constant would be wrong.
func (r *Repo) emptyTree(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "hash-object", "-t", "tree", "/dev/null")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// splitNUL splits NUL-delimited output, dropping the trailing empty field.
func splitNUL(b []byte) []string {
	s := strings.Split(string(b), "\x00")
	if n := len(s); n > 0 && s[n-1] == "" {
		s = s[:n-1]
	}
	return s
}
