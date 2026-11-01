package model

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func iv(start, end, note string) Interval {
	return Interval{Start: start, End: end, Tags: []string{"client.project"}, Annotation: note}
}

func day(s string) time.Time {
	d, err := ParseDay(s, time.Time{})
	if err != nil {
		panic(err)
	}
	return d
}

func isInvalid(err error) bool {
	var inv *Invalid
	return errors.As(err, &inv)
}

func TestTimewDateRoundTrip(t *testing.T) {
	ts, err := ParseTimew("20261026T110000Z")
	if err != nil || !ts.Equal(time.Date(2026, 10, 26, 11, 0, 0, 0, time.UTC)) {
		t.Fatal(ts, err)
	}
	if got := FormatTimew(ts); got != "20261026T110000Z" {
		t.Fatal(got)
	}
	if _, err := ParseTimew("2026-10-26"); !isInvalid(err) {
		t.Fatal("accepted a non-timew date")
	}
}

func TestParseHM(t *testing.T) {
	for in, want := range map[string]string{"9:05": "09:05", "1500": "15:00", " 23:59 ": "23:59"} {
		if got, err := ParseHM(in); err != nil || got != want {
			t.Errorf("%q → %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"24:00", "12:60", "noon", ""} {
		if _, err := ParseHM(bad); !isInvalid(err) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseDay(t *testing.T) {
	today := time.Date(2026, 10, 30, 15, 0, 0, 0, Dublin)
	if d, _ := ParseDay("", today); !d.Equal(time.Date(2026, 10, 30, 0, 0, 0, 0, Dublin)) {
		t.Fatal(d)
	}
	for _, bad := range []string{"26/10/2026", "2026-02-30"} {
		if _, err := ParseDay(bad, today); !isInvalid(err) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestTagsThatWouldChangeTheCommandAreRefused(t *testing.T) {
	got, err := CleanTags([]string{" a ", "", "two words"})
	if err != nil || strings.Join(got, "|") != "a|two words" {
		t.Fatal(got, err)
	}
	for _, bad := range []string{":yes", "@1", "-"} {
		if _, err := CleanTags([]string{bad}); !isInvalid(err) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestTrackArgs(t *testing.T) {
	args, _ := TrackArgs(day("2026-10-26"), "11:00", "13:00", []string{"t"}, false)
	if strings.Join(args, " ") != "track 2026-10-26T11:00:00 - 2026-10-26T13:00:00 t" {
		t.Fatal(args)
	}
	args, _ = TrackArgs(day("2026-10-26"), "11:00", "13:00", nil, true)
	if args[len(args)-1] != ":adjust" {
		t.Fatal(args)
	}
	if _, err := TrackArgs(day("2026-10-26"), "13:00", "11:00", nil, false); !isInvalid(err) {
		t.Fatal("accepted end before start")
	}
}

func TestRunningCountsToNow(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if d := Duration(iv("20261030T110000Z", "", ""), now); d != 3600 {
		t.Fatal(d)
	}
}

func TestTotalsUseIrishDaysAndMondayWeeks(t *testing.T) {
	// Sun 1 Nov 2026 is in GMT; its week started Mon 26 Oct.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ivs := []Interval{
		iv("20261026T110000Z", "20261026T130000Z", ""), // Mon, this week, October
		iv("20261101T090000Z", "20261101T100000Z", ""), // Sun, today, November
		iv("20261025T090000Z", "20261025T100000Z", ""), // previous week
	}
	if got := ComputeTotals(ivs, now); got != (Totals{Today: 3600, Week: 3 * 3600, Month: 3600}) {
		t.Fatal(got)
	}
}

func TestLocalDayCrossesUTCMidnightInSummer(t *testing.T) {
	// 23:30 UTC on 1 Jun is 00:30 IST on 2 Jun.
	if d := LocalDay(iv("20260601T233000Z", "", "")); !d.Equal(day("2026-06-02")) {
		t.Fatal(d)
	}
}

func TestFindID(t *testing.T) {
	ivs := []Interval{{ID: 2, Start: "20261026T110000Z"}, {ID: 1, Start: "20261026T150000Z"}}
	if id, ok := FindID(ivs, "20261026T150000Z"); !ok || id != 1 {
		t.Fatal(id, ok)
	}
	if _, ok := FindID(ivs, "20261026T160000Z"); ok {
		t.Fatal("found a missing start")
	}
}

func TestWeeklyFormat(t *testing.T) {
	ivs := []Interval{
		iv("20261026T110000Z", "20261026T130000Z", "Contractor training"),
		iv("20261026T150000Z", "20261026T200000Z", "1-1 with Sam."),
		iv("20261027T120000Z", "20261027T211105Z", "project setup"),
		iv("20261030T113000Z", "", ""), // still running: not reported
	}
	r := WeeklyReport(ivs, day("2026-10-26"), "Client – Project")
	want := strings.Join([]string{
		"Week of 26–27 Oct 2026 (Client – Project), 16:11 h total",
		"All times Irish time (GMT, UTC+0)",
		"",
		"Mon 26 Oct, 11:00–13:00 and 15:00–20:00 (7:00): Contractor training, 1-1 with Sam.",
		"Tue 27 Oct, 12:00–21:11 (9:11): project setup.",
	}, "\n")
	if r.Text != want {
		t.Fatalf("got\n%s\nwant\n%s", r.Text, want)
	}
	// Billing rounds each day down to whole hours: 7 + 9.
	if r.BilledHours != 16 || r.Tracked != 16*3600+11*60+5 {
		t.Fatal(r.BilledHours, r.Tracked)
	}
}

func TestWeeklyReportEdges(t *testing.T) {
	if r := WeeklyReport(nil, day("2026-10-26"), ""); r.Text != "" {
		t.Fatal(r.Text)
	}
	r := WeeklyReport([]Interval{iv("20261026T110000Z", "20261026T130000Z", "")}, day("2026-10-26"), "")
	if !strings.HasPrefix(r.Text, "Week of 26–26 Oct 2026, 2:00 h total") || !strings.Contains(r.Text, "No notes.") {
		t.Fatal(r.Text)
	}
	summer := WeeklyReport([]Interval{iv("20260601T080000Z", "20260601T160000Z", "x")}, day("2026-06-01"), "")
	if !strings.Contains(summer.Text, "IST, UTC+1") || !strings.Contains(summer.Text, "09:00–17:00") {
		t.Fatal(summer.Text)
	}
	// A week that spans two months names both.
	span := WeeklyReport([]Interval{
		iv("20261026T110000Z", "20261026T120000Z", "a"),
		iv("20261101T110000Z", "20261101T120000Z", "b"),
	}, day("2026-10-26"), "")
	if !strings.HasPrefix(span.Text, "Week of 26 Oct – 1 Nov 2026") {
		t.Fatal(span.Text)
	}
}
