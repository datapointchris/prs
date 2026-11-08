package board

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
)

// blocker is what keeps GitHub from merging a pull request as it stands, as the fix menu names
// it. Those defined from config are the kinds a merge fix names in its blockers and receives in
// {blocker}.
type blocker string

const (
	conflictsBlocker     blocker = config.Conflicts
	behindBlocker        blocker = config.Behind
	failingChecksBlocker blocker = config.FailingChecks
	draftBlocker         blocker = config.Draft
	// computingBlocker is a merge state GitHub has not worked out yet.
	computingBlocker blocker = "unknown"
	// heldBlocker is a block no fix here reaches: a required review, or a required check still
	// running.
	heldBlocker blocker = "held"
)

// blockerOf is what keeps GitHub from merging the pull request as it stands, as mergeMeanings
// says of its merge state, and empty where m merges it: its merge state is ready, GitHub merges
// it with checks still running, or its host reports no merge state.
func blockerOf(pr pullrequests.PullRequest) blocker {
	if pr.MergeStateStatus == nil {
		return ""
	}
	state := mergeMeanings[mergeStateOf(pr)]
	if state.checksBlock && checkMeaning(pr.Checks).failed {
		return failingChecksBlocker
	}
	return state.blocker
}

// The fix menu's own keys. A choice's key is the one the menu offers it on, whatever kind of
// blocker it is for, so no merge fix may take one.
const (
	resolveKey     = "n"
	closeKey       = "x"
	updateKey      = "u"
	logKey         = "l"
	rerunKey       = "r"
	readyKey       = "r"
	mergeAnywayKey = "m"
	abortKey       = "a"
	pushKey        = "p"
)

var menuKeys = []string{resolveKey, closeKey, updateKey, logKey, rerunKey, mergeAnywayKey, abortKey, pushKey}

// RefusedFix says why a merge fix cannot have its key, and is nil where the key is free: one the
// fix menu offers a choice of its own on, or one that closes the menu.
func RefusedFix(fix config.MergeFix) error {
	if slices.Contains(menuKeys, fix.Key) || fix.Key == "esc" || fix.Key == "ctrl+c" {
		return fmt.Errorf("key %q is the fix menu's own, and it keeps %s and esc", fix.Key, strings.Join(menuKeys, " "))
	}
	return nil
}

// fixFrom is what opened the fix menu.
type fixFrom struct {
	// merge is m, so a pull request that reads ready once read again asks to merge.
	merge bool
	// refusal is what the host said refusing a merge, which the menu opens with.
	refusal string
}

// fixMenu is what the menu is drawn from: the pull request as it was just read, its detail, for
// a conflict what the checkout here says of it, and the worktree the menu rebases the head in,
// nil where there is no checkout.
type fixMenu struct {
	from      fixFrom
	pr        pullrequests.PullRequest
	detail    *pullrequests.Detail
	conflicts conflicts
	worktree  *pullrequests.RebaseWorktree
	// left says the worktree is there, from an earlier n, and rebase where a rebase in it stands.
	left   bool
	rebase pullrequests.Rebase
}

// conflicts is what the checkout here says of a conflict: the files it is in, or why git could
// not name them.
type conflicts struct {
	files []string
	fault string
}

// fixChoice is one choice of the fix menu: its key, what it does, and what it runs.
type fixChoice struct {
	key, label string
	run        func(m *gotermboard.Model) tea.Cmd
}

// fix is f: the fix menu for what keeps the pull request from merging, once it is read again,
// so a row the list shows ready still offers to remove a worktree an earlier n left.
func (b *Board) fix(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	switch why := b.refusal(pr); {
	case why != "":
		m.Say(refused(pr, "fix", why).Error())
	case pr.MergeStateStatus == nil:
		m.Say(fmt.Sprintf("%s reports no merge state for %s #%d, so prs has no fix to offer for it", pr.Provider, pr.Slug, pr.Number))
	default:
		return b.readThenFix(m, pr, fixFrom{})
	}
	return nil
}

