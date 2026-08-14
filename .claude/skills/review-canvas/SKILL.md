---
name: review-canvas
description: Open a git diff on the diffcanvas review canvas — a local, loopback-only web UI that lays files out in 2D for reading large changes. Use when the user wants to look at, read, review, or visually inspect a diff, a branch, a pull request, a commit, or their uncommitted work, especially when it spans many files. Also use when they ask to "open the canvas", "show me this PR", or to see a change laid out rather than scrolled through.
---

# Opening a change on the review canvas

`diffcanvas` serves a local web UI that spreads a diff's files across a 2D
canvas. This skill turns "let me look at X" into the right command.

Your job is to work out **which revision range** the user means, start the
server, and hand them the URL. The tool is read-only: it never writes to the
repository under review.

## 1. Work out the revspec

This is the part that needs judgement. Map what the user said onto one of:

| They said | Use | Why |
|---|---|---|
| "my changes", "what I've been working on", "before I commit" | *(no revspec)* | uncommitted work against HEAD |
| "what I've staged", "what's about to be committed" | `--staged` | index only |
| "this PR", "my branch", "the feature branch", "review X against main" | `main...feature` | **three dots** — diff from the merge base, which is what a pull request shows |
| "the last commit", "what I just did" | `HEAD` | one commit against its first parent |
| "the last N commits" | `HEAD~N...HEAD` | |
| a commit hash or tag | that hash | |
| "what changed between these two things" | `A..B` | two dots, a literal diff |

**Use three dots for anything branch-shaped.** `main..feature` also includes
everything that landed on `main` after the branch point, which is how a 50-file
review turns into a 200-file one. If unsure which the user means, prefer
`A...B` and say so.

Determine the base branch from the repository rather than assuming `main`:

```sh
git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's|^origin/||'
# falls back to: git branch --list main master develop
```

## 2. Check the range is sane before opening it

```sh
git diff --numstat <revspec> | wc -l     # how many files
```

- **0 files** — say so and stop; do not open an empty canvas. Suggest the most
  likely alternative (often they meant `HEAD` rather than uncommitted work, or
  the branch is already merged).
- **More than ~400 files** — still works, but say it will be busy and offer to
  narrow the range (`HEAD~5...HEAD`) or filter to a subtree.

## 3. Start it

Run in the background; it serves until stopped.

```sh
diffcanvas -C <repo> -no-open -port <port> '<revspec>'
```

- `-no-open` — **always pass this.** You cannot see a browser, and on a headless
  or remote machine the launch attempt is noise. Give the user the URL instead.
- `-port` — pick a fixed port (8477 is a good default) so the URL is stable and
  can be tunnelled. Without it a random free port is chosen.
- Quote the revspec: `...` and `^` are shell-significant.

The command prints the URL with a single-use-per-run token:

```
open        http://127.0.0.1:8477/?t=<token>
```

**Give the user that whole URL, token included.** It will not work without it.

If the port is taken, drop `-port` and read the printed URL.

## 4. Tell them how to reach it

- **Same machine:** open the URL.
- **Remote host over SSH:** the server is loopback-only by design, so it needs
  a tunnel: `ssh -L 8477:127.0.0.1:8477 user@host`, then open the URL locally.
- **WSL:** run it inside WSL; WSL2 forwards localhost, so the URL opens
  directly in the Windows browser.

Mention the two or three controls that fit what they are doing — not all of
them. Reading a big change: `f` fits everything, and zoomed out each card shows
its shape. Tracing logic: ctrl-click an identifier opens its declaration as a
new card with an arrow back to the call site. Comparing: `s` switches to
side-by-side.

## 5. Stop it when they are done

It runs until killed. Note the PID when you start it and stop it when the user
says they are finished, or before starting another instance on the same port.

## Worked examples

**"Can you show me my PR?"**
```sh
git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|^origin/||'   # → main
git rev-parse --abbrev-ref HEAD                                          # → feat/import-arrows
git diff --numstat 'main...feat/import-arrows' | wc -l                   # → 23
diffcanvas -C . -no-open -port 8477 'main...feat/import-arrows'
```
> 23 files. http://127.0.0.1:8477/?t=… — press `f` to fit them all, then zoom
> out to see which files carry the bulk of the change.

**"What did I break in the last commit?"**
```sh
diffcanvas -C . -no-open -port 8477 HEAD
```

**"Let me look at my changes before I commit"**
```sh
git diff --numstat | wc -l    # → 0
```
> Nothing uncommitted — everything is committed or staged. Did you mean your
> staged changes (`--staged`), or the last commit (`HEAD`)?

**"Review the auth refactor branch"**
```sh
git diff --numstat 'main...auth-refactor' | wc -l    # → 312
diffcanvas -C . -no-open -port 8477 'main...auth-refactor'
```
> 312 files, which is a lot for one canvas. Opened anyway —
> http://127.0.0.1:8477/?t=… — but you may want `main...auth-refactor -- src/auth`
> style narrowing, or step through it a few commits at a time.

**"Compare v1.2.0 and v1.3.0"**
```sh
diffcanvas -C . -no-open -port 8477 'v1.2.0..v1.3.0'
```
Two dots here: the user asked for the difference between two points, not for
what one branch added.

## When not to use this

- The user wants the diff **as text** in the conversation — use `git diff`.
- They want to **change** something — this is read-only.
- A single small file — `git diff -- path` is faster than a canvas.
- No display and no way to tunnel — a canvas they cannot open helps nobody.
