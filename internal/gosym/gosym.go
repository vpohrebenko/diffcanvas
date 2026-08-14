// Package gosym resolves a Go identifier to the place it is declared.
//
// This is a navigation aid, not a type checker. It indexes top-level
// declarations with go/ast and resolves a clicked name using the calling
// file's imports plus package locality. Real resolution would need go/types
// and a full build, which would mean network access for dependencies and a
// working toolchain on the reviewer's machine — neither of which this tool
// assumes. The heuristics below are honest about being heuristics: an
// ambiguous name reports how many other candidates were found.
package gosym

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
)

// Def is one declaration.
type Def struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Kind string `json:"kind"`           // func, method, type, var, const
	Recv string `json:"recv,omitempty"` // receiver type, for methods
	Pkg  string `json:"pkg,omitempty"`  // package name as declared
}

// goModule is one go.mod found in the repository.
type goModule struct {
	dir  string // repo-relative directory holding go.mod ("" at the root)
	path string // the module path it declares
}

// Index holds every declaration in the repository at one revision.
type Index struct {
	byName map[string][]Def
	// imports maps a file to the import paths of its qualifiers, so `util.X`
	// in one file and `util.X` in another can resolve to different packages.
	imports map[string]map[string]string
	// Every module in the repository, not just one at the root: a repository
	// with go.mod in a subdirectory, or several modules, is normal, and
	// assuming a single root module made every qualified lookup fall through
	// to a repository-wide guess.
	modules []goModule
}

// maxIndexFiles bounds the work for very large repositories.
const maxIndexFiles = 20000

// Build parses every tracked Go file at rev and records its declarations.
func Build(ctx context.Context, repo *gitx.Repo, rev string) (*Index, error) {
	paths, err := gitx.ListFiles(ctx, repo, rev)
	if err != nil {
		return nil, err
	}

	ix := &Index{
		byName:  make(map[string][]Def),
		imports: make(map[string]map[string]string),
		modules: findModules(ctx, repo, rev, paths),
	}

	seen := 0
	for _, p := range paths {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		if seen++; seen > maxIndexFiles {
			break
		}
		ix.addFile(ctx, repo, rev, p)
	}
	return ix, nil
}

func (ix *Index) addFile(ctx context.Context, repo *gitx.Repo, rev, filePath string) {
	f, err := gitx.ReadFile(ctx, repo, rev, filePath)
	if err != nil || f.Binary {
		return
	}
	src := strings.Join(f.Lines, "\n")

	fset := token.NewFileSet()
	// SkipObjectResolution: we only need declarations and positions, and
	// building the object graph roughly doubles the cost.
	parsed, err := parser.ParseFile(fset, filePath, src, parser.SkipObjectResolution)
	if parsed == nil {
		return
	}

	pkgName := ""
	if parsed.Name != nil {
		pkgName = parsed.Name.Name
	}

	// Record the file's import qualifiers.
	quals := make(map[string]string)
	for _, spec := range parsed.Imports {
		if spec.Path == nil {
			continue
		}
		importPath := strings.Trim(spec.Path.Value, `"`)
		alias := path.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias == "_" || alias == "." {
			continue
		}
		quals[alias] = importPath
	}
	ix.imports[filePath] = quals

	line := func(pos token.Pos) int { return fset.Position(pos).Line }

	for _, decl := range parsed.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			def := Def{
				Name: d.Name.Name, Path: filePath, Line: line(d.Pos()),
				Kind: "func", Pkg: pkgName,
			}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				def.Kind = "method"
				def.Recv = receiverName(d.Recv.List[0].Type)
			}
			ix.add(def)

		case *ast.GenDecl:
			kind := ""
			switch d.Tok {
			case token.TYPE:
				kind = "type"
			case token.VAR:
				kind = "var"
			case token.CONST:
				kind = "const"
			default:
				continue // imports
			}
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name != nil {
						ix.add(Def{Name: s.Name.Name, Path: filePath, Line: line(s.Pos()),
							Kind: kind, Pkg: pkgName})
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name == "_" {
							continue
						}
						ix.add(Def{Name: n.Name, Path: filePath, Line: line(n.Pos()),
							Kind: kind, Pkg: pkgName})
					}
				}
			}
		}
	}
}

func (ix *Index) add(d Def) { ix.byName[d.Name] = append(ix.byName[d.Name], d) }

// receiverName reduces `*Foo[T]` to `Foo`.
func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// Result is a resolved lookup.
type Result struct {
	Def        Def    `json:"def"`
	Others     []Def  `json:"others,omitempty"`
	Ambiguous  bool   `json:"ambiguous"`
	Confidence string `json:"confidence"` // exact-package, same-package, unique, guess
}