// readThenFix reads the pull request again, waiting out a merge state GitHub is still working
// out, then opens the fix menu on what that read says. Where the read says it conflicts and its
// repo is checked out here, the conflicting files are named before anything is checked out.
// Whatever it says, a worktree an earlier n left is found.
func (b *Board) readThenFix(m *gotermboard.Model, pr pullrequests.PullRequest, from fixFrom) tea.Cmd {
	provider, err := pullrequests.ProviderOf(pr.Provider)
	if err != nil {
		m.Say(err.Error())
		return nil
	}
	ref := pullrequests.Reference{Provider: pr.Provider, Slug: pr.Slug, Number: pr.Number}
	menu := fixMenu{from: from}
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("reading %s #%d again", pr.Slug, pr.Number),
		run: func(ctx context.Context) error {
			fresh, detail, err := readMergeState(ctx, provider, ref)
			if err != nil {
				return err
			}
			menu.pr, menu.detail = pullrequests.Finish([]pullrequests.PullRequest{fresh}, b.repos, time.Now())[0], detail
			target, err := pullrequests.LocalDiffOf(ctx, menu.pr)
			if err == nil {
				w := pullrequests.RebaseWorktreeAt(b.worktreeRoot, target, menu.pr.Branch)
				menu.worktree, menu.left, menu.rebase = &w, w.Exists(), w.State(ctx)
			}
			if blockerOf(menu.pr) == conflictsBlocker {
				menu.conflicts = conflictsOf(ctx, target, err)
			}
			return nil
		},
		done: func(m *gotermboard.Model) tea.Cmd { return b.openFixMenu(m, menu) },
		failed: func(m *gotermboard.Model, err error) tea.Cmd {
			said := err.Error()
			if from.refusal != "" {
				said = fmt.Sprintf("%s. Reading #%d again to offer a fix failed: %s", from.refusal, pr.Number, said)
			}
			m.Say(said)
			return nil
		},
	})
}

// conflictsOf is what the checkout here says of the pull request's conflict with its base,
// after fetching both, as d does, or why there is no checkout to ask.
func conflictsOf(ctx context.Context, target pullrequests.LocalDiff, local error) conflicts {
	if local != nil {
		return conflicts{fault: local.Error()}
	}
	if _, err := pullrequests.Fetch(ctx, target); err != nil {
		return conflicts{fault: err.Error()}
	}
	files, err := pullrequests.ConflictingFiles(ctx, target)
	if err != nil {
		return conflicts{fault: err.Error()}
	}
	return conflicts{files: files}
}

// openFixMenu asks which fix to run for what keeps the menu's pull request from merging, after
// what the host said where a merge was refused. A pull request that reads ready asks to merge
// where m opened the menu, offers what a worktree an earlier n left needs where f did, and
// otherwise says so. A blocker with nothing to offer says why.
func (b *Board) openFixMenu(m *gotermboard.Model, menu fixMenu) tea.Cmd {
	pr := menu.pr
	kind := blockerOf(pr)
	if kind == "" {
		ready := fmt.Sprintf("GitHub reads %s #%d as %s now, which needs no fix; m merges it", pr.Slug, pr.Number, stateMeaning(pr, "").word)
		switch {
		case menu.from.refusal != "":
			m.Say(menu.from.refusal)
		case menu.from.merge:
			return b.askMerge(m, pr)
		case menu.left:
			b.askFix(m, pr, []gotermboard.Line{{gotermboard.C(ready+".", gotermboard.Bold)}, {gotermboard.C(leftLine(menu), "")}},
				b.leftChoices(pr, menu), fmt.Sprintf("left %s as it is", homeAsTilde(menu.worktree.Path)))
		default:
			m.Say(ready)
		}
		return nil
	}
	about, choices := b.fixesFor(kind, menu)
	if len(choices) == 0 {
		m.Say(strings.Join(slices.Concat([]string{menu.from.refusal}, about), " "))
		return nil
	}
	var listing []gotermboard.Line
	if menu.from.refusal != "" {
		listing = append(listing, gotermboard.Line{gotermboard.C(menu.from.refusal, gotermboard.Red)})
	}
	for i, line := range about {
		code := ""
		if i == 0 {
			code = gotermboard.Bold
		}
		listing = append(listing, gotermboard.Line{gotermboard.C(line, code)})
	}
	b.askFix(m, pr, listing, choices, fmt.Sprintf("did nothing to %s #%d", pr.Slug, pr.Number))
	return nil
}

