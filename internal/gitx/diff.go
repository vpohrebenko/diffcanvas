package gitx

import (
	"context"
	"strconv"
	"strings"
)

// FileChange is one entry in the change list. It deliberately carries no file
// contents: the change list must stay small and fast even for a 1500-file
// range, and bodies are fetched per-card on demand.
type FileChange struct {
	Path    string `json:"path"`              // path on the new side
	OldPath string `json:"oldPath,omitempty"` // set for renames and copies
	Status  string `json:"status"`
	Adds    int    `json:"adds"`
	Dels    int    `json:"dels"`
	Binary  bool   `json:"binary"`
	Score   int    `json:"score,omitempty"` // rename/copy similarity percentage
}

// Changes lists every file touched by the spec.
//
// It issues two cheap plumbing calls rather than generating patches. Both use
// -z, which makes paths NUL-delimited and therefore immune to the quoting that
// bites textual parsers on paths with spaces or non-ASCII characters.
func Changes(ctx context.Context, r *Repo, spec *Spec) ([]FileChange, error) {
	statusArgs := append([]string{"diff", "--name-status", "-z", "-M", "-C", "--no-ext-diff"}, spec.diffArgs...)
	statusOut, err := r.run(ctx, append(statusArgs, "--")...)
	if err != nil {
		return nil, err
	}

	numstatArgs := append([]string{"diff", "--numstat", "-z", "-M", "-C", "--no-ext-diff"}, spec.diffArgs...)
	numstatOut, err := r.run(ctx, append(numstatArgs, "--")...)
	if err != nil {
		return nil, err
	}

	changes := parseNameStatus(splitNUL(statusOut))
	applyNumstat(changes, splitNUL(numstatOut))
	return changes, nil
}

// parseNameStatus consumes `git diff --name-status -z` fields.
//
// Layout is a status field followed by one path, except for R (rename) and
// C (copy), which are followed by two paths: old then new.
func parseNameStatus(fields []string) []FileChange {
	var out []FileChange
	for i := 0; i < len(fields); {
		code := fields[i]
		if code == "" {
			i++
			continue
		}
		i++
		letter := code[0]
		score, _ := strconv.Atoi(strings.TrimLeft(code[1:], "0"))

		var oldPath, newPath string
		if letter == 'R' || letter == 'C' {
			if i+1 >= len(fields) {
				break
			}
			oldPath, newPath = fields[i], fields[i+1]
			i += 2
		} else {
			if i >= len(fields) {
				break
			}
			newPath = fields[i]
			i++
		}

		out = append(out, FileChange{
			Path:    newPath,
			OldPath: oldPath,
			Status:  statusName(letter),
			Score:   score,
		})
	}
	return out
}

// applyNumstat folds add/delete counts into the change list.
//
// Layout is "adds\tdels\tpath", except renames, which emit "adds\tdels\t" with
// the path field empty and the old and new paths in the two following fields.
// Binary files report "-" for both counts.
func applyNumstat(changes []FileChange, fields []string) {
	byPath := make(map[string]*FileChange, len(changes))
	for i := range changes {
		byPath[changes[i].Path] = &changes[i]
	}

	for i := 0; i < len(fields); {
		parts := strings.SplitN(fields[i], "\t", 3)
		i++
		if len(parts) < 3 {
			continue
		}

		path := parts[2]
		if path == "" { // rename: the two following fields are old and new
			if i+1 >= len(fields) {
				break
			}
			path = fields[i+1]
			i += 2
		}

		fc, ok := byPath[path]
		if !ok {
			continue
		}
		if parts[0] == "-" || parts[1] == "-" {
			fc.Binary = true
			continue
		}
		fc.Adds, _ = strconv.Atoi(parts[0])
		fc.Dels, _ = strconv.Atoi(parts[1])
	}
}

func statusName(letter byte) string {
	switch letter {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'T':
		return "typechanged"
	default:
		return "modified"
	}
}

// Hunk is one @@ block.
type Hunk struct {
	Header   string `json:"header"` // the section heading git puts after @@
	OldStart int    `json:"oldStart"`
	OldLines int    `json:"oldLines"`
	NewStart int    `json:"newStart"`
	NewLines int    `json:"newLines"`
	Lines    []Line `json:"lines"`
}

// Line is a single diff row. Old and New are 0 when the line does not exist on
// that side.
type Line struct {
	T    string `json:"t"` // " ", "+" or "-"
	Old  int    `json:"old"`
	New  int    `json:"new"`
	C    string `json:"c"`
	NoNL bool   `json:"noNL,omitempty"`
}

// Patch generates the diff for a single file with the requested amount of
// context.
//
// Because the pathspec pins the request to one known file, the returned patch
// needs no path parsing at all: everything before the first @@ is discarded.
// That sidesteps the quoting and rename-header edge cases that make generic
// `diff --git` header parsing fragile.
func Patch(ctx context.Context, r *Repo, spec *Spec, fc *FileChange, contextLines int) ([]Hunk, error) {
	if contextLines < 0 {
		contextLines = 3
	}
	args := []string{"diff", "--no-ext-diff", "--no-color", "-M", "-C",
		"--unified=" + strconv.Itoa(contextLines)}
	args = append(args, spec.diffArgs...)
	args = append(args, "--")

	// Renames and copies need both sides named, or the pathspec misses the
	// pairing. That can make git emit two file sections, so the reply is
	// demultiplexed by path below.
	if fc.OldPath != "" {
		args = append(args, literalPathspec(fc.OldPath))
	}
	args = append(args, literalPathspec(fc.Path))

	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseHunks(string(out), fc.Path), nil
}

