package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Diff is one pull request's changes as git prints them, from where its branch left its
// base, split by file. Where they change more lines than its reader keeps whole, Files is
// empty and Lines says how many they change.
type Diff struct {
	// Head is the commit it was read at, and empty where its source does not say.
	Head string
	// Lines is how many lines it adds and removes, together.
	Lines int
	// Whole says Files holds every file. It is false where Lines ran past the limit.
	Whole  bool
	Files  []FileDiff
	ReadAt time.Time
}

// FileDiff is one file's part of a Diff: its hunks, without the header git writes above them.
type FileDiff struct {
	Path string
	// OldPath is where a renamed or copied file came from, and empty otherwise.
	OldPath string
	// Change is added, deleted, renamed, copied or modified.
	Change string
	// Binary says git found the content binary, so there are no hunks to show.
	Binary bool
	// Hunks is each @@ hunk and its lines, as git printed them.
	Hunks string
}

// DiffState is how one pull request's diff stands.
type DiffState struct {
	Diff Diff
	// Held says Diff was read. A read under way or failed keeps the last one held.
	Held    bool
	Reading bool
	// Fault is why the newest read failed, and empty where it read.
	Fault string
	// Cannot is why prs reads no diff of it here, and empty where it can.
	Cannot string
	// InCheckout says it is read in its repo's checkout here rather than from its host.
	InCheckout bool
	// CheckedOut says its repo is checked out here, which is what d needs to open it.
	CheckedOut bool
	// Limit is the most lines a Diff changes for Files to be kept.
	Limit int
}

// diffsAtOnce is how many reads run at the same time. The reads in one checkout take one
// of them between them, and run in turn.
const diffsAtOnce = 4

// retryAtMost is the longest a failed read waits before it is read again. The wait starts at
// a minute and doubles with each failure in a row.
const retryAtMost = 30 * time.Minute

// Diffs holds each pull request's Diff in memory, by Key, read in the background after a
// listing lands, so the Diff section never waits on the network. A diff comes from its host
// where the host's client has one, and from the repo's checkout otherwise, its head fetched
// by FetchHead. One is read again when its row's head commit or base moves, or once a failed
// read has waited out its retry. A row naming no head commit is read again when its update
// time moves.
type Diffs struct {
	mu      sync.Mutex
	limit   int
	entries map[string]*diffEntry
}

type diffEntry struct {
	// from is who listed it. A listing from them that leaves it out drops it.
	from string
	// at is what the row it was read for said of its head and base, as diffAt writes it.
	at                     string
	inCheckout, checkedOut bool
	cannot                 string
	diff                   Diff
	held, reading          bool
	fault                  string
	// failures counts the reads in a row that failed for at, the last one at failedAt.
	failures int
	failedAt time.Time
}

// current says the entry stands for at as it is: read, settled as unreadable, or failed too
// recently to read again.
func (e *diffEntry) current(at string, now time.Time) bool {
	switch {
	case e.at != at:
		return false
	case e.fault != "":
		return now.Before(e.failedAt.Add(min(time.Minute<<min(e.failures-1, 5), retryAtMost)))
	}
	return e.held || e.cannot != ""
}

// NewDiffs is an empty cache whose diffs keep their files up to limit changed lines.
func NewDiffs(limit int) *Diffs { return &Diffs{limit: limit, entries: map[string]*diffEntry{}} }

// Of is how the pull request's diff stands now.
func (d *Diffs) Of(pr PullRequest) DiffState {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[pr.Key()]
	if !ok {
		return DiffState{Limit: d.limit}
	}
	return DiffState{
		Diff: entry.diff, Held: entry.held, Reading: entry.reading, Fault: entry.fault, Cannot: entry.cannot,
		InCheckout: entry.inCheckout, CheckedOut: entry.checkedOut, Limit: d.limit,
	}
}

// Prefetch reads, in the background, the diff of each of prs whose held diff is missing,
// older than its row, or failed and waited out. Each carries its Path, as Finish sets it.
// from names who listed prs, a provider or a read of one pull request, and Prefetch drops
// the diff of each pull request from listed that prs leaves out.
func (d *Diffs) Prefetch(ctx context.Context, from string, prs []PullRequest) {
	queues := inTurn(d.claim(from, prs, time.Now()))
	go func() {
		slots := make(chan struct{}, diffsAtOnce)
		for _, queue := range queues {
			slots <- struct{}{}
			go func() {
				defer func() { <-slots }()
				d.readInTurn(ctx, queue)
			}()
		}
	}()
}

type dueDiff struct {
	pr         PullRequest
	inCheckout bool
}

