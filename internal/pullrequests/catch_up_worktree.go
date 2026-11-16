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
// target's Ref conflicts in, from git merge-tree, which checks nothing out. The catch-up merge
// stops on the same files. It is empty where git merges the two cleanly here. merge-tree
// exits 1 both for a conflict and for a ref it cannot read, so a conflict is the one that
// wrote a tree.
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

// CatchUpWorktree is a worktree of a pull request's checkout, detached at its head branch on
// origin, where the fix menu merges origin/<base> into the head for a person to resolve its
// conflicts. Detached, it moves no local branch, so a worktree a session holds on the head
// branch, or a local branch with commits not pushed, is left as it is. A merge only adds
// commits, so the head branch on origin fast-forwards to it, and no push is forced.
type CatchUpWorktree struct {
	Target LocalDiff
	Path   string
	// Branch is the head branch on origin the merge starts from and is pushed to.
	Branch string
}

// CatchUpWorktreeAt is where the fix menu catches the target's head up: pr-<number>-catch-up
// in the directory under root named for the checkout's own directory. A tool keeping its own
// worktree of the pull request beside it more likely names it pr-<number>.
func CatchUpWorktreeAt(root string, t LocalDiff, branch string) CatchUpWorktree {
	return CatchUpWorktree{Target: t, Path: filepath.Join(root, filepath.Base(t.Checkout), "pr-"+strconv.Itoa(t.Number)+"-catch-up"), Branch: branch}
}

// CatchUp is where the merge in a CatchUpWorktree stands.
type CatchUp int

const (
	// NoCatchUp is no worktree, or one with no merge under way and nothing to push.
	NoCatchUp CatchUp = iota
	// MergeStopped is a merge under way, stopped for a person to resolve its conflicts.
	MergeStopped
	// MergeUnpushed is a merge committed and not pushed yet.
	MergeUnpushed
)

// subject is the merge's commit message. A strict conventional-commits hook refuses git's own
// "Merge branch" subject.
func (w CatchUpWorktree) subject() string {
	return "chore: merge " + w.Target.Base + " into " + w.Branch
}

// Exists says something is at the worktree's path.
func (w CatchUpWorktree) Exists() bool {
	_, err := os.Stat(w.Path)
	return err == nil
}

// State is where a merge in the worktree stands.
func (w CatchUpWorktree) State(ctx context.Context) CatchUp {
	switch {
	case !w.Exists():
		return NoCatchUp
	case w.merging(ctx):
		return MergeStopped
	case w.unpushed(ctx):
		return MergeUnpushed
	}
	return NoCatchUp
}

// tracking is the remote-tracking ref the head branch is fetched into, as a person's own
// git fetch would.
func (w CatchUpWorktree) tracking() string { return "refs/remotes/origin/" + w.Branch }

// Start merges origin/<base> into the head branch in the worktree, and says where the merge
// then stands. The head branch is fetched into origin/<branch> first, and the worktree added
// there, or moved there where it holds no merge. A merge already under way in it, or
// committed and not pushed, is kept as it is. A worktree holding changes is refused rather
// than lost, and one on a branch is no worktree prs added. A head that already holds the base
// needs no merge, and the worktree goes again.
func (w CatchUpWorktree) Start(ctx context.Context) (CatchUp, error) {
	lock := checkoutLock(w.Target.Checkout)
	lock.Lock()
	defer lock.Unlock()
	registered, err := w.registered(ctx)
	if err != nil {
		return NoCatchUp, err
	}
	switch {
	case registered && w.merging(ctx):
		return MergeStopped, nil
	case registered && w.unpushed(ctx):
		return MergeUnpushed, nil
	}
	if _, err := Run(ctx, w.Target.Checkout, "git", "fetch", "--quiet", "origin", "+refs/heads/"+w.Branch+":"+w.tracking()); err != nil {
		return NoCatchUp, fmt.Errorf("could not fetch %s for #%d in %s: %w", w.Branch, w.Target.Number, w.Target.Repo, err)
	}
	switch {
	case registered:
		if branch, _, err := run(ctx, w.Path, "git", "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			return NoCatchUp, fmt.Errorf("%s is a worktree on %s, which prs did not add, so prs leaves it as it is", w.Path, strings.TrimSpace(string(branch)))
		}
		if changed, err := w.git(ctx, "status", "--porcelain"); err != nil || len(changed) > 0 {
			return NoCatchUp, fmt.Errorf("%s holds changes and no merge is under way there, so prs leaves it as it is: %v", w.Path, orNothing(err, "commit or remove them"))
		}
		if _, err := w.git(ctx, "checkout", "--quiet", "--detach", w.tracking()); err != nil {
			return NoCatchUp, fmt.Errorf("could not move %s to #%d's head: %w", w.Path, w.Target.Number, err)
		}
	case w.Exists():
		return NoCatchUp, fmt.Errorf("%s is in the way, and it is no worktree of %s", w.Path, w.Target.Checkout)
	default:
		if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
			return NoCatchUp, err
		}
		if _, err := Run(ctx, w.Target.Checkout, "git", "worktree", "add", "--quiet", "--detach", w.Path, w.tracking()); err != nil {
			return NoCatchUp, fmt.Errorf("could not add a worktree for #%d at %s: %w", w.Target.Number, w.Path, err)
		}
	}
	_, said, err := runFor(ctx, hooksTimeout, w.Path, "git", "merge", "--no-ff", "-m", w.subject(), "origin/"+w.Target.Base)
	switch {
	case w.merging(ctx):
		return MergeStopped, nil
	case err != nil:
		return NoCatchUp, fmt.Errorf("git could not merge origin/%s into #%d in %s: %s", w.Target.Base, w.Target.Number, w.Path, orError(said, err))
	case !w.unpushed(ctx):
		return NoCatchUp, w.removeHeld(ctx)
	}
	return MergeUnpushed, nil
}

