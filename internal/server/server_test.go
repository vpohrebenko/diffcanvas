package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
)

type harness struct {
	srv *Server
	ts  *httptest.Server
	dir string
}

func newHarness(t *testing.T, revspec string) *harness {
	t.Helper()
	// Keep layout writes inside the test's temp area.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Symlinks resolved: on macOS a temp dir is /var/folders/... pointing at
	// /private/var/..., and git reports the resolved path while the test holds
	// the other one.
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@e")
	write("main.go", "package main\n\nfunc main() {\n\tprintln(\"one\")\n}\n")
	run("add", "-A")
	run("commit", "-qm", "first")
	write("main.go", "package main\n\nfunc main() {\n\tprintln(\"two\")\n\tprintln(\"three\")\n}\n")
	write("notes.md", "# notes\n")
	run("add", "-A")
	run("commit", "-qm", "second")

	ctx := context.Background()
	repo, err := gitx.Discover(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := gitx.ResolveSpec(ctx, repo, revspec)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(repo, spec)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.security(srv.mux))
	t.Cleanup(ts.Close)
	return &harness{srv: srv, ts: ts, dir: dir}
}

// do issues a request with the token in a header, the way the page's JavaScript does.
func (h *harness) do(t *testing.T, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Diffcanvas-Token", h.srv.Token)
	res, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *harness) getJSON(t *testing.T, path string, into any) {
	t.Helper()
	res := h.do(t, http.MethodGet, path)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		t.Fatalf("GET %s: decoding: %v", path, err)
	}
}

// TestAuthSplit pins down the asymmetry between asset and data requests. It is
// the regression guard for two opposite failures: assets 403ing because the
// browser cannot set a header, and repository data being reachable with only a
// cookie, which another local origin could supply.
func TestAuthSplit(t *testing.T) {
	h := newHarness(t, "HEAD")
	client := h.ts.Client()

	cases := []struct {
		name string
		path string
		set  func(*http.Request)
		want int
	}{
		{"api with header", "/api/meta", func(r *http.Request) {
			r.Header.Set("X-Diffcanvas-Token", h.srv.Token)
		}, http.StatusOK},
		{"api with cookie only", "/api/meta", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: tokenCookie, Value: h.srv.Token})
		}, http.StatusForbidden},
		{"api with query only", "/api/meta?t=" + h.srv.Token, func(*http.Request) {}, http.StatusForbidden},
		{"api bare", "/api/meta", func(*http.Request) {}, http.StatusForbidden},
		{"asset with cookie", "/static/app.js", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: tokenCookie, Value: h.srv.Token})
		}, http.StatusOK},
		{"asset bare", "/static/app.js", func(*http.Request) {}, http.StatusForbidden},
		{"asset with wrong cookie", "/static/app.js", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: tokenCookie, Value: "nope"})
		}, http.StatusForbidden},
		{"index with query", "/?t=" + h.srv.Token, func(*http.Request) {}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.ts.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			tc.set(req)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.name, tc.path, res.StatusCode, tc.want)
			}
		})
	}
}

// TestRebindHostRejected covers the DNS rebinding case: a hostile page resolves
// its own name to 127.0.0.1 and reaches us with that name in the Host header.
func TestRebindHostRejected(t *testing.T) {
	h := newHarness(t, "HEAD")
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/meta", nil)
	req.Header.Set("X-Diffcanvas-Token", h.srv.Token)
	req.Host = "attacker.example.com"

	res, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("non-loopback Host got %d, want 403", res.StatusCode)
	}
}

func TestIndexSetsCookieAndToken(t *testing.T) {
	h := newHarness(t, "HEAD")
	res := h.do(t, http.MethodGet, "/?t="+h.srv.Token)
	defer res.Body.Close()

	var found bool
	for _, c := range res.Cookies() {
		if c.Name == tokenCookie && c.Value == h.srv.Token {
			found = true
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Error("token cookie should be HttpOnly and SameSite=Strict")
			}
		}
	}
	if !found {
		t.Error("index did not set the token cookie")
	}

	body := make([]byte, 4096)
	n, _ := res.Body.Read(body)
	page := string(body[:n])
	if strings.Contains(page, "__TOKEN__") {
		t.Error("index still contains the token placeholder")
	}
	if !strings.Contains(page, h.srv.Token) {
		t.Error("index does not carry the token")
	}
}

