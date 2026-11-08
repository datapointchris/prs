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
// target's Ref conflicts in, from git merge-tree, which checks nothing out. It is empty where
// git merges the two cleanly here. merge-tree exits 1 both for a conflict and for a ref it
// cannot read, so a conflict is the one that wrote a tree.
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

// MergeWorktree is a worktree of a pull request's checkout, detached at its head, where the fix
// menu merges origin/<base> in for a person to resolve. Detached, it moves no branch, so a
// worktree a session holds on the head branch, or a local branch with commits not pushed, is
// left as it is, and the merge commit is pushed to the head branch on origin.
type MergeWorktree struct {
	Target LocalDiff
	Path   string
	// Branch is the head branch on origin the merge commit is pushed to.
	Branch string
}

// MergeWorktreeAt is where the fix menu merges the target's base in: pr-<number>-merge in the
// directory under root named for the checkout's own directory. A tool keeping its own
// worktree of the pull request beside it more likely names it pr-<number>.
func MergeWorktreeAt(root string, t LocalDiff, branch string) MergeWorktree {
	return MergeWorktree{Target: t, Path: filepath.Join(root, filepath.Base(t.Checkout), "pr-"+strconv.Itoa(t.Number)+"-merge"), Branch: branch}
}

// message is the merge commit's message, as git merge words its own.
func (w MergeWorktree) message() string {
	return fmt.Sprintf("Merge branch '%s' into %s", w.Target.Base, w.Branch)
}

// Exists says something is at the worktree's path.
func (w MergeWorktree) Exists() bool {
	_, err := os.Stat(w.Path)
	return err == nil
}

// Start merges origin/<base> into head in the worktree, adding the worktree where it is not
// there, and says whether a merge is then under way. None is where head already holds the base,
// and the worktree goes again. A merge already under way there, left by an earlier Start, is
// kept as it is. A worktree with no merge under way is moved to head, unless it holds changes,
// which are refused rather than lost, or sits on a branch, which is no worktree prs added. The
// merge stops before committing, conflicts or not, so the person sees it in nvim before it is
// pushed.
func (w MergeWorktree) Start(ctx context.Context, head string) (bool, error) {
	lock := checkoutLock(w.Target.Checkout)
	lock.Lock()
	defer lock.Unlock()
	registered, err := w.registered(ctx)
	if err != nil {
		return false, err
	}
	switch {
	case registered && w.merging(ctx):
		return true, nil
	case registered:
		if branch, _, err := run(ctx, w.Path, "git", "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			return false, fmt.Errorf("%s is a worktree on %s, which prs did not add, so prs leaves it as it is", w.Path, strings.TrimSpace(string(branch)))
		}
		if changed, err := w.git(ctx, "status", "--porcelain"); err != nil || len(changed) > 0 {
			return false, fmt.Errorf("%s holds changes and no merge is under way there, so prs leaves it as it is: %v", w.Path, orNothing(err, "commit or remove them"))
		}
		if _, err := w.git(ctx, "checkout", "--quiet", "--detach", head); err != nil {
			return false, fmt.Errorf("could not move %s to #%d's head: %w", w.Path, w.Target.Number, err)
		}
	case w.Exists():
		return false, fmt.Errorf("%s is in the way, and it is no worktree of %s", w.Path, w.Target.Checkout)
	default:
		if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
			return false, err
		}
		if _, err := Run(ctx, w.Target.Checkout, "git", "worktree", "add", "--quiet", "--detach", w.Path, head); err != nil {
			return false, fmt.Errorf("could not add a worktree for #%d at %s: %w", w.Target.Number, w.Path, err)
		}
	}
	_, said, err := run(ctx, w.Path, "git", "merge", "--quiet", "--no-ff", "--no-commit", "-m", w.message(), "origin/"+w.Target.Base)
	merging := w.merging(ctx)
	switch {
	case err != nil && !merging:
		return false, fmt.Errorf("git could not merge origin/%s into #%d in %s: %s", w.Target.Base, w.Target.Number, w.Path, orError(said, err))
	case !merging:
		return false, w.removeHeld(ctx)
	}
	return true, nil
}

// registered says git lists the worktree's path as a worktree of the checkout.
func (w MergeWorktree) registered(ctx context.Context) (bool, error) {
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

// merging says a merge is under way in the worktree.
func (w MergeWorktree) merging(ctx context.Context) bool {
	_, _, err := run(ctx, w.Path, "git", "rev-parse", "--quiet", "--verify", "MERGE_HEAD")
	return err == nil
}

// Merging says a merge Start began is still under way in the worktree, conflicted or ready to
// commit.
func (w MergeWorktree) Merging(ctx context.Context) bool { return w.Exists() && w.merging(ctx) }

// Unmerged is each path the merge left conflicted that is not resolved and staged yet.
func (w MergeWorktree) Unmerged(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("git could not say what is still unmerged in %s: %w", w.Path, err)
	}
	return strings.Fields(string(out)), nil
}

