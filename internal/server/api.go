package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/vpohrebenko/diffcanvas/internal/diffx"
	"github.com/vpohrebenko/diffcanvas/internal/gitx"
	"github.com/vpohrebenko/diffcanvas/internal/goimports"
	"github.com/vpohrebenko/diffcanvas/internal/gosym"
	"github.com/vpohrebenko/diffcanvas/internal/highlight"
)

// changeCache memoises the change list. The spec is fixed for the lifetime of
// the server, so this only needs computing once. It lives on the Server rather
// than in a package variable so several servers can coexist in one process.
type changeCache struct {
	mu      sync.Mutex
	ready   bool
	changes []gitx.FileChange
	byPath  map[string]*gitx.FileChange
}

// changes returns the cached change list, computing it on first use.
//
// Two details matter, and getting either wrong bricks the server for its whole
// lifetime. Failures are never cached: a sync.Once would memoise the error and
// every later request would fail. And the git command runs on a background
// context rather than the request's, because reloading the page mid-load
// cancels the request, which would otherwise kill git and poison the result.
func (s *Server) changes(context.Context) ([]gitx.FileChange, map[string]*gitx.FileChange, error) {
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()

	if s.cache.ready {
		return s.cache.changes, s.cache.byPath, nil
	}

	changes, err := gitx.Changes(context.Background(), s.Repo, s.Spec)
	if err != nil {
		return nil, nil, err // retried on the next request
	}

	byPath := make(map[string]*gitx.FileChange, len(changes))
	for i := range changes {
		byPath[changes[i].Path] = &changes[i]
	}
	s.cache.changes, s.cache.byPath, s.cache.ready = changes, byPath, true
	return changes, byPath, nil
}

func (s *Server) handleMeta(ctx context.Context, r *http.Request) (any, error) {
	changes, _, err := s.changes(ctx)
	if err != nil {
		return nil, err
	}
	var adds, dels int
	for _, c := range changes {
		adds += c.Adds
		dels += c.Dels
	}
	return map[string]any{
		"repo":  s.Repo.Root,
		"spec":  s.Spec,
		"files": len(changes),
		"adds":  adds,
		"dels":  dels,
	}, nil
}

func (s *Server) handleChanges(ctx context.Context, r *http.Request) (any, error) {
	changes, _, err := s.changes(ctx)
	if err != nil {
		return nil, err
	}
	if changes == nil {
		changes = []gitx.FileChange{}
	}
	return map[string]any{"changes": changes}, nil
}

// maxWordDiffPairs bounds the intra-line comparison per file. Each pair costs
// an O(n*m) table, so a machine-generated file with thousands of similar
// replaced lines otherwise dominates the whole request.
const maxWordDiffPairs = 3000

// LineView is one rendered row: the diff metadata plus its highlighted text.
type LineView struct {
	T    string              `json:"t"`
	Old  int                 `json:"old"`
	New  int                 `json:"new"`
	Segs []highlight.Segment `json:"segs"`
	NoNL bool                `json:"noNL,omitempty"`
}

// HunkView is a hunk with highlighted lines.
//
// Pairs is computed here rather than in the browser so that the side-by-side
// layout and the word-level diff agree on which removed line corresponds to
// which added line. Each entry indexes into Lines; -1 means a blank cell.
type HunkView struct {
	Header   string     `json:"header"`
	OldStart int        `json:"oldStart"`
	NewStart int        `json:"newStart"`
	Lines    []LineView `json:"lines"`
	Pairs    [][2]int   `json:"pairs"`
}

// FileView is the payload for one card.
type FileView struct {
	Path    string     `json:"path"`
	OldPath string     `json:"oldPath,omitempty"`
	Status  string     `json:"status"`
	Lang    string     `json:"lang"`
	Mode    string     `json:"mode"`
	Binary  bool       `json:"binary"`
	Trunc   bool       `json:"truncated"`
	Adds    int        `json:"adds"`
	Dels    int        `json:"dels"`
	Hunks   []HunkView `json:"hunks,omitempty"`
	Lines   []LineView `json:"lines,omitempty"`
	Changed []int      `json:"changed,omitempty"` // new-side line numbers that were added
	Context int        `json:"context"`
}