// Continue commits the merge once its conflicts are resolved and staged, with the message
// Start gave it, and says where it then stands. The commit runs the checkout's hooks. A merge
// git cannot commit with nothing unmerged, such as one a hook refuses, stays stopped, and
// git's words say why.
func (w CatchUpWorktree) Continue(ctx context.Context) (CatchUp, error) {
	_, said, err := runFor(ctx, hooksTimeout, w.Path, "git", "merge", "--continue")
	switch merging := w.merging(ctx); {
	case merging && err == nil:
		return MergeStopped, nil
	case merging:
		return MergeStopped, fmt.Errorf("git could not commit the merge in %s: %s", w.Path, orError(said, err))
	case err != nil:
		return NoCatchUp, fmt.Errorf("git could not commit the merge in %s: %s", w.Path, orError(said, err))
	}
	return MergeUnpushed, nil
}

// registered says git lists the worktree's path as a worktree of the checkout.
func (w CatchUpWorktree) registered(ctx context.Context) (bool, error) {
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

// merging says a merge is under way in the worktree, by the MERGE_HEAD git keeps while one is.
func (w CatchUpWorktree) merging(ctx context.Context) bool {
	_, err := w.git(ctx, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	return err == nil
}

// unpushed says the worktree's HEAD is not the head branch's commit on origin as last fetched,
// so it holds a merge nobody pushed.
func (w CatchUpWorktree) unpushed(ctx context.Context) bool {
	out, err := w.git(ctx, "rev-parse", "HEAD", w.tracking())
	heads := strings.Fields(string(out))
	return err == nil && len(heads) == 2 && heads[0] != heads[1]
}

// Unmerged is each path the merge left conflicted that is not resolved and staged yet.
func (w CatchUpWorktree) Unmerged(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("git could not say what is still unmerged in %s: %w", w.Path, err)
	}
	return strings.Fields(string(out)), nil
}

// Commits is each commit the push adds to the head branch as last fetched, the merge first, as
// git log --oneline prints it.
func (w CatchUpWorktree) Commits(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "log", "--oneline", w.tracking()+"..HEAD")
	if err != nil {
		return nil, fmt.Errorf("git could not list the commits to push from %s: %w", w.Path, err)
	}
	var commits []string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimRight(line, "\n"); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// Pushed is what Push left: the merge on the head branch, and why the worktree is still there
// where removing it failed.
type Pushed struct {
	Commit   string
	Unremove string
}

// Push pushes the merge to the head branch on origin, then removes the worktree. The merge's
// first parent is the head it started from, so the push fast-forwards the branch and carries
// no force. Origin refuses it where anyone pushed to the branch since, and a refused push
// leaves the merge where it is.
func (w CatchUpWorktree) Push(ctx context.Context) (Pushed, error) {
	if _, err := RunWithin(ctx, hooksTimeout, w.Path, "git", "push", "--quiet", "origin", "HEAD:refs/heads/"+w.Branch); err != nil {
		return Pushed{}, fmt.Errorf("git could not push the merge in %s to %s: %w", w.Path, w.Branch, err)
	}
	out, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Pushed{}, fmt.Errorf("pushed the merge in %s, and git could not name its head: %w", w.Path, err)
	}
	pushed := Pushed{Commit: strings.TrimSpace(string(out))}
	if err := w.Remove(ctx); err != nil {
		pushed.Unremove = err.Error()
	}
	return pushed, nil
}

// Abort abandons any merge under way and removes the worktree, and says whether a merge was.
func (w CatchUpWorktree) Abort(ctx context.Context) (bool, error) {
	merging := w.merging(ctx)
	if merging {
		if _, err := w.git(ctx, "merge", "--abort"); err != nil {
			return true, fmt.Errorf("git could not abort the merge in %s: %w", w.Path, err)
		}
	}
	return merging, w.Remove(ctx)
}

// Remove removes the worktree from the checkout, which git refuses while it holds changes.
func (w CatchUpWorktree) Remove(ctx context.Context) error {
	lock := checkoutLock(w.Target.Checkout)
	lock.Lock()
	defer lock.Unlock()
	return w.removeHeld(ctx)
}

// removeHeld is Remove for a caller already holding the checkout's lock. The checkout's
// directory under the root goes with its last worktree, so the root holds a directory for a
// repo exactly where a worktree of it is.
func (w CatchUpWorktree) removeHeld(ctx context.Context) error {
	if _, err := Run(ctx, w.Target.Checkout, "git", "worktree", "remove", w.Path); err != nil {
		return fmt.Errorf("git could not remove the worktree at %s: %w", w.Path, err)
	}
	_ = os.Remove(filepath.Dir(w.Path))
	return nil
}

func (w CatchUpWorktree) git(ctx context.Context, args ...string) ([]byte, error) {
	return Run(ctx, w.Path, append([]string{"git"}, args...)...)
}

// orNothing is err's text, or else what to do instead where there was no error.
func orNothing(err error, instead string) string {
	if err != nil {
		return err.Error()
	}
	return instead
}
