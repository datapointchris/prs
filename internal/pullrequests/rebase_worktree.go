package pullrequests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// treeID is the tree git merge-tree writes on its first line, SHA-1 or SHA-256.
var treeID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ConflictingFiles is each file a merge of origin/<base> into the head fetched into the
// target's Ref conflicts in, from git merge-tree, which checks nothing out. A rebase stops on
// the same files, a commit at a time. It is empty where git merges the two cleanly here.
// merge-tree exits 1 both for a conflict and for a ref it cannot read, so a conflict is the
// one that wrote a tree.
func ConflictingFiles(ctx context.Context, t LocalDiff) ([]string, error) {
	out, said, err := run(ctx, t.Checkout, "git", "merge-tree", "--write-tree", "--name-only", "origin/"+t.Base, t.Ref())
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	switch {
	case err == nil:
		return nil, nil
	case !exitedWith(err, 1) || !treeID.MatchString(lines[0]):
		return nil, fmt.Errorf("git could not merge origin/%s into #%d's head here: %s", t.Base, t.Number, orError(said, err))
	}
	var files []string
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		files = append(files, line)
	}
	return files, nil
}

// RebaseWorktree is a worktree of a pull request's checkout, detached at its head branch on
// origin, where the fix menu rebases the head onto origin/<base> for a person to resolve each
// stop. Detached, it moves no local branch, so a worktree a session holds on the head branch,
// or a local branch with commits not pushed, is left as it is, and the rebase is force-pushed
// to the head branch on origin with a lease.
type RebaseWorktree struct {
	Target LocalDiff
	Path   string
	// Branch is the head branch on origin the rebase starts from and is pushed to.
	Branch string
}

// RebaseWorktreeAt is where the fix menu rebases the target's head: pr-<number>-rebase in the
// directory under root named for the checkout's own directory. A tool keeping its own
// worktree of the pull request beside it more likely names it pr-<number>.
func RebaseWorktreeAt(root string, t LocalDiff, branch string) RebaseWorktree {
	return RebaseWorktree{Target: t, Path: filepath.Join(root, filepath.Base(t.Checkout), "pr-"+strconv.Itoa(t.Number)+"-rebase"), Branch: branch}
}

// Rebase is where a rebase in a RebaseWorktree stands.
type Rebase int

const (
	// NoRebase is no worktree, or one with no rebase under way and nothing to push.
	NoRebase Rebase = iota
	// Stopped is a rebase under way, stopped at a commit for a person to resolve.
	Stopped
	// Rebased is a rebase finished and not pushed yet.
	Rebased
)

// Exists says something is at the worktree's path.
func (w RebaseWorktree) Exists() bool {
	_, err := os.Stat(w.Path)
	return err == nil
}

// State is where a rebase in the worktree stands.
func (w RebaseWorktree) State(ctx context.Context) Rebase {
	switch {
	case !w.Exists():
		return NoRebase
	case w.rebasing(ctx):
		return Stopped
	case w.unpushed(ctx):
		return Rebased
	}
	return NoRebase
}

// tracking is the remote-tracking ref the head branch is fetched into, as a person's own
// git fetch would.
func (w RebaseWorktree) tracking() string { return "refs/remotes/origin/" + w.Branch }

