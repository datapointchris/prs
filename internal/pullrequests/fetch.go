package pullrequests

import (
	"context"
	"fmt"
	"os"
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

// CheckoutOf is the pull request's checkout on this disk, which d, a window action and bbkt
// each run in.
func CheckoutOf(pr PullRequest) (string, error) {
	if pr.Path == "" {
		return "", fmt.Errorf("%s is not checked out on this machine", pr.Repo)
	}
	if info, err := os.Stat(pr.Path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is registered at %s, which does not exist", pr.Repo, pr.Path)
	}
	return pr.Path, nil
}

// LocalDiff is a pull request as git diffs it in its checkout here: its head fetched into a
// ref of its own, diffed three-dot from origin/<base>, from where the branch left its base,
// which is what the provider shows. d opens it, and the Diff section reads it where the
// host's client has no diff.
type LocalDiff struct {
	Number   int
	Repo     string
	Checkout string
	// Head is the pull request's head on origin, as its provider names it.
	Head string
	// Base is the branch the listing says it targets, or origin's default where it names none.
	Base string
}

// LocalDiffOf is the pull request's LocalDiff, or why it has none. The base is the one the
// listing reports, because a pull request stacked on another targets its parent, and a diff
// against main would show both as one change.
func LocalDiffOf(ctx context.Context, pr PullRequest) (LocalDiff, error) {
	provider, err := ProviderOf(pr.Provider)
	if err != nil {
		return LocalDiff{}, err
	}
	checkout, err := CheckoutOf(pr)
	if err != nil {
		return LocalDiff{}, err
	}
	target := LocalDiff{Number: pr.Number, Repo: pr.Repo, Checkout: checkout, Head: provider.Head(pr), Base: pr.Base}
	if target.Base == "" {
		target.Base = defaultBranch(ctx, checkout)
	}
	switch {
	case target.Base == "":
		return LocalDiff{}, fmt.Errorf("could not work out what #%d is based on", pr.Number)
	case target.Head == "":
		return LocalDiff{}, fmt.Errorf("#%d in %s names no source branch to fetch", pr.Number, pr.Repo)
	}
	return target, nil
}

// Ref is where the head is fetched to. It sits under refs/remotes/origin, so it never shows
// as a branch.
func (t LocalDiff) Ref() string { return "refs/remotes/origin/pr/" + strconv.Itoa(t.Number) }

// Range is what git diffs: three-dot, from where the branch left origin/<base>.
func (t LocalDiff) Range() string { return "origin/" + t.Base + "..." + t.Ref() }

// RangeFrom is what git diffs from commit, which the head holds, to the head: what was pushed
// after it.
func (t LocalDiff) RangeFrom(commit string) string { return commit + ".." + t.Ref() }

// HeadHolds says the head fetched into the target's Ref has commit in its history. A commit
// git does not have here is in no history the fetch brought.
func HeadHolds(ctx context.Context, t LocalDiff, commit string) bool {
	_, err := Run(ctx, t.Checkout, "git", "merge-base", "--is-ancestor", commit, t.Ref())
	return err == nil
}

// defaultBranch is the branch origin's HEAD names, for a provider whose listing reports no
// base.
func defaultBranch(ctx context.Context, checkout string) string {
	out, err := Run(ctx, checkout, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
}

// Fetched is what a fetch left in the checkout.
type Fetched struct {
	// Head is the commit the target's Ref holds.
	Head string
	// BaseFault is why fetching the base failed. A diff then runs against the origin/<base>
	// an earlier fetch left.
	BaseFault string
}

// Fetch is d's fetch, which the person asked for: the target's head into its Ref, then its
// base, as their own git fetch would move origin/<base>. It never checks the branch out,
// which would abort on uncommitted work and disturb the tree where there is none.
func Fetch(ctx context.Context, t LocalDiff) (Fetched, error) { return fetch(ctx, t, true) }

// FetchHead is the Diff section's fetch, which nobody asked for: the target's head into its
// Ref and nothing more. It writes no FETCH_HEAD and starts no maintenance, so every ref and
// file another program reads stays as it was, and the diff runs against the origin/<base>
// already there.
func FetchHead(ctx context.Context, t LocalDiff) (Fetched, error) { return fetch(ctx, t, false) }

func fetch(ctx context.Context, t LocalDiff, withBase bool) (Fetched, error) {
	lock := checkoutLock(t.Checkout)
	lock.Lock()
	defer lock.Unlock()
	head := []string{"git", "fetch", "--quiet", "--force"}
	if !withBase {
		head = append(head, "--no-write-fetch-head", "--no-auto-maintenance")
	}
	if _, err := Run(ctx, t.Checkout, append(head, "origin", t.Head+":"+t.Ref())...); err != nil {
		return Fetched{}, fmt.Errorf("could not fetch #%d in %s: %w", t.Number, t.Repo, err)
	}
	var got Fetched
	if withBase {
		if _, err := Run(ctx, t.Checkout, "git", "fetch", "--quiet", "origin", t.Base); err != nil {
			got.BaseFault = err.Error()
		}
	}
	if _, err := Run(ctx, t.Checkout, "git", "rev-parse", "--verify", "--quiet", "origin/"+t.Base); err != nil {
		switch {
		case !withBase:
			return Fetched{}, fmt.Errorf("%s has no origin/%s to diff against, which d fetches", t.Repo, t.Base)
		case got.BaseFault != "":
			return Fetched{}, fmt.Errorf("%s has no origin/%s to diff against: %s", t.Repo, t.Base, got.BaseFault)
		}
		return Fetched{}, fmt.Errorf("%s has no origin/%s to diff against", t.Repo, t.Base)
	}
	out, err := Run(ctx, t.Checkout, "git", "rev-parse", "--verify", t.Ref())
	if err != nil {
		return Fetched{}, fmt.Errorf("could not read the head fetched for #%d: %w", t.Number, err)
	}
	got.Head = strings.TrimSpace(string(out))
	return got, nil
}