// Staged is what the merge commit would change, as git diff --stat prints it.
func (w MergeWorktree) Staged(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "diff", "--cached", "--stat")
	if err != nil {
		return nil, fmt.Errorf("git could not say what the merge in %s changes: %w", w.Path, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), nil
}

// Pushed is what CommitAndPush left: the merge commit on the head branch, and why the
// worktree is still there where removing it failed.
type Pushed struct {
	Commit   string
	Unremove string
}

// CommitAndPush commits the merge under way, through the repo's own hooks, and pushes it to the
// head branch on origin, then removes the worktree. The commit sits on top of the head, so the
// push is a plain one, which origin refuses where someone pushed to the branch since. A commit
// or push that fails leaves the merge under way as it was, resolved, for nvim to open again.
func (w MergeWorktree) CommitAndPush(ctx context.Context) (Pushed, error) {
	if _, err := RunWithin(ctx, hooksTimeout, w.Path, "git", "commit", "--quiet", "-m", w.message()); err != nil {
		return Pushed{}, fmt.Errorf("git could not commit the merge in %s: %w", w.Path, err)
	}
	if _, err := RunWithin(ctx, hooksTimeout, w.Path, "git", "push", "--quiet", "origin", "HEAD:refs/heads/"+w.Branch); err != nil {
		if undo := w.uncommit(ctx); undo != nil {
			return Pushed{}, fmt.Errorf("git could not push the merge in %s to %s: %w, and the merge stays committed there, unpushed, because git could not take the commit back: %w", w.Path, w.Branch, err, undo)
		}
		return Pushed{}, fmt.Errorf("git could not push the merge in %s to %s: %w", w.Path, w.Branch, err)
	}
	out, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Pushed{}, fmt.Errorf("pushed the merge in %s, and git could not name its commit: %w", w.Path, err)
	}
	pushed := Pushed{Commit: strings.TrimSpace(string(out))}
	if err := w.Remove(ctx); err != nil {
		pushed.Unremove = err.Error()
	}
	return pushed, nil
}

// uncommit takes the merge commit back off the worktree's HEAD and leaves the merge under way
// again, its resolution staged, so a push origin refused loses none of it. git refuses a soft
// reset during a merge, and refuses update-ref on MERGE_HEAD, so MERGE_HEAD is written after
// the reset, where git merge writes it.
func (w MergeWorktree) uncommit(ctx context.Context) error {
	revs, err := w.git(ctx, "rev-parse", "HEAD", "HEAD^2")
	if err != nil {
		return err
	}
	where, err := w.git(ctx, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
	if err != nil {
		return err
	}
	commit, merged, _ := strings.Cut(strings.TrimSpace(string(revs)), "\n")
	if _, err := w.git(ctx, "reset", "--quiet", "--soft", "HEAD^"); err != nil {
		return err
	}
	if err := os.WriteFile(strings.TrimSpace(string(where)), []byte(merged+"\n"), 0o644); err != nil {
		return fmt.Errorf("its resolution is staged, and the merge commit is %s, because %w", commit, err)
	}
	return nil
}

// Abort abandons any merge under way and removes the worktree, and says whether a merge was.
func (w MergeWorktree) Abort(ctx context.Context) (bool, error) {
	merging := w.merging(ctx)
	if merging {
		if _, err := w.git(ctx, "merge", "--abort"); err != nil {
			return true, fmt.Errorf("git could not abort the merge in %s: %w", w.Path, err)
		}
	}
	return merging, w.Remove(ctx)
}

// Remove removes the worktree from the checkout, which git refuses while it holds changes.
func (w MergeWorktree) Remove(ctx context.Context) error {
	lock := checkoutLock(w.Target.Checkout)
	lock.Lock()
	defer lock.Unlock()
	return w.removeHeld(ctx)
}

// removeHeld is Remove for a caller already holding the checkout's lock. The checkout's
// directory under the root goes with its last worktree, so the root holds a directory for a
// repo exactly where a worktree of it is.
func (w MergeWorktree) removeHeld(ctx context.Context) error {
	if _, err := Run(ctx, w.Target.Checkout, "git", "worktree", "remove", w.Path); err != nil {
		return fmt.Errorf("git could not remove the worktree at %s: %w", w.Path, err)
	}
	_ = os.Remove(filepath.Dir(w.Path))
	return nil
}

func (w MergeWorktree) git(ctx context.Context, args ...string) ([]byte, error) {
	return Run(ctx, w.Path, append([]string{"git"}, args...)...)
}

// orNothing is err's text, or else what to do instead where there was no error.
func orNothing(err error, instead string) string {
	if err != nil {
		return err.Error()
	}
	return instead
}
