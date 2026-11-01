// Package model holds the pure logic: parsing timew's output, validating
// input, totals and the weekly report. Nothing here runs a process or touches
// the network, so all of it is unit tested directly.
package model

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the zone is compiled in, so the binary needs no zoneinfo
)

// Dublin is the zone every day boundary and report uses.
var Dublin = mustZone("Europe/Dublin")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Interval is one entry as `timew export` prints it.
type Interval struct {
	ID         int      `json:"id"`
	Start      string   `json:"start"`
	End        string   `json:"end,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Annotation string   `json:"annotation,omitempty"`
}

// Public is what the API returns for an entry. It has no id: timew renumbers
// ids whenever anything changes, so entries are named by their start.
type Public struct {
	Start      string   `json:"start"`
	End        *string  `json:"end"`
	Tags       []string `json:"tags"`
	Annotation string   `json:"annotation"`
	Seconds    *int64   `json:"seconds,omitempty"`
}

// Invalid is an input error: the request was wrong, nothing was changed.
type Invalid struct{ Msg string }

func (e *Invalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &Invalid{fmt.Sprintf(format, a...)} }

const timewLayout = "20060102T150405Z"

// ParseTimew reads timew's compact UTC timestamp, e.g. 20261026T110000Z.
func ParseTimew(s string) (time.Time, error) {
	t, err := time.Parse(timewLayout, s)
	if err != nil {
		return time.Time{}, invalid("not a timew timestamp: %q", s)
	}
	return t, nil
}

// FormatTimew is the inverse of ParseTimew.
func FormatTimew(t time.Time) string { return t.UTC().Format(timewLayout) }

func mustTimew(s string) time.Time {
	t, _ := ParseTimew(s)
	return t
}

// Seconds in an interval; the running one (no end) counts up to now.
func Duration(iv Interval, now time.Time) int64 {
	start := mustTimew(iv.Start)
	end := now
	if iv.End != "" {
		end = mustTimew(iv.End)
	}
	if d := int64(end.Sub(start) / time.Second); d > 0 {
		return d
	}
	return 0
}

func Running(ivs []Interval) *Interval {
	for i := range ivs {
		if ivs[i].End == "" {
			return &ivs[i]
		}
	}
	return nil
}

// Day is local midnight in Dublin, used as a calendar date.
func Day(t time.Time) time.Time {
	t = t.In(Dublin)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, Dublin)
}

func LocalDay(iv Interval) time.Time { return Day(mustTimew(iv.Start)) }

// WeekStart is the Monday of d's week, matching timew's own `:week`.
func WeekStart(d time.Time) time.Time {
	d = Day(d)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

type Totals struct {
	Today int64 `json:"today"`
	Week  int64 `json:"week"`
	Month int64 `json:"month"`
}

func ComputeTotals(ivs []Interval, now time.Time) Totals {
	today := Day(now)
	week, month := WeekStart(today), today.AddDate(0, 0, 1-today.Day())
	var t Totals
	for _, iv := range ivs {
		d, start := Duration(iv, now), mustTimew(iv.Start)
		if LocalDay(iv).Equal(today) {
			t.Today += d
		}
		if !start.Before(week) {
			t.Week += d
		}
		if !start.Before(month) {
			t.Month += d
		}
	}
	return t
}

// HM is hours:minutes, the unit hours get billed in: 9:11.
func HM(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%d:%02d", sec/3600, sec%3600/60)
}

var hmRe = regexp.MustCompile(`^\s*(\d{1,2}):?(\d{2})\s*$`)

// ParseHM accepts "15:00", "9:05" or "1500" and returns "15:00".
func ParseHM(text string) (string, error) {
	m := hmRe.FindStringSubmatch(text)
	if m == nil {
		return "", invalid("time must be HH:MM, got %q", text)
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	if h > 23 || mi > 59 {
		return "", invalid("time out of range: %q", text)
	}
	return fmt.Sprintf("%02d:%02d", h, mi), nil
}

// ParseDay accepts YYYY-MM-DD, or empty for the fallback.
func ParseDay(text string, fallback time.Time) (time.Time, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Day(fallback), nil
	}
	d, err := time.ParseInLocation("2006-01-02", text, Dublin)
	if err != nil {
		return time.Time{}, invalid("date must be a real YYYY-MM-DD, got %q", text)
	}
	return d, nil
}

// CleanTags trims and drops empty tags. A tag that looks like a timew hint
// or id would change the command's meaning, so those are refused.
func CleanTags(tags []string) ([]string, error) {
	out := []string{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, ":") || strings.HasPrefix(t, "@") || t == "-" {
			return nil, invalid("tag not allowed: %q", t)
		}
		out = append(out, t)
	}
	return out, nil
}

func StartArgs(tags []string, at string) []string {
	args := []string{"start"}
	if at != "" {
		args = append(args, at)
	}
	return append(args, tags...)
}

// TrackArgs builds a past block. timew reads these bare datetimes as local time.
func TrackArgs(day time.Time, from, to string, tags []string, adjust bool) ([]string, error) {
	if to <= from {
		return nil, invalid("end must be after start")
	}
	d := day.Format("2006-01-02")
	args := append([]string{"track", d + "T" + from + ":00", "-", d + "T" + to + ":00"}, tags...)
	if adjust {
		args = append(args, ":adjust")
	}
	return args, nil
}

// FindID looks an entry's current id up by its start timestamp.
func FindID(ivs []Interval, start string) (int, bool) {
	for _, iv := range ivs {
		if iv.Start == start {
			return iv.ID, true
		}
	}
	return 0, false
}

func IsOverlap(msg string) bool { return strings.Contains(strings.ToLower(msg), "overlap") }

func ToPublic(iv Interval) Public {
	p := Public{Start: iv.Start, Tags: iv.Tags, Annotation: iv.Annotation}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	if iv.End != "" {
		end := iv.End
		p.End = &end
	}
	return p
}

func TZLabel(day time.Time) string {
	if _, off := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, Dublin).Zone(); off != 0 {
		return "Irish time (IST, UTC+1)"
	}
	return "Irish time (GMT, UTC+0)"
}

type Report struct {
	Text        string `json:"text"`
	Tracked     int64  `json:"tracked"`
	BilledHours int64  `json:"billed_hours"`
}

// WeeklyReport is the plain-text week, ready to paste into a timesheet tool.
// Each day lists its start–end ranges and tracked h:mm. Billing rounds each
// day down to whole hours; that figure is returned alongside, never instead.
func WeeklyReport(ivs []Interval, monday time.Time, project string) Report {
	byDay := map[time.Time][]Interval{}
	sorted := append([]Interval(nil), ivs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	for _, iv := range sorted {
		if iv.End == "" {
			continue
		}
		if d := LocalDay(iv); WeekStart(d).Equal(Day(monday)) {
			byDay[d] = append(byDay[d], iv)
		}
	}
	if len(byDay) == 0 {
		return Report{}
	}
	days := make([]time.Time, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	var lines []string
	var r Report
	for _, d := range days {
		var secs int64
		var ranges, notes []string
		for _, iv := range byDay[d] {
			secs += Duration(iv, time.Time{})
			ranges = append(ranges, mustTimew(iv.Start).In(Dublin).Format("15:04")+"–"+mustTimew(iv.End).In(Dublin).Format("15:04"))
			// The same note on two entries of a day (e.g. both halves around
			// a break) is shown once.
			if n := strings.TrimRight(strings.TrimSpace(iv.Annotation), "."); n != "" && !slices.Contains(notes, n) {
				notes = append(notes, n)
			}
		}
		r.Tracked += secs
		r.BilledHours += secs / 3600
		note := strings.Join(notes, ", ")
		if note == "" {
			note = "No notes"
		}
		lines = append(lines, fmt.Sprintf("%s, %s (%s): %s.", d.Format("Mon 2 Jan"), strings.Join(ranges, " and "), HM(secs), note))
	}

	first, last := days[0], days[len(days)-1]
	span := fmt.Sprintf("%d–%s", first.Day(), last.Format("2 Jan 2006"))
	if first.Month() != last.Month() {
		span = first.Format("2 Jan") + " – " + last.Format("2 Jan 2006")
	}
	label := ""
	if project != "" {
		label = " (" + project + ")"
	}
	head := []string{fmt.Sprintf("Week of %s%s, %s h total", span, label, HM(r.Tracked)), "All times " + TZLabel(first), ""}
	r.Text = strings.Join(append(head, lines...), "\n")
	return r
}
