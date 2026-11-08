package board

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermboard/chart"

	"github.com/datapointchris/prs/internal/summary"
)

// The charts' layout. A screen this wide sets the two smaller charts side by side under the
// merges, and the charts take at most this share of the screen's lines, so the counts show.
const (
	summaryPairedFrom  = 150
	summaryChartsShare = 0.55
	summaryChartGap    = 3
	summaryLeastLines  = 5
)

// The colors a person's and an app's pull requests draw in, on every chart.
const (
	peopleCode = gotermboard.Cyan
	botsCode   = gotermboard.Magenta
)

// summaryCharts draws the merges over the window, the open pull requests over it and the
// times to merge, in width by height. end is when the window ends, as "now" or "at" an
// instant.
func summaryCharts(n summary.Numbers, end string, width, height int) []gotermboard.Line {
	room := int(float64(height) * summaryChartsShare)
	openOver := func(n summary.Numbers, width, height int) []gotermboard.Line {
		return openOverLines(n, end, width, height)
	}
	var out []gotermboard.Line
	if width >= summaryPairedFrom {
		lines := max((room-2)/2, summaryLeastLines)
		left := (width - summaryChartGap) / 2
		right := width - left - summaryChartGap
		out = append(out, fittedTo(mergedOverLines(n, width, lines), width)...)
		out = append(out, gotermboard.Line{})
		out = append(out, chart.Beside(summaryChartGap, fittedTo(openOver(n, left, lines), left), fittedTo(timeToMergeLines(n, right, lines), right))...)
		return append(out, gotermboard.Line{})
	}
	lines := max((room-3)/3, summaryLeastLines)
	for _, draw := range []func(summary.Numbers, int, int) []gotermboard.Line{mergedOverLines, openOver, timeToMergeLines} {
		out = append(append(out, fittedTo(draw(n, width, lines), width)...), gotermboard.Line{})
	}
	return out
}

// mergedOverLines is the merges per hour or per day as columns, the bots' drawn over the
// whole, so each column reads as the bots' merges under the people's.
func mergedOverLines(n summary.Numbers, width, height int) []gotermboard.Line {
	all := make([]chart.Point, 0, len(n.MergedOver))
	bots := make([]chart.Point, 0, len(n.MergedOver))
	for _, period := range n.MergedOver {
		all = append(all, chart.Point{At: period.Start, Value: float64(period.Merged.All)})
		bots = append(bots, chart.Point{At: period.Start, Value: float64(period.Merged.Bots)})
	}
	ts := chart.TimeSeries{
		Series: []chart.Series{
			{Name: fmt.Sprintf("people %d", n.Merged.People), Points: all, Code: peopleCode, Columns: true},
			{Name: fmt.Sprintf("bots %d", n.Merged.Bots), Points: bots, Code: botsCode, Columns: true},
		},
		To: n.Until, Min: 0, ValueLabel: wholeCount,
	}
	if len(n.MergedOver) > 0 {
		ts.From = n.MergedOver[0].Start
	}
	title := chartTitle("MERGED PER "+strings.ToUpper(n.MergedPer), fmt.Sprintf("%d in %s", n.Merged.All, n.Window))
	return append([]gotermboard.Line{append(append(title, gotermboard.C("  ", "")), ts.Legend()...)}, ts.Lines(width, height-1)...)
}

// openOverLines is the open pull requests across the window as a sparkline, under the count
// at its end and the most and fewest open at once.
func openOverLines(n summary.Numbers, end string, width, height int) []gotermboard.Line {
	values := make([]float64, 0, len(n.OpenOver))
	most, fewest := 0, -1
	for _, at := range n.OpenOver {
		values = append(values, float64(at.Open.All))
		most = max(most, at.Open.All)
		if fewest < 0 || at.Open.All < fewest {
			fewest = at.Open.All
		}
	}
	title := chartTitle("OPEN", fmt.Sprintf("%d %s, %d at most, %d at fewest", n.Open.All, end, most, max(fewest, 0)))
	spark := chart.Sparkline{Values: values, Code: peopleCode, Max: float64(most), Fill: true}
	return append([]gotermboard.Line{title}, spark.Lines(width, height-1)...)
}

// timeToMergeLines is the merges by how long each took from opening, the bots' stacked on
// the people's.
func timeToMergeLines(n summary.Numbers, width, height int) []gotermboard.Line {
	bars := make([]chart.Bar, 0, len(n.TimeToMerge))
	for _, bucket := range n.TimeToMerge {
		bars = append(bars, chart.Bar{Label: bucket.Label, Stack: []chart.Part{
			{Value: float64(bucket.Merged.People), Code: peopleCode},
			{Value: float64(bucket.Merged.Bots), Code: botsCode},
		}})
	}
	says := "none merged"
	if median := n.MedianSecondsToMerge.All; median != nil {
		says = "median " + durationText(*median) + ", p90 " + durationText(*n.P90SecondsToMerge.All)
	}
	title := chartTitle("TIME TO MERGE", says)
	return append([]gotermboard.Line{title}, chart.Bars{Bars: bars, ValueLabel: wholeCount}.Lines(width, height-1)...)
}

// chartTitle is a chart's title in bold, then what it measures.
func chartTitle(title, says string) gotermboard.Line {
	return gotermboard.Line{gotermboard.C(title, gotermboard.Bold), gotermboard.C("  "+says, gotermboard.RuleCode)}
}

// wholeCount writes a count on an axis, a whole number where it is one.
func wholeCount(v float64) string {
	if v == float64(int(v)) {
		return fmt.Sprint(int(v))
	}
	return chart.Compact(v)
}

// fittedTo is lines cut to width, so a chart laid beside another keeps to its share.
func fittedTo(lines []gotermboard.Line, width int) []gotermboard.Line {
	out := make([]gotermboard.Line, len(lines))
	for i, line := range lines {
		left := width
		for _, cell := range line {
			if left <= 0 {
				break
			}
			if w := ansi.StringWidth(cell.Text); w > left {
				cell.Text = ansi.Truncate(cell.Text, left, "…")
			}
			left -= ansi.StringWidth(cell.Text)
			out[i] = append(out[i], cell)
		}
	}
	return out
}
