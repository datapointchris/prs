package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/tuidrive"
)

// historyPage is how many pull requests the stand-in sends a page, fewer than GitHub's 100,
// so a read of a few runs over pages as a read of a month does.
const historyPage = 2

// stretch reads the updated: qualifier of a history search: >=from, or from..to.
var stretch = regexp.MustCompile(`updated:(?:>=(\S+)|(\S+)\.\.(\S+))`)

// serveHistory answers a history search from g.history, each alias's matches by when each
// was last updated, a page at a time from the cursor it was handed.
func (g *gitHub) serveHistory(w http.ResponseWriter, variables map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if g.down {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message": "Server Error"}`))
		return
	}
	data := map[string]any{}
	for i := 0; ; i++ {
		query, ok := variables[fmt.Sprintf("q%d", i)].(string)
		if !ok {
			break
		}
		g.historyQueries = append(g.historyQueries, query)
		m := stretch.FindStringSubmatch(query)
		if m == nil {
			http.Error(w, "a history search names no stretch: "+query, http.StatusBadRequest)
			return
		}
		from, _ := time.Parse(time.RFC3339, m[1]+m[2])
		to, _ := time.Parse(time.RFC3339, m[3])
		var matched []map[string]any
		for _, node := range g.history {
			at, _ := time.Parse(time.RFC3339, fmt.Sprint(node["updatedAt"]))
			if !at.Before(from) && (to.IsZero() || !at.After(to)) {
				matched = append(matched, node)
			}
		}
		offset := 0
		if after, _ := variables[fmt.Sprintf("a%d", i)].(string); after != "" {
			offset, _ = strconv.Atoi(after)
		}
		end := min(offset+historyPage, len(matched))
		data[fmt.Sprintf("s%d", i)] = map[string]any{
			"issueCount": len(matched),
			"pageInfo":   map[string]any{"hasNextPage": end < len(matched), "endCursor": strconv.Itoa(end)},
			"nodes":      matched[offset:end],
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// reopen has the history read the pull request numbered number as open, updated now.
func (g *gitHub) reopen(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, node := range g.history {
		if node["number"] == number {
			node["state"], node["closedAt"], node["updatedAt"] = "OPEN", nil, time.Now().UTC().Format(time.RFC3339)
		}
	}
}

// queries is each history search the stand-in was asked, in order.
func (g *gitHub) queries() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.historyQueries)
}

// closedHistory is the merged and closed pull requests the history search finds, placed
// back from now. Within 7 days: people merged #30 in a day with a round's review and #31 in
// an hour, the bot merged #32 in 4 hours, and #36 closed unmerged. Within 30 days as well:
// people merged #33 in a day with a round's review, and #34 closed unmerged. The bot merged
// #35 35 days back, outside both.
func closedHistory(now time.Time) []map[string]any {
	ago := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	hours := func(n float64) time.Duration { return time.Duration(n * float64(time.Hour)) }
	node := func(slug string, number int, bot bool, opened, closed time.Duration, merged, round bool) map[string]any {
		author := map[string]any{"__typename": "User", "login": "octo"}
		if bot {
			author = map[string]any{"__typename": "Bot", "login": "acme-automation"}
		}
		state, mergedAt := "CLOSED", any(nil)
		if merged {
			state, mergedAt = "MERGED", ago(closed)
		}
		var reviews []any
		if round {
			reviews = append(reviews, map[string]any{
				"body":              "<!-- pr-review v6 reviewer=G1 round=1 -->\n## Review\n\nNo findings.",
				"authorAssociation": "NONE", "author": map[string]any{"__typename": "Bot", "login": "acme-reviews"},
			})
		}
		return map[string]any{
			"number": number, "title": fmt.Sprintf("Change %d", number), "url": fmt.Sprintf("https://github.com/%s/pull/%d", slug, number),
			"state": state, "createdAt": ago(opened), "updatedAt": ago(closed), "mergedAt": mergedAt, "closedAt": ago(closed),
			"author": author, "repository": map[string]any{"nameWithOwner": slug},
			"reviews": map[string]any{"nodes": reviews},
		}
	}
	return []map[string]any{
		node("acme/widget", 30, false, hours(72), hours(48), true, true),
		node("acme/widget", 31, false, hours(5), hours(4), true, false),
		node("acme/gadget", 32, true, hours(144), hours(140), true, false),
		node("acme/widget", 36, false, hours(24), hours(2), false, false),
		node("acme/widget", 33, false, hours(20*24), hours(19*24), true, true),
		node("acme/gadget", 34, false, hours(12*24), hours(10*24), false, false),
		node("acme/gadget", 35, true, hours(40*24), hours(35*24), true, false),
	}
}

// The Summary tab counts the window it opens on, 30d, and each window w and W step it to, and
// draws each chart. Its second start draws the counts its cache kept while GitHub fails.
func TestTheSummaryTabCountsEachWindowChartsItAndPaintsFromItsCacheOnTheNextStart(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	s.env["NO_COLOR"] = "1"
	s.github.history = closedHistory(time.Now())
	program := tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--view", "summary"}, Env: s.env, Cols: 200, Rows: 70}
	term := tuidrive.Start(t, program)
	for _, pattern := range []string{
		`merged\s+4\s+3\s+1`, `closed unmerged\s+2\s+2\s+0`, `open now\s+4\s+3\s+1`,
		`median to merge\s+14\.0h\s+24\.0h\s+4\.0h`, `reviewed by a round\s+50%\s+67%\s+0%`,
		`acme/widget\s+3\s+3\s+0`, "MERGED PER DAY", "OPEN  4 now", "TIME TO MERGE  median 14.0h", "MERGES BY REPO · 2",
	} {
		term.WaitForPattern(pattern)
	}
	if !strings.Contains(term.Frame().Text(), "█") {
		t.Errorf("no chart drew a column:\n%s", term.Frame().Text())
	}

	term.Press("W", "W")
	term.WaitFor("counting the last 7d")
	for _, pattern := range []string{`merged\s+3\s+2\s+1`, `closed unmerged\s+1\s+1\s+0`, `median to merge\s+4\.0h\s+12\.5h\s+4\.0h`, `reviewed by a round\s+33%`} {
		term.WaitForPattern(pattern)
	}
	term.Press("w", "w")
	term.WaitFor("counting the last 30d")
	term.WaitForPattern(`merged\s+4\s+3\s+1`)
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
	if queries := s.github.queries(); len(queries) == 0 || !strings.Contains(queries[0], " updated:>=") {
		t.Errorf("the first read should read every pull request updated within the window, asked %q", queries)
	}

	s.github.fail()
	term = tuidrive.Start(t, program)
	term.WaitForPattern(`merged\s+4\s+3\s+1`)
	term.WaitForPattern(`github history.*502`)
	term.Press("q")
	term.WaitExit()
}

// prs summary --json prints the tab's numbers in the shape its schema gives. A second run
// reads only what changed since the first, and a window past what the cache reaches reads
// back to its start. A pull request reopened after a run leaves the closed counts on the
// next.
func TestSummaryJSONHoldsItsSchemaAndReadsOnlyWhatChangedOrWhatTheCacheLacks(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	s.github.history = closedHistory(time.Now())
	schema := schemaIn(t, "summary.schema.json")
	type counted struct {
		Merged         struct{ All, People, Bots int } `json:"merged"`
		ClosedUnmerged struct{ All int }               `json:"closed_unmerged"`
		MergedPer      string                          `json:"merged_per"`
		Repos          []struct {
			Slug string `json:"slug"`
		} `json:"repos"`
		Cache *struct {
			Path string `json:"path"`
		} `json:"cache"`
	}
	read := func(args ...string) counted {
		t.Helper()
		out, said, code := s.run(t, append([]string{"summary", "--json"}, args...)...)
		if code != 0 {
			t.Fatalf("prs summary %q exited %d: %s", args, code, said)
		}
		if err := schema.Validate(decoded(t, out)); err != nil {
			t.Fatalf("prs summary %q printed what its schema refuses: %v\n%s", args, err, out)
		}
		var n counted
		if err := json.Unmarshal([]byte(out), &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	n := read("--since", "30d")
	if n.Merged.All != 4 || n.Merged.People != 3 || n.Merged.Bots != 1 || n.ClosedUnmerged.All != 2 || n.MergedPer != "day" {
		t.Errorf("30d counted %+v", n)
	}
	if len(n.Repos) != 2 || n.Repos[0].Slug != "acme/widget" || n.Cache == nil || n.Cache.Path == "" {
		t.Errorf("30d should name widget's merges first and its cache, read %+v", n)
	}
	first := len(s.github.queries())

	if n := read("--since", "1d"); n.Merged.All != 1 || n.MergedPer != "hour" {
		t.Errorf("1d counted %+v", n)
	}
	for _, query := range s.github.queries()[first:] {
		m := stretch.FindStringSubmatch(query)
		if from, _ := time.Parse(time.RFC3339, m[1]); m[1] == "" || time.Since(from) > time.Hour {
			t.Errorf("a window the cache reaches should read only what changed since the last run, asked %q", query)
		}
	}
	second := len(s.github.queries())

	s.github.reopen(36)
	if n := read("--since", "90d"); n.Merged.All != 5 || n.Merged.Bots != 2 || n.ClosedUnmerged.All != 1 {
		t.Errorf("90d, with #36 reopened, counted %+v", n)
	}
	if !slices.ContainsFunc(s.github.queries()[second:], func(query string) bool { return regexp.MustCompile(`updated:\S+\.\.\S+`).MatchString(query) }) {
		t.Errorf("a window past the cache should read back to its start, asked %q", s.github.queries()[second:])
	}
}
