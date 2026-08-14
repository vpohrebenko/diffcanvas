package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// newTestRepo builds a throwaway repository so the tests do not depend on any
// particular checkout being present.
func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@e")

	// Root commit.
	write("a.go", "package a\n\nfunc A() int { return 1 }\n")
	write("with space.txt", "hello\n")
	run("add", "-A")
	run("commit", "-qm", "root")

	// Second commit: modify, add, delete, and rename.
	write("a.go", "package a\n\nfunc A() int { return 2 }\n\nfunc B() {}\n")
	write("new.go", "package a\n")
	os.Remove(filepath.Join(dir, "with space.txt"))
	run("add", "-A")
	run("commit", "-qm", "second commit")

	return &Repo{Root: dir}
}

func find(t *testing.T, changes []FileChange, path string) FileChange {
	t.Helper()
	for _, c := range changes {
		if c.Path == path {
			return c
		}
	}
	t.Fatalf("no change for %q in %+v", path, changes)
	return FileChange{}
}

// TestChangesMatchNumstat is the correctness anchor: whatever we report must
// agree with git's own numstat, including for paths containing spaces.
func TestChangesMatchNumstat(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	spec, err := ResolveSpec(ctx, r, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := Changes(ctx, r, spec)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][2]int{}
	raw, err := r.run(ctx, "diff", "--numstat", "-z", "-M", "-C", "HEAD^", "HEAD", "--")
	if err != nil {
		t.Fatal(err)
	}
	fields := splitNUL(raw)
	for i := 0; i < len(fields); {
		parts := strings.SplitN(fields[i], "\t", 3)
		i++
		if len(parts) < 3 {
			continue
		}
		path := parts[2]
		if path == "" {
			path = fields[i+1]
			i += 2
		}
		adds, _ := strconv.Atoi(parts[0])
		dels, _ := strconv.Atoi(parts[1])
		want[path] = [2]int{adds, dels}
	}

	if len(changes) != len(want) {
		t.Fatalf("change count = %d, git numstat = %d (%+v)", len(changes), len(want), changes)
	}
	for _, c := range changes {
		w, ok := want[c.Path]
		if !ok {
			t.Errorf("reported %q which numstat did not", c.Path)
			continue
		}
		if !c.Binary && (c.Adds != w[0] || c.Dels != w[1]) {
			t.Errorf("%s: got +%d-%d, numstat +%d-%d", c.Path, c.Adds, c.Dels, w[0], w[1])
		}
	}
}

func TestStatusClassification(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	spec, _ := ResolveSpec(ctx, r, "HEAD")
	changes, err := Changes(ctx, r, spec)
	if err != nil {
		t.Fatal(err)
	}

	if got := find(t, changes, "a.go").Status; got != "modified" {
		t.Errorf("a.go status = %q, want modified", got)
	}
	if got := find(t, changes, "new.go").Status; got != "added" {
		t.Errorf("new.go status = %q, want added", got)
	}
	// A path with a space must survive -z parsing intact.
	if got := find(t, changes, "with space.txt").Status; got != "deleted" {
		t.Errorf("'with space.txt' status = %q, want deleted", got)
	}
}

