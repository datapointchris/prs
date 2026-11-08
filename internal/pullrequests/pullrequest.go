// Package pullrequests reads the open pull requests on the repos a registry names, from
// each provider that hosts one, through the provider's own command-line client.
//
// gh and bbkt are run rather than replaced with an HTTP client, because they hold the
// credentials.
package pullrequests

import (
	"context"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PullRequest is one open pull request, in the fields and names pull-requests printed.
type PullRequest struct {
	// Provider says who hosts it. Every action but the local diff dispatches on it.
	Provider string `json:"provider"`
	// Repo is the bare name and only a label, since two repositories can share one. Slug
	// identifies the repository.
	Repo   string `json:"repo"`
	Slug   string `json:"slug"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	// Author is the login that opened it, and empty for a deleted account.
	Author string `json:"author"`
	// Bot is true where the provider says an app opened it, and null where its listing
	// does not say.
	Bot       *bool     `json:"bot"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	Branch    string    `json:"branch"`
	Base      string    `json:"base"`
	Draft     bool      `json:"draft"`
	CreatedAt time.Time `json:"created_at"`
	// The diff counts are null, never 0, from a provider whose listing does not count
	// them. A pull request nobody counted is not one that changed nothing.
	Additions    *int `json:"additions"`
	Deletions    *int `json:"deletions"`
	ChangedFiles *int `json:"changed_files"`
	// Review is the provider's decision, and Reviews how many were submitted. A review can
	// set no decision, so the two disagree routinely. Both review and checks are empty
	// until a review is requested and a workflow has run.
	Review  string `json:"review"`
	Reviews int    `json:"reviews"`
	Checks  string `json:"checks"`
	// AgeDays is how long it has been open, and Path where its repo is checked out here.
	AgeDays int    `json:"age_days"`
	Path    string `json:"path"`
}

// Key names a pull request across reads, when its position and every field can change.
func (pr PullRequest) Key() string { return pr.Slug + "#" + strconv.Itoa(pr.Number) }

// Listing is what one provider's read found: its pull requests, and what it could not
// cover while still reading.
type Listing struct {
	PullRequests []PullRequest
	Notes        []string
}

// Note is the listing's notes as one line.
func (l Listing) Note() string { return strings.Join(l.Notes, "; ") }

// readTimeout bounds one client call, so a hung network never holds a read forever.
const readTimeout = 2 * time.Minute

// run runs a client with stdin closed and returns its stdout, and stderr's first line on
// a failure.
func run(ctx context.Context, dir string, name string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		said := strings.TrimSpace(stderr.String())
		if said == "" {
			said = strings.TrimSpace(string(out))
		}
		first, _, _ := strings.Cut(said, "\n")
		return out, first, err
	}
	return out, "", nil
}

// Finish sets each pull request's age and checkout path, and sorts them oldest first,
// because a backlog is a gauge and age is what it measures.
func Finish(prs []PullRequest, paths map[string]string, now time.Time) []PullRequest {
	for i := range prs {
		prs[i].Path = paths[prs[i].Slug]
		if !prs[i].CreatedAt.IsZero() {
			prs[i].AgeDays = int(now.Sub(prs[i].CreatedAt).Hours() / 24)
		}
	}
	slices.SortStableFunc(prs, func(a, b PullRequest) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return prs
}
