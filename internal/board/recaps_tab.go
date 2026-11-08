package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermboard/markdown"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
)

// RecapsView is the Recaps tab's name, the name --view takes.
const RecapsView = "recaps"

// recapsListEvery is how often the Recaps tab runs its list again while it shows. A list
// changes only when a recap is written, and a write the tab started reads it again as it ends.
const recapsListEvery = 10 * time.Minute

// recapWriteTimeout bounds a write, which waits on a model call and on whatever admits it.
const recapWriteTimeout = 30 * time.Minute

// recapReadsAtOnce is how many recaps the tab reads at once once a list lands.
const recapReadsAtOnce = 4

// RecapWindow is one window the list command printed: its kind, such as day, and its label,
// such as 2026-11-06. prs reads nothing else of what the commands print.
type RecapWindow struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

func (w RecapWindow) String() string { return w.Kind + " " + w.Label }

func (w RecapWindow) key() string { return "recaps/" + w.String() }

// recapWindowOf is the window a row's key names, and false for a key no window row has.
func recapWindowOf(key string) (RecapWindow, bool) {
	rest, ok := strings.CutPrefix(key, "recaps/")
	if !ok {
		return RecapWindow{}, false
	}
	kind, label, ok := strings.Cut(rest, " ")
	return RecapWindow{Kind: kind, Label: label}, ok
}

// recapList is the windows the list command printed, in its order, and how many lines it
// printed that were not a kind and a label.
type recapList struct {
	windows []RecapWindow
	skipped int
}

// parseRecapList reads the list command's lines, each a kind and a label. A window listed
// twice keeps its first place.
func parseRecapList(out []byte) recapList {
	var list recapList
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case len(fields) != 2:
			list.skipped++
		case !slices.Contains(list.windows, RecapWindow{Kind: fields[0], Label: fields[1]}):
			list.windows = append(list.windows, RecapWindow{Kind: fields[0], Label: fields[1]})
		}
	}
	return list
}

// recapRecord is how one window's recap stands: what show printed, when, and why the newest
// read failed. stale says a write here ended since it was read, so the next read reads it.
type recapRecord struct {
	markdown string
	held     bool
	readAt   time.Time
	fault    string
	reading  bool
	stale    bool
}

// due says the recap is to be read: never read, or written here since, and not reading now.
func (r recapRecord) due() bool { return !r.reading && (!r.held || r.stale) }

type recapWrite struct {
	id     int
	window RecapWindow
}

// recapWritten is what a write's command sends when it ends.
type recapWritten struct {
	id     int
	window RecapWindow
	err    error
}

// Recaps is the Recaps tab: the windows the config's list command prints, newest first, and
// the recap its show command prints for each, as a markdown document beside the list. Its
// write command writes a window's recap with a model call, on w after asking. prs runs the
// three as argv and reads nothing of what they print but the list's kind and label.
type Recaps struct {
	board    *Board
	commands config.Recaps
	list     *gotermboard.Source[recapList]
	// records reads each listed window's recap not yet read, the cursor's first. Its value is
	// how many it read.
	records  *gotermboard.Source[int]
	renderer *markdown.Renderer

	mu     sync.Mutex
	recaps map[RecapWindow]*recapRecord
	cursor string

	// writing is every write w started that has not ended, oldest first.
	writing    []recapWrite
	writeCount int
}

// OpenRecaps is the Recaps tab, running commands. An open board reads every listed window's
// recap once the list lands, and a printed one only the recap it draws, since a board printed
// once reads no detail either.
func (b *Board) OpenRecaps(commands config.Recaps) *Recaps {
	r := &Recaps{board: b, commands: commands, renderer: markdown.New(nil), recaps: map[RecapWindow]*recapRecord{}}
	r.list = gotermboard.NewSource("list", recapsListEvery, r.readList)
	r.records = gotermboard.NewSource("recaps", recapsListEvery, r.readRecaps)
	return r
}

// readList runs the list command, then reads the recap the screen shows again, so a read
// again of the list shows the newest of both. The rest read on records, which this starts.
func (r *Recaps) readList(ctx context.Context) (recapList, string, error) {
	out, err := pullrequests.Run(ctx, "", r.commands.List...)
	if err != nil {
		return recapList{}, "", err
	}
	list := parseRecapList(out)
	if shown, ok := r.shownIn(list); ok {
		r.readRecap(ctx, shown, true)
	}
	r.records.Force()
	note := ""
	if list.skipped > 0 {
		note = fmt.Sprintf("%s printed not a kind and a label, so left out", counted(list.skipped, "line"))
	}
	return list, note, nil
}

