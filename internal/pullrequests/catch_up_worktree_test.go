package pullrequests_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// TestMain clears the variables git exports into the processes it launches, such as a hook,
// because GIT_DIR outranks a command's directory and would aim every fixture at the repo being
// committed. It points HOME and git's global config at a temp directory, so no test reads the
// person's config, hooks or signing key.
func TestMain(m *testing.M) {
	os.Exit(isolated(m))
}

func isolated(m *testing.M) int {
	names, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git could not name its repository variables: %v\n", err)
		return 1
	}
	for _, name := range strings.Fields(string(names)) {
		_ = os.Unsetenv(name)
	}
	home, err := os.MkdirTemp("", "prs-pullrequests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(home) }()
	config := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(config, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for name, value := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "GIT_CONFIG_GLOBAL": config, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		_ = os.Setenv(name, value)
	}
	return m.Run()
}

// A head whose base moved on in other files catches up in one merge: its first parent the
// head as pushed, its second the base, its subject a conventional one. The push fast-forwards
// a branch origin refuses to rewind, and the worktree goes.
func TestACatchUpMergesTheBaseIntoTheHeadAndPushesItWithNoForce(t *testing.T) {
	t.Parallel()
	target, origin, head := diverged(t, "feature.txt", "main.txt")
	w := pullrequests.CatchUpWorktreeAt(filepath.Join(t.TempDir(), "worktrees"), target, "feature")

	state, err := w.Start(t.Context())
	if err != nil || state != pullrequests.MergeUnpushed {
		t.Fatalf("Start = %v, %v; want the merge committed and unpushed", state, err)
	}
	assertCaughtUp(t, w, origin, head)
}

// A head that conflicts with its base stops in git's merge state with the path named. Once a
// person resolves and stages it, the merge commits with Start's subject and no conflict
// comments, and pushes as a fast-forward.
func TestACatchUpThatConflictsStopsForAPersonThenCommitsAndPushesWithNoForce(t *testing.T) {
	t.Parallel()
	target, origin, head := diverged(t, "base.txt", "base.txt")
	w := pullrequests.CatchUpWorktreeAt(filepath.Join(t.TempDir(), "worktrees"), target, "feature")

	state, err := w.Start(t.Context())
	if err != nil || state != pullrequests.MergeStopped || w.State(t.Context()) != pullrequests.MergeStopped {
		t.Fatalf("Start = %v, %v; want the merge stopped on its conflict", state, err)
	}
	if unmerged, err := w.Unmerged(t.Context()); err != nil || strings.Join(unmerged, " ") != "base.txt" {
		t.Fatalf("Unmerged = %v, %v; want base.txt", unmerged, err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "base.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, w.Path, "add", "base.txt")
	if state, err := w.Continue(t.Context()); err != nil || state != pullrequests.MergeUnpushed {
		t.Fatalf("Continue = %v, %v; want the merge committed and unpushed", state, err)
	}
	if message := git(t, w.Path, "log", "-1", "--format=%B"); message != "chore: merge main into feature" {
		t.Errorf("the resolved merge's message is %q; want its subject alone", message)
	}
	assertCaughtUp(t, w, origin, head)
}

// assertCaughtUp checks the worktree's HEAD is a merge of origin/main into head with the
// catch-up subject, then pushes it and checks origin's branch holds it with head as its first
// parent, and the worktree is gone.
func assertCaughtUp(t *testing.T, w pullrequests.CatchUpWorktree, origin, head string) {
	t.Helper()
	base := git(t, w.Target.Checkout, "rev-parse", "origin/main")
	if parents := git(t, w.Path, "rev-parse", "HEAD^1", "HEAD^2"); parents != head+"\n"+base {
		t.Errorf("the merge's parents are %q; want the head %s, then origin/main %s", parents, head, base)
	}
	if subject := git(t, w.Path, "log", "-1", "--format=%s"); subject != "chore: merge main into feature" {
		t.Errorf("the merge's subject is %q", subject)
	}
	pushed, err := w.Push(t.Context())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if on := git(t, origin, "rev-parse", "feature", "feature^1"); on != pushed.Commit+"\n"+head {
		t.Errorf("origin's feature and its first parent are %q; want the merge %s on the head %s", on, pushed.Commit, head)
	}
	if w.Exists() || pushed.Unremove != "" {
		t.Errorf("the worktree should go once the merge is pushed: %s", pushed.Unremove)
	}
}

// diverged builds a bare origin that refuses to rewind a branch, and a checkout of it, where
// feature left main and each then committed once, feature to featureFile and main to
// mainFile. It returns the pull request as the fix menu targets it, fetched as the menu
// fetches it, the origin, and feature's head.
func diverged(t *testing.T, featureFile, mainFile string) (pullrequests.LocalDiff, string, string) {
	t.Helper()
	dir := t.TempDir()
	origin, checkout := filepath.Join(dir, "widget.git"), filepath.Join(dir, "widget")
	git(t, dir, "init", "--quiet", "--bare", origin)
	git(t, origin, "config", "receive.denyNonFastForwards", "true")
	git(t, dir, "clone", "--quiet", origin, checkout)
	commit(t, checkout, "base.txt", "base\n", "Add the base")
	git(t, checkout, "push", "--quiet", "origin", "main")
	git(t, checkout, "switch", "--quiet", "-c", "feature")
	commit(t, checkout, featureFile, "feature\n", "Change it on feature")
	git(t, checkout, "push", "--quiet", "origin", "feature")
	head := git(t, checkout, "rev-parse", "HEAD")
	git(t, checkout, "switch", "--quiet", "main")
	commit(t, checkout, mainFile, "main\n", "Change it on main")
	git(t, checkout, "push", "--quiet", "origin", "main")
	target := pullrequests.LocalDiff{Number: 7, Repo: "acme/widget", Checkout: checkout, Head: "feature", Base: "main"}
	if _, err := pullrequests.Fetch(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	return target, origin, head
}

// commit writes content to file in dir and commits it alone.
func commit(t *testing.T, dir, file, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", file)
	git(t, dir, "commit", "--quiet", "-m", message)
}

// git runs a fixture's git in dir, and returns what it printed with the space around it trimmed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, exit.Stderr)
		}
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
}