// literalPathspec disables git's pathspec magic for a path that came from the
// filesystem rather than from the user.
//
// Without it a file named `a*.txt` globs onto its neighbours and a file named
// `:weird.txt` is read as the `:<path>` magic prefix — in both cases the card
// silently shows the wrong file, or none at all.
func literalPathspec(path string) string {
	return ":(literal)" + path
}

// fileSection is one `diff --git` block within a patch.
type fileSection struct {
	newPath string
	oldPath string
	hunks   []Hunk
}

// parseHunks reads a patch and returns the hunks belonging to wantPath.
//
// A patch can legitimately contain more than one file section — asking for a
// copy names both the source and the destination — and the sections are
// ordered by path, not by the order the pathspecs were given. Treating the
// whole patch as one file therefore concatenated an unrelated file's hunks and
// turned its `--- a/x` / `+++ b/x` headers into diff rows, because they start
// with '-' and '+'.
func parseHunks(patch string, wantPath string) []Hunk {
	var sections []fileSection
	var cur *fileSection
	var hunk *Hunk
	var oldLine, newLine int

	flushHunk := func() {
		if hunk != nil && cur != nil {
			cur.hunks = append(cur.hunks, *hunk)
		}
		hunk = nil
	}
	flushSection := func() {
		flushHunk()
		if cur != nil {
			sections = append(sections, *cur)
		}
		cur = nil
	}

	for _, raw := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			flushSection()
			cur = &fileSection{}
			continue

		case hunk == nil && strings.HasPrefix(raw, "--- "):
			if cur != nil {
				cur.oldPath = headerPath(raw[4:])
			}
			continue

		case hunk == nil && strings.HasPrefix(raw, "+++ "):
			if cur != nil {
				cur.newPath = headerPath(raw[4:])
			}
			continue

		case strings.HasPrefix(raw, "@@"):
			flushHunk()
			if cur == nil {
				cur = &fileSection{}
			}
			h := parseHunkHeader(raw)
			hunk = &h
			oldLine, newLine = h.OldStart, h.NewStart
			continue
		}

		if hunk == nil {
			continue // mode lines, similarity index, "Binary files differ"
		}

		switch {
		case strings.HasPrefix(raw, "\\"):
			// "\ No newline at end of file" annotates the preceding line.
			if n := len(hunk.Lines); n > 0 {
				hunk.Lines[n-1].NoNL = true
			}
		case strings.HasPrefix(raw, "+"):
			hunk.Lines = append(hunk.Lines, Line{T: "+", New: newLine, C: raw[1:]})
			newLine++
		case strings.HasPrefix(raw, "-"):
			hunk.Lines = append(hunk.Lines, Line{T: "-", Old: oldLine, C: raw[1:]})
			oldLine++
		case strings.HasPrefix(raw, " "):
			hunk.Lines = append(hunk.Lines, Line{T: " ", Old: oldLine, New: newLine, C: raw[1:]})
			oldLine++
			newLine++
		}
	}
	flushSection()

	return pickSection(sections, wantPath)
}

// pickSection chooses the file section the caller asked for, preferring the
// new-side path and falling back to the old side for deletions.
func pickSection(sections []fileSection, wantPath string) []Hunk {
	if len(sections) == 0 {
		return nil
	}
	if wantPath != "" {
		for _, s := range sections {
			if s.newPath == wantPath {
				return s.hunks
			}
		}
		for _, s := range sections {
			if s.oldPath == wantPath {
				return s.hunks
			}
		}
	}
	// Unrecognised header quoting: fall back to the first section that has
	// content rather than returning nothing.
	for _, s := range sections {
		if len(s.hunks) > 0 {
			return s.hunks
		}
	}
	return sections[0].hunks
}

// headerPath strips the a/ or b/ prefix git puts on patch header paths.
func headerPath(field string) string {
	field = strings.TrimSuffix(field, "\r")
	if field == "/dev/null" {
		return ""
	}
	// Quoted paths (embedded quotes or control characters) are left alone; the
	// caller falls back to positional matching for those.
	if strings.HasPrefix(field, `"`) {
		return ""
	}
	if len(field) > 2 && (field[0] == 'a' || field[0] == 'b') && field[1] == '/' {
		return field[2:]
	}
	return field
}

// parseHunkHeader reads "@@ -a,b +c,d @@ section heading".
func parseHunkHeader(raw string) Hunk {
	h := Hunk{OldLines: 1, NewLines: 1}

	rest := strings.TrimPrefix(raw, "@@")
	ranges, heading, found := strings.Cut(rest, "@@")
	if found {
		h.Header = strings.TrimSpace(heading)
	}

	for _, field := range strings.Fields(ranges) {
		if len(field) < 2 {
			continue
		}
		start, count := field[1:], 1
		if s, c, ok := strings.Cut(field[1:], ","); ok {
			start = s
			count, _ = strconv.Atoi(c)
		}
		n, _ := strconv.Atoi(start)
		switch field[0] {
		case '-':
			h.OldStart, h.OldLines = n, count
		case '+':
			h.NewStart, h.NewLines = n, count
		}
	}
	return h
}