func (s *Server) handleFile(ctx context.Context, r *http.Request) (any, error) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	mode := q.Get("mode")
	if mode == "" {
		mode = "diff"
	}
	contextLines := 3
	if v := q.Get("context"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100000 {
			contextLines = n
		}
	}

	_, byPath, err := s.changes(ctx)
	if err != nil {
		return nil, err
	}
	fc := byPath[path]

	if mode == "full" || fc == nil {
		return s.fullFileView(ctx, path, fc, contextLines)
	}
	return s.diffFileView(ctx, fc, contextLines)
}

// diffFileView builds the unified diff for one file.
//
// Highlighting is computed over the complete old and new files and then mapped
// onto diff rows by line number. Highlighting each diff fragment on its own
// would break every construct that spans lines.
func (s *Server) diffFileView(ctx context.Context, fc *gitx.FileChange, contextLines int) (*FileView, error) {
	view := &FileView{
		Path: fc.Path, OldPath: fc.OldPath, Status: fc.Status,
		Lang: highlight.Detect(fc.Path), Mode: "diff",
		Binary: fc.Binary, Adds: fc.Adds, Dels: fc.Dels, Context: contextLines,
	}
	if fc.Binary {
		return view, nil
	}

	hunks, err := gitx.Patch(ctx, s.Repo, s.Spec, fc, contextLines)
	if err != nil {
		return nil, err
	}

	oldSegs := s.highlightSide(ctx, s.Spec.OldRev, oldPathOf(fc), fc.Path)
	newSegs := s.highlightSide(ctx, s.Spec.NewRev, fc.Path, fc.Path)

	view.Hunks = make([]HunkView, 0, len(hunks))
	for _, h := range hunks {
		hv := HunkView{Header: h.Header, OldStart: h.OldStart, NewStart: h.NewStart}
		hv.Lines = make([]LineView, 0, len(h.Lines))
		for _, l := range h.Lines {
			lv := LineView{T: l.T, Old: l.Old, New: l.New, NoNL: l.NoNL}
			switch l.T {
			case "-":
				lv.Segs = segsAt(oldSegs, l.Old, l.C)
			default:
				lv.Segs = segsAt(newSegs, l.New, l.C)
			}
			hv.Lines = append(hv.Lines, lv)
		}
		hv.Pairs = pairsOf(h.Lines)
		markWords(hv.Lines, hv.Pairs, h.Lines)
		view.Hunks = append(view.Hunks, hv)
	}
	return view, nil
}

func pairsOf(lines []gitx.Line) [][2]int {
	kinds := make([]diffx.Kind, len(lines))
	for i, l := range lines {
		switch l.T {
		case "-":
			kinds[i] = diffx.Removed
		case "+":
			kinds[i] = diffx.Added
		default:
			kinds[i] = diffx.Context
		}
	}
	pairs := diffx.PairLines(kinds)
	out := make([][2]int, len(pairs))
	for i, p := range pairs {
		out[i] = [2]int{p.Old, p.New}
	}
	return out
}

// markWords flags the segments inside each replaced line that actually
// changed, so a one-word edit no longer reads as two rewritten lines.
func markWords(views []LineView, pairs [][2]int, raw []gitx.Line) {
	budget := maxWordDiffPairs
	for _, p := range pairs {
		if budget <= 0 {
			return // huge rewrite: line-level colour is enough
		}
		oldIdx, newIdx := p[0], p[1]
		if oldIdx < 0 || newIdx < 0 || oldIdx == newIdx {
			continue // an unpaired line is wholly added or removed
		}
		if raw[oldIdx].T != "-" || raw[newIdx].T != "+" {
			continue
		}
		oldMarks, newMarks, ok := diffx.Compare(raw[oldIdx].C, raw[newIdx].C)
		if !ok {
			continue // too dissimilar to align; leave the whole line marked
		}
		budget--
		views[oldIdx].Segs = splitOnMarks(views[oldIdx].Segs, oldMarks)
		views[newIdx].Segs = splitOnMarks(views[newIdx].Segs, newMarks)
	}
}

