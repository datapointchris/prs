package pullrequests

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// checkouts holds a lock per checkout path, so git fetches into one checkout one at a time.
// Two at once contend for its ref locks, and one of them fails.
var checkouts = struct {
	sync.Mutex
	locks map[string]*sync.Mutex
}{locks: map[string]*sync.Mutex{}}

func checkoutLock(dir string) *sync.Mutex {
	checkouts.Lock()
	defer checkouts.Unlock()
	lock, ok := checkouts.locks[dir]
	if !ok {
		lock = &sync.Mutex{}
		checkouts.locks[dir] = lock
	}
	return lock
}

// Fetched is a pull request's head as Fetch left it in a checkout.
type Fetched struct {
	// Ref holds the head, and Head is its commit.
	Ref  string
	Head string
	// BaseFault is why fetching the base failed. A diff then runs against the origin/<base>
	// an earlier fetch left.
	BaseFault string
}

// Fetch fetches head, the pull request's head on origin, into a ref of its own in checkout,
// then base, so a three-dot diff from origin/<base> starts where the branch left it. It never
// checks the branch out, which would abort on uncommitted work and disturb the tree where
// there is none. The ref sits under refs/remotes/origin, so it never shows as a branch.
func Fetch(ctx context.Context, pr PullRequest, checkout, head, base string) (Fetched, error) {
	lock := checkoutLock(checkout)
	lock.Lock()
	defer lock.Unlock()
	got := Fetched{Ref: "refs/remotes/origin/pr/" + strconv.Itoa(pr.Number)}
	if _, err := Run(ctx, checkout, "git", "fetch", "--quiet", "--force", "origin", head+":"+got.Ref); err != nil {
		return Fetched{}, fmt.Errorf("could not fetch #%d in %s: %w", pr.Number, pr.Repo, err)
	}
	if _, err := Run(ctx, checkout, "git", "fetch", "--quiet", "origin", base); err != nil {
		got.BaseFault = err.Error()
	}
	if _, err := Run(ctx, checkout, "git", "rev-parse", "--verify", "--quiet", "origin/"+base); err != nil {
		said := got.BaseFault
		if said == "" {
			said = "-"
		}
		return Fetched{}, fmt.Errorf("%s has no origin/%s to diff against: %s", pr.Repo, base, said)
	}
	out, err := Run(ctx, checkout, "git", "rev-parse", "--verify", got.Ref)
	if err != nil {
		return Fetched{}, fmt.Errorf("could not read the head fetched for #%d: %w", pr.Number, err)
	}
	got.Head = strings.TrimSpace(string(out))
	return got, nil
}