// inTurn is the due reads as queues, each read in turn: one per checkout, whose fetches run
// one at a time anyway, and one for each read from a host.
func inTurn(due []dueDiff) [][]dueDiff {
	var queues [][]dueDiff
	checkout := map[string]int{}
	for _, read := range due {
		if !read.inCheckout {
			queues = append(queues, []dueDiff{read})
			continue
		}
		i, ok := checkout[read.pr.Path]
		if !ok {
			i = len(queues)
			checkout[read.pr.Path] = i
			queues = append(queues, nil)
		}
		queues[i] = append(queues[i], read)
	}
	return queues
}

// readInTurn reads each of queue in turn. A fetch that got no answer fails the rest of its
// checkout's unread, because each would wait out the same timeout, and d waits behind them.
func (d *Diffs) readInTurn(ctx context.Context, queue []dueDiff) {
	for i, read := range queue {
		diff, err := d.read(ctx, read.pr, read.inCheckout)
		d.store(read.pr, diff, err)
		if read.inCheckout && errors.Is(err, ErrNoAnswer) {
			for _, rest := range queue[i+1:] {
				d.store(rest.pr, Diff{}, fmt.Errorf("not fetched, because a fetch in %s just got %w", rest.pr.Path, err))
			}
			return
		}
	}
}

// claim marks each pull request whose diff is due as reading, and returns them. A diff its
// row's counts already put past the limit, and one prs cannot read here, are settled at once
// rather than read. It drops the entry of each pull request from listed that prs does not
// hold.
func (d *Diffs) claim(from string, prs []PullRequest, now time.Time) []dueDiff {
	d.mu.Lock()
	defer d.mu.Unlock()
	listed := make(map[string]bool, len(prs))
	var due []dueDiff
	for _, pr := range prs {
		listed[pr.Key()] = true
		entry, ok := d.entries[pr.Key()]
		if !ok {
			entry = &diffEntry{from: from}
			d.entries[pr.Key()] = entry
		}
		at := diffAt(pr)
		if entry.reading || entry.current(at, now) {
			continue
		}
		if entry.at != at {
			entry.at, entry.failures = at, 0
		}
		_, err := CheckoutOf(pr)
		entry.checkedOut = err == nil
		entry.inCheckout, entry.cannot = diffRoute(pr)
		if entry.cannot != "" {
			entry.diff, entry.held, entry.fault = Diff{}, false, ""
			continue
		}
		if pr.Additions != nil && pr.Deletions != nil && *pr.Additions+*pr.Deletions > d.limit {
			entry.diff, entry.held, entry.fault = Diff{Lines: *pr.Additions + *pr.Deletions, ReadAt: now.UTC()}, true, ""
			continue
		}
		entry.reading = true
		due = append(due, dueDiff{pr: pr, inCheckout: entry.inCheckout})
	}
	for key, entry := range d.entries {
		if entry.from == from && !listed[key] && !entry.reading {
			delete(d.entries, key)
		}
	}
	return due
}

func (d *Diffs) store(pr PullRequest, diff Diff, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// claim keeps an entry while it is reading, so the one this read claimed is still there.
	entry := d.entries[pr.Key()]
	entry.reading = false
	if err != nil {
		entry.fault, entry.failures, entry.failedAt = err.Error(), entry.failures+1, time.Now()
		return
	}
	entry.diff, entry.held, entry.fault, entry.failures = diff, true, "", 0
}

// diffAt is what a diff read for the row stands for: its head commit and base, or where the
// row names no head, its update time and base.
func diffAt(pr PullRequest) string {
	switch {
	case pr.HeadOID != nil && *pr.HeadOID != "":
		return *pr.HeadOID + " into " + pr.Base
	case pr.UpdatedAt != nil:
		return pr.UpdatedAt.UTC().Format(time.RFC3339) + " into " + pr.Base
	}
	return "into " + pr.Base
}

// diffRoute says whether the pull request is diffed in its checkout, or else why prs cannot
// diff it here. A host whose client has a diff sends it, so prs fetches nothing into that
// checkout unasked. Only a host with none is fetched into its checkout, and there a merged
// branch sits in its base, so its diff from where it left the base is empty.
func diffRoute(pr PullRequest) (inCheckout bool, cannot string) {
	provider, err := ProviderOf(pr.Provider)
	if err != nil {
		return false, err.Error()
	}
	if provider.Diff != nil {
		return false, ""
	}
	why := fmt.Sprintf("prs diffs a pull request on %s only in its checkout, because its client has no diff", pr.Provider)
	if standing, _ := pr.Standing(); standing == Merged {
		return false, why + ", and a merged branch diffs as nothing against its base there."
	}
	if _, err := CheckoutOf(pr); err != nil {
		return false, fmt.Sprintf("%s, and %s is not checked out here.", why, pr.Slug)
	}
	return true, ""
}