// Lookup resolves name (optionally qualified) as referenced from fromPath.
//
// Resolution order, most to least trustworthy:
//  1. `pkg.Name` where pkg is an import of the calling file — the package
//     directory is known exactly.
//  2. An unqualified name declared in the calling file's own package.
//  3. A unique declaration anywhere in the repository.
//  4. Otherwise the best guess, with the alternatives returned alongside.
func (ix *Index) Lookup(name, qual, fromPath string) (Result, error) {
	candidates := ix.byName[name]
	if len(candidates) == 0 {
		return Result{}, fmt.Errorf("no declaration of %q found", name)
	}

	fromDir := path.Dir(fromPath)

	// 1. Qualified by a real import of this file.
	if qual != "" {
		if importPath, ok := ix.imports[fromPath][qual]; ok {
			dir, internal := ix.dirOf(importPath)
			if internal {
				if hit := filterDir(candidates, dir); len(hit) > 0 {
					return result(hit, "exact-package"), nil
				}
			} else if len(filterPkg(candidates, qual)) == 0 {
				// The qualifier names an import outside this repository — the
				// standard library or a dependency — and nothing here declares
				// a package by that name. Falling through would "resolve"
				// fmt.Print to an unrelated local method, which is worse than
				// saying no.
				//
				// When a local package *is* named after the qualifier, the
				// import simply could not be mapped (a vendored path, a
				// replace directive, a module layout we did not find), so the
				// package-name match below is the better answer.
				return Result{}, fmt.Errorf("%s.%s is declared in %s, outside this repository",
					qual, name, importPath)
			}
		}
	}

	// 2. The qualifier matches a package's declared name. Weaker than reading
	// the import, but far better than a repository-wide guess, and it is what
	// rescues a repository whose module layout we could not resolve.
	if qual != "" {
		if hit := filterPkg(candidates, qual); len(hit) > 0 {
			return result(preferNonTest(hit), "package-name"), nil
		}
	}

	// 3. Same package as the caller. For a qualified name this is wrong unless
	// the qualifier was a variable (a method call), which is exactly the case
	// go/types would be needed to settle.
	if hit := filterDir(candidates, fromDir); len(hit) > 0 {
		// A method call on a local variable resolves to a method of that name.
		if qual != "" {
			if methods := filterKind(hit, "method"); len(methods) > 0 {
				return result(methods, "same-package"), nil
			}
		}
		return result(hit, "same-package"), nil
	}

	// 4/5. Anywhere, preferring non-test declarations.
	preferred := preferNonTest(candidates)
	confidence := "guess"
	if len(preferred) == 1 {
		confidence = "unique"
	}
	return result(preferred, confidence), nil
}

// dirOf maps an import path to a repository directory, using the most
// specific module that claims it. Longest match wins, so a nested module
// beats its parent.
func (ix *Index) dirOf(importPath string) (string, bool) {
	best := -1
	for i, m := range ix.modules {
		if m.path == "" {
			continue
		}
		if importPath != m.path && !strings.HasPrefix(importPath, m.path+"/") {
			continue
		}
		if best < 0 || len(m.path) > len(ix.modules[best].path) {
			best = i
		}
	}
	if best < 0 {
		return "", false // external dependency, not in this repository
	}
	m := ix.modules[best]
	sub := strings.TrimPrefix(strings.TrimPrefix(importPath, m.path), "/")
	dir := path.Join(m.dir, sub)
	if dir == "" {
		dir = "."
	}
	return dir, true
}

// findModules locates every go.mod in the repository.
func findModules(ctx context.Context, repo *gitx.Repo, rev string, paths []string) []goModule {
	var out []goModule
	for _, p := range paths {
		if path.Base(p) != "go.mod" {
			continue
		}
		f, err := gitx.ReadFile(ctx, repo, rev, p)
		if err != nil {
			continue
		}
		for _, line := range f.Lines {
			rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module")
			if !ok {
				continue
			}
			mp := strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), `"`))
			if mp == "" {
				break
			}
			dir := path.Dir(p)
			if dir == "." {
				dir = ""
			}
			out = append(out, goModule{dir: dir, path: mp})
			break
		}
	}
	return out
}

func filterDir(defs []Def, dir string) []Def {
	var out []Def
	for _, d := range defs {
		if path.Dir(d.Path) == dir {
			out = append(out, d)
		}
	}
	return out
}

func filterPkg(defs []Def, pkgName string) []Def {
	var out []Def
	for _, d := range defs {
		if d.Pkg == pkgName {
			out = append(out, d)
		}
	}
	return out
}

func filterKind(defs []Def, kind string) []Def {
	var out []Def
	for _, d := range defs {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

func preferNonTest(defs []Def) []Def {
	var out []Def
	for _, d := range defs {
		if !strings.HasSuffix(d.Path, "_test.go") {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return defs
	}
	return out
}

// result orders candidates so the most useful one is first: declarations
// before variables, then shortest path, so `util/util.go` beats a deep test.
func result(defs []Def, confidence string) Result {
	sorted := append([]Def(nil), defs...)
	rank := map[string]int{"func": 0, "method": 0, "type": 1, "const": 2, "var": 3}
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := rank[sorted[i].Kind], rank[sorted[j].Kind]
		if ri != rj {
			return ri < rj
		}
		return len(sorted[i].Path) < len(sorted[j].Path)
	})

	res := Result{Def: sorted[0], Confidence: confidence}
	if len(sorted) > 1 {
		res.Ambiguous = true
		res.Others = sorted[1:]
		if len(res.Others) > 30 {
			res.Others = res.Others[:30]
		}
	}
	return res
}

// modulePath reads the root go.mod, for callers that only need the main one.
func modulePath(ctx context.Context, repo *gitx.Repo, rev string) string {
	f, err := gitx.ReadFile(ctx, repo, rev, "go.mod")
	if err != nil {
		return ""
	}
	for _, line := range f.Lines {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module"); ok {
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), `"`))
		}
	}
	return ""
}