// Start rebases the head branch onto origin/<base> in the worktree, and says where the rebase
// then stands. The head branch is fetched into origin/<branch> first, and the worktree added
// there, or moved there where it holds no rebase. A rebase already under way in it, or
// finished and not pushed, is kept as it is. A worktree holding changes is refused rather than
// lost, and one on a branch is no worktree prs added. A head that already holds the base needs
// no rebase, and the worktree goes again.
func (w RebaseWorktree) Start(ctx context.Context) (Rebase, error) {
	lock := checkoutLock(w.Target.Checkout)
	lock.Lock()
	defer lock.Unlock()
	registered, err := w.registered(ctx)
	if err != nil {
		return NoRebase, err
	}
	switch {
	case registered && w.rebasing(ctx):
		return Stopped, nil
	case registered && w.unpushed(ctx):
		return Rebased, nil
	}
	if _, err := Run(ctx, w.Target.Checkout, "git", "fetch", "--quiet", "origin", "+refs/heads/"+w.Branch+":"+w.tracking()); err != nil {
		return NoRebase, fmt.Errorf("could not fetch %s for #%d in %s: %w", w.Branch, w.Target.Number, w.Target.Repo, err)
	}
	switch {
	case registered:
		if branch, _, err := run(ctx, w.Path, "git", "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			return NoRebase, fmt.Errorf("%s is a worktree on %s, which prs did not add, so prs leaves it as it is", w.Path, strings.TrimSpace(string(branch)))
		}
		if changed, err := w.git(ctx, "status", "--porcelain"); err != nil || len(changed) > 0 {
			return NoRebase, fmt.Errorf("%s holds changes and no rebase is under way there, so prs leaves it as it is: %v", w.Path, orNothing(err, "commit or remove them"))
		}
		if _, err := w.git(ctx, "checkout", "--quiet", "--detach", w.tracking()); err != nil {
			return NoRebase, fmt.Errorf("could not move %s to #%d's head: %w", w.Path, w.Target.Number, err)
		}
	case w.Exists():
		return NoRebase, fmt.Errorf("%s is in the way, and it is no worktree of %s", w.Path, w.Target.Checkout)
	default:
		if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
			return NoRebase, err
		}
		if _, err := Run(ctx, w.Target.Checkout, "git", "worktree", "add", "--quiet", "--detach", w.Path, w.tracking()); err != nil {
			return NoRebase, fmt.Errorf("could not add a worktree for #%d at %s: %w", w.Target.Number, w.Path, err)
		}
	}
	_, said, err := runFor(ctx, hooksTimeout, w.Path, "git", "rebase", "--quiet", "origin/"+w.Target.Base)
	switch {
	case w.rebasing(ctx):
		return Stopped, nil
	case err != nil:
		return NoRebase, fmt.Errorf("git could not rebase #%d onto origin/%s in %s: %s", w.Target.Number, w.Target.Base, w.Path, orError(said, err))
	case !w.unpushed(ctx):
		return NoRebase, w.removeHeld(ctx)
	}
	return Rebased, nil
}

// Continue goes on with the rebase once its stop is resolved and staged, and says where it
// then stands: stopped at the next commit to resolve, or finished. Each commit keeps its
// message, because no client prs runs opens an editor. A stop git cannot go on from with
// nothing unmerged, such as a commit the resolution left empty, stays stopped, and git's
// words say why.
func (w RebaseWorktree) Continue(ctx context.Context) (Rebase, error) {
	_, said, err := runFor(ctx, hooksTimeout, w.Path, "git", "rebase", "--continue")
	switch {
	case !w.rebasing(ctx) && err != nil:
		return NoRebase, fmt.Errorf("git could not go on with the rebase in %s: %s", w.Path, orError(said, err))
	case !w.rebasing(ctx):
		return Rebased, nil
	case err == nil:
		return Stopped, nil
	}
	if unmerged, uerr := w.Unmerged(ctx); uerr == nil && len(unmerged) == 0 {
		return Stopped, fmt.Errorf("git could not go on with the rebase in %s: %s", w.Path, orError(said, err))
	}
	return Stopped, nil
}

// registered says git lists the worktree's path as a worktree of the checkout.
func (w RebaseWorktree) registered(ctx context.Context) (bool, error) {
	out, err := Run(ctx, w.Target.Checkout, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git could not list %s's worktrees: %w", w.Target.Repo, err)
	}
	for line := range strings.Lines(string(out)) {
		if path, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "worktree "); ok && filepath.Clean(path) == filepath.Clean(w.Path) {
			return true, nil
		}
	}
	return false, nil
}

