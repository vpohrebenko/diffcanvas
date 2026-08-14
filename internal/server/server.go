// Package server exposes the review canvas over loopback HTTP.
//
// A server rather than a pre-rendered file is what allows file bodies, extra
// context and repo-wide search to be fetched on demand. That keeps the initial
// payload small no matter how large the change set is.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
	"github.com/vpohrebenko/diffcanvas/internal/webui"
)

// Server holds everything a request needs.
type Server struct {
	Repo  *gitx.Repo
	Spec  *gitx.Spec
	Token string

	store    *layoutStore
	mux      *http.ServeMux
	cache    changeCache
	symbols_ symbolIndex
}

// New builds a server with a fresh random token.
func New(repo *gitx.Repo, spec *gitx.Spec) (*Server, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generating token: %w", err)
	}
	store, err := newLayoutStore(repo.Root)
	if err != nil {
		return nil, err
	}

	s := &Server{
		Repo:  repo,
		Spec:  spec,
		Token: hex.EncodeToString(buf),
		store: store,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	assets, err := fs.Sub(webui.FS, "assets")
	if err != nil {
		panic(err) // embedded tree is fixed at build time
	}

	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(assets))))

	s.mux.HandleFunc("/api/meta", s.jsonHandler(s.handleMeta))
	s.mux.HandleFunc("/api/changes", s.jsonHandler(s.handleChanges))
	s.mux.HandleFunc("/api/file", s.jsonHandler(s.handleFile))
	s.mux.HandleFunc("/api/tree", s.jsonHandler(s.handleTree))
	s.mux.HandleFunc("/api/grep", s.jsonHandler(s.handleGrep))
	s.mux.HandleFunc("/api/imports", s.jsonHandler(s.handleImports))
	s.mux.HandleFunc("/api/def", s.jsonHandler(s.handleDef))
	s.mux.HandleFunc("/api/layout", s.jsonHandler(s.handleLayout))
}

// Listen binds a loopback port. Port 0 asks the kernel for a free one.
//
// Binding 127.0.0.1 rather than the wildcard address is deliberate: on a work
// machine the review may contain confidential code, and a wildcard bind would
// expose it to anything that can reach the host. A fixed port stays loopback
// only; reaching it from another machine takes a deliberate SSH tunnel.
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler: s.security(s.mux),
		// Loopback only, but a stuck client should not hold a connection for
		// the life of the process. Generous, because a whole-file request on a
		// large repository is legitimately slow.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.Serve(ln)
}

// URL is the address to open, including the token.
func (s *Server) URL(ln net.Listener) string {
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", ln.Addr().(*net.TCPAddr).Port, s.Token)
}

// security enforces the two checks that keep a local server honest.
func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A browser will happily resolve an attacker-controlled hostname to
		// 127.0.0.1 and then talk to us with that hostname in the Host header.
		// Refusing anything but a loopback Host defeats DNS rebinding.
		if !loopbackHost(r.Host) {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		// Nothing here is a legitimate cross-origin target.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		// The page is fully self-contained; forbid every outbound fetch so a
		// bug cannot turn into an exfiltration path.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' data:; font-src 'self'; connect-src 'self'; "+
				"form-action 'none'; frame-ancestors 'none'; base-uri 'none'")

		if !s.authorized(r) {
			http.Error(w, "missing or invalid token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(host string) bool {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(name, "[]"))
	return ip != nil && ip.IsLoopback()
}

// tokenCookie carries the token for requests the browser issues itself.
const tokenCookie = "dc_token"

// authorized checks the token, with deliberately different rules for data and
// for assets.
//
// Anything under /api/ must present the token in a header. A page on another
// origin cannot set a custom header without a CORS preflight, which is never
// granted, so repository content is unreachable from any other origin even if
// a cookie were somehow sent. Ports do not distinguish sites for cookie
// purposes, so another local server could otherwise be same-site with us.
//
// The page itself and its static assets accept the query parameter or the
// cookie, because the browser fetches stylesheets and module scripts on its
// own and cannot attach headers to those requests. Those assets are the tool's
// own JavaScript and CSS and contain nothing from the repository.
func (s *Server) authorized(r *http.Request) bool {
	if header := r.Header.Get("X-Diffcanvas-Token"); header != "" {
		return tokenEqual(header, s.Token)
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if q := r.URL.Query().Get("t"); q != "" {
		return tokenEqual(q, s.Token)
	}
	if c, err := r.Cookie(tokenCookie); err == nil {
		return tokenEqual(c.Value, s.Token)
	}
	return false
}

func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handler is an API handler that returns a value to encode, or an error.
type handler func(context.Context, *http.Request) (any, error)

func (s *Server) jsonHandler(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := h(r.Context(), r)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			// Response already started; nothing useful left to do.
			return
		}
	}
}

// handleIndex serves the page with the token baked in, so the browser can send
// it back on API calls without it ever being typed by hand.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := webui.FS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "index missing", http.StatusInternalServerError)
		return
	}
	// Lets the browser fetch the stylesheet and module scripts, which it does
	// without any opportunity for us to attach a header.
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    s.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	// The token is a hex string, so there is nothing to escape.
	body := strings.Replace(string(page), "__TOKEN__", s.Token, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}
