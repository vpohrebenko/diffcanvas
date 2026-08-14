// Package highlight turns source text into per-line coloured segments.
//
// Segments carry their own text rather than offsets into the line. Offsets
// would have to survive Go's byte indexing meeting JavaScript's UTF-16 string
// indexing, which silently corrupts any line containing non-ASCII characters.
// Shipping the pieces themselves removes that entire class of bug, at the cost
// of a slightly larger payload — an easy trade when files load lazily.
package highlight

import (
	"path/filepath"
	"strings"
)

// Segment is a run of text sharing one class. An empty Class means ordinary
// text and is omitted from the JSON.
type Segment struct {
	T string `json:"t"`
	C string `json:"c,omitempty"`
	// W marks a run that changed relative to its counterpart line, set by the
	// word-level diff. Orthogonal to C: a run can be both a keyword and changed.
	W bool `json:"w,omitempty"`
}

// Classes emitted by the highlighters.
const (
	ClassKeyword = "kw"  // language keywords
	ClassString  = "str" // string and rune literals
	ClassNumber  = "num" // numeric literals
	ClassComment = "com" // comments
	ClassType    = "typ" // predeclared types
	ClassBuiltin = "bui" // predeclared functions and constants
)

// span is an internal, offset-based marker over the joined source.
type span struct {
	start, end int
	class      string
}

// Lines highlights a whole file and returns one segment list per input line.
//
// Highlighting runs over the entire buffer rather than line by line, because a
// raw string or block comment can span many lines. A per-line highlighter
// cannot see that context and mis-colours everything after the opening token.
func Lines(path string, lines []string) [][]Segment {
	if len(lines) == 0 {
		return nil
	}
	src := strings.Join(lines, "\n")

	var spans []span
	if isGo(path) {
		spans = scanGo(src)
	} else if lang, ok := lookupLang(path); ok {
		spans = scanGeneric(src, lang)
	} else {
		return plain(lines)
	}
	return segmentize(src, lines, spans)
}

func isGo(path string) bool { return strings.HasSuffix(path, ".go") }

// plain returns the lines with no highlighting at all.
func plain(lines []string) [][]Segment {
	out := make([][]Segment, len(lines))
	for i, l := range lines {
		if l == "" {
			out[i] = []Segment{}
			continue
		}
		out[i] = []Segment{{T: l}}
	}
	return out
}

// segmentize slices the source into per-line segments according to spans.
//
// Spans must be sorted and non-overlapping; both scanners guarantee that.
// Any gap between spans becomes an unclassified segment, so the concatenated
// text of every line is always byte-identical to the input.
func segmentize(src string, lines []string, spans []span) [][]Segment {
	out := make([][]Segment, len(lines))

	lineStart := 0
	si := 0
	for i, line := range lines {
		lineEnd := lineStart + len(line)
		segs := []Segment{}
		cursor := lineStart

		// Rewind past spans that ended before this line began.
		for si < len(spans) && spans[si].end <= lineStart {
			si++
		}

		for j := si; j < len(spans) && spans[j].start < lineEnd; j++ {
			s := spans[j]
			if s.end <= cursor {
				continue
			}
			if s.start > cursor {
				segs = append(segs, Segment{T: src[cursor:min(s.start, lineEnd)]})
				cursor = min(s.start, lineEnd)
			}
			stop := min(s.end, lineEnd)
			if stop > cursor {
				segs = append(segs, Segment{T: src[cursor:stop], C: s.class})
				cursor = stop
			}
		}
		if cursor < lineEnd {
			segs = append(segs, Segment{T: src[cursor:lineEnd]})
		}

		out[i] = segs
		lineStart = lineEnd + 1 // step over the newline
	}
	return out
}

// Detect reports a coarse language name for a path, used by the UI for its
// per-card label.
func Detect(path string) string {
	if isGo(path) {
		return "go"
	}
	if lang, ok := lookupLang(path); ok {
		return lang.name
	}
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return "text"
	}
	return ext
}
