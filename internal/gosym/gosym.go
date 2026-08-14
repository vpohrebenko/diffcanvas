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
	"github.com/vpohrebenko/diffcanvas/internal/gomod"
)

// Def is one declaration.
type Def struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Kind string `json:"kind"`           // func, method, type, var, const
	Recv string `json:"recv,omitempty"` // receiver type, for methods
	Pkg  string `json:"pkg,omitempty"`  // package name as declared
	// Result type names, so a constructor call can be followed back to the
	// type it produces.
	Results []string `json:"-"`
}

// Index holds every declaration in the repository at one revision.
type Index struct {
	byName map[string][]Def
	// imports maps a file to the import paths of its qualifiers, so `util.X`
	// in one file and `util.X` in another can resolve to different packages.
	imports map[string]map[string]string
	// Every module in the repository, not just one at the root.
	modules gomod.Set
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
		modules: gomod.Find(ctx, repo, rev, paths),
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
			if d.Type != nil && d.Type.Results != nil {
				for _, r := range d.Type.Results.List {
					if n := typeName(r.Type); n != "" {
						def.Results = append(def.Results, n)
					}
				}
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

// typeName reduces an expression like `*pkg.Foo[T]` to `Foo`.
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name // pkg.Foo -> Foo
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	case *ast.ArrayType:
		return typeName(t.Elt)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

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

// inferLocalType works out the type of a local identifier at a given line.
//
// `req.Validate()` gives no clue which Validate is meant — every type tends to
// have one — so resolving by method name alone returns a different answer each
// time. Reading the declaration of `req` in the same file recovers the receiver
// type for the cases that matter in practice: a method receiver, a function
// parameter, a var declaration, a composite literal, and a constructor call
// whose result type is known from the index.
//
// This is still not type checking: it does not follow fields, interfaces or
// values from other packages. It returns "" when it cannot tell, and the
// caller then offers a choice rather than pretending.
func (ix *Index) inferLocalType(src, qual string, line int, fromDir string) string {
	if src == "" || qual == "" || line <= 0 {
		return ""
	}
	fset := token.NewFileSet()
	file, _ := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if file == nil {
		return ""
	}
	lineOf := func(p token.Pos) int { return fset.Position(p).Line }

	// The function containing the click; nearest declaration wins.
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		f, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if lineOf(f.Pos()) <= line && line <= lineOf(f.End()) {
			fn = f
			break
		}
	}
	if fn == nil {
		return ""
	}

	// A method receiver.
	if fn.Recv != nil {
		for _, f := range fn.Recv.List {
			for _, n := range f.Names {
				if n.Name == qual {
					return typeName(f.Type)
				}
			}
		}
	}
	// Parameters and named results.
	for _, list := range []*ast.FieldList{fn.Type.Params, fn.Type.Results} {
		if list == nil {
			continue
		}
		for _, f := range list.List {
			for _, n := range f.Names {
				if n.Name == qual {
					return typeName(f.Type)
				}
			}
		}
	}

	// A declaration with no body — assembly-backed, cgo, //go:linkname — is
	// valid Go and parses to a nil Body, which ast.Inspect dereferences.
	if fn.Body == nil {
		return ""
	}

	// Assignments and var declarations in the body, taking the last one
	// before the click.
	best := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil || lineOf(n.Pos()) > line {
			return true
		}
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range stmt.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != qual || i >= len(stmt.Rhs) {
					continue
				}
				if t := ix.typeOfExpr(stmt.Rhs[i], fromDir); t != "" {
					best = t
				}
			}
		case *ast.ValueSpec:
			for _, id := range stmt.Names {
				if id.Name != qual {
					continue
				}
				if stmt.Type != nil {
					best = typeName(stmt.Type)
				} else if len(stmt.Values) > 0 {
					if t := ix.typeOfExpr(stmt.Values[0], fromDir); t != "" {
						best = t
					}
				}
			}
		}
		return true
	})
	return best
}

// typeOfExpr names the type an expression produces, where that is decidable
// without a type checker.
func (ix *Index) typeOfExpr(expr ast.Expr, fromDir string) string {
	switch e := expr.(type) {
	case *ast.CompositeLit:
		return typeName(e.Type) // Foo{...}
	case *ast.UnaryExpr:
		return ix.typeOfExpr(e.X, fromDir) // &Foo{...}
	case *ast.CallExpr:
		// A constructor: follow it to its declared result type. Only an
		// unqualified call in the caller's own package can be resolved this
		// way — matching a bare name across the repository picked whichever
		// New() sorted first and then reported the wrong type as certain,
		// which was worse than not inferring at all.
		id, ok := e.Fun.(*ast.Ident)
		if !ok || fromDir == "" {
			return ""
		}
		for _, d := range ix.byName[id.Name] {
			if d.Kind == "func" && len(d.Results) > 0 && path.Dir(d.Path) == fromDir {
				return d.Results[0]
			}
		}
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
func (ix *Index) Lookup(name, qual, fromPath string, line int, src string) (Result, error) {
	candidates := ix.byName[name]
	if len(candidates) == 0 {
		return Result{}, fmt.Errorf("no declaration of %q found", name)
	}

	fromDir := path.Dir(fromPath)

	// 1. Qualified by a real import of this file.
	if qual != "" {
		if importPath, ok := ix.imports[fromPath][qual]; ok {
			dir, internal := ix.modules.Dir(importPath)
			if internal {
				if hit := filterDir(candidates, dir); len(hit) > 0 {
					return result(hit, "exact-package"), nil
				}
				// The package is in this repository and does not declare the
				// name. Falling through used to hand back an unrelated
				// declaration labelled "unique", i.e. a wrong answer presented
				// as fact.
				return Result{}, fmt.Errorf("%s.%s: package %s does not declare %s",
					qual, name, importPath, name)
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

	// 3. The qualifier is a local variable: infer its type and take the method
	// on that type. This is what makes `req.Validate()` land on the right
	// Validate rather than on whichever one happened to sort first.
	if qual != "" {
		if recv := ix.inferLocalType(src, qual, line, fromDir); recv != "" {
			var onType []Def
			for _, d := range candidates {
				if d.Kind == "method" && d.Recv == recv {
					onType = append(onType, d)
				}
			}
			if len(onType) > 0 {
				return result(onType, "receiver-type"), nil
			}
		}
	}

	// 4. Same package as the caller.
	if hit := filterDir(candidates, fromDir); len(hit) > 0 {
		if qual != "" {
			if methods := filterKind(hit, "method"); len(methods) > 0 {
				// The type could not be inferred, so this is a name match
				// among the caller's own package: plausible, not certain.
				return result(methods, "guess"), nil
			}
		}
		return result(hit, "same-package"), nil
	}

	// 5. Anywhere, preferring non-test declarations.
	preferred := preferNonTest(candidates)
	confidence := "guess"
	if len(preferred) == 1 {
		confidence = "unique"
	}
	return result(preferred, confidence), nil
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
