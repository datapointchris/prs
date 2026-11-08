// Package pullrequests reads the open pull requests on the repos a registry names, from
// each provider that hosts one.
//
// GitHub is read in-process through go-gh, with the token gh holds or GH_TOKEN, so prs
// keeps no credentials of its own. Bitbucket is read through bbkt, which holds its own.
package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/datapointchris/prs/internal/registry"
)

// PullRequest is one pull request, in the fields `prs list --json` prints, in the order it
// prints them. cmd/prs/list.schema.json is that shape.
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
	Bot    *bool  `json:"bot"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Draft  bool   `json:"draft"`
	// HeadInFork says the head branch is in another repo than the pull request's, which
	// origin is not, so the fix menu pushes no merge to it. Bitbucket's listing does not say,
	// so it stays out of --json rather than reading false there.
	HeadInFork bool      `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
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
	// State is OPEN, MERGED or CLOSED on GitHub, and bbkt's word on Bitbucket.
	State string `json:"state"`
	// The times are null until they happen, and UpdatedAt where the listing omits it.
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	UpdatedAt *time.Time `json:"updated_at"`
	// Mergeable and MergeStateStatus are GitHub's enums, which read UNKNOWN until GitHub has
	// computed them. They, InMergeQueue and HeadOID are null where the listing does not
	// carry them, as bbkt's does not.
	Mergeable        *string `json:"mergeable"`
	MergeStateStatus *string `json:"merge_state_status"`
	InMergeQueue     *bool   `json:"in_merge_queue"`
	HeadOID          *string `json:"head_oid"`
}

// Standing is where a pull request stands, in the three words prs acts on, whatever word its
// host prints for it.
type Standing int

const (
	Open Standing = iota
	Merged
	Closed
)

func (s Standing) String() string { return [...]string{"open", "merged", "closed"}[s] }

// Standing is where the pull request stands, by its provider's Standings. A word the
// provider does not name is refused, because a guess would draw a declined pull request as
// open, or the reverse.
func (pr PullRequest) Standing() (Standing, error) {
	provider, err := ProviderOf(pr.Provider)
	if err != nil {
		return Open, err
	}
	standing, known := provider.Standings[pr.State]
	if !known {
		return Open, fmt.Errorf("%s says %s#%d is %s, a state prs does not know", pr.Provider, pr.Slug, pr.Number, pr.State)
	}
	return standing, nil
}

// IsBot says an app opened it. One whose listing does not say, or whose account is gone,
// counts as a person's.
func (pr PullRequest) IsBot() bool { return pr.Bot != nil && *pr.Bot }

func authorIsBot(typename string) bool { return typename == "Bot" }

// Key names a pull request across reads, when its position and every field can change. It
// carries the provider, because a GitHub repo and a Bitbucket one can share owner/name.
func (pr PullRequest) Key() string {
	return pr.Provider + ":" + pr.Slug + "#" + strconv.Itoa(pr.Number)
}

// Listing is what one provider's read found: its pull requests, and what it could not
// cover while still reading.
type Listing struct {
	PullRequests []PullRequest
	Notes        []string
}

// Note is the listing's notes as one line.
func (l Listing) Note() string { return strings.Join(l.Notes, "; ") }

// Scope is what one provider's read covers: the registry's repos it hosts, each asked for
// by name, or with no registry the whole of some owners.
type Scope struct {
	Repos []registry.Repo
	// Owners are GitHub owners searched whole. ViewerOwner stands for whoever the token is.
	Owners []string
}

// ViewerOwner is the owner GitHub's search reads as the token's own account.
const ViewerOwner = "@me"

// Empty says the scope names nothing to read.
func (s Scope) Empty() bool { return len(s.Repos) == 0 && len(s.Owners) == 0 }

// Names are the scope's repos as owner/name and its owners, each in lower case, sorted and
// named once, because GitHub matches both in any case. A search asks for them, and a cache
// of what the scope read is named by them, so a registry edit that changes neither keeps
// its cache. Neither is nil.
func (s Scope) Names() (repos, owners []string) {
	repos, owners = []string{}, []string{}
	for _, repo := range s.Repos {
		repos = append(repos, strings.ToLower(repo.Slug))
	}
	for _, owner := range s.Owners {
		owners = append(owners, strings.ToLower(owner))
	}
	slices.Sort(repos)
	slices.Sort(owners)
	return slices.Compact(repos), slices.Compact(owners)
}

// readTimeout bounds one client call, so a hung network never holds a read forever.
const readTimeout = 2 * time.Minute

