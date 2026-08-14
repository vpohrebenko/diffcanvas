// Package gomod maps Go import paths onto directories in the repository.
//
// It exists because two callers need the same answer and had drifted apart:
// jump-to-definition handled repositories with several modules, or a go.mod in
// a subdirectory, while the import-arrow feature still assumed a single module
// at the root and silently produced nothing for every other layout.
package gomod

import (
	"context"
	"path"
	"strings"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
)

// Module is one go.mod found in the repository.
type Module struct {
	Dir  string // repo-relative directory holding go.mod ("" at the root)
	Path string // the module path it declares
}

// Set is every module in the repository.
type Set []Module

// Candidates returns the go.mod paths that could govern the given files: one
// in every ancestor directory, plus the repository root.
//
// Deriving them this way avoids listing the whole repository, and works before
// a file list is available at all.
func Candidates(paths []string) []string {
	seen := map[string]bool{"go.mod": true}
	for _, p := range paths {
		for dir := path.Dir(p); ; dir = path.Dir(dir) {
			if dir == "." || dir == "/" || dir == "" {
				break
			}
			seen[path.Join(dir, "go.mod")] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

// Find locates each go.mod among the given paths and reads its module line.
func Find(ctx context.Context, repo *gitx.Repo, rev string, paths []string) Set {
	var out Set
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
			out = append(out, Module{Dir: dir, Path: mp})
			break
		}
	}
	return out
}

// Dir maps an import path to a repository directory, using the most specific
// module that claims it, so a nested module beats its parent. The second
// result is false for anything outside the repository.
func (s Set) Dir(importPath string) (string, bool) {
	best := -1
	for i, m := range s {
		if m.Path == "" {
			continue
		}
		if importPath != m.Path && !strings.HasPrefix(importPath, m.Path+"/") {
			continue
		}
		if best < 0 || len(m.Path) > len(s[best].Path) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	m := s[best]
	sub := strings.TrimPrefix(strings.TrimPrefix(importPath, m.Path), "/")
	dir := path.Join(m.Dir, sub)
	if dir == "" {
		dir = "."
	}
	return dir, true
}
