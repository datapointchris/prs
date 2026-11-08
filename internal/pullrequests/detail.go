package pullrequests

import (
	"context"
	"sync"
	"time"
)

// Detail is what a pull request's own read adds to its row: who reviewed it at which
// commit, its conversation, its files, its commits and its head's checks. Each list holds
// as many as its query asks for, and each count is the whole.
type Detail struct {
	// HeadOID and UpdatedAt are the pull request's as this read found them. A listing
	// whose row differs in either reads it again.
	HeadOID        string    `json:"head_oid"`
	UpdatedAt      time.Time `json:"updated_at"`
	ReadAt         time.Time `json:"read_at"`
	ReviewRequests []string  `json:"review_requests"`
	Reviews        []Review  `json:"reviews"`
	ReviewCount    int       `json:"review_count"`
	Comments       []Comment `json:"comments"`
	CommentCount   int       `json:"comment_count"`
	Threads        []Thread  `json:"threads"`
	ThreadCount    int       `json:"thread_count"`
	Files          []File    `json:"files"`
	FileCount      int       `json:"file_count"`
	Commits        []Commit  `json:"commits"`
	CommitCount    int       `json:"commit_count"`
	Checks         []Check   `json:"checks"`
}

// Review is one submitted review, and the commit it was submitted against, which is what
// says how much changed since.
type Review struct {
	Author string `json:"author"`
	State  string `json:"state"`
	Body   string `json:"body"`
	// SubmittedAt is null for a review still pending, and CommitOID empty where the commit
	// is gone from the branch.
	SubmittedAt *time.Time `json:"submitted_at"`
	CommitOID   string     `json:"commit_oid"`
	URL         string     `json:"url"`
}

// Comment is one comment, on the conversation or in a review thread.
type Comment struct {
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
}

// Thread is one review thread on a line of a file. Line is null where the thread's line is
// gone from the diff.
type Thread struct {
	Path         string    `json:"path"`
	Line         *int      `json:"line"`
	Resolved     bool      `json:"resolved"`
	Outdated     bool      `json:"outdated"`
	Comments     []Comment `json:"comments"`
	CommentCount int       `json:"comment_count"`
}

// File is one changed file. Change is the provider's word, such as MODIFIED or ADDED.
type File struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Change    string `json:"change"`
}

// Commit is one commit on the branch, newest last.
type Commit struct {
	OID         string    `json:"oid"`
	Headline    string    `json:"headline"`
	Author      string    `json:"author"`
	CommittedAt time.Time `json:"committed_at"`
}

// Check is one check run or commit status on the head commit. State is a run's conclusion
// once it has one and its status before, or a status's state.
type Check struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url"`
}

// detailsAtOnce is how many pull requests' details are read at the same time.
const detailsAtOnce = 4

// Details holds each pull request's Detail in memory, by Key, read in the background
// after a listing lands, so moving the cursor never waits on the network. An entry is read
// again only when its row's head commit or update time moves, so an unchanged pull request
// is asked for once.
type Details struct {
	mu      sync.Mutex
	entries map[string]*detailEntry
}

type detailEntry struct {
	provider string
	detail   Detail
	held     bool
	reading  bool
	fault    string
}

// DetailState is how one pull request's detail stands.
type DetailState struct {
	Detail Detail
	// Held says Detail was read. A read under way or failed keeps the last one held.
	Held    bool
	Reading bool
	// Fault is why the newest read failed, and empty where it read.
	Fault string
}

// NewDetails is an empty cache.
func NewDetails() *Details { return &Details{entries: map[string]*detailEntry{}} }

// Of is how the pull request's detail stands now.
func (d *Details) Of(pr PullRequest) DetailState {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[pr.Key()]
	if !ok {
		return DetailState{}
	}
	return DetailState{Detail: entry.detail, Held: entry.held, Reading: entry.reading, Fault: entry.fault}
}

// Prefetch reads, in the background and detailsAtOnce at a time, the detail of each of
// provider's pull requests in prs whose held detail is missing, failed or older than its
// row. prs is that provider's whole listing, so it also drops the detail of each of the
// provider's pull requests prs does not hold. It returns at once, and the channel closes
// when every read it started has ended.
func (d *Details) Prefetch(ctx context.Context, provider Provider, prs []PullRequest) <-chan struct{} {
	done := make(chan struct{})
	if provider.Detail == nil {
		close(done)
		return done
	}
	due := d.claim(provider.Name, prs)
	go func() {
		defer close(done)
		slots := make(chan struct{}, detailsAtOnce)
		var wg sync.WaitGroup
		for _, pr := range due {
			slots <- struct{}{}
			wg.Go(func() {
				defer func() { <-slots }()
				detail, err := provider.Detail(ctx, pr)
				d.store(pr, detail, err)
			})
		}
		wg.Wait()
	}()
	return done
}

// claim marks each pull request whose detail is due as reading, and drops the entry of
// each of the provider's pull requests prs does not hold.
func (d *Details) claim(provider string, prs []PullRequest) []PullRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	listed := make(map[string]bool, len(prs))
	var due []PullRequest
	for _, pr := range prs {
		listed[pr.Key()] = true
		entry, ok := d.entries[pr.Key()]
		if !ok {
			entry = &detailEntry{provider: provider}
			d.entries[pr.Key()] = entry
		}
		if entry.reading || (entry.held && entry.fault == "" && current(entry.detail, pr)) {
			continue
		}
		entry.reading = true
		due = append(due, pr)
	}
	for key, entry := range d.entries {
		if entry.provider == provider && !listed[key] && !entry.reading {
			delete(d.entries, key)
		}
	}
	return due
}

// current says the detail was read at the row's head commit and update time.
func current(detail Detail, pr PullRequest) bool {
	return pr.HeadOID != nil && pr.UpdatedAt != nil &&
		detail.HeadOID == *pr.HeadOID && detail.UpdatedAt.Equal(*pr.UpdatedAt)
}

func (d *Details) store(pr PullRequest, detail Detail, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// claim keeps an entry while it is reading, so the one this read claimed is still there.
	entry := d.entries[pr.Key()]
	entry.reading = false
	if err != nil {
		entry.fault = err.Error()
		return
	}
	entry.detail, entry.held, entry.fault = detail, true, ""
}
