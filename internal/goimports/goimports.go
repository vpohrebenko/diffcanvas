// Package goimports works out which of the open Go files import one another.
//
// This is cheap to do well for Go specifically: the standard library parses
// import declarations exactly, and a module's own import paths map onto
// directories in the repository.
package goimports

import (
	"context"
	"go/parser"
	"go/token"
	"path"
	"strings"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
	"github.com/vpohrebenko/diffcanvas/internal/gomod"
)

// Edge is a directed dependency from one file to another.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Pkg  string `json:"pkg"`
}

// Edges returns the import relationships among the given paths.
//
// Only the supplied paths are considered, so the result is exactly the set of
// arrows that can be drawn between cards already on the canvas. Drawing edges
// to files that are not open would produce nothing useful.
func Edges(ctx context.Context, repo *gitx.Repo, rev string, paths []string) ([]Edge, error) {
	// Every go.mod that could govern these files, not just one at the root.
	// Assuming a single root module meant this returned nothing at all for a
	// repository whose module lives in a subdirectory — no error, no arrows.
	modules := gomod.Find(ctx, repo, rev, gomod.Candidates(paths))
	if len(modules) == 0 {
		return []Edge{}, nil
	}

	// Group the candidate files by the directory they live in, since a Go
	// import names a package directory rather than a file.
	byDir := map[string][]string{}
	var goFiles []string
	for _, p := range paths {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		goFiles = append(goFiles, p)
		dir := path.Dir(p)
		byDir[dir] = append(byDir[dir], p)
	}

	var edges []Edge
	seen := map[[2]string]bool{}

	for _, file := range goFiles {
		imports, err := importsOf(ctx, repo, rev, file)
		if err != nil {
			continue // unparseable file: skip rather than fail the request
		}
		for _, imp := range imports {
			dir, internal := modules.Dir(imp)
			if !internal {
				continue // external dependency
			}
			// One edge per imported package, not one per file in it. An import
			// names a directory, so fanning out to every open file there
			// produces arrows nobody asked for — including to _test.go files,
			// which are never the thing being imported.
			target := representative(byDir[dir], dir)
			if target == "" || target == file {
				continue
			}
			key := [2]string{file, target}
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, Edge{From: file, To: target, Pkg: imp})
		}
	}
	if edges == nil {
		edges = []Edge{}
	}
	return edges, nil
}

// representative picks the single open file that best stands for a package.
//
// Preference order: a file named after its directory (util/util.go), then any
// other non-test file, shortest name first so core.go beats core_windows.go.
// Test files are only chosen if the package has nothing else open, since an
// arrow into a _test.go file almost always misrepresents the dependency.
func representative(files []string, dir string) string {
	if len(files) == 0 {
		return ""
	}
	pkgName := path.Base(dir)

	best := ""
	bestRank := 99
	for _, f := range files {
		name := path.Base(f)
		rank := 2
		switch {
		case strings.HasSuffix(name, "_test.go"):
			rank = 3
		case name == pkgName+".go":
			rank = 0
		default:
			rank = 1
		}
		if rank < bestRank || (rank == bestRank && shorter(f, best)) {
			best, bestRank = f, rank
		}
	}
	return best
}

func shorter(a, b string) bool {
	if b == "" {
		return true
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// importsOf parses just the import block, which is all we need and is far
// cheaper than a full parse.
func importsOf(ctx context.Context, repo *gitx.Repo, rev, file string) ([]string, error) {
	f, err := gitx.ReadFile(ctx, repo, rev, file)
	if err != nil || f.Binary {
		return nil, err
	}
	src := strings.Join(f.Lines, "\n")

	parsed, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ImportsOnly)
	if parsed == nil {
		return nil, err
	}

	out := make([]string, 0, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		if spec.Path == nil {
			continue
		}
		out = append(out, strings.Trim(spec.Path.Value, `"`))
	}
	return out, nil
}
