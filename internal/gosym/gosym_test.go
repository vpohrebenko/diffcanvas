package gosym

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

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func buildIndex(t *testing.T, files map[string]string) *Index {
	t.Helper()
	// ListFiles needs a git repo; index the worktree directly instead by
	// calling addFile for each known path.
	repo := newRepo(t, files)
	ix := &Index{
		byName:  map[string][]Def{},
		imports: map[string]map[string]string{},
		modules: findModules(context.Background(), repo, gitx.RevWorktree, keys(files)),
	}
	for name := range files {
		if filepath.Ext(name) == ".go" {
			ix.addFile(context.Background(), repo, gitx.RevWorktree, name)
		}
	}
	return ix
}

const testModule = "module example.com/proj\n\ngo 1.22\n"

func TestQualifiedNameResolvesThroughImports(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"main.go": `package main

import (
	"example.com/proj/internal/store"
	"example.com/proj/internal/cache"
)

func main() { store.New(); cache.New() }
`,
		// The same name exists in two packages: only the import decides.
		"internal/store/store.go": "package store\n\nfunc New() int { return 1 }\n",
		"internal/cache/cache.go": "package cache\n\nfunc New() int { return 2 }\n",
	})

	got, err := ix.Lookup("New", "store", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "internal/store/store.go" {
		t.Errorf("store.New resolved to %s, want internal/store/store.go", got.Def.Path)
	}
	if got.Confidence != "exact-package" {
		t.Errorf("confidence = %q, want exact-package", got.Confidence)
	}

	got, err = ix.Lookup("New", "cache", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "internal/cache/cache.go" {
		t.Errorf("cache.New resolved to %s, want internal/cache/cache.go", got.Def.Path)
	}
}

// TestAliasedImportResolves: the qualifier is the alias, not the directory.
func TestAliasedImportResolves(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"main.go": `package main

import st "example.com/proj/internal/store"

func main() { st.New() }
`,
		"internal/store/store.go": "package store\n\nfunc New() int { return 1 }\n",
	})
	got, err := ix.Lookup("New", "st", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "internal/store/store.go" || got.Confidence != "exact-package" {
		t.Errorf("aliased import resolved to %+v", got)
	}
}

func TestUnqualifiedPrefersOwnPackage(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod":                  testModule,
		"internal/store/a.go":     "package store\n\nfunc helper() {}\n",
		"internal/store/b.go":     "package store\n\nfunc use() { helper() }\n",
		"internal/elsewhere/c.go": "package elsewhere\n\nfunc helper() {}\n",
	})
	got, err := ix.Lookup("helper", "", "internal/store/b.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "internal/store/a.go" {
		t.Errorf("resolved to %s, want the caller's own package", got.Def.Path)
	}
	if got.Confidence != "same-package" {
		t.Errorf("confidence = %q, want same-package", got.Confidence)
	}
}

func TestMethodResolution(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"internal/store/store.go": `package store

type Store struct{}

func (s *Store) Add(n int) {}

func use(s *Store) { s.Add(1) }
`,
	})
	got, err := ix.Lookup("Add", "s", "internal/store/store.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Kind != "method" || got.Def.Recv != "Store" {
		t.Errorf("got %+v, want a method on Store", got.Def)
	}
}

func TestTypesConstsAndVars(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"a.go": `package main

type Widget struct{}

const MaxWidgets = 10

var registry = map[string]int{}
`,
	})
	for name, wantKind := range map[string]string{
		"Widget": "type", "MaxWidgets": "const", "registry": "var",
	} {
		got, err := ix.Lookup(name, "", "a.go")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Def.Kind != wantKind {
			t.Errorf("%s kind = %q, want %q", name, got.Def.Kind, wantKind)
		}
		if got.Def.Line == 0 {
			t.Errorf("%s has no line number", name)
		}
	}
}

// TestAmbiguityIsReported: without a type checker some names genuinely cannot
// be resolved, and the caller must be told rather than shown a confident lie.
func TestAmbiguityIsReported(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"a/a.go": "package a\n\nfunc Run() {}\n",
		"b/b.go": "package b\n\nfunc Run() {}\n",
		"c/c.go": "package c\n\nfunc caller() {}\n",
	})
	got, err := ix.Lookup("Run", "", "c/c.go")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ambiguous || len(got.Others) == 0 {
		t.Errorf("expected an ambiguous result, got %+v", got)
	}
	if got.Confidence != "guess" {
		t.Errorf("confidence = %q, want guess", got.Confidence)
	}
}

