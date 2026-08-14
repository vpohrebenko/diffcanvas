package goimports

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
)

func newRepo(t *testing.T, files map[string]string) *gitx.Repo {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &gitx.Repo{Root: dir}
}

func has(edges []Edge, from, to string) bool {
	for _, e := range edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func TestEdgesResolveModuleInternalImports(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"go.mod": "module example.com/proj\n\ngo 1.22\n",
		"main.go": `package main

import (
	"fmt"
	"example.com/proj/internal/store"
	"github.com/external/thing"
)

func main() { fmt.Println(store.New(), thing.X) }
`,
		"internal/store/store.go":      "package store\n\nfunc New() int { return 1 }\n",
		"internal/store/store_test.go": "package store\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {}\n",
		"internal/util/util.go":        "package util\n",
	})

	paths := []string{
		"main.go",
		"internal/store/store.go",
		"internal/store/store_test.go",
		"internal/util/util.go",
	}
	edges, err := Edges(context.Background(), repo, gitx.RevWorktree, paths)
	if err != nil {
		t.Fatal(err)
	}

	if !has(edges, "main.go", "internal/store/store.go") {
		t.Errorf("missing edge main.go -> internal/store/store.go; got %+v", edges)
	}
	// One edge per imported package. Fanning out to every file in the package
	// buries the canvas in arrows, and a _test.go file is never the import
	// target: on a real repository this turned 15 useful edges into 51.
	if has(edges, "main.go", "internal/store/store_test.go") {
		t.Errorf("edge into a _test.go file; got %+v", edges)
	}
	for _, e := range edges {
		if e.From == "main.go" && e.Pkg == "example.com/proj/internal/store" {
			// exactly one edge for this import
			count := 0
			for _, x := range edges {
				if x.From == e.From && x.Pkg == e.Pkg {
					count++
				}
			}
			if count != 1 {
				t.Errorf("%d edges for one import, want 1: %+v", count, edges)
			}
			break
		}
	}
	// External modules must never produce edges.
	for _, e := range edges {
		if e.Pkg == "github.com/external/thing" || e.Pkg == "fmt" {
			t.Errorf("edge produced for external import %q", e.Pkg)
		}
	}
	// Nothing imports util, so it must be isolated.
	for _, e := range edges {
		if e.To == "internal/util/util.go" {
			t.Errorf("unexpected edge to util: %+v", e)
		}
	}
}

// TestEdgesOnlyAmongGivenPaths: arrows can only be drawn between cards that
// exist, so a file that is not open must never appear.
// TestRepresentativePrefersPackageNamedFile pins the choice of which file in
// an imported package the arrow should point at.
func TestRepresentativePrefersPackageNamedFile(t *testing.T) {
	cases := []struct {
		files []string
		dir   string
		want  string
	}{
		{[]string{"src/util/chars.go", "src/util/util.go"}, "src/util", "src/util/util.go"},
		{[]string{"src/util/chars_test.go", "src/util/chars.go"}, "src/util", "src/util/chars.go"},
		{[]string{"a/z.go", "a/b.go"}, "a", "a/b.go"},
		{[]string{"a/only_test.go"}, "a", "a/only_test.go"},
		{nil, "a", ""},
	}
	for _, tc := range cases {
		if got := representative(tc.files, tc.dir); got != tc.want {
			t.Errorf("representative(%v) = %q, want %q", tc.files, got, tc.want)
		}
	}
}

func TestEdgesOnlyAmongGivenPaths(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"go.mod":                  "module example.com/proj\n",
		"main.go":                 "package main\n\nimport \"example.com/proj/internal/store\"\n\nvar _ = store.New\n",
		"internal/store/store.go": "package store\n\nfunc New() {}\n",
	})

	edges, err := Edges(context.Background(), repo, gitx.RevWorktree, []string{"main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Errorf("edges to unopened files: %+v", edges)
	}
}

func TestNonModuleRepoIsQuiet(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"main.go": "package main\n\nfunc main() {}\n",
	})
	edges, err := Edges(context.Background(), repo, gitx.RevWorktree, []string{"main.go"})
	if err != nil {
		t.Fatalf("a repo without go.mod should not error: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("got %+v", edges)
	}
}

func TestUnparseableFileIsSkipped(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"go.mod":    "module example.com/proj\n",
		"broken.go": "package main\n\nimport ( \"example.com/proj/x\"\n\nfunc (((\n",
		"good.go":   "package main\n\nimport \"example.com/proj/x\"\n",
		"x/x.go":    "package x\n",
	})
	edges, err := Edges(context.Background(), repo, gitx.RevWorktree,
		[]string{"broken.go", "good.go", "x/x.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !has(edges, "good.go", "x/x.go") {
		t.Errorf("a broken sibling should not suppress valid edges; got %+v", edges)
	}
}