func TestChangesAndFile(t *testing.T) {
	h := newHarness(t, "HEAD")

	var meta struct {
		Files int `json:"files"`
		Adds  int `json:"adds"`
		Dels  int `json:"dels"`
	}
	h.getJSON(t, "/api/meta", &meta)
	if meta.Files != 2 {
		t.Errorf("meta files = %d, want 2", meta.Files)
	}

	var changes struct {
		Changes []gitx.FileChange `json:"changes"`
	}
	h.getJSON(t, "/api/changes", &changes)
	if len(changes.Changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(changes.Changes))
	}

	var view FileView
	h.getJSON(t, "/api/file?path=main.go&mode=diff", &view)
	if view.Lang != "go" {
		t.Errorf("lang = %q, want go", view.Lang)
	}
	if len(view.Hunks) == 0 {
		t.Fatal("no hunks for main.go")
	}

	// Go highlighting must have reached the diff rows.
	var sawKeyword bool
	for _, hunk := range view.Hunks {
		for _, line := range hunk.Lines {
			for _, seg := range line.Segs {
				if seg.C == "kw" {
					sawKeyword = true
				}
			}
		}
	}
	if !sawKeyword {
		t.Error("diff rows carry no keyword highlighting")
	}

	// Full mode must return the whole file and mark the added lines.
	var full FileView
	h.getJSON(t, "/api/file?path=main.go&mode=full", &full)
	if full.Mode != "full" || len(full.Lines) != 6 {
		t.Errorf("full view: mode=%q lines=%d, want full/6", full.Mode, len(full.Lines))
	}
	if len(full.Changed) == 0 {
		t.Error("full view did not mark any changed lines")
	}
}

// TestWordMarksNarrowTheChange is the payoff test for the word diff: on a
// line where one literal changed, only that literal may be flagged.
func TestWordMarksNarrowTheChange(t *testing.T) {
	h := newHarness(t, "HEAD")
	var view FileView
	h.getJSON(t, "/api/file?path=main.go&mode=diff", &view)

	changed := map[string]string{} // line type -> marked text
	for _, hunk := range view.Hunks {
		for _, line := range hunk.Lines {
			var marked, whole string
			for _, seg := range line.Segs {
				whole += seg.T
				if seg.W {
					marked += seg.T
				}
			}
			if marked != "" {
				changed[line.T] = marked
				if marked == whole {
					t.Errorf("%s line marked in full (%q); the word diff added nothing", line.T, whole)
				}
			}
		}
	}

	if changed["-"] != "one" {
		t.Errorf(`removed line marked %q, want "one"`, changed["-"])
	}
	if changed["+"] != "two" {
		t.Errorf(`added line marked %q, want "two"`, changed["+"])
	}
}

// TestPairsCoverEveryLine guards the contract the split view depends on:
// every line appears in exactly one pair.
func TestPairsCoverEveryLine(t *testing.T) {
	h := newHarness(t, "HEAD")
	var view FileView
	h.getJSON(t, "/api/file?path=main.go&mode=diff", &view)

	for hi, hunk := range view.Hunks {
		seen := map[int]int{}
		for _, p := range hunk.Pairs {
			if p[0] >= 0 {
				seen[p[0]]++
			}
			if p[1] >= 0 && p[1] != p[0] {
				seen[p[1]]++
			}
		}
		for i := range hunk.Lines {
			if seen[i] != 1 {
				t.Errorf("hunk %d line %d appears in %d pairs, want 1", hi, i, seen[i])
			}
		}
	}
}

// TestHostilePathsRejected covers every shape of path that must never reach
// the filesystem or git's revision parser.
//
// The colon and slash cases matter because `<rev>:<path>` is a revision
// expression: `:/text` means "youngest commit whose message matches", which
// otherwise returns a whole commit rendered as if it were a file.
func TestHostilePathsRejected(t *testing.T) {
	h := newHarness(t, "HEAD")
	for _, path := range []string{
		"../../../../etc/passwd",
		"../outside",
		"a/../../etc/passwd",
		"/etc/passwd",
		":/two",
		":(literal)main.go",
	} {
		t.Run(path, func(t *testing.T) {
			res := h.do(t, http.MethodGet, "/api/file?path="+url.QueryEscape(path)+"&mode=full")
			defer res.Body.Close()
			if res.StatusCode == http.StatusOK {
				var view FileView
				if err := json.NewDecoder(res.Body).Decode(&view); err == nil && len(view.Lines) > 0 {
					t.Errorf("%q returned %d lines of content", path, len(view.Lines))
				}
			}
		})
	}
}