// rebasing says a rebase is under way in the worktree, by the directory git keeps one in.
func (w RebaseWorktree) rebasing(ctx context.Context) bool {
	out, err := w.git(ctx, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge", "--git-path", "rebase-apply")
	if err != nil {
		return false
	}
	for dir := range strings.Lines(string(out)) {
		if info, err := os.Stat(strings.TrimRight(dir, "\n")); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// unpushed says the worktree's HEAD is not the head branch's commit on origin as last fetched,
// so it holds a rebase nobody pushed.
func (w RebaseWorktree) unpushed(ctx context.Context) bool {
	out, err := w.git(ctx, "rev-parse", "HEAD", w.tracking())
	heads := strings.Fields(string(out))
	return err == nil && len(heads) == 2 && heads[0] != heads[1]
}

// Unmerged is each path the rebase's stop left conflicted that is not resolved and staged yet.
func (w RebaseWorktree) Unmerged(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("git could not say what is still unmerged in %s: %w", w.Path, err)
	}
	return strings.Fields(string(out)), nil
}

// Commits is each commit the rebase put on origin/<base>, as git log --oneline prints it.
func (w RebaseWorktree) Commits(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "log", "--oneline", "origin/"+w.Target.Base+"..HEAD")
	if err != nil {
		return nil, fmt.Errorf("git could not list the commits rebased in %s: %w", w.Path, err)
	}
	var commits []string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimRight(line, "\n"); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// Pushed is what Push left: the rebased head on the head branch, and why the worktree is still
// there where removing it failed.
type Pushed struct {
	Commit   string
	Unremove string
}

// Push force-pushes the finished rebase to the head branch on origin, then removes the
// worktree. The lease is on ORIG_HEAD, the head the rebase started from, so origin refuses the
// push where anyone pushed to the branch since, and a refused push leaves the rebase where it
// is. The lease names its commit, because git's --force-if-includes reads the reflog of a
// local branch of the head's name, and a detached worktree pushes from none.
func (w RebaseWorktree) Push(ctx context.Context) (Pushed, error) {
	from, err := w.git(ctx, "rev-parse", "--verify", "--quiet", "ORIG_HEAD")
	if err != nil {
		return Pushed{}, fmt.Errorf("git names no ORIG_HEAD in %s, so prs cannot say which head the rebase started from: %w", w.Path, err)
	}
	lease := "--force-with-lease=refs/heads/" + w.Branch + ":" + strings.TrimSpace(string(from))
	if _, err := RunWithin(ctx, hooksTimeout, w.Path, "git", "push", "--quiet", lease, "origin", "HEAD:refs/heads/"+w.Branch); err != nil {
		return Pushed{}, fmt.Errorf("git could not push the rebase in %s to %s: %w", w.Path, w.Branch, err)
	}
	out, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Pushed{}, fmt.Errorf("pushed the rebase in %s, and git could not name its head: %w", w.Path, err)
	}
	pushed := Pushed{Commit: strings.TrimSpace(string(out))}
	if err := w.Remove(ctx); err != nil {
		pushed.Unremove = err.Error()
	}
	return pushed, nil
}

// Abort abandons any rebase under way and removes the worktree, and says whether a rebase was.
func (w RebaseWorktree) Abort(ctx context.Context) (bool, error) {
	rebasing := w.rebasing(ctx)
	if rebasing {
		if _, err := w.git(ctx, "rebase", "--abort"); err != nil {
			return true, fmt.Errorf("git could not abort the rebase in %s: %w", w.Path, err)
		}
	}
	return rebasing, w.Remove(ctx)
}

// Remove removes the worktree from the checkout, which git refuses while it holds changes.
func (w RebaseWorktree) Remove(ctx context.Context) error {
	lock := checkoutLock(w.Target.Checkout)
	lock.Lock()
	defer lock.Unlock()
	return w.removeHeld(ctx)
}

// removeHeld is Remove for a caller already holding the checkout's lock. The checkout's
// directory under the root goes with its last worktree, so the root holds a directory for a
// repo exactly where a worktree of it is.
func (w RebaseWorktree) removeHeld(ctx context.Context) error {
	if _, err := Run(ctx, w.Target.Checkout, "git", "worktree", "remove", w.Path); err != nil {
		return fmt.Errorf("git could not remove the worktree at %s: %w", w.Path, err)
	}
	_ = os.Remove(filepath.Dir(w.Path))
	return nil
}

func (w RebaseWorktree) git(ctx context.Context, args ...string) ([]byte, error) {
	return Run(ctx, w.Path, append([]string{"git"}, args...)...)
}

// orNothing is err's text, or else what to do instead where there was no error.
func orNothing(err error, instead string) string {
	if err != nil {
		return err.Error()
	}
	return instead
}