// shownIn is the window whose recap the screen shows: the cursor's where the list holds it,
// else the list's first, where the cursor starts.
func (r *Recaps) shownIn(list recapList) (RecapWindow, bool) {
	r.mu.Lock()
	cursor := r.cursor
	r.mu.Unlock()
	if w, ok := recapWindowOf(cursor); ok && slices.Contains(list.windows, w) {
		return w, true
	}
	if len(list.windows) == 0 {
		return RecapWindow{}, false
	}
	return list.windows[0], true
}

// readRecaps reads each listed window's recap that is due, the cursor's first, a few at a
// time. A printed tab reads only the cursor's. It fails naming the first recap it could not
// read, so the strip says so.
func (r *Recaps) readRecaps(ctx context.Context) (int, string, error) {
	list := r.list.Value()
	shown, onList := r.shownIn(list)
	var due []RecapWindow
	r.mu.Lock()
	for _, w := range list.windows {
		if r.recordLocked(w).due() && (r.board.readDetails || w == shown) {
			due = append(due, w)
		}
	}
	r.mu.Unlock()
	if onList {
		if at := slices.Index(due, shown); at > 0 {
			due = slices.Insert(slices.Delete(due, at, at+1), 0, shown)
		}
	}
	slots := make(chan struct{}, recapReadsAtOnce)
	var wg sync.WaitGroup
	for _, w := range due {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			r.readRecap(ctx, w, false)
		})
	}
	wg.Wait()
	var faults []string
	r.mu.Lock()
	for _, w := range due {
		if fault := r.recordLocked(w).fault; fault != "" {
			faults = append(faults, w.String()+": "+fault)
		}
	}
	r.mu.Unlock()
	if len(faults) > 0 {
		return len(due), "", fmt.Errorf("could not read %s of %d, first %s", counted(len(faults), "recap"), len(due), faults[0])
	}
	return len(due), "", nil
}

// readRecap runs show for the window, where always says to or its recap is due, unless a read
// of it is under way. A failure keeps the recap last read.
func (r *Recaps) readRecap(ctx context.Context, w RecapWindow, always bool) {
	r.mu.Lock()
	record := r.recordLocked(w)
	if record.reading || !always && !record.due() {
		r.mu.Unlock()
		return
	}
	record.reading = true
	r.mu.Unlock()
	out, err := pullrequests.Run(ctx, "", config.FillRecap(r.commands.Show, w.Kind, w.Label)...)
	r.mu.Lock()
	defer r.mu.Unlock()
	record.reading = false
	if err != nil {
		record.fault = err.Error()
		return
	}
	record.markdown, record.held, record.readAt, record.fault, record.stale = string(out), true, time.Now(), "", false
}

// recordLocked is the window's record, made where there is none. The caller holds mu.
func (r *Recaps) recordLocked(w RecapWindow) *recapRecord {
	record, ok := r.recaps[w]
	if !ok {
		record = &recapRecord{}
		r.recaps[w] = record
	}
	return record
}

// recordOf is a copy of how the window's recap stands now.
func (r *Recaps) recordOf(w RecapWindow) recapRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.recordLocked(w)
}

// Sources is gotermboard.Board's: the list, then the recaps it names.
func (r *Recaps) Sources() []gotermboard.Refresher {
	return []gotermboard.Refresher{r.list, r.records}
}

// Groupings is gotermboard.Board's. The tab has one.
func (r *Recaps) Groupings() []string { return nil }

// Look is gotermboard.LookBoard's: the board's own, so the margin holds still across tabs,
// with the columns kept together, since two short ones spread across the width read apart.
func (r *Recaps) Look(width int) gotermboard.Look {
	look := r.board.Look(width)
	look.Spread = false
	return look
}

// CursorOn is gotermboard.CursorBoard's. A cursor landing on a window whose recap is not read
// reads it on the next tick, ahead of the rest.
func (r *Recaps) CursorOn(key string) {
	r.mu.Lock()
	r.cursor = key
	w, ok := recapWindowOf(key)
	due := ok && r.recordLocked(w).due()
	r.mu.Unlock()
	if due {
		r.records.Force()
	}
}

// Actions is gotermboard.Keyed's: w writes a recap, where the config names a write command.
func (r *Recaps) Actions() []gotermboard.Action {
	if len(r.commands.Write) == 0 {
		return nil
	}
	return []gotermboard.Action{{
		Keys:   []string{"w"},
		Label:  "write a recap with a model call, after asking: the cursor's window, or another typed as its kind and label",
		Footer: "w write",
		Run:    r.askWrite,
	}}
}

