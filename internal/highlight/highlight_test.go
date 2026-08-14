package highlight

import (
	"strings"
	"testing"
)

// classOf returns the class covering the first occurrence of needle in the
// line, or "" if the needle is unstyled or absent.
func classOf(segs []Segment, needle string) string {
	for _, s := range segs {
		if strings.Contains(s.T, needle) {
			return s.C
		}
	}
	return ""
}

// checkLossless asserts the invariant the whole design rests on: rejoining the
// segments must reproduce the input exactly. If this holds, the browser cannot
// render corrupted source no matter how the scanner classifies things.
func checkLossless(t *testing.T, path string, lines []string) [][]Segment {
	t.Helper()
	got := Lines(path, lines)
	if len(got) != len(lines) {
		t.Fatalf("%s: got %d segment lines, want %d", path, len(got), len(lines))
	}
	for i, segs := range got {
		var sb strings.Builder
		for _, s := range segs {
			sb.WriteString(s.T)
		}
		if sb.String() != lines[i] {
			t.Errorf("%s line %d: reassembled %q, want %q", path, i+1, sb.String(), lines[i])
		}
	}
	return got
}

// TestGoTrickyLiterals covers exactly the constructs a regex highlighter gets
// wrong. Each one, mishandled, bleeds colour across the rest of the file.
func TestGoTrickyLiterals(t *testing.T) {
	lines := []string{
		"package main",
		"",
		"// a comment with \"quotes\" and /* block */ inside",
		"var raw = `a raw string",
		"spanning lines with \"quotes\" and // a fake comment",
		"and a } brace`",
		"var s = \"escaped \\\" quote\"",
		"var r = '\\''",
		"/* block comment",
		"   containing func and \"strings\"",
		"*/",
		"func Ünïcödé(x int) error { return nil }",
	}
	got := checkLossless(t, "x.go", lines)

	if c := classOf(got[2], "a comment"); c != ClassComment {
		t.Errorf("line 3 comment classified %q", c)
	}
	// The middle of the raw string must still be a string, not a comment.
	if c := classOf(got[4], "fake comment"); c != ClassString {
		t.Errorf("raw string continuation classified %q, want %q", c, ClassString)
	}
	if c := classOf(got[5], "brace"); c != ClassString {
		t.Errorf("raw string close classified %q, want %q", c, ClassString)
	}
	// Code after the block comment closes must recover.
	if c := classOf(got[9], "containing func"); c != ClassComment {
		t.Errorf("block comment interior classified %q", c)
	}
	if c := classOf(got[11], "func"); c != ClassKeyword {
		t.Errorf("func after block comment classified %q, want keyword", c)
	}
	if c := classOf(got[11], "int"); c != ClassType {
		t.Errorf("int classified %q, want %q", c, ClassType)
	}
	if c := classOf(got[11], "nil"); c != ClassBuiltin {
		t.Errorf("nil classified %q, want %q", c, ClassBuiltin)
	}
}

// TestNonASCIIAlignment is the regression guard for the byte-vs-UTF-16 problem
// that motivated shipping segment text instead of offsets.
func TestNonASCIIAlignment(t *testing.T) {
	lines := []string{
		"// 日本語のコメント",
		"var emoji = \"🎉 party\"",
		"func Ünïcödé() {}",
		"var s = \"naïve café\" // trailing",
	}
	got := checkLossless(t, "u.go", lines)

	if c := classOf(got[1], "party"); c != ClassString {
		t.Errorf("string after emoji classified %q", c)
	}
	if c := classOf(got[3], "trailing"); c != ClassComment {
		t.Errorf("comment after accented string classified %q", c)
	}
}

// TestGoBrokenSyntax: a diff may contain a file that does not compile, and
// best-effort colouring must still be produced rather than nothing.
func TestGoBrokenSyntax(t *testing.T) {
	lines := []string{
		"package main",
		"func broken( {{{ ]]] unclosed",
		"var x = \"unterminated",
		"func After() {}",
	}
	got := checkLossless(t, "b.go", lines)
	if c := classOf(got[3], "func"); c != ClassKeyword {
		t.Errorf("keyword after syntax error classified %q", c)
	}
}

func TestGenericLanguages(t *testing.T) {
	cases := []struct {
		path   string
		lines  []string
		line   int
		needle string
		want   string
	}{
		{"a.java", []string{"public class Foo {"}, 0, "public", ClassKeyword},
		{"a.java", []string{"/* multi", "line */ int x;"}, 0, "multi", ClassComment},
		{"a.py", []string{"def f(): # note"}, 0, "def", ClassKeyword},
		{"a.py", []string{"def f(): # note"}, 0, "# note", ClassComment},
		{"a.yml", []string{"key: value # note"}, 0, "# note", ClassComment},
		{"a.json", []string{`{"k": true, "n": 42}`}, 0, "true", ClassKeyword},
		{"a.json", []string{`{"k": true, "n": 42}`}, 0, "42", ClassNumber},
		{"a.sh", []string{"if [ -f x ]; then # go"}, 0, "if", ClassKeyword},
		{"Makefile", []string{"ifeq ($(A),b) # c"}, 0, "ifeq", ClassKeyword},
		{"Dockerfile", []string{"FROM alpine:3 # base"}, 0, "FROM", ClassKeyword},
		{"a.sql", []string{"SELECT * FROM t -- note"}, 0, "-- note", ClassComment},
		{"a.proto", []string{"message Foo { string a = 1; }"}, 0, "message", ClassKeyword},
	}
	for _, tc := range cases {
		got := checkLossless(t, tc.path, tc.lines)
		if c := classOf(got[tc.line], tc.needle); c != tc.want {
			t.Errorf("%s: %q classified %q, want %q", tc.path, tc.needle, c, tc.want)
		}
	}
}

// TestUnterminatedStringDoesNotSwallowFile: one stray quote in a config file
// must not colour every following line as a string.
func TestUnterminatedStringDoesNotSwallowFile(t *testing.T) {
	lines := []string{
		`name: "oops`,
		`other: value`,
		`third: # comment`,
	}
	got := checkLossless(t, "a.yml", lines)
	if c := classOf(got[2], "# comment"); c != ClassComment {
		t.Errorf("comment after unterminated string classified %q, want comment", c)
	}
}

func TestUnknownExtensionIsPlain(t *testing.T) {
	lines := []string{"some arbitrary ~~~ content", ""}
	got := checkLossless(t, "notes.xyz", lines)
	for _, s := range got[0] {
		if s.C != "" {
			t.Errorf("unknown extension produced class %q", s.C)
		}
	}
}

func TestEmptyAndBlankLines(t *testing.T) {
	checkLossless(t, "a.go", []string{})
	checkLossless(t, "a.go", []string{"", "", ""})
	checkLossless(t, "a.go", []string{"package a", "", "func F() {}", ""})
}

func TestDetect(t *testing.T) {
	for path, want := range map[string]string{
		"a.go": "go", "a.java": "java", "Makefile": "make",
		"Dockerfile": "dockerfile", "a.yml": "yaml", "weird.xyz": "xyz",
		"noext": "text",
	} {
		if got := Detect(path); got != want {
			t.Errorf("Detect(%q) = %q, want %q", path, got, want)
		}
	}
}
