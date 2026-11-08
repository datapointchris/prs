package board

import (
	"context"
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

// workTimeout bounds a command a key runs in the background, as a read is bounded, so a hung
// network never leaves work under way on the footer.
const workTimeout = 2 * time.Minute

// work is a command a key runs in the background while the footer names it after a spinner.
type work struct {
	// doing is what the footer says while it runs.
	doing string
	run   func(ctx context.Context) error
	// done runs once run succeeds, to say what was done and return the next step. A failure
	// says run's error instead.
	done func(m *gotermboard.Model) tea.Cmd
}

type underWay struct {
	id    int
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

// start puts w on the footer and runs it in the background.
func (b *Board) start(w work) tea.Cmd {
	b.workCount++
	id := b.workCount
	b.working = append(b.working, underWay{id: id, doing: w.doing})
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), workTimeout)
		defer cancel()
		return workEnded{id: id, work: w, err: w.run(ctx)}
	}
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
func (b *Board) settle(m *gotermboard.Model, pr pullrequests.PullRequest, state string) {
	b.settled[pr.Key()] = state
	m.Rebuild()
}

// readAgain reads every provider on the board's next tick.
func (b *Board) readAgain() {
	for _, source := range b.sources {
		source.Force()
	}
}

// runClient is a provider's client run with no terminal, in the checkout where inCheckout
// says, failing with the first line it printed. gh and bbkt each print the reason first and
// advice after it.
func runClient(pr pullrequests.PullRequest, verb string, argv []string, inCheckout bool) func(context.Context) error {
	return func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		if inCheckout {
			checkout, err := checkoutOf(pr)
			if err != nil {
				return err
			}
			cmd.Dir = checkout
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s could not %s #%d: %s", argv[0], verb, pr.Number, firstLine(out, err))
		}
		return nil
	}
}

// git runs git in checkout with no terminal to prompt on.
func git(ctx context.Context, checkout string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}

// firstLine is the first thing a failed command said, or its exit status where it said
// nothing.
func firstLine(out []byte, err error) string {
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return err.Error()
}