// splitOnMarks cuts highlighted segments at the word-diff boundaries and flags
// the pieces that changed.
//
// Splitting rather than overlaying keeps the invariant that a line is exactly
// the concatenation of its segments, so syntax colour and change marks compose
// instead of fighting.
func splitOnMarks(segs []highlight.Segment, marks []diffx.Mark) []highlight.Segment {
	if len(marks) == 0 || len(segs) == 0 {
		return segs
	}
	out := make([]highlight.Segment, 0, len(segs)+len(marks)*2)
	offset := 0

	for _, seg := range segs {
		start, end := offset, offset+len(seg.T)
		offset = end
		cursor := start

		for _, m := range marks {
			if m.End <= cursor || m.Start >= end {
				continue
			}
			if m.Start > cursor {
				out = append(out, highlight.Segment{T: seg.T[cursor-start : m.Start-start], C: seg.C})
				cursor = m.Start
			}
			stop := min(m.End, end)
			out = append(out, highlight.Segment{T: seg.T[cursor-start : stop-start], C: seg.C, W: true})
			cursor = stop
		}
		if cursor < end {
			out = append(out, highlight.Segment{T: seg.T[cursor-start:], C: seg.C})
		}
	}
	return out
}

// fullFileView returns a whole file, optionally annotating which lines the
// change set added so the diff is still visible in full context.
func (s *Server) fullFileView(ctx context.Context, path string, fc *gitx.FileChange, contextLines int) (*FileView, error) {
	rev := s.Spec.NewRev
	status := "unchanged"
	if fc != nil {
		status = fc.Status
		// A deleted file has no new-side content; show it as it last existed.
		if fc.Status == "deleted" {
			rev = s.Spec.OldRev
			path = oldPathOf(fc)
		}
	}

	file, err := gitx.ReadFile(ctx, s.Repo, rev, path)
	if err != nil {
		return nil, err
	}

	view := &FileView{
		Path: path, Status: status, Lang: highlight.Detect(path), Mode: "full",
		Binary: file.Binary, Trunc: file.Trunc, Context: contextLines,
	}
	if fc != nil {
		view.OldPath, view.Adds, view.Dels = fc.OldPath, fc.Adds, fc.Dels
	}
	if file.Binary {
		return view, nil
	}

	segs := highlight.Lines(path, file.Lines)
	view.Lines = make([]LineView, len(file.Lines))
	for i, text := range file.Lines {
		view.Lines[i] = LineView{T: " ", Old: i + 1, New: i + 1, Segs: segsAt(segs, i+1, text)}
	}

	// Mark added lines so a full-file card still shows where the change is.
	if fc != nil && !fc.Binary && fc.Status != "deleted" {
		if hunks, err := gitx.Patch(ctx, s.Repo, s.Spec, fc, 0); err == nil {
			for _, h := range hunks {
				for _, l := range h.Lines {
					if l.T == "+" {
						view.Changed = append(view.Changed, l.New)
					}
				}
			}
		}
	}
	return view, nil
}

// highlightSide reads and highlights one side of the diff. Failures degrade to
// no highlighting rather than failing the request.
func (s *Server) highlightSide(ctx context.Context, rev, path, langPath string) [][]highlight.Segment {
	if path == "" {
		return nil
	}
	file, err := gitx.ReadFile(ctx, s.Repo, rev, path)
	if err != nil || file.Binary {
		return nil
	}
	return highlight.Lines(langPath, file.Lines)
}

