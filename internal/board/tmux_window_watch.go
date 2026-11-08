package board

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
)

// tmuxWindowsEvery is how often tmux is asked which windows are open. tmux answers from
// memory, so a short interval costs nothing a provider would charge for.
const tmuxWindowsEvery = 5 * time.Second

// tmuxWindows is which tmux windows are open, read every tmuxWindowsEvery, and when this board
// opened each one it opened that runs a review round or a merge fix.
type tmuxWindows struct {
	source *gotermboard.Source[map[string]bool]
	mu     sync.Mutex
	opened map[string]time.Time
}

// watchTmuxWindows reads tmux's windows where a window action runs a review round or the
// config adds a merge fix, and prs runs inside tmux, and is nil elsewhere.
func watchTmuxWindows(reviews []config.WindowAction, fixes []config.MergeFix) *tmuxWindows {
	if len(reviews)+len(fixes) == 0 || os.Getenv("TMUX") == "" || len(config.MissingPrograms([]string{"tmux"})) > 0 {
		return nil
	}
	w := &tmuxWindows{opened: map[string]time.Time{}}
	w.source = gotermboard.NewSource("tmux windows", tmuxWindowsEvery, w.read)
	return w
}

// read is every window name tmux holds, across its sessions. A window this board opened is
// closed once a read that began after it omits it, and its opening time goes with it.
func (w *tmuxWindows) read(ctx context.Context) (map[string]bool, string, error) {
	started := time.Now()
	out, err := pullrequests.Run(ctx, "", "tmux", "list-windows", "-a", "-F", "#{window_name}")
	if err != nil {
		return nil, "", fmt.Errorf("tmux could not list its windows: %w", err)
	}
	open := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		if name := strings.TrimRight(line, "\n"); name != "" {
			open[name] = true
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for name, at := range w.opened {
		if !open[name] && at.Before(started) {
			delete(w.opened, name)
		}
	}
	return open, "", nil
}

// openedHere records a window this board opened, which is open from now, and reads tmux
// again at once.
func (w *tmuxWindows) openedHere(name string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.opened[name] = time.Now()
	w.mu.Unlock()
	w.source.Force()
}

// open says the window is open, and when this board opened it, zero where it did not. A
// window opened after the last read began is open though that read could not list it.
func (w *tmuxWindows) open(name string) (bool, time.Time) {
	if w == nil {
		return false, time.Time{}
	}
	w.mu.Lock()
	at, here := w.opened[name]
	w.mu.Unlock()
	switch {
	case w.source.Value()[name]:
		return true, at
	case here && at.After(w.source.Status().TriedAt):
		return true, at
	}
	return false, time.Time{}
}
