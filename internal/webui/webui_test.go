package webui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The frontend has no build step, so nothing checks that the JavaScript and
// the markup still agree. A renamed id, a moved module or a class with no rule
// throws at load time and blanks the whole page — a failure that has shipped
// here more than once and was only caught by looking at a screenshot.
//
// These tests are the cheap half of that safety net: they need no browser and
// run in `go test ./...`.

func assets(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(FS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		out[path.Base(p)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 5 {
		t.Fatalf("only %d embedded assets; the embed pattern is probably wrong", len(out))
	}
	return out
}

func jsFiles(all map[string]string) map[string]string {
	out := map[string]string{}
	for name, body := range all {
		if strings.HasSuffix(name, ".js") {
			out[name] = body
		}
	}
	return out
}

var (
	reGetByID = regexp.MustCompile(`getElementById\(\s*['"]([^'"]+)['"]\s*\)`)
	reImport  = regexp.MustCompile(`from\s+['"]\./([^'"]+)['"]`)
	reIDAttr  = regexp.MustCompile(`id="([^"]+)"`)
)

// createdInJS are elements the scripts build themselves, so they are
// legitimately absent from index.html.
var createdInJS = map[string]bool{
	"def-picker": true, // the jump-to-definition candidate list
	"dc-error":   true, // only exists once a script has failed
}

func TestEveryReferencedElementIDExists(t *testing.T) {
	all := assets(t)
	html, ok := all["index.html"]
	if !ok {
		t.Fatal("index.html is not embedded")
	}

	declared := map[string]bool{}
	for _, m := range reIDAttr.FindAllStringSubmatch(html, -1) {
		declared[m[1]] = true
	}
	if len(declared) < 10 {
		t.Fatalf("found only %d ids in index.html; the regexp is probably wrong", len(declared))
	}

	for name, body := range jsFiles(all) {
		for _, m := range reGetByID.FindAllStringSubmatch(body, -1) {
			id := m[1]
			if declared[id] || createdInJS[id] {
				continue
			}
			t.Errorf("%s: getElementById(%q) but no id=%q in index.html "+
				"(a rename here throws on load and blanks the page)", name, id, id)
		}
	}
}

func TestEveryModuleImportResolves(t *testing.T) {
	all := assets(t)
	for name, body := range jsFiles(all) {
		for _, m := range reImport.FindAllStringSubmatch(body, -1) {
			if _, ok := all[m[1]]; !ok {
				t.Errorf("%s imports ./%s, which is not embedded", name, m[1])
			}
		}
	}
}

// TestScriptTagsResolve: index.html loads modules by path, and a stale <script>
// src fails silently in the console rather than anywhere a Go test would see.
func TestScriptTagsResolve(t *testing.T) {
	all := assets(t)
	re := regexp.MustCompile(`src="/static/([^"]+)"|href="/static/([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(all["index.html"], -1) {
		ref := m[1]
		if ref == "" {
			ref = m[2]
		}
		if _, ok := all[ref]; !ok {
			t.Errorf("index.html references /static/%s, which is not embedded", ref)
		}
	}
}

// TestSyntaxClassesHaveRules pairs the classes the highlighter can emit with
// the stylesheet. A token class with no rule renders as unstyled text, which
// looks like the highlighter silently failing.
func TestSyntaxClassesHaveRules(t *testing.T) {
	all := assets(t)
	css := all["style.css"]

	// These are the classes internal/highlight can produce.
	for _, class := range []string{"kw", "str", "num", "com", "typ", "bui"} {
		if !strings.Contains(css, ".tok-"+class) {
			t.Errorf("highlighter emits tok-%s but style.css has no rule for it", class)
		}
	}
	// And the two non-syntax marks the diff adds.
	for _, class := range []string{".w", ".nonl"} {
		if !strings.Contains(css, class) {
			t.Errorf("style.css has no rule for %s", class)
		}
	}
}

// TestRowHeightAgrees guards the one number row virtualisation depends on:
// store.js computes it, style.css carries a default, and if the default is
// wildly different the first paint before JS runs is visibly wrong.
func TestRowHeightAgrees(t *testing.T) {
	all := assets(t)
	js := regexp.MustCompile(`BASE_ROW_H\s*=\s*(\d+)`).FindStringSubmatch(all["store.js"])
	css := regexp.MustCompile(`--row-h:\s*(\d+)px`).FindStringSubmatch(all["style.css"])
	if js == nil || css == nil {
		t.Fatalf("could not find BASE_ROW_H (%v) or --row-h (%v)", js != nil, css != nil)
	}
	if js[1] != css[1] {
		t.Errorf("BASE_ROW_H is %s but --row-h defaults to %spx", js[1], css[1])
	}
}

// TestNoInlineScriptOrHandlers: the Content-Security-Policy forbids both, and
// either one fails only in the browser console.
func TestNoInlineScriptOrHandlers(t *testing.T) {
	html := assets(t)["index.html"]
	if regexp.MustCompile(`<script[^>]*>\s*[^<\s]`).MatchString(html) {
		t.Error("index.html contains an inline script; the CSP blocks it")
	}
	if m := regexp.MustCompile(`\son[a-z]+="`).FindString(html); m != "" {
		t.Errorf("index.html has an inline event handler (%q); the CSP blocks it", strings.TrimSpace(m))
	}
}

// TestTokenPlaceholderPresent: the server substitutes this one string, and
// losing it means the page loads with no way to authenticate.
func TestTokenPlaceholderPresent(t *testing.T) {
	if !strings.Contains(assets(t)["index.html"], "__TOKEN__") {
		t.Error("index.html has no __TOKEN__ placeholder for the server to fill")
	}
}