// askWrite asks which window's recap to write, naming the model call. Enter alone writes the
// cursor's, and a kind and a label typed write that window, so one the list does not hold yet
// can be written.
func (r *Recaps) askWrite(m *gotermboard.Model) tea.Cmd {
	shown, onRow := recapWindowOf(m.Cursor())
	text := "write which window's recap, with a model call? Type its kind and its label, as the list names a window."
	if onRow {
		text = fmt.Sprintf("write the recap of %s with a model call? [Y/n], or type another window's kind and label", shown)
	}
	m.Ask(gotermboard.Question{
		Text:  text,
		Typed: true,
		Answer: func(typed string) tea.Cmd {
			w, said := answeredWindow(typed, shown, onRow)
			if said != "" {
				m.Say(said)
				return nil
			}
			if r.writingOf(w) {
				m.Say(fmt.Sprintf("a write of the recap of %s is under way already", w))
				return nil
			}
			cmd := r.startWrite(w)
			m.Rebuild()
			return cmd
		},
		Cancel: func() tea.Cmd {
			m.Say("wrote no recap")
			return nil
		},
	})
	return nil
}

// answeredWindow is the window a typed answer names, or what the footer says where it names
// none: enter or yes is the cursor's window, no writes nothing, and two words are a kind and
// a label.
func answeredWindow(typed string, shown RecapWindow, onRow bool) (RecapWindow, string) {
	fields := strings.Fields(typed)
	switch strings.ToLower(typed) {
	case "", "y", "yes":
		if onRow {
			return shown, ""
		}
		return RecapWindow{}, "nothing typed, so no recap is written"
	case "n", "no":
		return RecapWindow{}, "wrote no recap"
	}
	if len(fields) != 2 {
		return RecapWindow{}, fmt.Sprintf("%q is not a kind and a label, as day 2026-11-06, so no recap is written", typed)
	}
	return RecapWindow{Kind: fields[0], Label: fields[1]}, ""
}

func (r *Recaps) writingOf(w RecapWindow) bool {
	return slices.ContainsFunc(r.writing, func(each recapWrite) bool { return each.window == w })
}

// startWrite runs the write command for the window in the background, while the footer names
// it after a spinner.
func (r *Recaps) startWrite(w RecapWindow) tea.Cmd {
	r.writeCount++
	id := r.writeCount
	r.writing = append(r.writing, recapWrite{id: id, window: w})
	argv := config.FillRecap(r.commands.Write, w.Kind, w.Label)
	return func() tea.Msg {
		_, err := pullrequests.RunWithin(context.Background(), recapWriteTimeout, "", argv...)
		return recapWritten{id: id, window: w, err: err}
	}
}

// UnderWay is gotermboard.BusyBoard's: every write w started that has not ended.
func (r *Recaps) UnderWay() string {
	doing := make([]string, len(r.writing))
	for i, w := range r.writing {
		doing[i] = "writing the recap of " + w.window.String()
	}
	return strings.Join(doing, " · ")
}

// Receive is gotermboard.ReceivingBoard's. A write ending reads the list again, and its
// window's recap with it, so a window written for the first time joins the list.
func (r *Recaps) Receive(m *gotermboard.Model, msg tea.Msg) tea.Cmd {
	written, ok := msg.(recapWritten)
	if !ok {
		return nil
	}
	r.writing = slices.DeleteFunc(r.writing, func(w recapWrite) bool { return w.id == written.id })
	r.mu.Lock()
	r.recordLocked(written.window).stale = true
	r.mu.Unlock()
	r.list.Force()
	switch err := written.err; {
	case errors.Is(err, pullrequests.ErrNoAnswer):
		m.Say(fmt.Sprintf("the write of the recap of %s gave %v, so it may still be running; R reads the list again", written.window, err))
	case err != nil:
		m.Say(fmt.Sprintf("could not write the recap of %s: %v", written.window, err))
	default:
		m.Say(fmt.Sprintf("wrote the recap of %s, so the list reads again", written.window))
	}
	m.Rebuild()
	return nil
}

func (r *Recaps) statuses() []gotermboard.Status {
	return []gotermboard.Status{r.list.Status(), r.records.Status()}
}

func (r *Recaps) reading() bool {
	return r.list.Status().Reading || r.records.Status().Reading
}

// MoveEvery is gotermboard.MovingBoard's. The strip turns its spinner while a read runs.
func (r *Recaps) MoveEvery() time.Duration {
	if r.reading() {
		return gotermboard.SpinEvery
	}
	return 0
}

// StripAt is gotermboard.MovingBoard's: how the list and the recaps' reads stand, with a
// spinner after their age while one runs.
func (r *Recaps) StripAt(now time.Time) []gotermboard.Line {
	line := gotermboard.ReadStatus(r.statuses(), now)
	if r.reading() {
		line = slices.Insert(line, 1, gotermboard.C(" "+gotermboard.SpinnerAt(now), gotermboard.Yellow))
	}
	return []gotermboard.Line{line}
}

// recapColumns are the table's: the window's kind, its label, then what is under way on it.
var recapColumns = []gotermboard.Column{{Title: "KIND"}, {Title: "WINDOW"}, {Title: ""}}

