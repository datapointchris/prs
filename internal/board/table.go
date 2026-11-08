package board

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/datapointchris/gotermboard"
)

// columnGap is the space between two columns of the printed table.
const columnGap = "  "

// Table is what `prs list` prints: how each read stands, then the board's rows as a table
// with a header, in the board's order, each row's title last and unwrapped, for output nobody
// scrolls.
func (b *Board) Table(now time.Time) []gotermboard.Line {
	prs := b.records(now)
	parents := stackParents(prs)
	listed := b.listedColumns(prs, now)
	columns := headers(listed)
	lines := []gotermboard.Line{gotermboard.ReadStatus(b.statuses(), now)}
	if len(prs) == 0 {
		return append(lines, gotermboard.Line{gotermboard.C(b.emptyReason(), dim)})
	}
	header := make([]gotermboard.Cell, len(columns))
	for i, column := range columns {
		header[i] = gotermboard.C(column.Title, gotermboard.Bold)
	}
	rows := [][]gotermboard.Cell{header}
	for _, p := range ordered(placedBy(prs)) {
		at := rowAt{pr: p.pr, state: b.stateOf(p.pr), now: now}
		rows = append(rows, append(b.cells(listed, at), titleLine(p.pr, parentOf(p.pr, parents), at.state)...))
	}
	widths := make([]int, len(columns)-1)
	for _, cells := range rows {
		for i := range widths {
			widths[i] = max(widths[i], ansi.StringWidth(cells[i].Text))
		}
	}
	for _, cells := range rows {
		var line gotermboard.Line
		for i, width := range widths {
			pad := gotermboard.C(strings.Repeat(" ", width-ansi.StringWidth(cells[i].Text)), "")
			if columns[i].Right {
				line = append(line, pad, cells[i])
			} else {
				line = append(line, cells[i], pad)
			}
			line = append(line, gotermboard.C(columnGap, ""))
		}
		lines = append(lines, append(line, cells[len(widths):]...))
	}
	return lines
}