// segsAt returns the highlighted segments for a 1-based line number, falling
// back to the raw diff text when the file could not be read. The fallback
// matters for the worktree spec, where a line may not exist on disk.
func segsAt(all [][]highlight.Segment, lineNo int, raw string) []highlight.Segment {
	// File contents are read with CRLF normalised away, but the diff line
	// keeps its carriage return. Comparing them raw fails on every line of a
	// Windows-line-ending repository, which then renders entirely unstyled.
	trimmed := strings.TrimSuffix(raw, "\r")
	if lineNo >= 1 && lineNo <= len(all) {
		segs := all[lineNo-1]
		if joined(segs) == trimmed {
			return segs
		}
	}
	if trimmed == "" {
		return []highlight.Segment{}
	}
	return []highlight.Segment{{T: trimmed}}
}

func joined(segs []highlight.Segment) string {
	if len(segs) == 1 {
		return segs[0].T
	}
	n := 0
	for _, s := range segs {
		n += len(s.T)
	}
	buf := make([]byte, 0, n)
	for _, s := range segs {
		buf = append(buf, s.T...)
	}
	return string(buf)
}

func oldPathOf(fc *gitx.FileChange) string {
	if fc.OldPath != "" {
		return fc.OldPath
	}
	return fc.Path
}

func (s *Server) handleTree(ctx context.Context, r *http.Request) (any, error) {
	paths, err := gitx.ListFiles(ctx, s.Repo, s.Spec.NewRev)
	if err != nil {
		return nil, err
	}
	if paths == nil {
		paths = []string{}
	}
	return map[string]any{"paths": paths}, nil
}

func (s *Server) handleGrep(ctx context.Context, r *http.Request) (any, error) {
	q := r.URL.Query()
	hits, err := gitx.Grep(ctx, s.Repo, s.Spec.NewRev, q.Get("q"), q.Get("i") == "1")
	if err != nil {
		return nil, err
	}
	return map[string]any{"hits": hits}, nil
}

// handleImports reports which of the supplied Go files import one another.
//
// The client sends the paths it currently has open, so the response is exactly
// the set of arrows that can be drawn between existing cards.
func (s *Server) handleImports(ctx context.Context, r *http.Request) (any, error) {
	paths := r.URL.Query()["p"]
	if len(paths) == 0 {
		return map[string]any{"edges": []goimports.Edge{}}, nil
	}
	if len(paths) > 500 {
		paths = paths[:500]
	}
	edges, err := goimports.Edges(ctx, s.Repo, s.Spec.NewRev, paths)
	if err != nil {
		return nil, err
	}
	return map[string]any{"edges": edges}, nil
}

// symbolIndex is built once, on the first jump-to-definition request. Parsing
// every Go file in the repository is too expensive to do at startup for a
// feature the reviewer may never use.
type symbolIndex struct {
	mu    sync.Mutex
	ready bool
	index *gosym.Index
}

func (s *Server) symbols(ctx context.Context) (*gosym.Index, error) {
	s.symbols_.mu.Lock()
	defer s.symbols_.mu.Unlock()
	if s.symbols_.ready {
		return s.symbols_.index, nil
	}
	// Background context: a cancelled request must not poison the cache.
	index, err := gosym.Build(context.Background(), s.Repo, s.Spec.NewRev)
	if err != nil {
		return nil, err
	}
	s.symbols_.index, s.symbols_.ready = index, true
	return index, nil
}

// handleDef resolves a clicked identifier to its declaration.
func (s *Server) handleDef(ctx context.Context, r *http.Request) (any, error) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	index, err := s.symbols(ctx)
	if err != nil {
		return nil, err
	}
	res, err := index.Lookup(name, q.Get("qual"), q.Get("from"))
	if err != nil {
		return nil, err
	}
	return res, nil
}
