package pullrequests

import "time"

// ParseUntil reads --until in --since's grammar, as the instant a window ends before. A span
// counts back from now, and an instant is itself. A day takes in the whole of it, so the
// window ends at the next day's first instant in loc.
func ParseUntil(text string, now time.Time, loc *time.Location) (time.Time, error) {
	if day, err := time.ParseInLocation(time.DateOnly, text, loc); err == nil {
		return time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc), nil
	}
	since, err := ParseSince(text, loc)
	if err != nil {
		return time.Time{}, err
	}
	return since.From(now), nil
}
