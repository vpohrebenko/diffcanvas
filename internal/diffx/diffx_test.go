package diffx

import "testing"

// apply renders the marked ranges as [brackets] so expectations read as the
// thing a human would see.
func apply(line string, marks []Mark) string {
	out := ""
	prev := 0
	for _, m := range marks {
		out += line[prev:m.Start] + "[" + line[m.Start:m.End] + "]"
		prev = m.End
	}
	return out + line[prev:]
}

func TestCompareMarksOnlyTheChange(t *testing.T) {
	cases := []struct {
		name             string
		oldLine, newLine string
		wantOld, wantNew string
	}{
		{
			name:    "single argument changed",
			oldLine: `fmt.Println("hello", a)`,
			newLine: `fmt.Println("hello", b)`,
			wantOld: `fmt.Println("hello", [a])`,
			wantNew: `fmt.Println("hello", [b])`,
		},
		{
			name:    "one identifier renamed",
			oldLine: "	result := computeValue(x)",
			newLine: "	result := computeTotal(x)",
			wantOld: "	result := [computeValue](x)",
			wantNew: "	result := [computeTotal](x)",
		},
		{
			name:    "insertion only",
			oldLine: "if err != nil {",
			newLine: "if err != nil && retry {",
			wantOld: "if err != nil {",
			wantNew: "if err != nil [&& retry] {",
		},
		{
			name:    "trailing removal",
			oldLine: "call(a, b, c)",
			newLine: "call(a, b)",
			wantOld: "call(a, b[, c])",
			wantNew: "call(a, b)",
		},
		{
			// Marks must never begin or end on whitespace.
			name:    "no whitespace-edged marks",
			oldLine: "x := foo(1)",
			newLine: "x := bar baz(1)",
			wantOld: "x := [foo](1)",
			wantNew: "x := [bar baz](1)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldMarks, newMarks, ok := Compare(tc.oldLine, tc.newLine)
			if !ok {
				t.Fatal("Compare reported the lines as unrelated")
			}
			if got := apply(tc.oldLine, oldMarks); got != tc.wantOld {
				t.Errorf("old:\n got %q\nwant %q", got, tc.wantOld)
			}
			if got := apply(tc.newLine, newMarks); got != tc.wantNew {
				t.Errorf("new:\n got %q\nwant %q", got, tc.wantNew)
			}
		})
	}
}

// TestUnrelatedLinesGetNoMarks: when two lines share almost nothing, marking
// nearly every word is worse than marking none, so Compare declines.
func TestUnrelatedLinesGetNoMarks(t *testing.T) {
	_, _, ok := Compare(
		"func handleRequest(w http.ResponseWriter, r *http.Request) {",
		"var tableConfiguration = map[string]int{}",
	)
	if ok {
		t.Error("wholly different lines should not be word-diffed")
	}
}

func TestIdenticalLines(t *testing.T) {
	oldMarks, newMarks, ok := Compare("same", "same")
	if !ok || len(oldMarks) != 0 || len(newMarks) != 0 {
		t.Errorf("identical lines produced marks: %v %v", oldMarks, newMarks)
	}
}

func TestNonASCIIBoundaries(t *testing.T) {
	oldLine := `label := "naïve café 日本"`
	newLine := `label := "naïve tea 日本"`
	oldMarks, newMarks, ok := Compare(oldLine, newLine)
	if !ok {
		t.Fatal("should be comparable")
	}
	// Marks must land on rune boundaries, or slicing corrupts the text.
	for _, m := range oldMarks {
		if !utf8Boundary(oldLine, m.Start) || !utf8Boundary(oldLine, m.End) {
			t.Errorf("old mark %v splits a rune in %q", m, oldLine)
		}
	}
	for _, m := range newMarks {
		if !utf8Boundary(newLine, m.Start) || !utf8Boundary(newLine, m.End) {
			t.Errorf("new mark %v splits a rune in %q", m, newLine)
		}
	}
	if got := apply(newLine, newMarks); got != `label := "naïve [tea] 日本"` {
		t.Errorf("got %q", got)
	}
}

func utf8Boundary(s string, i int) bool {
	return i == 0 || i == len(s) || s[i]&0xC0 != 0x80
}

func TestHugeLinesAreSkipped(t *testing.T) {
	long := ""
	for i := 0; i < maxTokens+50; i++ {
		long += "tok "
	}
	if _, _, ok := Compare(long, long+"x"); ok {
		t.Error("lines beyond the token cap should be skipped")
	}
}

func TestPairLines(t *testing.T) {
	// context, del, del, add, context  ->  pairs del/add, then del/blank
	kinds := []Kind{Context, Removed, Removed, Added, Context}
	got := PairLines(kinds)
	want := []Pair{{0, 0}, {1, 3}, {2, -1}, {4, 4}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPairLinesPureInsertion(t *testing.T) {
	got := PairLines([]Kind{Added, Added})
	want := []Pair{{-1, 0}, {-1, 1}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPairLinesTerminates(t *testing.T) {
	// Every ordering must terminate and account for every line exactly once.
	orders := [][]Kind{
		{Added, Removed},
		{Added, Removed, Added, Removed},
		{Removed}, {Added}, {Context},
		{Added, Context, Removed},
	}
	for _, kinds := range orders {
		pairs := PairLines(kinds)
		seen := map[int]int{}
		for _, p := range pairs {
			if p.Old >= 0 {
				seen[p.Old]++
			}
			if p.New >= 0 && p.New != p.Old {
				seen[p.New]++
			}
		}
		for i := range kinds {
			if seen[i] != 1 {
				t.Errorf("kinds %v: line %d appears %d times", kinds, i, seen[i])
			}
		}
	}
}
