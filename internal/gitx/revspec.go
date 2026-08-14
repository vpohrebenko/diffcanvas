package gitx

import (
	"context"
	"fmt"
	"strings"
)

// Content revisions that are not commits.
const (
	RevWorktree = ""  // read from the filesystem
	RevIndex    = ":" // read from the index (git show :path)
)

// Spec is a resolved description of "what to diff". Every kind resolves down
// to an explicit (OldRev, NewRev) pair so the rest of the code has exactly one
// path to handle.
type Spec struct {
	Raw     string `json:"raw"`
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Subject string `json:"subject,omitempty"`
	Author  string `json:"author,omitempty"`
	Date    string `json:"date,omitempty"`

	OldRev string `json:"-"`
	NewRev string `json:"-"`

	// diffArgs select the change set for `git diff`.
	diffArgs []string
}

// ResolveSpec turns a user-supplied revspec into a Spec.
//
// Supported forms:
//
//	""            uncommitted changes (staged and unstaged) against HEAD
//	--staged      staged changes only
//	A...B         changes on B since it diverged from A  (merge-base; PR view)
//	A..B          direct diff between two revisions
//	<rev>         a single commit against its first parent
func ResolveSpec(ctx context.Context, r *Repo, raw string) (*Spec, error) {
	raw = strings.TrimSpace(raw)

	switch raw {
	case "", "worktree":
		return &Spec{
			Raw: raw, Kind: "worktree", Label: "uncommitted changes",
			OldRev: "HEAD", NewRev: RevWorktree,
			diffArgs: []string{"HEAD"},
		}, nil
	case "--staged", "--cached", "staged":
		return &Spec{
			Raw: raw, Kind: "staged", Label: "staged changes",
			OldRev: "HEAD", NewRev: RevIndex,
			diffArgs: []string{"--cached", "HEAD"},
		}, nil
	}

	// Three-dot: diff from the merge base. This is what a pull request shows,
	// and is the reason it is the recommended form for branch review.
	if a, b, ok := strings.Cut(raw, "..."); ok {
		a, b = defaultRev(a), defaultRev(b)
		base, err := r.mergeBase(ctx, a, b)
		if err != nil {
			return nil, err
		}
		return &Spec{
			Raw: raw, Kind: "range",
			Label:  fmt.Sprintf("%s...%s (from merge-base %s)", a, b, short(base)),
			OldRev: base, NewRev: b,
			diffArgs: []string{base, b},
		}, nil
	}

	// Two-dot: literal diff between two revisions.
	if a, b, ok := strings.Cut(raw, ".."); ok {
		a, b = defaultRev(a), defaultRev(b)
		oldSHA, err := r.resolveCommit(ctx, a)
		if err != nil {
			return nil, err
		}
		newSHA, err := r.resolveCommit(ctx, b)
		if err != nil {
			return nil, err
		}
		return &Spec{
			Raw: raw, Kind: "range", Label: fmt.Sprintf("%s..%s", a, b),
			OldRev: oldSHA, NewRev: newSHA,
			diffArgs: []string{oldSHA, newSHA},
		}, nil
	}

	// A single commit, diffed against its first parent.
	sha, err := r.resolveCommit(ctx, raw)
	if err != nil {
		return nil, err
	}
	parent, err := r.firstParent(ctx, sha)
	if err != nil {
		return nil, err
	}
	spec := &Spec{
		Raw: raw, Kind: "commit", Label: short(sha),
		OldRev: parent, NewRev: sha,
		diffArgs: []string{parent, sha},
	}
	if meta, err := r.commitMeta(ctx, sha); err == nil {
		spec.Subject, spec.Author, spec.Date = meta[0], meta[1], meta[2]
		spec.Label = short(sha) + " " + meta[0]
	}
	return spec, nil
}

func defaultRev(s string) string {
	if s == "" {
		return "HEAD"
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (r *Repo) resolveCommit(ctx context.Context, rev string) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil || len(out) == 0 {
		return "", fmt.Errorf("unknown revision %q", rev)
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) mergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := r.run(ctx, "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("no common ancestor between %s and %s", a, b)
	}
	return strings.TrimSpace(string(out)), nil
}

// firstParent returns the commit's first parent, or the empty-tree hash when
// the commit is a root. Using the first parent matches what `git show` does
// for a merge commit.
func (r *Repo) firstParent(ctx context.Context, sha string) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", sha+"^1^{commit}")
	if err == nil && len(out) > 0 {
		return strings.TrimSpace(string(out)), nil
	}
	return r.emptyTree(ctx)
}

func (r *Repo) commitMeta(ctx context.Context, sha string) ([3]string, error) {
	var meta [3]string
	out, err := r.run(ctx, "log", "-1", "--format=%s%x00%an%x00%ad", "--date=short", sha)
	if err != nil {
		return meta, err
	}
	parts := strings.Split(strings.TrimRight(string(out), "\n"), "\x00")
	for i := 0; i < 3 && i < len(parts); i++ {
		meta[i] = parts[i]
	}
	return meta, nil
}