// pipesGrace is how long a client's Wait waits on pipes a process it started still holds,
// once the client itself has ended.
const pipesGrace = 5 * time.Second

// hooksTimeout bounds a git client that runs the repo's own hooks, such as a commit or a push,
// which can run a repo's whole test suite.
const hooksTimeout = 10 * time.Minute

// ErrNoAnswer is a client killed at its timeout, which errors.Is finds whatever the timeout
// was. A client that changes a pull request may have changed it by then.
var ErrNoAnswer = errors.New("no answer")

// noAnswer is ErrNoAnswer naming the timeout.
type noAnswer struct{ within time.Duration }

func (e noAnswer) Error() string        { return fmt.Sprintf("no answer in %s", e.within) }
func (e noAnswer) Is(target error) bool { return target == ErrNoAnswer }

// Run runs a client a key uses as a read runs one, and returns its stdout. It fails with the
// first line the client said, or with ErrNoAnswer.
func Run(ctx context.Context, dir string, argv ...string) ([]byte, error) {
	return RunWithin(ctx, readTimeout, dir, argv...)
}

// RunWithin is Run, bounded by within rather than a read's timeout.
func RunWithin(ctx context.Context, within time.Duration, dir string, argv ...string) ([]byte, error) {
	out, said, err := runFor(ctx, within, dir, argv[0], argv[1:]...)
	switch {
	case errors.Is(err, ErrNoAnswer), err != nil && said == "":
		return out, err
	case err != nil:
		return out, errors.New(said)
	}
	return out, nil
}

// RunDetached runs a command that is to finish whether or not prs is still open, such as one
// waiting on a model call, and returns when it ends. It runs as run runs a client, in a
// session of its own with stdin closed, with its output written to the file at log in place
// of a pipe, so neither prs exiting nor its terminal closing stops it, and nothing bounds it.
// It fails with the last line the command wrote.
func RunDetached(dir, log string, argv ...string) error {
	out, err := os.Create(log)
	if err != nil {
		return fmt.Errorf("could not open the log %s: %w", log, err)
	}
	defer func() { _ = out.Close() }()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		if line := lastLineOf(log); line != "" {
			return errors.New(line)
		}
		return err
	}
	return nil
}

// lastLineOf is the file's last line holding text, and empty where it holds none or cannot be read.
func lastLineOf(path string) string {
	written, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(written)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// run runs a client with stdin closed and returns its stdout, and failureLine of its stderr on
// a failure. git and ssh ask on the terminal itself rather than on stdin, so the client runs
// in a session of its own, with no terminal to open, and git's prompt is turned off as well.
// git's editor is true, so a commit git words itself, as a rebase going on does, keeps the
// message it has rather than wait on an editor nobody can see. At the timeout its whole
// process group is killed, ssh included.
func run(ctx context.Context, dir string, name string, args ...string) ([]byte, string, error) {
	return runFor(ctx, readTimeout, dir, name, args...)
}

// runFor is run bounded by within.
func runFor(ctx context.Context, within time.Duration, dir string, name string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = pipesGrace
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		late := noAnswer{within: within}
		return out, late.Error(), late
	}
	if err != nil {
		said := strings.TrimSpace(stderr.String())
		if said == "" {
			said = strings.TrimSpace(string(out))
		}
		return out, failureLine(said), err
	}
	return out, "", nil
}

// failureLine is the line of what a client said that names its failure: the first naming a
// ref git refused, as ` ! [rejected] HEAD -> main (fetch first)` under a push's `To <remote>`,
// and otherwise the first.
func failureLine(said string) string {
	for line := range strings.Lines(said) {
		if refused, ok := strings.CutPrefix(strings.TrimSpace(line), "! "); ok {
			return "! " + strings.Join(strings.Fields(refused), " ")
		}
	}
	first, _, _ := strings.Cut(said, "\n")
	return first
}

// Finish sets each pull request's age and its repo's checkout, and sorts them oldest
// first, because a backlog is a gauge and age is what it measures. A slug matches in any
// case, as GitHub matches owners and names.
func Finish(prs []PullRequest, repos []registry.Repo, now time.Time) []PullRequest {
	paths := make(map[string]string, len(repos))
	for _, repo := range repos {
		paths[strings.ToLower(repo.Slug)] = repo.Path
	}
	for i := range prs {
		prs[i].Path = paths[strings.ToLower(prs[i].Slug)]
		if !prs[i].CreatedAt.IsZero() {
			prs[i].AgeDays = int(now.Sub(prs[i].CreatedAt).Hours() / 24)
		}
	}
	slices.SortStableFunc(prs, func(a, b PullRequest) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return prs
}
