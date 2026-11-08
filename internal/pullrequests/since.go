package pullrequests

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// Since is where the recently closed pull requests start: a span back from each read, so a
// board left open keeps the same stretch behind it, or a fixed instant.
type Since struct {
	text string
	// span is a span's clock part, and days its calendar part, which steps the calendar so
	// 1d at 09:00 is 09:00 the day before across a change of offset too.
	span time.Duration
	days int
	at   time.Time
}

// span is a whole number and one unit, m meaning minutes. There is no month or year,
// because neither has a fixed length.
var span = regexp.MustCompile(`^(\d+)([smhdw])$`)

var spanUnits = map[string]time.Duration{
	"s": time.Second,
	"m": time.Minute,
	"h": time.Hour,
	"d": 24 * time.Hour,
	"w": 7 * 24 * time.Hour,
}

// ParseSince reads --since: a span such as 30m, 12h, 2d or 1w, a day such as 2026-10-01,
// whose first instant in loc it starts at, or an RFC 3339 instant.
func ParseSince(text string, loc *time.Location) (Since, error) {
	if m := span.FindStringSubmatch(text); m != nil {
		unit := spanUnits[m[2]]
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n > math.MaxInt64/int64(unit) {
			return Since{}, fmt.Errorf("%s is longer than a span can count; write a day instead", text)
		}
		if day := 24 * time.Hour; unit%day == 0 {
			return Since{text: text, days: int(n) * int(unit/day)}, nil
		}
		return Since{text: text, span: time.Duration(n) * unit}, nil
	}
	if day, err := time.ParseInLocation(time.DateOnly, text, loc); err == nil {
		return Since{text: text, at: day}, nil
	}
	if at, err := time.Parse(time.RFC3339, text); err == nil {
		return Since{text: text, at: at}, nil
	}
	return Since{}, fmt.Errorf("%q is not a span (30m, 12h, 2d, 1w), a day (2026-10-01) or an RFC 3339 instant (2026-10-01T14:00:00-04:00)", text)
}

// From is the first instant inside the window for a read at now.
func (s Since) From(now time.Time) time.Time {
	if s.at.IsZero() {
		return now.AddDate(0, 0, -s.days).Add(-s.span)
	}
	return s.at
}

func (s Since) String() string { return s.text }
