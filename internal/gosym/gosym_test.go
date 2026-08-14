package gosym

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
	"github.com/vpohrebenko/diffcanvas/internal/gomod"
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
		modules: gomod.Find(context.Background(), repo, gitx.RevWorktree, keys(files)),
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

	got, err := ix.Lookup("New", "store", "main.go", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Path != "internal/store/store.go" {
		t.Errorf("store.New resolved to %s, want internal/store/store.go", got.Def.Path)
	}
	if got.Confidence != "exact-package" {
		t.Errorf("confidence = %q, want exact-package", got.Confidence)
	}

	got, err = ix.Lookup("New", "cache", "main.go", 0, "")
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
	got, err := ix.Lookup("New", "st", "main.go", 0, "")
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
	got, err := ix.Lookup("helper", "", "internal/store/b.go", 0, "")
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
	got, err := ix.Lookup("Add", "s", "internal/store/store.go", 0, "")
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
		got, err := ix.Lookup(name, "", "a.go", 0, "")
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
	got, err := ix.Lookup("Run", "", "c/c.go", 0, "")
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
	got, err := ix.Lookup("New", "thing", "main.go", 0, "")
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
	if got, err := ix.Lookup("Print", "fmt", "main.go", 0, ""); err == nil {
		t.Errorf("fmt.Print resolved to %+v; want an error", got.Def)
	}
}