func TestExternalPackageIsNotResolved(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"main.go": `package main

import "github.com/other/thing"

func main() { thing.New() }
`,
		// A same-named symbol exists locally; the external qualifier must not
		// silently resolve to it with high confidence.
		"internal/store/store.go": "package store\n\nfunc New() {}\n",
	})
	// Falling through to a repo-wide guess would "resolve" thing.New to the
	// unrelated local store.New, which is worse than reporting nothing.
	got, err := ix.Lookup("New", "thing", "main.go")
	if err == nil {
		t.Errorf("external qualifier resolved to %+v; want an error", got)
	}
}

// TestStdlibQualifierRejected is the same rule for the standard library, which
// is how this surfaced on real code: fmt.Print resolved to a local method.
func TestStdlibQualifierRejected(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"main.go": `package main

import "fmt"

func main() { fmt.Print("x") }
`,
		"internal/tui/tui.go": "package tui\n\ntype W struct{}\n\nfunc (w *W) Print() {}\n",
	})
	if got, err := ix.Lookup("Print", "fmt", "main.go"); err == nil {
		t.Errorf("fmt.Print resolved to %+v; want an error", got.Def)
	}
}

func TestUnknownNameErrors(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"a.go":   "package main\n\nfunc main() {}\n",
	})
	if _, err := ix.Lookup("NoSuchThing", "", "a.go"); err == nil {
		t.Error("expected an error for an unknown name")
	}
}

func TestGenericReceiverName(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"a.go": `package main

type Box[T any] struct{}

func (b *Box[T]) Put(v T) {}
`,
	})
	got, err := ix.Lookup("Put", "b", "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Recv != "Box" {
		t.Errorf("receiver = %q, want Box", got.Def.Recv)
	}
}

// TestNestedModuleResolves is the monorepo shape: go.mod lives in a
// subdirectory, not at the repository root. Assuming a single root module made
// every qualified lookup fall through to a repository-wide guess, which is why
// resolution was near-useless on a real work repository.
func TestNestedModuleResolves(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"services/api/go.mod": "module corp.example/api\n",
		"services/api/cmd/main.go": `package main

import "corp.example/api/internal/usecases"

func main() { usecases.NewFilesInteractor() }
`,
		"services/api/internal/usecases/files.go": "package usecases\n\nfunc NewFilesInteractor() {}\n",
		// A same-named decoy elsewhere in the repo, outside the module.
		"tools/scratch/files.go": "package scratch\n\nfunc NewFilesInteractor() {}\n",
	})

	got, err := ix.Lookup("NewFilesInteractor", "usecases", "services/api/cmd/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "services/api/internal/usecases/files.go" {
		t.Errorf("resolved to %s, want the module-internal declaration", got.Def.Path)
	}
	if got.Confidence != "exact-package" {
		t.Errorf("confidence = %q, want exact-package", got.Confidence)
	}
}

// TestMultipleModulesPickMostSpecific: a nested module must beat its parent.
func TestMultipleModulesPickMostSpecific(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod":             "module corp.example/root\n",
		"sub/go.mod":         "module corp.example/root/sub\n",
		"sub/pkg/thing.go":   "package pkg\n\nfunc Do() {}\n",
		"other/pkg/thing.go": "package pkg\n\nfunc Do() {}\n",
		"main.go": `package main

import "corp.example/root/sub/pkg"

func main() { pkg.Do() }
`,
	})
	got, err := ix.Lookup("Do", "pkg", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "sub/pkg/thing.go" {
		t.Errorf("resolved to %s, want sub/pkg/thing.go (nested module wins)", got.Def.Path)
	}
}

// TestPackageNameFallback: when the module layout cannot be resolved at all,
// matching the qualifier against the declared package name still beats
// guessing across the whole repository.
func TestPackageNameFallback(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		// No go.mod anywhere: nothing can be resolved through imports.
		"cmd/main.go": `package main

import "vendored/usecases"

func main() { usecases.NewFilesInteractor() }
`,
		"internal/usecases/files.go": "package usecases\n\nfunc NewFilesInteractor() {}\n",
		"internal/other/files.go":    "package other\n\nfunc NewFilesInteractor() {}\n",
	})
	got, err := ix.Lookup("NewFilesInteractor", "usecases", "cmd/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "internal/usecases/files.go" {
		t.Errorf("resolved to %s, want the package actually named usecases", got.Def.Path)
	}
	if got.Confidence != "package-name" {
		t.Errorf("confidence = %q, want package-name", got.Confidence)
	}
}