// Frame is gotermboard.Board's: the windows in the list's order, each opening its recap.
func (r *Recaps) Frame(now time.Time, _ string) gotermboard.Frame {
	list := r.list.Value()
	group := gotermboard.Group{
		Key: "recaps", Title: gotermboard.Line{gotermboard.C("RECAPS", gotermboard.Bold)},
		Count: len(list.windows), Counted: true, Still: true,
	}
	if len(list.windows) == 0 {
		group.Below = []gotermboard.Line{{gotermboard.C(r.emptyReason(), dim)}}
	}
	for _, w := range list.windows {
		group.Rows = append(group.Rows, r.row(w))
	}
	return gotermboard.Frame{Strip: r.StripAt(now), Columns: recapColumns, Groups: []gotermboard.Group{group}}
}

// emptyReason is what the tab says with no window to list.
func (r *Recaps) emptyReason() string {
	status := r.list.Status()
	switch {
	case status.ReadAt.IsZero() && status.Fault != "":
		return "The list could not be run, and the line above says why."
	case status.ReadAt.IsZero():
		return "Running the list."
	case len(r.commands.Write) > 0:
		return "The list names no window, so no recap is written yet. w writes one."
	}
	return "The list names no window, so no recap is written yet."
}

// row is one window: its kind and label, what is under way on it, and its recap to open.
func (r *Recaps) row(w RecapWindow) gotermboard.Row {
	record := r.recordOf(w)
	state := gotermboard.C("", "")
	switch {
	case r.writingOf(w):
		state = gotermboard.C("writing", gotermboard.Yellow)
	case record.fault != "":
		state = gotermboard.C("could not read", gotermboard.Red)
	}
	return gotermboard.Row{
		Key:         w.key(),
		Cells:       []gotermboard.Cell{gotermboard.C(w.Kind, ""), gotermboard.C(w.Label, ""), state},
		Document:    r.document(w),
		ReaderTitle: w.String(),
		Text:        record.markdown,
	}
}

// document is the window's recap laid out at the width it is drawn at.
func (r *Recaps) document(w RecapWindow) gotermboard.Document {
	return func(width int) []gotermboard.Line {
		return r.renderer.Lines(r.markdownOf(w, time.Now()), min(width, gotermboard.ReadingWidth))
	}
}

// markdownOf is the window's recap as show printed it, under a line saying a write is under
// way or the newest read failed, or what stands in for it before a read lands.
func (r *Recaps) markdownOf(w RecapWindow, now time.Time) string {
	record := r.recordOf(w)
	var lead []string
	if r.writingOf(w) {
		lead = append(lead, "_A write of this recap is under way, and the list reads again once it ends._")
	}
	switch {
	case record.held && record.fault != "":
		lead = append(lead, fmt.Sprintf("_Read %s ago, and the newest read failed: %s_", gotermboard.Ago(now.Sub(record.readAt)), record.fault))
	case !record.held && record.fault != "":
		lead = append(lead, "_Could not read it: "+record.fault+"_")
	case !record.held:
		lead = append(lead, "_Reading._")
	}
	return strings.Join(append(lead, record.markdown), "\n\n")
}

// Faults name the list's failure, the lines it printed that were not a window, and each recap
// read that failed, so a printed tab exits 1 where it misses one.
func (r *Recaps) Faults() []string {
	var faults []string
	status := r.list.Status()
	if status.Fault != "" {
		faults = append(faults, "could not run the list: "+status.Fault)
	}
	if status.Note != "" {
		faults = append(faults, "the list's "+status.Note)
	}
	for _, w := range r.list.Value().windows {
		if fault := r.recordOf(w).fault; fault != "" {
			faults = append(faults, fmt.Sprintf("could not read the recap of %s: %s", w, fault))
		}
	}
	return faults
}

// RecapsSnapshot is what prs tui --view recaps --json prints: each window the list named, in
// its order, with the recap read for it, and how each read stands.
// cmd/prs/recaps.schema.json is its shape.
type RecapsSnapshot struct {
	View    string               `json:"view"`
	Windows []RecapSnapshot      `json:"windows"`
	Sources []gotermboard.Status `json:"sources"`
}

// RecapSnapshot is one window, its recap as show printed it, null where none was read, and
// why the newest read failed.
type RecapSnapshot struct {
	RecapWindow
	Recap *string `json:"recap"`
	Fault string  `json:"fault,omitempty"`
}

// Snapshot reads what the tab holds now.
func (r *Recaps) Snapshot() RecapsSnapshot {
	snap := RecapsSnapshot{View: RecapsView, Windows: []RecapSnapshot{}, Sources: r.statuses()}
	for _, w := range r.list.Value().windows {
		record := r.recordOf(w)
		window := RecapSnapshot{RecapWindow: w, Fault: record.fault}
		if record.held {
			window.Recap = &record.markdown
		}
		snap.Windows = append(snap.Windows, window)
	}
	return snap
}
