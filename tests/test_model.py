import unittest
from datetime import date, datetime, timezone

from timew_api import model


def iv(start, end=None, tags=("client.project",), note="", id=1):
    d = {"id": id, "start": start, "tags": list(tags)}
    if end:
        d["end"] = end
    if note:
        d["annotation"] = note
    return d


class Parsing(unittest.TestCase):
    def test_timew_date_round_trip(self):
        dt = model.parse_timew_date("20261026T110000Z")
        self.assertEqual(dt, datetime(2026, 10, 26, 11, tzinfo=timezone.utc))
        self.assertEqual(model.to_timew_date(dt), "20261026T110000Z")

    def test_bad_timew_date(self):
        with self.assertRaises(model.InvalidInput):
            model.parse_timew_date("2026-10-26")

    def test_parse_hm(self):
        self.assertEqual(model.parse_hm("9:05"), "09:05")
        self.assertEqual(model.parse_hm("1500"), "15:00")
        for bad in ("24:00", "12:60", "noon", ""):
            with self.assertRaises(model.InvalidInput):
                model.parse_hm(bad)

    def test_parse_day(self):
        today = date(2026, 10, 30)
        self.assertEqual(model.parse_day("", today), today)
        self.assertEqual(model.parse_day("2026-10-26", today), date(2026, 10, 26))
        for bad in ("26/10/2026", "2026-02-30"):
            with self.assertRaises(model.InvalidInput):
                model.parse_day(bad, today)

    def test_tags_that_would_change_the_command_are_refused(self):
        self.assertEqual(model.clean_tags([" a ", "", "two words"]), ["a", "two words"])
        for bad in ([":yes"], ["@1"], ["-"], "a b", [1]):
            with self.assertRaises(model.InvalidInput):
                model.clean_tags(bad)

    def test_track_args(self):
        self.assertEqual(
            model.track_args(date(2026, 10, 26), "11:00", "13:00", ["t"]),
            ["track", "2026-10-26T11:00:00", "-", "2026-10-26T13:00:00", "t"],
        )
        self.assertEqual(model.track_args(date(2026, 10, 26), "11:00", "13:00", [], adjust=True)[-1], ":adjust")
        with self.assertRaises(model.InvalidInput):
            model.track_args(date(2026, 10, 26), "13:00", "11:00", [])


class Totals(unittest.TestCase):
    def test_running_counts_to_now(self):
        now = datetime(2026, 10, 30, 12, tzinfo=timezone.utc)
        self.assertEqual(model.duration(iv("20261030T110000Z"), now), 3600)

    def test_totals_use_irish_days_and_monday_weeks(self):
        # Sun 1 Nov 2026 is in GMT; the week started Mon 26 Oct.
        now = datetime(2026, 11, 1, 12, tzinfo=timezone.utc)
        ivs = [
            iv("20261026T110000Z", "20261026T130000Z"),   # Mon, this week, October
            iv("20261101T090000Z", "20261101T100000Z"),   # Sun, today, November
            iv("20261025T090000Z", "20261025T100000Z"),   # previous week
        ]
        self.assertEqual(model.totals(ivs, now), {"today": 3600, "week": 3 * 3600, "month": 3600})

    def test_find_id_by_start(self):
        ivs = [iv("20261026T110000Z", id=2), iv("20261026T150000Z", id=1)]
        self.assertEqual(model.find_id(ivs, "20261026T150000Z"), 1)
        self.assertIsNone(model.find_id(ivs, "20261026T160000Z"))


class Report(unittest.TestCase):
    def test_weekly_format(self):
        ivs = [
            iv("20261026T110000Z", "20261026T130000Z", note="Contractor training"),
            iv("20261026T150000Z", "20261026T200000Z", note="1-1 with Sam."),
            iv("20261027T120000Z", "20261027T211105Z", note="project setup"),
            iv("20261030T113000Z"),  # still running: not reported
        ]
        r = model.weekly_report(ivs, date(2026, 10, 26), "Client – Project")
        self.assertEqual(r["text"], "\n".join([
            "Week of 26–27 Oct 2026 (Client – Project), 16:11 h total",
            "All times Irish time (GMT, UTC+0)",
            "",
            "Mon 26 Oct, 11:00–13:00 and 15:00–20:00 (7:00): Contractor training, 1-1 with Sam.",
            "Tue 27 Oct, 12:00–21:11 (9:11): project setup.",
        ]))
        # Billing rounds each day down to whole hours: 7 + 9.
        self.assertEqual(r["billed_hours"], 16)

    def test_no_project_label(self):
        r = model.weekly_report([iv("20261026T110000Z", "20261026T130000Z")], date(2026, 10, 26))
        self.assertTrue(r["text"].startswith("Week of 26–26 Oct 2026, 2:00 h total"))

    def test_summer_weeks_say_ist(self):
        ivs = [iv("20260601T080000Z", "20260601T160000Z")]
        r = model.weekly_report(ivs, date(2026, 6, 1))
        self.assertIn("IST, UTC+1", r["text"])
        self.assertIn("09:00–17:00", r["text"])

    def test_empty_week(self):
        self.assertEqual(model.weekly_report([], date(2026, 10, 26))["text"], "")


if __name__ == "__main__":
    unittest.main()
