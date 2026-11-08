package board

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// work is a command a key runs on one pull request in the background, while the footer names
// it after a spinner.
type work struct {
	pr pullrequests.PullRequest
	// doing is what the footer says while it runs.
	doing string
	run   func(ctx context.Context) error
	// done runs once run succeeds, to say what was done and return the next step. A failure
	// runs failed, where set, and otherwise says run's error.
	done   func(m *gotermboard.Model) tea.Cmd
	failed func(m *gotermboard.Model, err error) tea.Cmd
}

type underWay struct {
	id    int
	key   string
	doing string
}

// workEnded is what a work's command sends when it ends.
type workEnded struct {
	id   int
	work work
	err  error
}

// start puts w on the footer and runs it in the background. Each client it runs is bounded
// by pullrequests.Run.
func (b *Board) start(w work) tea.Cmd {
	b.workCount++
	id := b.workCount
	b.working = append(b.working, underWay{id: id, key: w.pr.Key(), doing: w.doing})
	return func() tea.Msg {
		return workEnded{id: id, work: w, err: w.run(context.Background())}
	}
}

// busy is what is under way on a pull request, or empty.
func (b *Board) busy(pr pullrequests.PullRequest) string {
	for _, w := range b.working {
		if w.key == pr.Key() {
			return w.doing
		}
	}
	return ""
}

// UnderWay is gotermboard.BusyBoard's: every command a key started that has not ended.
func (b *Board) UnderWay() string {
	doing := make([]string, len(b.working))
	for i, w := range b.working {
		doing[i] = w.doing
	}
	return strings.Join(doing, " · ")
}

// Receive is gotermboard.ReceivingBoard's. Each provider is read again when the terminal gains
// focus and when a program prs handed the terminal to exits, because either is someone coming
// back to the list, maybe from changing a pull request. The model runs a handback's then after
// this.
func (b *Board) Receive(m *gotermboard.Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case workEnded:
		b.working = slices.DeleteFunc(b.working, func(w underWay) bool { return w.id == msg.id })
		switch {
		case msg.err != nil && msg.work.failed != nil:
			return msg.work.failed(m, msg.err)
		case msg.err != nil:
			m.Say(msg.err.Error())
			return nil
		}
		return msg.work.done(m)
	case gotermboard.HandedBack, tea.FocusMsg:
		b.readAgain()
	}
	return nil
}

// closeHere moves a pull request a key merged or closed into recently closed at once, in the
// state the key left it in, until a listing read after the key holds it. A cursor on its row
// moves on to the next open row in its group, else the one above, else another group's first,
// so a queue reads one key a pull request.
func (b *Board) closeHere(m *gotermboard.Model, pr pullrequests.PullRequest, standing pullrequests.Standing) {
	next := b.openRowsAfter(pr.Key())
	b.keepClosed(m, pr, standing)
	if m.Cursor() != pr.Key() {
		return
	}
	for _, key := range next {
		if m.MoveCursorTo(key) {
			return
		}
	}
}

// openRowsAfter is every open row the last frame drew but key's, in the order the cursor
// prefers them once key's row leaves: the rest of its group below it, then above it nearest
// first, then the other groups' as they drew.
func (b *Board) openRowsAfter(key string) []string {
	at := slices.Index(b.drawnOrder, key)
	if at < 0 {
		return nil
	}
	in := b.drawnIn[key]
	var below, above, others []string
	for i, k := range b.drawnOrder {
		switch {
		case i == at || !b.listedOpen(k):
		case b.drawnIn[k] != in:
			others = append(others, k)
		case i > at:
			below = append(below, k)
		default:
			above = slices.Insert(above, 0, k)
		}
	}
	return slices.Concat(below, above, others)
}

// keepClosed holds the pull request in the state the key left it in, in recently closed.
func (b *Board) keepClosed(m *gotermboard.Model, pr pullrequests.PullRequest, standing pullrequests.Standing) {
	if provider, err := pullrequests.ProviderOf(pr.Provider); err == nil {
		pr.State = provider.Word(standing)
	}
	at := time.Now()
	stamp := at.UTC().Truncate(time.Second)
	pr.ClosedAt, pr.UpdatedAt = &stamp, &stamp
	if standing == pullrequests.Merged {
		pr.MergedAt = &stamp
	}
	b.closedHere[pr.Key()] = closeMadeHere{pr: pr, at: at}
	m.Rebuild()
}

// freshFor is how recent a provider's last good read is to stand when someone comes back to
// the list. Focus arrives on every move between tmux panes, and each GitHub read spends from
// the token's hourly GraphQL allowance, so rows read moments ago are not read again.
const freshFor = 10 * time.Second

// readAgain reads, on the board's next tick, every provider's open pull requests whose last
// good read is older than freshFor, and every one that has not read yet here. The recently
// closed read keeps its own timer, because it costs as much as the open one. The open read
// already sees a close made elsewhere, as a row reading merged or closed or as a row it drops.
func (b *Board) readAgain() {
	now := time.Now()
	for _, source := range b.sources {
		if now.Sub(source.Status().ReadAt) >= freshFor {
			source.Force()
		}
	}
}

// runClient is a provider's client run as pullrequests.Run runs it, in the checkout where
// inCheckout says. A client killed at its timeout may have acted already, so that failure
// says the outcome is unknown rather than that it failed.
func runClient(pr pullrequests.PullRequest, verb string, argv []string, inCheckout bool) func(context.Context) error {
	return func(ctx context.Context) error {
		dir := ""
		if inCheckout {
			checkout, err := pullrequests.CheckoutOf(pr)
			if err != nil {
				return err
			}
			dir = checkout
		}
		_, err := pullrequests.Run(ctx, dir, argv...)
		switch {
		case errors.Is(err, pullrequests.ErrNoAnswer):
			return fmt.Errorf("%s gave %w to %s #%d, so it may have happened; o opens its page", argv[0], err, verb, pr.Number)
		case err != nil:
			return fmt.Errorf("%s could not %s #%d: %w", argv[0], verb, pr.Number, err)
		}
		return nil
	}
}

// nameWindow names prs's tmux window for the length of a handover, and returns what puts its
// own name back. A program split into the window, such as a Claude session explaining the
// diff, reads the pull request off that name. Outside tmux it does nothing.
func nameWindow(name string) (restore func()) {
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || pane == "" {
		return func() {}
	}
	out, err := exec.Command("tmux", "display-message", "-p", "-t", pane, "#{window_name}\t#{automatic-rename}").Output()
	own, automatic, found := strings.Cut(strings.TrimRight(string(out), "\n"), "\t")
	if err != nil || !found {
		return func() {}
	}
	if exec.Command("tmux", "rename-window", "-t", pane, name).Run() != nil {
		return func() {}
	}
	return func() {
		// A rename turns automatic-rename off, so a window tmux named gets tmux's naming back.
		if automatic == "1" {
			_ = exec.Command("tmux", "set-option", "-w", "-t", pane, "automatic-rename", "on").Run()
			return
		}
		_ = exec.Command("tmux", "rename-window", "-t", pane, own).Run()
	}
}
