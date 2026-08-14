package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The frontend has no build step and no unit tests, so a module that throws on
// load blanks the page and nothing notices until a human looks at it. That has
// happened here more than once.
//
// This drives the real page in a headless browser and asserts two things: a
// diff row actually rendered, and no script failed. It is the screenshot loop
// without the eyeballs. Skipped when no browser is installed, so `go test ./...`
// stays green on a machine that has none.

func findBrowser() string {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// dumpDOM loads a URL and returns the rendered DOM.
func dumpDOM(t *testing.T, browser, url string) string {
	t.Helper()
	profile := t.TempDir()
	cmd := exec.Command(browser,
		"--headless", "--disable-gpu", "--no-sandbox", "--hide-scrollbars",
		"--user-data-dir="+profile,
		"--virtual-time-budget=12000",
		"--dump-dom", url,
	)
	// A wedged browser must fail the test rather than hang the suite.
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("browser did not finish within 90s")
	}
	if err != nil {
		t.Fatalf("browser failed: %v", err)
	}
	return string(out)
}

// seedLayout puts a card on the canvas without driving a mouse, so the page
// under test has real content to render.
func (h *harness) seedLayout(t *testing.T, path string) {
	t.Helper()
	layout := map[string]any{
		"v": 1, "pan": map[string]int{"x": 20, "y": 20}, "scale": 1,
		"diffMode": "unified", "lodStyle": "text", "fontScale": 1,
		"viewed": []string{}, "treeCollapsed": []string{}, "arrows": []any{},
		"cards": []map[string]any{{
			"path": path, "x": 40, "y": 40, "w": 700, "h": 400,
			"collapsed": false, "context": 3, "view": "auto", "fontScale": 0, "id": 1,
		}},
	}
	body, err := json.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, h.ts.URL+"/api/layout", strings.NewReader(string(body)))
	req.Header.Set("X-Diffcanvas-Token", h.srv.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("seeding the layout: status %d", res.StatusCode)
	}
}

func TestPageRendersWithoutScriptErrors(t *testing.T) {
	browser := findBrowser()
	if browser == "" {
		t.Skip("no chromium/chrome on PATH")
	}
	if os.Getenv("CI") != "" && os.Getenv("DIFFCANVAS_BROWSER_TEST") == "" {
		t.Skip("browser test disabled on CI; set DIFFCANVAS_BROWSER_TEST=1 to run")
	}

	h := newHarness(t, "HEAD")
	h.seedLayout(t, "main.go")

	dom := dumpDOM(t, browser, fmt.Sprintf("%s/?t=%s", h.ts.URL, h.srv.Token))

	// A script that failed says so in the document; the console is not
	// somewhere a test can look.
	if strings.Contains(dom, `id="dc-error"`) {
		start := strings.Index(dom, `id="dc-error"`)
		end := start + 300
		if end > len(dom) {
			end = len(dom)
		}
		t.Fatalf("the page reported a script failure:\n%s", dom[start:end])
	}

	// The bootstrap ran: the spec label is filled in from /api/meta.
	if strings.Contains(dom, ">loading…<") {
		t.Error("the page never got past loading; /api/meta likely failed")
	}

	// The sidebar built a tree from the change list.
	if !strings.Contains(dom, "node file") {
		t.Error("no file rows in the sidebar; the change tree did not render")
	}

	// The seeded card rendered actual diff content, not an empty shell.
	for _, want := range []string{"card-head", "row add", "tok-kw"} {
		if !strings.Contains(dom, want) {
			t.Errorf("rendered DOM has no %q; the card did not draw its diff", want)
		}
	}

	// Row virtualisation puts a pixel height on the spacer; a zero here means
	// the row model never built.
	if !strings.Contains(dom, "spacer") {
		t.Error("no row spacer; virtualisation did not run")
	}
}

// TestPageSurvivesAnEmptyLayout: opening with nothing saved must still produce
// a working page, which is the first thing anyone sees.
func TestPageSurvivesAnEmptyLayout(t *testing.T) {
	browser := findBrowser()
	if browser == "" {
		t.Skip("no chromium/chrome on PATH")
	}
	if os.Getenv("CI") != "" && os.Getenv("DIFFCANVAS_BROWSER_TEST") == "" {
		t.Skip("browser test disabled on CI; set DIFFCANVAS_BROWSER_TEST=1 to run")
	}

	h := newHarness(t, "HEAD")
	dom := dumpDOM(t, browser, fmt.Sprintf("%s/?t=%s", h.ts.URL, h.srv.Token))

	if strings.Contains(dom, `id="dc-error"`) {
		t.Fatal("script failure on a fresh canvas")
	}
	if !strings.Contains(dom, "empty-state") {
		t.Error("no empty state shown with no cards open")
	}
	if !strings.Contains(dom, "node file") {
		t.Error("the sidebar did not render without a saved layout")
	}
}
