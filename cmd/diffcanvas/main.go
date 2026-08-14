// Command diffcanvas serves a local, infinite-canvas view of a git diff.
//
//	diffcanvas                 uncommitted changes
//	diffcanvas --staged        staged changes
//	diffcanvas HEAD~3          a single commit
//	diffcanvas main...feature  a branch, from its merge base
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/vpohrebenko/diffcanvas/internal/gitx"
	"github.com/vpohrebenko/diffcanvas/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "diffcanvas:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir    = flag.String("C", ".", "run as if started in this directory")
		staged = flag.Bool("staged", false, "review staged changes")
		noOpen = flag.Bool("no-open", false, "do not launch a browser")
		port   = flag.Int("port", 0, "loopback port to listen on (0 picks a free one)")
	)
	flag.Usage = usage
	flag.Parse()

	revspec := strings.Join(flag.Args(), " ")
	if *staged {
		revspec = "--staged"
	}

	ctx := context.Background()
	repo, err := gitx.Discover(ctx, *dir)
	if err != nil {
		return err
	}
	spec, err := gitx.ResolveSpec(ctx, repo, revspec)
	if err != nil {
		return err
	}

	srv, err := server.New(repo, spec)
	if err != nil {
		return err
	}
	ln, err := server.Listen(*port)
	if err != nil {
		return fmt.Errorf("binding loopback port: %w", err)
	}

	url := srv.URL(ln)
	fmt.Printf("diffcanvas  %s\n", repo.Root)
	fmt.Printf("reviewing   %s\n", spec.Label)
	fmt.Printf("open        %s\n\n", url)
	fmt.Println("Serving on loopback only. Ctrl-C to stop.")

	if !*noOpen {
		openBrowser(url)
	}
	return srv.Serve(ln)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: diffcanvas [flags] [revspec]

Revspecs:
  (none)            uncommitted changes against HEAD
  --staged          staged changes only
  <rev>             one commit against its first parent
  A...B             changes on B since it diverged from A   (what a PR shows)
  A..B              direct diff between two revisions

Flags:
`)
	flag.PrintDefaults()
}

// openBrowser launches the default browser, trying the openers most likely to
// exist. Under WSL the Windows browser reaches the loopback listener directly,
// so no path translation is needed.
func openBrowser(url string) {
	var candidates [][]string
	if b := os.Getenv("BROWSER"); b != "" {
		candidates = append(candidates, []string{b, url})
	}
	switch runtime.GOOS {
	case "windows":
		candidates = append(candidates, []string{"rundll32", "url.dll,FileProtocolHandler", url})
	case "darwin":
		candidates = append(candidates, []string{"open", url})
	default:
		candidates = append(candidates,
			[]string{"wslview", url},      // WSL, from wslu
			[]string{"xdg-open", url},     // Linux desktop
			[]string{"explorer.exe", url}, // WSL without wslu
		)
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		if err := cmd.Start(); err == nil {
			// Reap it, or the opener lingers as a zombie for the whole run.
			go func() { _ = cmd.Wait() }()
			return
		}
	}
	// Not being able to open a browser is not a failure; the URL is printed.
}
