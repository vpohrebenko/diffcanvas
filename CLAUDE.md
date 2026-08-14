# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

`diffcanvas` is a local-only git diff reviewer: a single static Go binary that
serves an infinite-canvas web UI on loopback, for reading large pull requests
(50+ files) without jumping between files one at a time.

It is a **reading tool**. Code is the content; everything else is chrome and
should stay quiet.

## Hard constraints

These are not preferences. Breaking any of them defeats the point of the tool.

1. **Zero external dependencies.** Standard library only. `go.mod` must have no
   `require` block. The tool is built and run on machines where the Go proxy
   may be unreachable and where a supply chain cannot be vetted.
2. **No network egress, ever.** No CDN, no fonts, no telemetry, no update
   check. The only subprocess is `git`, in the repository the user pointed at.
   The CSP forbids all outbound requests; do not relax it.
3. **Loopback only.** Never bind the wildcard address. Reviewed code is
   frequently confidential.
4. **No build step for the frontend.** Vanilla ES modules, embedded with
   `go:embed`. No npm, no bundler, no framework. `node` is not assumed to
   exist on the developer's machine.
5. **File content is never rendered as markup.** Everything goes through
   `textContent`. A repository containing `<script>` must never execute it.

## Layout

```
cmd/diffcanvas/      flags, browser launch
internal/gitx/       git plumbing: revspecs, diffs, file reads, grep
internal/highlight/  go/scanner tokenizer + table-driven fallback
internal/diffx/      line pairing and word-level intra-line diff
internal/goimports/  which open Go files import one another
internal/gosym/      go/ast declaration index for jump-to-definition
internal/server/     routes, token auth, layout persistence
internal/webui/      embedded browser assets (the whole frontend)
```

## Invariants that are easy to break

**Segments must reassemble exactly.** `highlight.Lines` returns per-line
`[]Segment` carrying *text*, not offsets. Concatenating a line's segments must
reproduce the input byte for byte. Offsets were rejected deliberately: Go
indexes bytes, JavaScript indexes UTF-16, and the mismatch silently corrupts
any line containing non-ASCII. `server.splitOnMarks` must preserve this when it
splits segments for the word diff.

**`ROW_H` (card.js) must equal `--row-h` (style.css).** Row virtualisation
assumes every row is exactly that tall. If they drift, rows overlap or gap.

**Pathspecs must be `:(literal)`-prefixed.** A file named `a*.txt` otherwise
globs onto its neighbours and `:weird.txt` is parsed as pathspec magic. Use
`literalPathspec()` in `gitx`.

**Patches can contain more than one file section.** Asking for a copy names
both source and destination, and sections are ordered by path, not by pathspec
order. `parseHunks` demultiplexes by path; do not simplify it back to "parse
the whole patch as one file", or a card will show another file's diff and the
`--- a/x` / `+++ b/x` headers will appear as diff rows.

**Never cache an error, and never compute cached state on a request context.**
`server.changes` and `server.symbols` run on `context.Background()`. Using the
request's context meant that reloading the page mid-load killed git and
poisoned the cache for the life of the process.

**Paths from the client are hostile.** `gitx.validRepoPath` rejects absolute
paths, `..` segments and anything starting with `:` (which reaches git's
revision parser via `<rev>:<path>`). `safeJoin` resolves symlinks before the
containment check — a tracked symlink pointing at `~/.ssh` is a real scenario.

**File contents are read with CRLF normalised, diff lines keep their `\r`.**
`segsAt` trims the carriage return before comparing, or a whole Windows repo
renders unstyled.

## Auth model — do not "simplify" it

The asymmetry is deliberate and load-bearing:

- **`/api/*` requires the token in the `X-Diffcanvas-Token` header.** A page on
  another origin cannot set a custom header without a CORS preflight, which is
  never granted. This is what keeps repository data unreachable cross-origin.
- **The page and `/static/*` also accept a query param or a `SameSite=Strict`
  cookie**, because the browser fetches stylesheets and module scripts itself
  and cannot attach headers to those requests.

Ports do not distinguish sites for cookie purposes, so another local server
could be same-site with us. That is why API data never accepts the cookie.
`server_test.go:TestAuthSplit` pins every case.

## Conventions

- Comments explain **why**, not what. Prefer one sentence of rationale over a
  restatement of the code.
- Errors from git include stderr; failures should be diagnosable from the
  message alone.
- Degrade rather than fail: an unparseable file, a missing symbol or an
  unreadable layout should lose a feature, not the request.
- Heuristics must say they are heuristics. `gosym` returns a confidence level
  and refuses to guess when the qualifier is an external package.
- Frontend: no inline scripts or inline event handlers (the CSP forbids them).
  Use `addEventListener` and the `dc:*` custom events for cross-module signals.

## Testing

```sh
go test ./...        # all packages
go vet ./...
gofmt -l ./cmd ./internal
```

Tests build throwaway git repositories in `t.TempDir()`; they do not depend on
any checkout being present. When fixing a bug found against a real repository,
add the case to the synthetic fixture — several bugs here (pathspec globbing,
multi-file patches, external qualifiers) were invisible in hand-made fixtures
and only appeared on real code.

To try it against something real, clone any Go project and run:

```sh
go run ./cmd/diffcanvas -C /path/to/repo 'HEAD~40...HEAD'
```

Headless screenshots are the fastest way to check frontend changes:

```sh
go run ./cmd/diffcanvas -C /path/to/repo -no-open -port 8500 HEAD &
chromium --headless --disable-gpu --no-sandbox --window-size=1600,900 \
  --virtual-time-budget=12000 --screenshot=out.png "http://127.0.0.1:8500/?t=<token>"
```

The layout API accepts a seeded arrangement, which is how to put specific cards
on screen without driving the mouse.

## Things deliberately not done

- **`go/types`-accurate symbol resolution.** It would need a real build and
  network access for dependencies. `gosym` is a navigation aid and labels its
  confidence.
- **A frontend framework or build step.** See constraint 4.
- **Multi-user or remote access.** Loopback only, by design. Reaching it from
  another machine is an SSH tunnel, deliberately.