// askFix asks which of choices to run on the pull request, under listing, and says left on esc.
// Two choices on one key would leave the second unreachable, so it names them rather than ask.
func (b *Board) askFix(m *gotermboard.Model, pr pullrequests.PullRequest, listing []gotermboard.Line, choices []fixChoice, left string) {
	asked := make([]gotermboard.Choice, len(choices))
	for i, choice := range choices {
		if at := slices.IndexFunc(choices[:i], func(c fixChoice) bool { return c.key == choice.key }); at >= 0 {
			m.Say(fmt.Sprintf("the fix menu of %s #%d offers %s for both %q and %q, so it opens on neither", pr.Slug, pr.Number, choice.key, choices[at].label, choice.label))
			return
		}
		asked[i] = gotermboard.Choice{Key: choice.key, Label: choice.label}
	}
	m.Ask(gotermboard.Question{
		Listing: listing,
		Text:    fmt.Sprintf("fix %s #%d — %s?", pr.Slug, pr.Number, pr.Title),
		Choices: asked,
		Answer: func(key string) tea.Cmd {
			at := slices.IndexFunc(choices, func(choice fixChoice) bool { return choice.key == key })
			return choices[at].run(m)
		},
		Cancel: func() tea.Cmd {
			m.Say(left)
			return nil
		},
	})
}

// fixesFor is what the menu says of the blocker, a line each, the first leading, and the choices
// it offers for it: its own, then the config's fixes for that kind, then pushing or removing a
// worktree an earlier n left.
func (b *Board) fixesFor(kind blocker, menu fixMenu) ([]string, []fixChoice) {
	pr := menu.pr
	provider, _ := pullrequests.ProviderOf(pr.Provider)
	var about []string
	var choices []fixChoice
	worktree := ""
	if menu.rebase != pullrequests.NoRebase {
		worktree = menu.worktree.Path
	}
	switch kind {
	case conflictsBlocker:
		about, choices = b.conflictFixes(menu)
	case behindBlocker:
		about = []string{fmt.Sprintf("Its branch is behind %s, and GitHub refuses the merge until it holds %s's newest commits.", pr.Base, pr.Base)}
		if provider.UpdateBranch != nil {
			argv, inCheckout := provider.UpdateBranch(pr)
			choices = append(choices, fixChoice{
				key: updateKey, label: fmt.Sprintf("update the branch: %s rebases it onto %s on GitHub, and its checks run again", strings.Join(argv[:3], " "), pr.Base),
				run: func(*gotermboard.Model) tea.Cmd {
					return b.hostVerb(pr, argv, inCheckout, "update", fmt.Sprintf("rebasing the branch of %s #%d onto %s on GitHub", pr.Slug, pr.Number, pr.Base),
						fmt.Sprintf("rebased the branch of %s #%d onto %s on GitHub, so its checks run again", pr.Slug, pr.Number, pr.Base))
				},
			})
		}
	case failingChecksBlocker:
		about, choices = b.checkFixes(pr, menu.detail, provider)
	case draftBlocker:
		about = []string{"It is a draft, which GitHub does not merge."}
		if provider.Ready != nil {
			argv, inCheckout := provider.Ready(pr)
			choices = append(choices, fixChoice{
				key: readyKey, label: fmt.Sprintf("ready for review: %s takes it out of draft", strings.Join(argv[:3], " ")),
				run: func(*gotermboard.Model) tea.Cmd {
					return b.hostVerb(pr, argv, inCheckout, "mark ready", fmt.Sprintf("marking %s #%d ready for review", pr.Slug, pr.Number),
						fmt.Sprintf("marked %s #%d ready for review", pr.Slug, pr.Number))
				},
			})
		}
	case computingBlocker:
		about = []string{fmt.Sprintf("GitHub is still working out whether %s #%d merges; f reads it again.", pr.Slug, pr.Number)}
	case heldBlocker:
		about = []string{fmt.Sprintf("GitHub blocks merging %s #%d for something no fix here reaches. Its review reads %s and its checks %s, so a required review, or a required check still running, holds it.",
			pr.Slug, pr.Number, reviewMeaning(pr.Review).word, checkMeaning(pr.Checks).word)}
	}
	choices = append(choices, b.configuredFixes(pr, kind, menu, worktree)...)
	if menu.left {
		about = append(about, leftLine(menu))
		choices = append(choices, b.leftChoices(pr, menu)...)
	}
	if kind == conflictsBlocker {
		choices = append(choices, fixChoice{key: closeKey, label: "close without merging, after asking", run: func(m *gotermboard.Model) tea.Cmd { return b.closePR(m, pr) }})
	}
	if kind == failingChecksBlocker && mergeMeanings[mergeStateOf(pr)].mergesFailing {
		choices = append(choices, fixChoice{
			key: mergeAnywayKey, label: "merge anyway: GitHub merges it with these checks failing",
			run: func(m *gotermboard.Model) tea.Cmd {
				merge, why := b.mergeOf(pr)
				if why != "" {
					m.Say(refused(pr, "merge", why).Error())
					return nil
				}
				return b.start(merge.work)
			},
		})
	}
	return about, choices
}

// leftLine is what the menu says of a worktree an earlier n left.
func leftLine(menu fixMenu) string {
	where := homeAsTilde(menu.worktree.Path)
	switch menu.rebase {
	case pullrequests.Stopped:
		return fmt.Sprintf("A rebase of it onto origin/%s stopped in %s.", menu.pr.Base, where)
	case pullrequests.Rebased:
		return fmt.Sprintf("A rebase of it onto origin/%s waits to be pushed in %s.", menu.pr.Base, where)
	}
	return fmt.Sprintf("A worktree an earlier rebase here left, with no rebase under way, is in %s.", where)
}

// leftChoices are what the menu offers for a worktree an earlier n left: pushing a rebase that
// finished, and aborting a rebase under way or removing the worktree.
func (b *Board) leftChoices(pr pullrequests.PullRequest, menu fixMenu) []fixChoice {
	w := *menu.worktree
	where := homeAsTilde(w.Path)
	var choices []fixChoice
	label := fmt.Sprintf("remove %s: git worktree remove, which refuses it while it holds changes", where)
	switch menu.rebase {
	case pullrequests.Stopped:
		label = fmt.Sprintf("abort the rebase stopped in %s: git rebase --abort, then remove the worktree", where)
	case pullrequests.Rebased:
		choices = append(choices, fixChoice{key: pushKey, label: fmt.Sprintf("push the rebase waiting in %s, after asking", where), run: func(*gotermboard.Model) tea.Cmd {
			return b.readPushable(pr, w)
		}})
		label = fmt.Sprintf("remove %s and the rebase in it, unpushed: git worktree remove", where)
	}
	return append(choices, fixChoice{key: abortKey, label: label, run: func(*gotermboard.Model) tea.Cmd { return b.abortRebase(pr, w) }})
}

// conflictFixes is what the menu says of a conflict, and rebasing it to resolve in nvim, where
// its repo is checked out here, git and nvim are installed, and the head branch is on origin
// rather than in a fork, which prs pushes nothing to. A rebase that finished and waits to be
// pushed is offered p among the left worktree's choices instead.
func (b *Board) conflictFixes(menu fixMenu) ([]string, []fixChoice) {
	pr, c := menu.pr, menu.conflicts
	var about []string
	switch {
	case len(c.files) > 0:
		about = append(about, fmt.Sprintf("It conflicts with %s in %s:", pr.Base, counted(len(c.files), "file")))
		for _, file := range c.files {
			about = append(about, "    "+file)
		}
	case c.fault != "":
		about = append(about, fmt.Sprintf("It conflicts with %s, and git could not name the files here: %s.", pr.Base, strings.TrimSuffix(c.fault, ".")))
	default:
		about = append(about, fmt.Sprintf("GitHub reads it as conflicting with %s, and git merges the two cleanly here, so GitHub may not have read the newest of either yet.", pr.Base))
	}
	if pr.HeadInFork {
		about = append(about, fmt.Sprintf("Its branch %s is in a fork rather than on origin, so prs pushes no rebase to it.", pr.Branch))
	}
	if menu.worktree == nil || pr.HeadInFork || menu.rebase == pullrequests.Rebased || len(config.MissingPrograms([]string{"git", "nvim"})) > 0 {
		return about, nil
	}
	w := *menu.worktree
	label := fmt.Sprintf("resolve in nvim: rebase it onto origin/%s in %s, then resolve each stop in diffview", pr.Base, homeAsTilde(w.Path))
	if menu.rebase == pullrequests.Stopped {
		label = fmt.Sprintf("resolve in nvim: open the rebase stopped in %s", homeAsTilde(w.Path))
	}
	return about, []fixChoice{{key: resolveKey, label: label, run: func(m *gotermboard.Model) tea.Cmd { return b.resolve(m, pr, w) }}}
}

// checkFixes is what the menu says of failing checks, and reading or rerunning the GitHub
// Actions runs among them.
func (b *Board) checkFixes(pr pullrequests.PullRequest, detail *pullrequests.Detail, provider pullrequests.Provider) ([]string, []fixChoice) {
	names, runs := failedChecks(detail, provider)
	about := []string{"Its checks fail."}
	if len(names) > 0 {
		about = []string{fmt.Sprintf("Its checks fail: %s.", strings.Join(names, ", "))}
	}
	if len(runs) == 0 || provider.FailedLog == nil || provider.Rerun == nil {
		if len(names) > 0 {
			about = append(about, "None of them is a GitHub Actions run, so prs can neither read nor rerun them.")
		}
		return about, nil
	}
	which := "run " + strings.Join(runs, ", ")
	return about, []fixChoice{
		{key: logKey, label: fmt.Sprintf("read the failing log: gh run view --log-failed on %s, full screen", which), run: func(m *gotermboard.Model) tea.Cmd {
			return b.readFailingLog(pr, provider, runs)
		}},
		{key: rerunKey, label: fmt.Sprintf("rerun the failed jobs: gh run rerun --failed on %s", which), run: func(m *gotermboard.Model) tea.Cmd {
			return b.rerunFailed(pr, provider, runs)
		}},
	}
}

// failedChecks is the name of each failed check the detail holds, and each run among them, as
// the provider reads one from a check's URL, in the order they first appear.
func failedChecks(detail *pullrequests.Detail, provider pullrequests.Provider) (names, runs []string) {
	if detail == nil {
		return nil, nil
	}
	for _, check := range detail.Checks {
		if !checkMeaning(check.State).failed {
			continue
		}
		names = append(names, check.Name)
		if provider.CheckRun == nil {
			continue
		}
		if run := provider.CheckRun(check.URL); run != "" && !slices.Contains(runs, run) {
			runs = append(runs, run)
		}
	}
	return names, runs
}

// configuredFixes is each merge fix the config offers for the kind, whose programs are on
// PATH, each opening its window behind the board. worktree is where a rebase stopped or waits
// to be pushed, for {worktree}.
func (b *Board) configuredFixes(pr pullrequests.PullRequest, kind blocker, menu fixMenu, worktree string) []fixChoice {
	_, runs := failedChecks(menu.detail, mustProvider(pr))
	var choices []fixChoice
	for _, fix := range b.fixes {
		if !slices.Contains(fix.Blockers, string(kind)) || len(fix.Missing()) > 0 {
			continue
		}
		label := fix.Label
		if fix.Detail != "" {
			label += ": " + fix.Detail
		}
		choices = append(choices, fixChoice{key: fix.Key, label: label, run: func(m *gotermboard.Model) tea.Cmd {
			return b.runFix(m, fix, pr, kind, runs, worktree)
		}})
	}
	return choices
}

// mustProvider is the pull request's provider, which a listing read only from a known one.
func mustProvider(pr pullrequests.PullRequest) pullrequests.Provider {
	provider, _ := pullrequests.ProviderOf(pr.Provider)
	return provider
}

// runFix opens the fix's window behind the board, in the checkout, with the fields its command
// takes. A fix for failing checks gets their log, written to a file first, in {log}.
func (b *Board) runFix(m *gotermboard.Model, fix config.MergeFix, pr pullrequests.PullRequest, kind blocker, runs []string, worktree string) tea.Cmd {
	checkout, err := pullrequests.CheckoutOf(pr)
	if err != nil {
		m.Say(err.Error())
		return nil
	}
	fields := config.Fields{
		Number: pr.Number, Repo: pr.Repo, Slug: pr.Slug, Path: checkout, Worktree: worktree,
		Head: pr.Branch, Base: pr.Base, Blocker: string(kind),
	}
	name := config.Fill(fix.Window, fields)
	open := func(m *gotermboard.Model, fields config.Fields) tea.Cmd {
		if err := gotermboard.OpenWindowBehind(name, checkout, []string{config.FillCommand(fix.Command, fields)}, fix.Needs...); err != nil {
			m.Say(err.Error())
			return nil
		}
		b.windows.openedHere(name)
		m.Rebuild()
		m.Say(fmt.Sprintf("opened %s in a tmux window behind this one, and %s #%d reads %s while it is open", name, pr.Slug, pr.Number, fix.Reads()))
		return nil
	}
	provider := mustProvider(pr)
	if kind != failingChecksBlocker || len(runs) == 0 || provider.FailedLog == nil {
		return open(m, fields)
	}
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("reading the failing log of %s #%d for %s", pr.Slug, pr.Number, name),
		run: func(ctx context.Context) error {
			fields.Log, _, err = writeFailingLog(ctx, pr, provider, runs)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd { return open(m, fields) },
	})
}

// hostVerb runs one of the host's fix commands on the pull request in the background, and reads
// the listings again once it has, because each moves the row's merge state or checks.
func (b *Board) hostVerb(pr pullrequests.PullRequest, argv []string, inCheckout bool, verb, doing, said string) tea.Cmd {
	return b.start(work{
		pr: pr, doing: doing, run: runClient(pr, verb, argv, inCheckout),
		done: func(m *gotermboard.Model) tea.Cmd {
			b.readAgain()
			m.Say(said)
			return nil
		},
	})
}

// rerunFailed runs each run's failed jobs again, one run at a time, and goes on past one that
// fails, so a failure costs only its own run and the footer names each that reran.
func (b *Board) rerunFailed(pr pullrequests.PullRequest, provider pullrequests.Provider, runs []string) tea.Cmd {
	var reran, failed []string
	reranAll := func() string {
		return fmt.Sprintf("reran the failed jobs of %s #%d, in run %s, so its checks run again", pr.Slug, pr.Number, strings.Join(reran, ", "))
	}
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("rerunning the failed jobs of %s #%d", pr.Slug, pr.Number),
		run: func(ctx context.Context) error {
			for _, run := range runs {
				argv, inCheckout := provider.Rerun(pr, run)
				if err := runClient(pr, "rerun run "+run+" of", argv, inCheckout)(ctx); err != nil {
					failed = append(failed, err.Error())
					continue
				}
				reran = append(reran, run)
			}
			if len(failed) > 0 {
				return errors.New(strings.Join(failed, "; "))
			}
			return nil
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			b.readAgain()
			m.Say(reranAll())
			return nil
		},
		failed: func(m *gotermboard.Model, err error) tea.Cmd {
			if len(reran) == 0 {
				m.Say(err.Error())
				return nil
			}
			b.readAgain()
			m.Say(fmt.Sprintf("%s, and %v", reranAll(), err))
			return nil
		},
	})
}

// readFailingLog reads what each run's failed jobs logged and opens it full screen, kept in a
// file a merge fix can be handed too.
func (b *Board) readFailingLog(pr pullrequests.PullRequest, provider pullrequests.Provider, runs []string) tea.Cmd {
	var path, log string
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("reading the failing log of %s #%d", pr.Slug, pr.Number),
		run: func(ctx context.Context) error {
			var err error
			path, log, err = writeFailingLog(ctx, pr, provider, runs)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			if strings.TrimSpace(log) == "" {
				m.Say(fmt.Sprintf("run %s of %s #%d logged nothing for a failed job", strings.Join(runs, ", "), pr.Slug, pr.Number))
				return nil
			}
			m.Read(fmt.Sprintf("failing log of %s #%d, kept in %s", pr.Slug, pr.Number, homeAsTilde(path)), logDocument(log))
			return nil
		},
	})
}

// writeFailingLog is what each run's failed jobs logged, run by run, written to
// prs-failing-log-<repo>-<n>.txt in the temporary directory, and that file's path.
func writeFailingLog(ctx context.Context, pr pullrequests.PullRequest, provider pullrequests.Provider, runs []string) (string, string, error) {
	var log strings.Builder
	for _, run := range runs {
		argv, inCheckout := provider.FailedLog(pr, run)
		out, err := clientOutput(ctx, pr, "read run "+run+" of", argv, inCheckout)
		if err != nil {
			return "", "", err
		}
		log.Write(out)
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("prs-failing-log-%s-%d.txt", pr.Repo, pr.Number))
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		return "", "", fmt.Errorf("could not keep the failing log of #%d: %w", pr.Number, err)
	}
	return path, log.String(), nil
}

// logDocument is a log as the Reader draws it, each line wrapped at the width, its colors and
// tabs taken out because the Reader lays out plain cells.
func logDocument(log string) gotermboard.Document {
	return func(width int) []gotermboard.Line {
		var lines []gotermboard.Line
		for line := range strings.Lines(log) {
			plain := strings.ReplaceAll(ansi.Strip(strings.TrimRight(line, "\r\n")), "\t", "  ")
			for wrapped := range strings.SplitSeq(ansi.Hardwrap(plain, max(width, 1), true), "\n") {
				lines = append(lines, gotermboard.Line{gotermboard.C(wrapped, "")})
			}
		}
		return lines
	}
}

// resolve rebases the pull request's head onto origin/<base> in the worktree, after fetching
// the base, and hands each stop to nvim's resolver there. A rebase already under way there
// opens as it was left, and one that finished asks to push. A head that already holds the base
// has nothing to rebase, and says so.
func (b *Board) resolve(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.RebaseWorktree) tea.Cmd {
	state := pullrequests.NoRebase
	var commits []string
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("rebasing %s #%d onto origin/%s in %s", pr.Slug, pr.Number, pr.Base, homeAsTilde(w.Path)),
		run: func(ctx context.Context) error {
			if _, err := pullrequests.Fetch(ctx, w.Target); err != nil {
				return err
			}
			var err error
			if state, err = w.Start(ctx); err != nil || state != pullrequests.Rebased {
				return err
			}
			commits, err = w.Commits(ctx)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			switch state {
			case pullrequests.Stopped:
				return b.resolveInNvim(m, pr, w)
			case pullrequests.Rebased:
				b.askPush(m, pr, w, commits)
				return nil
			}
			m.Say(fmt.Sprintf("the head of %s #%d already holds origin/%s, so there was nothing to rebase", pr.Slug, pr.Number, pr.Base))
			return nil
		},
	})
}

// resolveInNvim hands the terminal to nvim in the worktree, where Diffview with no revisions
// opens its three-way resolver on the rebase's stop, and goes on from what nvim left once it
// exits.
func (b *Board) resolveInNvim(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.RebaseWorktree) tea.Cmd {
	nvim := exec.Command("nvim", "-c", openDiffview(""))
	nvim.Dir = w.Path
	return m.HandOver(nvim, func(m *gotermboard.Model, err error, stderr []byte) tea.Cmd {
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit) && exit.ExitCode() == noDiffview:
			m.Say(fmt.Sprintf("nvim has no :DiffviewOpen, which resolving needs from the diffview.nvim plugin; the rebase waits in %s", homeAsTilde(w.Path)))
			return nil
		case err != nil:
			m.Say(fmt.Sprintf("nvim ended resolving #%d: %s; the rebase waits in %s", pr.Number, gotermboard.LastLine(stderr, err), homeAsTilde(w.Path)))
			return nil
		}
		return b.afterResolving(pr, w)
	})
}

// afterResolving reads what nvim left unmerged. With none left it goes on with the rebase,
// opening nvim again at its next stop and asking to push once it finishes. With some left, or
// at a stop git cannot go on from, it offers the stop's choices.
func (b *Board) afterResolving(pr pullrequests.PullRequest, w pullrequests.RebaseWorktree) tea.Cmd {
	var unmerged, commits []string
	state, stuck := pullrequests.Stopped, ""
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("going on with the rebase in %s", homeAsTilde(w.Path)),
		run: func(ctx context.Context) error {
			var err error
			if unmerged, err = w.Unmerged(ctx); err != nil || len(unmerged) > 0 {
				return err
			}
			switch state, err = w.Continue(ctx); {
			case err != nil && state == pullrequests.Stopped:
				stuck = err.Error()
				return nil
			case err != nil || state != pullrequests.Rebased:
				return err
			}
			commits, err = w.Commits(ctx)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			switch {
			case len(unmerged) > 0:
				listing := []gotermboard.Line{{gotermboard.C(fmt.Sprintf("%s still unmerged in %s:", counted(len(unmerged), "path"), homeAsTilde(w.Path)), gotermboard.Bold)}}
				for _, path := range unmerged {
					listing = append(listing, gotermboard.Line{gotermboard.C("    "+path, "")})
				}
				b.stoppedMenu(m, pr, w, listing)
			case stuck != "":
				b.stoppedMenu(m, pr, w, []gotermboard.Line{{gotermboard.C(stuck, gotermboard.Red)}})
			case state == pullrequests.Stopped:
				return b.resolveInNvim(m, pr, w)
			default:
				b.askPush(m, pr, w, commits)
			}
			return nil
		},
	})
}

// stoppedMenu offers what to do with a rebase stopped with paths unmerged, or at a stop git
// cannot go on from: open nvim again, abort it, or hand it to a merge fix for conflicts.
func (b *Board) stoppedMenu(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.RebaseWorktree, listing []gotermboard.Line) {
	choices := []fixChoice{
		{key: resolveKey, label: "resolve in nvim: open the rebase again", run: func(m *gotermboard.Model) tea.Cmd { return b.resolveInNvim(m, pr, w) }},
		{key: abortKey, label: "abort the rebase: git rebase --abort, then remove the worktree", run: func(*gotermboard.Model) tea.Cmd { return b.abortRebase(pr, w) }},
	}
	choices = append(choices, b.configuredFixes(pr, conflictsBlocker, fixMenu{pr: pr}, w.Path)...)
	b.askFix(m, pr, listing, choices, fmt.Sprintf("left the rebase stopped in %s; f, then n, opens it again", homeAsTilde(w.Path)))
}

// readPushable reads the commits a finished rebase put on the base, then asks to push them.
func (b *Board) readPushable(pr pullrequests.PullRequest, w pullrequests.RebaseWorktree) tea.Cmd {
	var commits []string
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("reading the rebase in %s", homeAsTilde(w.Path)),
		run: func(ctx context.Context) error {
			var err error
			commits, err = w.Commits(ctx)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			b.askPush(m, pr, w, commits)
			return nil
		},
	})
}

// maxListedCommits is the most commits the push question lists, so a long branch leaves the
// question on the screen.
const maxListedCommits = 10

// askPush lists the commits the rebase put on the base and asks to force-push them to the head
// branch. Enter alone pushes, because the person just resolved the rebase, and the lease
// refuses the push where anyone pushed to the branch since.
func (b *Board) askPush(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.RebaseWorktree, commits []string) {
	listing := []gotermboard.Line{{gotermboard.C(fmt.Sprintf("%s rebased onto origin/%s in %s:", counted(len(commits), "commit"), pr.Base, homeAsTilde(w.Path)), gotermboard.Bold)}}
	for i, commit := range commits {
		if i == maxListedCommits {
			listing = append(listing, gotermboard.Line{gotermboard.C(fmt.Sprintf("    and %d more", len(commits)-i), "")})
			break
		}
		listing = append(listing, gotermboard.Line{gotermboard.C("    "+commit, "")})
	}
	left := func() tea.Cmd {
		m.Say(fmt.Sprintf("left the rebase unpushed in %s; f, then p, pushes it, and f, then a, removes it", homeAsTilde(w.Path)))
		return nil
	}
	m.Ask(gotermboard.Question{
		Listing: listing,
		Text:    fmt.Sprintf("force-push it to %s, with a lease on the head it started from? [Y/n]", w.Branch),
		Typed:   true,
		Answer: func(typed string) tea.Cmd {
			if !slices.Contains([]string{"", "y", "yes"}, strings.ToLower(typed)) {
				return left()
			}
			return b.pushRebase(pr, w)
		},
		Cancel: left,
	})
}

// pushRebase force-pushes the rebase to the head branch and removes the worktree, then reads
// the listings again for the merge state the push moves.
func (b *Board) pushRebase(pr pullrequests.PullRequest, w pullrequests.RebaseWorktree) tea.Cmd {
	var pushed pullrequests.Pushed
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("pushing the rebase of %s #%d to %s", pr.Slug, pr.Number, w.Branch),
		run: func(ctx context.Context) error {
			var err error
			pushed, err = w.Push(ctx)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			b.readAgain()
			said := fmt.Sprintf("pushed %s to %s, rebasing %s #%d onto origin/%s", short(pushed.Commit), w.Branch, pr.Slug, pr.Number, pr.Base)
			if pushed.Unremove != "" {
				said += ". " + pushed.Unremove
			}
			m.Say(said)
			return nil
		},
		failed: func(m *gotermboard.Model, err error) tea.Cmd {
			m.Say(fmt.Sprintf("%v; the rebase waits in %s: f, then p, pushes it again, and f, then a, removes it, so n rebases the newest head", err, homeAsTilde(w.Path)))
			return nil
		},
	})
}

// abortRebase abandons any rebase under way in the worktree and removes it.
func (b *Board) abortRebase(pr pullrequests.PullRequest, w pullrequests.RebaseWorktree) tea.Cmd {
	aborted := false
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("removing %s", homeAsTilde(w.Path)),
		run: func(ctx context.Context) error {
			var err error
			aborted, err = w.Abort(ctx)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			if aborted {
				m.Say(fmt.Sprintf("aborted the rebase of %s #%d onto origin/%s and removed %s", pr.Slug, pr.Number, pr.Base, homeAsTilde(w.Path)))
			} else {
				m.Say(fmt.Sprintf("removed %s, which held no rebase under way", homeAsTilde(w.Path)))
			}
			return nil
		},
	})
}

// fixingCell is what the row reads while a merge fix's window is open on its pull request, and
// empty where none is. Only a row with a merge state can have opened one.
func (b *Board) fixingCell(pr pullrequests.PullRequest) gotermboard.Cell {
	if b.windows == nil || pr.MergeStateStatus == nil {
		return gotermboard.C("", "")
	}
	fields := config.Fields{Number: pr.Number, Repo: pr.Repo, Slug: pr.Slug}
	for _, fix := range b.fixes {
		if open, _ := b.windows.open(config.Fill(fix.Window, fields)); open {
			return gotermboard.C(fixGlyph+"  "+fix.Reads(), gotermboard.Yellow)
		}
	}
	return gotermboard.C("", "")
}