func TestUnknownNameErrors(t *testing.T) {
	ix := buildIndex(t, map[string]string{
		"go.mod": testModule,
		"a.go":   "package main\n\nfunc main() {}\n",
	})
	if _, err := ix.Lookup("NoSuchThing", "", "a.go", 0, ""); err == nil {
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
	got, err := ix.Lookup("Put", "b", "a.go", 0, "")
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

	got, err := ix.Lookup("NewFilesInteractor", "usecases", "services/api/cmd/main.go", 0, "")
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
	got, err := ix.Lookup("Do", "pkg", "main.go", 0, "")
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
	got, err := ix.Lookup("NewFilesInteractor", "usecases", "cmd/main.go", 0, "")
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

// TestReceiverTypeInference is the case that made navigation unusable:
// `x.Validate()` where x is a variable. Every type tends to have a Validate,
// so resolving by method name alone lands on a different one each time.
func TestReceiverTypeInference(t *testing.T) {
	files := map[string]string{
		"go.mod": testModule,
		"a/types.go": `package a

type Request struct{}

func (r *Request) Validate() error { return nil }

type Config struct{}

func (c *Config) Validate() error { return nil }

type Session struct{}

func (s *Session) Validate() error { return nil }

func NewSession() *Session { return &Session{} }
`,
		"a/use.go": `package a

func handleParam(req *Request) {
	req.Validate()
}

func handleLiteral() {
	cfg := Config{}
	cfg.Validate()
}

func handleConstructor() {
	sess := NewSession()
	sess.Validate()
}

func handleVarDecl() {
	var cfg2 Config
	cfg2.Validate()
}
`,
	}
	ix := buildIndex(t, files)
	src := files["a/use.go"]

	cases := []struct {
		qual string
		line int
		want string
	}{
		{"req", 4, "Request"},   // function parameter
		{"cfg", 9, "Config"},    // composite literal
		{"sess", 14, "Session"}, // constructor result
		{"cfg2", 19, "Config"},  // var declaration
	}
	for _, tc := range cases {
		got, err := ix.Lookup("Validate", tc.qual, "a/use.go", tc.line, src)
		if err != nil {
			t.Errorf("%s: %v", tc.qual, err)
			continue
		}
		if got.Def.Recv != tc.want {
			t.Errorf("%s.Validate resolved to receiver %q, want %q",
				tc.qual, got.Def.Recv, tc.want)
		}
		if got.Confidence != "receiver-type" {
			t.Errorf("%s.Validate confidence = %q, want receiver-type", tc.qual, got.Confidence)
		}
	}
}

// TestMethodOnReceiverInsideMethod: the enclosing method's own receiver.
func TestMethodOnReceiverInsideMethod(t *testing.T) {
	files := map[string]string{
		"go.mod": testModule,
		"a/a.go": `package a

type Server struct{}

func (s *Server) Validate() error { return nil }

type Other struct{}

func (o *Other) Validate() error { return nil }

func (s *Server) run() {
	s.Validate()
}
`,
	}
	ix := buildIndex(t, files)
	got, err := ix.Lookup("Validate", "s", "a/a.go", 12, files["a/a.go"])
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Recv != "Server" {
		t.Errorf("receiver = %q, want Server", got.Def.Recv)
	}
}

// TestUninferableStaysHonest: when the type genuinely cannot be worked out,
// the result must be marked a guess so the UI offers a choice.
func TestUninferableStaysHonest(t *testing.T) {
	files := map[string]string{
		"go.mod": testModule,
		"a/a.go": `package a

type A struct{}

func (a *A) Validate() error { return nil }

type B struct{}

func (b *B) Validate() error { return nil }

func run(things map[string]interface{}) {
	x := things["k"]
	x.Validate()
}
`,
	}
	ix := buildIndex(t, files)
	got, err := ix.Lookup("Validate", "x", "a/a.go", 13, files["a/a.go"])
	if err != nil {
		t.Fatal(err)
	}
	if got.Confidence == "receiver-type" {
		t.Errorf("claimed a receiver type it cannot know: %+v", got)
	}
	if !got.Ambiguous {
		t.Errorf("expected alternatives to be offered, got %+v", got)
	}
}

// TestBodylessFuncDoesNotPanic: assembly-backed, cgo and //go:linkname
// declarations are valid Go and parse with a nil Body, which the AST walk
// dereferenced.
func TestBodylessFuncDoesNotPanic(t *testing.T) {
	files := map[string]string{
		"go.mod": testModule,
		"a/asm.go": `package a

type T struct{}

func (t *T) Validate() error { return nil }

//go:noescape
func fastPath(x []byte) int
`,
	}
	ix := buildIndex(t, files)
	// The click lands on the bodyless declaration; must not panic.
	if _, err := ix.Lookup("Validate", "zz", "a/asm.go", 8, files["a/asm.go"]); err != nil {
		t.Logf("lookup error (acceptable): %v", err)
	}
}

// TestConstructorDoesNotCrossPackages: resolving a bare constructor name
// repository-wide returned an unrelated type and labelled it certain, which is
// worse than not inferring at all.
func TestConstructorDoesNotCrossPackages(t *testing.T) {
	files := map[string]string{
		"go.mod": testModule,
		"aaa/aaa.go": `package aaa

type Client struct{}

func (c *Client) Close() {}

func New() *Client { return &Client{} }
`,
		"zzz/zzz.go": `package zzz

type Server struct{}

func (s *Server) Close() {}

func New() *Server { return &Server{} }
`,
		"zzz/use.go": `package zzz

func run() {
	s := New()
	s.Close()
}
`,
	}
	ix := buildIndex(t, files)
	got, err := ix.Lookup("Close", "s", "zzz/use.go", 5, files["zzz/use.go"])
	if err != nil {
		t.Fatal(err)
	}
	if got.Def.Recv != "Server" {
		t.Errorf("resolved to %s.Close in %s, want Server.Close in zzz",
			got.Def.Recv, got.Def.Path)
	}
}

// TestInRepoPackageMissingNameErrors: an in-repo package that does not declare
// the name must not fall through to an unrelated match reported as "unique".
func TestInRepoPackageMissingNameErrors(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/m\n",
		"cmd/main.go": `package main

import "example.com/m/aaa"

func main() { aaa.Absent() }
`,
		"aaa/aaa.go":     "package aaa\n\nfunc Present() {}\n",
		"other/other.go": "package other\n\nfunc Absent() {}\n",
	}
	ix := buildIndex(t, files)
	got, err := ix.Lookup("Absent", "aaa", "cmd/main.go", 5, files["cmd/main.go"])
	if err == nil {
		t.Errorf("resolved to %+v with confidence %q; want an error",
			got.Def, got.Confidence)
	}
}
