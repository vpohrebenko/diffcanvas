package gitx

import (
	"bufio"
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// GrepHit is one matching line.
type GrepHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

const (
	// maxGrepHits bounds what is returned and, because the output is streamed,
	// also what is ever read into memory.
	maxGrepHits = 400
	// maxGrepLine keeps one enormous minified line from dominating a response.
	maxGrepLine = 400
)

// Grep searches the repository at the given revision.
//
// Output is streamed and the search is killed once enough hits are in hand.
// Buffering it instead would read the whole result: git's --max-count is per
// file, so a one-letter query over a large repository produces tens of
// megabytes on the way to returning 400 rows.
//
// -z NUL-delimits the path and line number, so a file whose name contains a
// colon parses correctly.
func Grep(ctx context.Context, r *Repo, rev, query string, ignoreCase bool) ([]GrepHit, error) {
	hits := []GrepHit{}
	if strings.TrimSpace(query) == "" {
		return hits, nil
	}

	args := append([]string(nil), baseArgs...)
	args = append(args, "grep", "--no-color", "-n", "-z", "--fixed-strings", "-I")
	if ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", query)

	// git grep cannot search a revision and the working tree in one call.
	withRev := rev != RevWorktree && rev != RevIndex
	if withRev {
		args = append(args, rev)
	}
	args = append(args, "--")

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return hits, nil
	}
	if err := cmd.Start(); err != nil {
		return hits, nil
	}
	defer func() {
		// Killing is expected: we stop reading as soon as the cap is reached.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	revPrefix := rev + ":"
	reader := bufio.NewReaderSize(stdout, 64*1024)

	for len(hits) < maxGrepHits {
		record, err := reader.ReadString('\n')
		if record == "" && err != nil {
			break
		}
		record = strings.TrimRight(record, "\n")

		// path \0 lineno \0 text
		parts := strings.SplitN(record, "\x00", 3)
		if len(parts) < 3 {
			if err != nil {
				break
			}
			continue
		}
		path := parts[0]
		if withRev {
			path = strings.TrimPrefix(path, revPrefix)
		}
		num, convErr := strconv.Atoi(parts[1])
		if convErr != nil {
			if err != nil {
				break
			}
			continue
		}

		text := parts[2]
		if len(text) > maxGrepLine {
			text = text[:maxGrepLine]
		}
		hits = append(hits, GrepHit{Path: path, Line: num, Text: text})

		if err != nil {
			break
		}
	}
	return hits, nil
}
