package pullrequests

import (
	"context"
	"fmt"
	"os"
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
	// BaseFault is why fetching the base failed, where the diff ran against the origin/<base>
	// an earlier fetch left.
	BaseFault string
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
	// InCheckout says it is diffed in its repo's checkout here, where d opens it too.
	InCheckout bool
	// Limit is the most lines a Diff changes for Files to be kept.
	Limit int
}

// diffsAtOnce is how many pull requests' diffs are read at the same time. Those in one
// checkout still fetch one at a time, under Fetch's lock.
const diffsAtOnce = 4

// Diffs holds each pull request's Diff in memory, by Key, read in the background after a
// listing lands, so the Diff section never waits on the network. A diff is read in the
// repo's checkout, fetched as d fetches it, and from the host where the repo is not checked
// out. One is read again when its row's head commit or base moves, or when its last read
// failed. A row naming no head commit is read again when its update time moves.
type Diffs struct {
	mu      sync.Mutex
	limit   int
	entries map[string]*diffEntry
}

type diffEntry struct {
	// from is who listed it. A listing from them that leaves it out drops it.
	from string
	// at is what the row it was read for said of its head and base, as diffAt writes it.
	at            string
	inCheckout    bool
	cannot        string
	diff          Diff
	held, reading bool
	fault         string
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
		Diff: entry.diff, Held: entry.held, Reading: entry.reading, Fault: entry.fault,
		Cannot: entry.cannot, InCheckout: entry.inCheckout, Limit: d.limit,
	}
}

// Prefetch reads, in the background and diffsAtOnce at a time, the diff of each of prs whose
// held diff is missing, failed or older than its row. Each carries its Path, as Finish sets
// it. from names who listed prs, a provider or a read of one pull request, and Prefetch
// drops the diff of each pull request from listed that prs leaves out.
func (d *Diffs) Prefetch(ctx context.Context, from string, prs []PullRequest) {
	due := d.claim(from, prs)
	go func() {
		slots := make(chan struct{}, diffsAtOnce)
		for _, read := range due {
			slots <- struct{}{}
			go func() {
				defer func() { <-slots }()
				diff, err := d.read(ctx, read.pr, read.inCheckout)
				d.store(read.pr, diff, err)
			}()
		}
	}()
}

type dueDiff struct {
	pr         PullRequest
	inCheckout bool
}

// claim marks each pull request whose diff is due as reading, and returns them. A diff its
// row's counts already put past the limit, and one prs cannot read here, are settled at once
// rather than read. It drops the entry of each pull request from listed that prs does not
// hold.
func (d *Diffs) claim(from string, prs []PullRequest) []dueDiff {
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
		if entry.reading || entry.at == at && entry.fault == "" && (entry.held || entry.cannot != "") {
			continue
		}
		entry.at = at
		entry.inCheckout, entry.cannot = diffRoute(pr)
		if entry.cannot != "" {
			entry.diff, entry.held, entry.fault = Diff{}, false, ""
			continue
		}
		if pr.Additions != nil && pr.Deletions != nil && *pr.Additions+*pr.Deletions > d.limit {
			entry.diff, entry.held, entry.fault = Diff{Lines: *pr.Additions + *pr.Deletions, ReadAt: time.Now().UTC()}, true, ""
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
		entry.fault = err.Error()
		return
	}
	entry.diff, entry.held, entry.fault = diff, true, ""
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
// diff it here, where its host's client has no diff either. A merged branch sits in its base,
// so in the checkout a diff from where it left the base is empty, and only the host still
// has it.
func diffRoute(pr PullRequest) (inCheckout bool, cannot string) {
	provider, err := ProviderOf(pr.Provider)
	if err != nil {
		return false, err.Error()
	}
	standing, _ := pr.Standing()
	cloned := checkedOut(pr.Path)
	switch {
	case cloned && standing != Merged:
		return true, ""
	case provider.Diff != nil:
		return false, ""
	case !cloned:
		return false, fmt.Sprintf("prs diffs a pull request on %s only in its checkout, because its client has no diff, and %s is not checked out here.",
			pr.Provider, pr.Slug)
	}
	return false, fmt.Sprintf("prs diffs a pull request on %s only in its checkout, because its client has no diff, and a merged branch diffs as nothing against its base there.",
		pr.Provider)
}

func checkedOut(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// read reads the pull request's diff where diffRoute sends it, and keeps its files where it
// changes no more lines than the limit.
func (d *Diffs) read(ctx context.Context, pr PullRequest, inCheckout bool) (Diff, error) {
	var diff Diff
	var text string
	if inCheckout {
		got, out, err := diffInCheckout(ctx, pr)
		if err != nil {
			return Diff{}, err
		}
		text, diff.Head, diff.BaseFault = out, got.Head, got.BaseFault
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

// diffInCheckout fetches the pull request as d does, then diffs it three-dot against its base,
// from where the branch left it, which is what the provider shows. The flags fix the output
// this package reads, whatever the user's git config says: no color, no external or textconv
// driver, a/ and b/ prefixes, renames found, and paths written as they are.
func diffInCheckout(ctx context.Context, pr PullRequest) (Fetched, string, error) {
	provider, err := ProviderOf(pr.Provider)
	if err != nil {
		return Fetched{}, "", err
	}
	head := provider.Head(pr)
	switch {
	case head == "":
		return Fetched{}, "", fmt.Errorf("#%d in %s names no source branch to fetch", pr.Number, pr.Repo)
	case pr.Base == "":
		return Fetched{}, "", fmt.Errorf("#%d in %s names no base to diff against", pr.Number, pr.Repo)
	}
	got, err := Fetch(ctx, pr, pr.Path, head, pr.Base)
	if err != nil {
		return Fetched{}, "", err
	}
	out, err := Run(ctx, pr.Path, "git", "-c", "core.quotePath=false", "--no-pager", "diff", "--no-color", "--no-ext-diff",
		"--no-textconv", "--find-renames", "--src-prefix=a/", "--dst-prefix=b/", "origin/"+pr.Base+"..."+got.Ref)
	if err != nil {
		return Fetched{}, "", fmt.Errorf("git could not diff #%d in %s: %w", pr.Number, pr.Repo, err)
	}
	return got, string(out), nil
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