// read reads the pull request's diff where diffRoute sends it, and keeps its files where it
// changes no more lines than the limit.
func (d *Diffs) read(ctx context.Context, pr PullRequest, inCheckout bool) (Diff, error) {
	var diff Diff
	var text string
	if inCheckout {
		head, out, err := diffInCheckout(ctx, pr)
		if err != nil {
			return Diff{}, err
		}
		text, diff.Head = out, head
	} else {
		provider, err := ProviderOf(pr.Provider)
		if err != nil {
			return Diff{}, err
		}
		if text, err = provider.Diff(ctx, pr); err != nil {
			return Diff{}, err
		}
		if pr.HeadOID != nil {
			diff.Head = *pr.HeadOID
		}
	}
	files, lines := splitDiff(text)
	diff.Lines, diff.ReadAt = lines, time.Now().UTC()
	if lines <= d.limit {
		diff.Whole, diff.Files = true, files
	}
	return diff, nil
}

// diffInCheckout fetches the pull request's head alone, then diffs its LocalDiff, and returns
// the head's commit and the diff. The flags fix the output this package reads, whatever the
// user's git config says: no color, no external or textconv driver, a/ and b/ prefixes,
// renames found, and paths written as they are.
func diffInCheckout(ctx context.Context, pr PullRequest) (string, string, error) {
	target, err := LocalDiffOf(ctx, pr)
	if err != nil {
		return "", "", err
	}
	got, err := FetchHead(ctx, target)
	if err != nil {
		return "", "", err
	}
	out, err := Run(ctx, target.Checkout, "git", "-c", "core.quotePath=false", "--no-pager", "diff", "--no-color", "--no-ext-diff",
		"--no-textconv", "--find-renames", "--src-prefix=a/", "--dst-prefix=b/", target.Range())
	if err != nil {
		return "", "", fmt.Errorf("git could not diff #%d in %s: %w", pr.Number, pr.Repo, err)
	}
	return got.Head, string(out), nil
}

// splitDiff is a diff in git's format split by file, and how many lines it adds and removes.
// Each file starts at its diff --git line. The header lines under it name the change and the
// paths, and everything from its first @@ on is its hunks.
func splitDiff(text string) ([]FileDiff, int) {
	var files []FileDiff
	var hunks strings.Builder
	changed := 0
	inHunks := false
	closeFile := func() {
		if len(files) > 0 {
			files[len(files)-1].Hunks = hunks.String()
		}
		hunks.Reset()
	}
	for line := range strings.Lines(text) {
		if sides, ok := strings.CutPrefix(line, "diff --git "); ok {
			closeFile()
			files = append(files, FileDiff{Path: bothSides(strings.TrimRight(sides, "\n")), Change: "modified"})
			inHunks = false
			continue
		}
		if len(files) == 0 {
			continue
		}
		inHunks = inHunks || strings.HasPrefix(line, "@@")
		if inHunks {
			hunks.WriteString(line)
			if line[0] == '+' || line[0] == '-' {
				changed++
			}
			continue
		}
		readHeader(&files[len(files)-1], strings.TrimRight(line, "\n"))
	}
	closeFile()
	return files, changed
}

// readHeader takes what one line of a file's header says into file.
func readHeader(file *FileDiff, line string) {
	field := func(prefix string) (string, bool) {
		value, ok := strings.CutPrefix(line, prefix)
		return unquoted(value), ok
	}
	if value, ok := field("rename from "); ok {
		file.OldPath, file.Change = value, "renamed"
	} else if value, ok := field("copy from "); ok {
		file.OldPath, file.Change = value, "copied"
	} else if value, ok := field("rename to "); ok {
		file.Path = value
	} else if value, ok := field("copy to "); ok {
		file.Path = value
	} else if value, ok := field("+++ "); ok && value != "/dev/null" && file.Path == "" {
		file.Path = strings.TrimPrefix(value, "b/")
	} else if value, ok := field("--- "); ok && value != "/dev/null" && file.Path == "" {
		file.Path = strings.TrimPrefix(value, "a/")
	}
	switch {
	case strings.HasPrefix(line, "new file mode"):
		file.Change = "added"
	case strings.HasPrefix(line, "deleted file mode"):
		file.Change = "deleted"
	case strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
		file.Binary = true
	}
}

// bothSides is the path a diff --git line names where its two sides name one path, and
// empty where they differ, as a rename's do, or where git quoted them. The header lines
// after it name the path then.
func bothSides(sides string) string {
	n := (len(sides) - len("a/ b/")) / 2
	if n < 1 || len(sides) != 2*n+len("a/ b/") {
		return ""
	}
	old, current := sides[:n+2], sides[n+3:]
	if sides[n+2] != ' ' || !strings.HasPrefix(old, "a/") || !strings.HasPrefix(current, "b/") || old[2:] != current[2:] {
		return ""
	}
	return old[2:]
}

// unquoted is a path as git wrote it, with the C-style quotes git puts around a path holding
// a character it escapes taken off.
func unquoted(path string) string {
	if !strings.HasPrefix(path, `"`) {
		return path
	}
	if plain, err := strconv.Unquote(path); err == nil {
		return plain
	}
	return path
}
