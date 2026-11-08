package board

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

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
	// says run's error instead.
	done func(m *gotermboard.Model) tea.Cmd
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

// handedBack is what a program prs handed the terminal to sends when it exits, with what to
// do next.
type handedBack struct {
	err  error
	then func(m *gotermboard.Model, err error) tea.Cmd
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

// handOver gives the terminal to cmd until it exits, then runs then.
func handOver(cmd *exec.Cmd, then func(m *gotermboard.Model, err error) tea.Cmd) tea.Cmd {
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return handedBack{err: err, then: then} })
}

// UnderWay is gotermboard.BusyBoard's: every command a key started that has not ended.
func (b *Board) UnderWay() string {
	doing := make([]string, len(b.working))
	for i, w := range b.working {
		doing[i] = w.doing
	}
	return strings.Join(doing, " · ")
}

// Receive is gotermboard.ReceivingBoard's.
func (b *Board) Receive(m *gotermboard.Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case workEnded:
		b.working = slices.DeleteFunc(b.working, func(w underWay) bool { return w.id == msg.id })
		if msg.err != nil {
			m.Say(msg.err.Error())
			return nil
		}
		return msg.work.done(m)
	case handedBack:
		return msg.then(m, msg.err)
	}
	return nil
}

// settle gives a pull request the state a merge or close left it in, which its row reads until
// a listing drops it, however long the provider's search takes to see the change.
func (b *Board) settle(m *gotermboard.Model, pr pullrequests.PullRequest, state settledState) {
	b.settled[pr.Key()] = state
	m.Rebuild()
}

// readAgain reads every provider on the board's next tick.
func (b *Board) readAgain() {
	for _, source := range b.sources {
		source.Force()
	}
}

// runClient is a provider's client run as pullrequests.Run runs it, in the checkout where
// inCheckout says. A client killed at its timeout may have acted already, so that failure
// says the outcome is unknown rather than that it failed.
func runClient(pr pullrequests.PullRequest, verb string, argv []string, inCheckout bool) func(context.Context) error {
	return func(ctx context.Context) error {
		dir := ""
		if inCheckout {
			checkout, err := checkoutOf(pr)
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