// TestSymlinkEscapeRejected: a symlink tracked inside the repository must not
// become a way to read the rest of the filesystem. Reviewing a repo containing
// `link -> ~/.ssh` should not turn a click into a key dump.
func TestSymlinkEscapeRejected(t *testing.T) {
	h := newHarness(t, "")

	outside := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(outside); err == nil {
		outside = resolved
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-VALUE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.dir, "esc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := h.do(t, http.MethodGet, "/api/file?path=esc%2Fsecret.txt&mode=full")
	defer res.Body.Close()

	var view FileView
	_ = json.NewDecoder(res.Body).Decode(&view)
	for _, line := range view.Lines {
		for _, seg := range line.Segs {
			if strings.Contains(seg.T, "TOP-SECRET-VALUE") {
				t.Fatal("read a file outside the repository through a symlink")
			}
		}
	}
}

func TestLayoutRoundTrip(t *testing.T) {
	h := newHarness(t, "HEAD")

	var empty struct {
		Layout json.RawMessage `json:"layout"`
	}
	h.getJSON(t, "/api/layout", &empty)
	if len(empty.Layout) != 0 && string(empty.Layout) != "null" {
		t.Errorf("fresh layout = %s, want null", empty.Layout)
	}

	body := `{"scale":0.5,"cards":[{"path":"main.go","x":10,"y":20}]}`
	req, _ := http.NewRequest(http.MethodPut, h.ts.URL+"/api/layout", strings.NewReader(body))
	req.Header.Set("X-Diffcanvas-Token", h.srv.Token)
	res, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT layout: %d", res.StatusCode)
	}

	var saved struct {
		Layout struct {
			Scale float64 `json:"scale"`
			Cards []struct {
				Path string `json:"path"`
			} `json:"cards"`
		} `json:"layout"`
	}
	h.getJSON(t, "/api/layout", &saved)
	if saved.Layout.Scale != 0.5 || len(saved.Layout.Cards) != 1 || saved.Layout.Cards[0].Path != "main.go" {
		t.Errorf("layout did not round trip: %+v", saved.Layout)
	}
}

// TestWorktreeAndStagedSpecs exercises the two specs that read from the index
// and the filesystem instead of a commit.
func TestWorktreeAndStagedSpecs(t *testing.T) {
	for _, revspec := range []string{"", "--staged"} {
		t.Run("spec="+revspec, func(t *testing.T) {
			h := newHarness(t, revspec)

			// Dirty the tree after the server started; the change list is
			// computed on first request, so this is still picked up.
			if err := os.WriteFile(filepath.Join(h.dir, "main.go"),
				[]byte("package main\n\nfunc main() {\n\tprintln(\"edited\")\n}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if revspec == "--staged" {
				cmd := exec.Command("git", "add", "-A")
				cmd.Dir = h.dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git add: %v\n%s", err, out)
				}
			}

			var changes struct {
				Changes []gitx.FileChange `json:"changes"`
			}
			h.getJSON(t, "/api/changes", &changes)
			if len(changes.Changes) != 1 || changes.Changes[0].Path != "main.go" {
				t.Fatalf("changes = %+v, want just main.go", changes.Changes)
			}

			var view FileView
			h.getJSON(t, "/api/file?path=main.go&mode=diff", &view)
			if len(view.Hunks) == 0 {
				t.Error("no hunks")
			}
			// The new side comes from the worktree or index, not a commit.
			var sawEdited bool
			for _, hunk := range view.Hunks {
				for _, line := range hunk.Lines {
					for _, seg := range line.Segs {
						if strings.Contains(seg.T, "edited") {
							sawEdited = true
						}
					}
				}
			}
			if !sawEdited {
				t.Error("diff does not contain the uncommitted edit")
			}
		})
	}
}

func TestGrep(t *testing.T) {
	h := newHarness(t, "HEAD")
	var out struct {
		Hits []gitx.GrepHit `json:"hits"`
	}
	h.getJSON(t, "/api/grep?q=println", &out)
	if len(out.Hits) == 0 {
		t.Fatal("no grep hits for println")
	}
	if out.Hits[0].Path != "main.go" || out.Hits[0].Line == 0 {
		t.Errorf("unexpected hit: %+v", out.Hits[0])
	}
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8080": true, "localhost:1": true, "[::1]:99": true,
		"127.0.0.1": true, "evil.com:8080": false, "192.168.1.5:80": false,
		"": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