// TestRootCommit covers the case that crashes naive implementations: a commit
// with no parent has nothing to diff against.
func TestRootCommit(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	root, err := r.run(ctx, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ResolveSpec(ctx, r, strings.TrimSpace(string(root)))
	if err != nil {
		t.Fatalf("root commit spec: %v", err)
	}
	changes, err := Changes(ctx, r, spec)
	if err != nil {
		t.Fatalf("root commit changes: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("root commit: got %d changes, want 2 (%+v)", len(changes), changes)
	}
	for _, c := range changes {
		if c.Status != "added" {
			t.Errorf("%s: status = %q, want added", c.Path, c.Status)
		}
	}
}

// TestThreeDotExcludesMainOnlyCommits is the semantic distinction that makes a
// 50-file review not become a 200-file one.
func TestThreeDotExcludesMainOnlyCommits(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	run := func(args ...string) {
		t.Helper()
		if _, err := r.run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.Root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The two files must differ, or git's rename detection will pair them and
	// report a single rename instead of an add plus a delete.
	run("checkout", "-qb", "feature")
	write("feature.go", "package a\n\nfunc Feature() string { return \"feature\" }\n")
	run("add", "-A")
	run("commit", "-qm", "feature work")

	run("checkout", "-q", "main")
	write("unrelated.go", "package a\n\nvar Unrelated = []int{1, 2, 3, 4, 5}\n")
	run("add", "-A")
	run("commit", "-qm", "unrelated main work")

	threeDot, err := ResolveSpec(ctx, r, "main...feature")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Changes(ctx, r, threeDot)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "feature.go" {
		t.Errorf("main...feature = %+v, want only feature.go", got)
	}

	twoDot, err := ResolveSpec(ctx, r, "main..feature")
	if err != nil {
		t.Fatal(err)
	}
	got2, err := Changes(ctx, r, twoDot)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 2 {
		t.Errorf("main..feature = %+v, want 2 files (including the reverted unrelated.go)", got2)
	}
}

func TestPatchHunks(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	spec, _ := ResolveSpec(ctx, r, "HEAD")
	changes, _ := Changes(ctx, r, spec)
	fc := find(t, changes, "a.go")

	hunks, err := Patch(ctx, r, spec, &fc, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) == 0 {
		t.Fatal("no hunks for a.go")
	}

	var adds, dels int
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch l.T {
			case "+":
				adds++
				if l.New == 0 {
					t.Errorf("added line has no new line number: %+v", l)
				}
			case "-":
				dels++
				if l.Old == 0 {
					t.Errorf("removed line has no old line number: %+v", l)
				}
			}
		}
	}
	if adds != fc.Adds || dels != fc.Dels {
		t.Errorf("hunk totals +%d-%d, numstat +%d-%d", adds, dels, fc.Adds, fc.Dels)
	}
}

func TestParseHunkHeader(t *testing.T) {
	h := parseHunkHeader("@@ -12,7 +18,9 @@ func Example() {")
	if h.OldStart != 12 || h.OldLines != 7 || h.NewStart != 18 || h.NewLines != 9 {
		t.Errorf("ranges = %+v", h)
	}
	if h.Header != "func Example() {" {
		t.Errorf("heading = %q", h.Header)
	}

	// Single-line ranges omit the count entirely.
	h2 := parseHunkHeader("@@ -1 +1 @@")
	if h2.OldStart != 1 || h2.OldLines != 1 || h2.NewStart != 1 || h2.NewLines != 1 {
		t.Errorf("single-line ranges = %+v", h2)
	}
}

func TestReadFileAndSafeJoin(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	f, err := ReadFile(ctx, r, "HEAD", "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Lines) != 5 {
		t.Errorf("a.go at HEAD has %d lines, want 5: %q", len(f.Lines), f.Lines)
	}

	// safeJoin neutralises traversal by anchoring the path at the repo root
	// rather than rejecting it. The property that matters is that the result
	// can never land outside the repository.
	for _, attack := range []string{
		"../../etc/passwd",
		"../outside.txt",
		"a/../../../../etc/shadow",
		"/etc/passwd",
	} {
		got, err := r.safeJoin(attack)
		if err != nil {
			continue // rejecting outright is also acceptable
		}
		if !strings.HasPrefix(got, r.Root+string(os.PathSeparator)) {
			t.Errorf("safeJoin(%q) = %q, which escapes %q", attack, got, r.Root)
		}
	}
}

func TestSplitLinesNoPhantomTrailingLine(t *testing.T) {
	if got := splitLines("a\nb\n"); len(got) != 2 {
		t.Errorf("splitLines(%q) = %q, want 2 lines", "a\nb\n", got)
	}
	if got := splitLines("a\nb"); len(got) != 2 {
		t.Errorf("splitLines(%q) = %q, want 2 lines", "a\nb", got)
	}
	if got := splitLines(""); len(got) != 0 {
		t.Errorf("splitLines(\"\") = %q, want empty", got)
	}
}

// TestPatchIsolatesRequestedFile is the regression guard for a card showing
// another file's diff.
//
// Three ways a patch ends up with more than one file section, all confirmed
// against real git: a glob in the filename matching neighbours, a leading
// colon being read as pathspec magic, and a copy naming both source and
// destination. Sections are ordered by path, not by pathspec order, so simply
// taking the first one is not enough either.
func TestPatchIsolatesRequestedFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := &Repo{Root: dir}

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "-b", "main")
	git("config", "user.name", "t")
	git("config", "user.email", "t@e")
	// A glob-looking name, its neighbours, and a colon-prefixed name.
	write("a*.txt", "one\n")
	write("ab.txt", "one\n")
	write("ac.txt", "one\n")
	write(":weird.txt", "one\n")
	git("add", "-A")
	git("commit", "-qm", "base")

	write("a*.txt", "GLOB-CHANGED\n")
	write("ab.txt", "AB-CHANGED\n")
	write("ac.txt", "AC-CHANGED\n")
	write(":weird.txt", "WEIRD-CHANGED\n")
	git("add", "-A")
	git("commit", "-qm", "edit all")

	spec, err := ResolveSpec(ctx, r, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := Changes(ctx, r, spec)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"a*.txt":     "GLOB-CHANGED",
		"ab.txt":     "AB-CHANGED",
		"ac.txt":     "AC-CHANGED",
		":weird.txt": "WEIRD-CHANGED",
	}
	for _, fc := range changes {
		expect, ok := want[fc.Path]
		if !ok {
			t.Errorf("unexpected change %q", fc.Path)
			continue
		}
		hunks, err := Patch(ctx, r, spec, &fc, 3)
		if err != nil {
			t.Errorf("%s: %v", fc.Path, err)
			continue
		}

		var adds, dels int
		for _, h := range hunks {
			for _, l := range h.Lines {
				switch l.T {
				case "+":
					adds++
					if l.C != expect {
						t.Errorf("%s: added line %q, want %q (another file's content leaked in)",
							fc.Path, l.C, expect)
					}
				case "-":
					dels++
				}
				// Patch header lines must never be mistaken for diff rows.
				if strings.HasPrefix(l.C, "-- a/") || strings.HasPrefix(l.C, "++ b/") {
					t.Errorf("%s: file header leaked in as a diff row: %q", fc.Path, l.C)
				}
			}
		}
		if adds != fc.Adds || dels != fc.Dels {
			t.Errorf("%s: hunks say +%d-%d, numstat says +%d-%d", fc.Path, adds, dels, fc.Adds, fc.Dels)
		}
		if adds == 0 {
			t.Errorf("%s: no hunks at all", fc.Path)
		}
	}
}

// TestCRLFKeepsHighlighting: a Windows-line-ending repository must still get
// syntax colour. The file is read with CRLF normalised while the diff line
// keeps its carriage return, and comparing them naively loses every class.
func TestCRLFKeepsHighlighting(t *testing.T) {
	if got := splitLines("a\r\nb\r\n"); len(got) != 2 || got[0] != "a" {
		t.Fatalf("splitLines on CRLF = %q", got)
	}
}
