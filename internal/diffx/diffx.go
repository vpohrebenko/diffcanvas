// Package diffx handles intra-line comparison: pairing a hunk's removed lines
// with the added lines that replaced them, and working out which words inside
// each pair actually changed.
//
// Without this a one-character edit paints two whole lines red and green, and
// the reader has to find the real change by eye.
package diffx

import "unicode/utf8"

// Mark is a byte range within a line that differs from its counterpart.
type Mark struct {
	Start, End int
}

// Pair links a removed line to the added line that replaced it. Either index
// may be -1, meaning the other side has no counterpart on that row.
type Pair struct {
	Old, New int
}

// Kind classifies a line for pairing purposes.
type Kind int

const (
	Context Kind = iota
	Removed
	Added
)

// PairLines arranges a hunk's lines into side-by-side rows.
//
// A run of removals followed immediately by a run of additions is one edit, so
// those lines belong on the same rows. Leftovers on either side get a blank
// cell opposite them.
func PairLines(kinds []Kind) []Pair {
	var pairs []Pair
	i := 0
	for i < len(kinds) {
		if kinds[i] == Context {
			pairs = append(pairs, Pair{Old: i, New: i})
			i++
			continue
		}
		start := i
		for i < len(kinds) && kinds[i] == Removed {
			i++
		}
		dels := makeRange(start, i)

		start = i
		for i < len(kinds) && kinds[i] == Added {
			i++
		}
		adds := makeRange(start, i)

		if len(dels) == 0 && len(adds) == 0 {
			i++ // defensive: never stall
			continue
		}
		for k := 0; k < max(len(dels), len(adds)); k++ {
			p := Pair{Old: -1, New: -1}
			if k < len(dels) {
				p.Old = dels[k]
			}
			if k < len(adds) {
				p.New = adds[k]
			}
			pairs = append(pairs, p)
		}
	}
	return pairs
}

func makeRange(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// maxTokens bounds the O(n*m) comparison. Machine-generated lines can be
// enormous, and a word diff of a 5000-token line helps nobody.
const maxTokens = 400

// minSimilarity is the fraction of shared tokens below which two lines are
// treated as unrelated. Marking almost every word adds noise rather than
// removing it, so below this the caller gets no marks at all.
const minSimilarity = 0.25

// Compare returns the byte ranges that differ between two lines.
//
// Returning no marks is meaningful: it says the lines are too dissimilar to
// usefully align, and the whole line should stay highlighted.
func Compare(oldLine, newLine string) (oldMarks, newMarks []Mark, ok bool) {
	if oldLine == newLine {
		return nil, nil, true
	}
	oldToks := tokenize(oldLine)
	newToks := tokenize(newLine)
	if len(oldToks) > maxTokens || len(newToks) > maxTokens {
		return nil, nil, false
	}

	common := lcs(oldToks, newToks)
	longest := max(len(oldToks), len(newToks))
	if longest == 0 {
		return nil, nil, true
	}
	if float64(len(common))/float64(longest) < minSimilarity {
		return nil, nil, false
	}

	return marksFrom(oldToks, common, func(p [2]int) int { return p[0] }),
		marksFrom(newToks, common, func(p [2]int) int { return p[1] }),
		true
}

type token struct {
	text       string
	start, end int
}

// tokenize splits a line into identifier runs, whitespace runs, and single
// punctuation characters. Grouping identifiers keeps the marks at a word
// granularity instead of flickering character by character.
func tokenize(s string) []token {
	var out []token
	i := 0
	for i < len(s) {
		start := i
		c := rune(s[i])
		switch {
		case isWordByte(s[i]):
			for i < len(s) && isWordByte(s[i]) {
				i++
			}
		case c == ' ' || c == '\t':
			for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
		default:
			// Advance a whole rune so multi-byte characters stay intact.
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
		}
		out = append(out, token{text: s[start:i], start: start, end: i})
	}
	return out
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' ||
		b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// lcs returns the indices of the longest common subsequence, as (old, new)
// index pairs.
func lcs(a, b []token) [][2]int {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return nil
	}
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i].text == b[j].text {
				table[i][j] = table[i+1][j+1] + 1
			} else {
				table[i][j] = max(table[i+1][j], table[i][j+1])
			}
		}
	}

	var out [][2]int
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i].text == b[j].text:
			out = append(out, [2]int{i, j})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}

// marksFrom turns "these tokens are common" into "these byte ranges changed",
// merging adjacent runs so the result is a small number of spans.
func marksFrom(toks []token, common [][2]int, pick func([2]int) int) []Mark {
	keep := make(map[int]bool, len(common))
	for _, p := range common {
		keep[pick(p)] = true
	}

	var marks []Mark
	for i := 0; i < len(toks); i++ {
		if keep[i] {
			continue
		}
		start := toks[i].start
		end := toks[i].end
		for i+1 < len(toks) && !keep[i+1] {
			i++
			end = toks[i].end
		}
		if n := len(marks); n > 0 && marks[n-1].End == start {
			marks[n-1].End = end
			continue
		}
		marks = append(marks, Mark{Start: start, End: end})
	}
	return trimEdges(marks, toks)
}

// trimEdges pulls whitespace off the ends of each mark.
//
// Two alignments can both be minimal while differing in whether the space
// before or after an inserted phrase is counted as changed. Trimming makes the
// result independent of that choice, and highlighting a lone trailing space
// looks like a mistake to the reader.
func trimEdges(marks []Mark, toks []token) []Mark {
	if len(toks) == 0 {
		return marks
	}
	line := lineOf(toks)

	out := marks[:0]
	for _, m := range marks {
		for m.Start < m.End && isSpace(line[m.Start]) {
			m.Start++
		}
		for m.End > m.Start && isSpace(line[m.End-1]) {
			m.End--
		}
		if m.End > m.Start {
			out = append(out, m)
		}
	}
	return out
}

// lineOf reconstructs the original line from its tokens, which tile it exactly.
func lineOf(toks []token) string {
	if len(toks) == 0 {
		return ""
	}
	buf := make([]byte, 0, toks[len(toks)-1].end)
	for _, t := range toks {
		buf = append(buf, t.text...)
	}
	return string(buf)
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }
