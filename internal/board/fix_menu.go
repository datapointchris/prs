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
// it. The first four are the kinds a merge fix names in its blockers and receives in {blocker}.
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

// blockerOf is what keeps GitHub from merging the pull request as it stands, and empty where m
// merges it: its merge state is ready, GitHub merges it with checks still running, or its host
// reports no merge state. A draft is one whatever else holds it, then a conflict, because
// fixing either runs every check again.
func blockerOf(pr pullrequests.PullRequest) blocker {
	if pr.MergeStateStatus == nil {
		return ""
	}
	state := *pr.MergeStateStatus
	switch {
	case pr.Draft || state == "DRAFT":
		return draftBlocker
	case pr.Mergeable != nil && *pr.Mergeable == "CONFLICTING", state == "DIRTY":
		return conflictsBlocker
	case state == "BEHIND":
		return behindBlocker
	case mergeMeanings[state].computing:
		return computingBlocker
	case checkMeaning(pr.Checks).failed && (state == "UNSTABLE" || state == "BLOCKED"):
		return failingChecksBlocker
	case state == "BLOCKED":
		return heldBlocker
	}
	return ""
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
)

var menuKeys = []string{resolveKey, closeKey, updateKey, logKey, rerunKey, mergeAnywayKey, abortKey}

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

// fixMenu is what the menu is drawn from: the pull request as it was just read, its detail, and
// for a conflict, what the checkout here says of it.
type fixMenu struct {
	from      fixFrom
	pr        pullrequests.PullRequest
	detail    *pullrequests.Detail
	conflicts conflicts
}

// conflicts is what the checkout here says of a conflict: the files it is in, or why git could
// not name them, and the worktree the menu merges the base into, nil where there is no
// checkout.
type conflicts struct {
	files    []string
	fault    string
	worktree *pullrequests.MergeWorktree
	// waiting says a merge an earlier n began is still under way in the worktree.
	waiting bool
}

// fixChoice is one choice of the fix menu: its key, what it does, and what it runs.
type fixChoice struct {
	key, label string
	run        func(m *gotermboard.Model) tea.Cmd
}

// fix is f: the fix menu for what keeps the pull request from merging, once it is read again.
func (b *Board) fix(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	switch why := b.refusal(pr); {
	case why != "":
		m.Say(refused(pr, "fix", why).Error())
	case pr.MergeStateStatus == nil:
		m.Say(fmt.Sprintf("%s reports no merge state for %s #%d, so prs has no fix to offer for it", pr.Provider, pr.Slug, pr.Number))
	case blockerOf(pr) == "":
		m.Say(fmt.Sprintf("GitHub reads %s #%d as %s, which needs no fix; m merges it", pr.Slug, pr.Number, stateMeaning(pr, "").word))
	default:
		return b.readThenFix(m, pr, fixFrom{})
	}
	return nil
}

// readThenFix reads the pull request again, waiting out a merge state GitHub is still working
// out, then opens the fix menu on what that read says. Where the read says it conflicts and its
// repo is checked out here, the conflicting files are named before anything is checked out.
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
			if blockerOf(menu.pr) == conflictsBlocker {
				menu.conflicts = b.conflictsOf(ctx, menu.pr)
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
// after fetching both, as d does.
func (b *Board) conflictsOf(ctx context.Context, pr pullrequests.PullRequest) conflicts {
	target, err := pullrequests.LocalDiffOf(ctx, pr)
	if err != nil {
		return conflicts{fault: err.Error()}
	}
	w := pullrequests.MergeWorktreeAt(b.worktreeRoot, target, pr.Branch)
	c := conflicts{worktree: &w, waiting: w.Merging(ctx)}
	if _, err := pullrequests.Fetch(ctx, target); err != nil {
		c.fault = err.Error()
		return c
	}
	if c.files, err = pullrequests.ConflictingFiles(ctx, target); err != nil {
		c.fault = err.Error()
	}
	return c
}

// openFixMenu asks which fix to run for what keeps the menu's pull request from merging, after
// what the host said where a merge was refused. A pull request that reads ready asks to merge
// where m opened the menu, and otherwise says so. A blocker with nothing to offer says why.
func (b *Board) openFixMenu(m *gotermboard.Model, menu fixMenu) tea.Cmd {
	pr := menu.pr
	kind := blockerOf(pr)
	if kind == "" {
		switch {
		case menu.from.refusal != "":
			m.Say(menu.from.refusal)
			return nil
		case menu.from.merge:
			return b.askMerge(m, pr)
		}
		m.Say(fmt.Sprintf("GitHub reads %s #%d as %s now, which needs no fix; m merges it", pr.Slug, pr.Number, stateMeaning(pr, "").word))
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
func (b *Board) askFix(m *gotermboard.Model, pr pullrequests.PullRequest, listing []gotermboard.Line, choices []fixChoice, left string) {
	asked := make([]gotermboard.Choice, len(choices))
	for i, choice := range choices {
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
// it offers for it: its own, then the config's fixes for that kind.
func (b *Board) fixesFor(kind blocker, menu fixMenu) ([]string, []fixChoice) {
	pr := menu.pr
	provider, _ := pullrequests.ProviderOf(pr.Provider)
	var about []string
	var choices []fixChoice
	worktree := ""
	switch kind {
	case conflictsBlocker:
		about, choices = b.conflictFixes(pr, menu.conflicts)
		if c := menu.conflicts; c.waiting {
			worktree = c.worktree.Path
		}
	case behindBlocker:
		about = []string{fmt.Sprintf("Its branch is behind %s, and GitHub refuses the merge until it holds %s's newest commits.", pr.Base, pr.Base)}
		if provider.UpdateBranch != nil {
			argv, inCheckout := provider.UpdateBranch(pr)
			choices = append(choices, fixChoice{
				key: updateKey, label: fmt.Sprintf("update the branch: %s merges %s into it on GitHub, and its checks run again", strings.Join(argv[:3], " "), pr.Base),
				run: func(*gotermboard.Model) tea.Cmd {
					return b.hostVerb(pr, argv, inCheckout, "update", fmt.Sprintf("updating the branch of %s #%d", pr.Slug, pr.Number),
						fmt.Sprintf("updated the branch of %s #%d with %s on GitHub, so its checks run again", pr.Slug, pr.Number, pr.Base))
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
		return []string{fmt.Sprintf("GitHub is still working out whether %s #%d merges; f reads it again.", pr.Slug, pr.Number)}, nil
	case heldBlocker:
		return []string{fmt.Sprintf("GitHub blocks merging %s #%d for something no fix here reaches. Its review reads %s and its checks %s, so a required review, or a required check still running, holds it.",
			pr.Slug, pr.Number, reviewMeaning(pr.Review).word, checkMeaning(pr.Checks).word)}, nil
	}
	choices = append(choices, b.configuredFixes(pr, kind, menu, worktree)...)
	if kind == conflictsBlocker {
		choices = append(choices, fixChoice{key: closeKey, label: "close without merging, after asking", run: func(m *gotermboard.Model) tea.Cmd { return b.closePR(m, pr) }})
	}
	if kind == failingChecksBlocker && *pr.MergeStateStatus == "UNSTABLE" {
		choices = append(choices, fixChoice{
			key: mergeAnywayKey, label: "merge anyway: GitHub merges it with these checks failing",
			run: func(m *gotermboard.Model) tea.Cmd {
				provider, why := b.mergeable(pr)
				if why != "" {
					m.Say(refused(pr, "merge", why).Error())
					return nil
				}
				head, _ := b.pinFor(pr)
				return b.start(b.mergeWork(pr, provider, head))
			},
		})
	}
	return about, choices
}

// conflictFixes is what the menu says of a conflict, and resolving it in nvim, where its repo is
// checked out here and git and nvim are installed.
func (b *Board) conflictFixes(pr pullrequests.PullRequest, c conflicts) ([]string, []fixChoice) {
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
	if c.waiting {
		about = append(about, fmt.Sprintf("A merge of origin/%s into it waits in %s.", pr.Base, homeAsTilde(c.worktree.Path)))
	}
	if c.worktree == nil || len(config.MissingPrograms([]string{"git", "nvim"})) > 0 {
		return about, nil
	}
	w := *c.worktree
	label := fmt.Sprintf("resolve in nvim: merge origin/%s into it in %s, then resolve it in diffview", pr.Base, homeAsTilde(w.Path))
	if c.waiting {
		label = fmt.Sprintf("resolve in nvim: open the merge waiting in %s", homeAsTilde(w.Path))
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
// PATH, each opening its window behind the board. worktree is where a merge of the base is
// under way, for {worktree}.
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

// rerunFailed runs each run's failed jobs again, one run at a time.
func (b *Board) rerunFailed(pr pullrequests.PullRequest, provider pullrequests.Provider, runs []string) tea.Cmd {
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("rerunning the failed jobs of %s #%d", pr.Slug, pr.Number),
		run: func(ctx context.Context) error {
			for _, run := range runs {
				argv, inCheckout := provider.Rerun(pr, run)
				if err := runClient(pr, "rerun run "+run+" of", argv, inCheckout)(ctx); err != nil {
					return err
				}
			}
			return nil
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			b.readAgain()
			m.Say(fmt.Sprintf("reran the failed jobs of %s #%d, in run %s, so its checks run again", pr.Slug, pr.Number, strings.Join(runs, ", ")))
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

// resolve merges origin/<base> into the pull request's head in the worktree, after fetching
// both, then hands the terminal to nvim's resolver there. A merge already under way there opens
// as it was left. A head that already holds the base has nothing to resolve, and says so.
func (b *Board) resolve(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.MergeWorktree) tea.Cmd {
	merging := false
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("merging origin/%s into %s #%d in %s", pr.Base, pr.Slug, pr.Number, homeAsTilde(w.Path)),
		run: func(ctx context.Context) error {
			got, err := pullrequests.Fetch(ctx, w.Target)
			if err != nil {
				return err
			}
			merging, err = w.Start(ctx, got.Head)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			if !merging {
				m.Say(fmt.Sprintf("the head of %s #%d already holds origin/%s, so there was nothing to merge", pr.Slug, pr.Number, pr.Base))
				return nil
			}
			return b.resolveInNvim(m, pr, w)
		},
	})
}

// resolveInNvim hands the terminal to nvim in the worktree, where Diffview with no revisions
// opens its three-way resolver on the merge under way, and reads what is left unmerged once
// nvim exits.
func (b *Board) resolveInNvim(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.MergeWorktree) tea.Cmd {
	nvim := exec.Command("nvim", "-c", openDiffview(""))
	nvim.Dir = w.Path
	return m.HandOver(nvim, func(m *gotermboard.Model, err error, stderr []byte) tea.Cmd {
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit) && exit.ExitCode() == noDiffview:
			m.Say(fmt.Sprintf("nvim has no :DiffviewOpen, which resolving needs from the diffview.nvim plugin; the merge waits in %s", homeAsTilde(w.Path)))
			return nil
		case err != nil:
			m.Say(fmt.Sprintf("nvim ended resolving #%d: %s; the merge waits in %s", pr.Number, gotermboard.LastLine(stderr, err), homeAsTilde(w.Path)))
			return nil
		}
		return b.afterResolving(pr, w)
	})
}

// afterResolving reads what nvim left unmerged, then asks to commit and push a merge with none
// left, or offers the merge's choices again where some are.
func (b *Board) afterResolving(pr pullrequests.PullRequest, w pullrequests.MergeWorktree) tea.Cmd {
	var unmerged, staged []string
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("reading what is left unmerged in %s", homeAsTilde(w.Path)),
		run: func(ctx context.Context) error {
			var err error
			if unmerged, err = w.Unmerged(ctx); err != nil || len(unmerged) > 0 {
				return err
			}
			staged, err = w.Staged(ctx)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			if len(unmerged) > 0 {
				b.unmergedMenu(m, pr, w, unmerged)
				return nil
			}
			b.askPush(m, pr, w, staged)
			return nil
		},
	})
}

// unmergedMenu offers what to do with a merge nvim left paths unmerged in: open nvim again, abort
// it, or hand it to a merge fix for conflicts.
func (b *Board) unmergedMenu(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.MergeWorktree, paths []string) {
	listing := []gotermboard.Line{{gotermboard.C(fmt.Sprintf("%s still unmerged in %s:", counted(len(paths), "path"), homeAsTilde(w.Path)), gotermboard.Bold)}}
	for _, path := range paths {
		listing = append(listing, gotermboard.Line{gotermboard.C("    "+path, "")})
	}
	choices := []fixChoice{
		{key: resolveKey, label: "resolve in nvim: open the merge again", run: func(m *gotermboard.Model) tea.Cmd { return b.resolveInNvim(m, pr, w) }},
		{key: abortKey, label: "abort the merge: git merge --abort, then remove the worktree", run: func(*gotermboard.Model) tea.Cmd { return b.abortMerge(pr, w) }},
	}
	choices = append(choices, b.configuredFixes(pr, conflictsBlocker, fixMenu{pr: pr}, w.Path)...)
	b.askFix(m, pr, listing, choices, fmt.Sprintf("left the merge under way in %s; f, then n, opens it again", homeAsTilde(w.Path)))
}

// askPush shows what the merge commit changes and asks to commit it and push it to the head
// branch. Enter alone pushes, because the person just resolved it, and the push only adds the
// merge commit on top of the branch.
func (b *Board) askPush(m *gotermboard.Model, pr pullrequests.PullRequest, w pullrequests.MergeWorktree, staged []string) {
	listing := []gotermboard.Line{{gotermboard.C(fmt.Sprintf("The merge of origin/%s into %s, ready to commit in %s:", pr.Base, w.Branch, homeAsTilde(w.Path)), gotermboard.Bold)}}
	for _, line := range staged {
		listing = append(listing, gotermboard.Line{gotermboard.C(line, "")})
	}
	left := func() tea.Cmd {
		m.Say(fmt.Sprintf("left the merge uncommitted in %s; f, then n, opens it again", homeAsTilde(w.Path)))
		return nil
	}
	m.Ask(gotermboard.Question{
		Listing: listing,
		Text:    fmt.Sprintf("commit it and push it to %s? [Y/n]", w.Branch),
		Typed:   true,
		Answer: func(typed string) tea.Cmd {
			if !slices.Contains([]string{"", "y", "yes"}, strings.ToLower(typed)) {
				return left()
			}
			return b.commitAndPush(pr, w)
		},
		Cancel: left,
	})
}

// commitAndPush commits the merge through the repo's hooks, pushes it to the head branch, and
// removes the worktree, then reads the listings again for the merge state the push moves.
func (b *Board) commitAndPush(pr pullrequests.PullRequest, w pullrequests.MergeWorktree) tea.Cmd {
	var pushed pullrequests.Pushed
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("committing the merge into %s #%d and pushing it to %s", pr.Slug, pr.Number, w.Branch),
		run: func(ctx context.Context) error {
			var err error
			pushed, err = w.CommitAndPush(ctx)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			b.readAgain()
			said := fmt.Sprintf("pushed %s to %s, merging origin/%s into %s #%d", short(pushed.Commit), w.Branch, pr.Base, pr.Slug, pr.Number)
			if pushed.Unremove != "" {
				said += ". " + pushed.Unremove
			}
			m.Say(said)
			return nil
		},
		failed: func(m *gotermboard.Model, err error) tea.Cmd {
			m.Say(fmt.Sprintf("%v; the merge waits in %s, and f, then n, opens it again", err, homeAsTilde(w.Path)))
			return nil
		},
	})
}

// abortMerge abandons the merge under way and removes its worktree.
func (b *Board) abortMerge(pr pullrequests.PullRequest, w pullrequests.MergeWorktree) tea.Cmd {
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("aborting the merge in %s", homeAsTilde(w.Path)),
		run:   w.Abort,
		done: func(m *gotermboard.Model) tea.Cmd {
			m.Say(fmt.Sprintf("aborted the merge of origin/%s into %s #%d and removed %s", pr.Base, pr.Slug, pr.Number, homeAsTilde(w.Path)))
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
