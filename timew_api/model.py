"""Pure helpers: parsing, validation, totals and the weekly report.

Nothing here runs a process or touches the network, so all of it is unit
tested directly. Intervals are the dicts `timew export` produces.
"""

import re
from datetime import date, datetime, time, timedelta, timezone
from zoneinfo import ZoneInfo

TZ = ZoneInfo("Europe/Dublin")

_COMPACT = re.compile(r"^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$")
_HM = re.compile(r"^\s*(\d{1,2}):?(\d{2})\s*$")
_DATE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})$")


class InvalidInput(ValueError):
    pass


def parse_timew_date(s):
    """timew exports UTC in compact form: 20261026T110000Z."""
    m = _COMPACT.match(s or "")
    if not m:
        raise InvalidInput(f"not a timew timestamp: {s!r}")
    return datetime(*map(int, m.groups()), tzinfo=timezone.utc)


def to_timew_date(dt):
    return dt.astimezone(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def duration(iv, now):
    """Seconds in an interval; the running one (no end) counts up to now."""
    start = parse_timew_date(iv["start"])
    end = parse_timew_date(iv["end"]) if iv.get("end") else now
    return max(0, int((end - start).total_seconds()))


def running(intervals):
    return next((iv for iv in intervals if not iv.get("end")), None)


def local_day(iv):
    return parse_timew_date(iv["start"]).astimezone(TZ).date()


def week_start(d):
    """Monday of d's week, matching timew's own `:week`."""
    return d - timedelta(days=d.weekday())


def total_since(intervals, since_day, now):
    """Seconds in intervals that start on or after local midnight of since_day."""
    since = datetime.combine(since_day, time(), TZ)
    return sum(duration(iv, now) for iv in intervals if parse_timew_date(iv["start"]) >= since)


def totals(intervals, now):
    today = now.astimezone(TZ).date()
    return {
        "today": sum(duration(iv, now) for iv in intervals if local_day(iv) == today),
        "week": total_since(intervals, week_start(today), now),
        "month": total_since(intervals, today.replace(day=1), now),
    }


def hm(seconds):
    """'9:11' — hours:minutes, the unit hours get billed in."""
    seconds = max(0, int(seconds))
    return f"{seconds // 3600}:{seconds % 3600 // 60:02d}"


def parse_hm(text):
    """'15:00', '9:05', '1500' → '15:00'."""
    m = _HM.match(text or "")
    if not m:
        raise InvalidInput(f"time must be HH:MM, got {text!r}")
    h, mi = int(m[1]), int(m[2])
    if h > 23 or mi > 59:
        raise InvalidInput(f"time out of range: {text!r}")
    return f"{h:02d}:{mi:02d}"


def parse_day(text, today):
    """'2026-10-26', or empty for today."""
    text = (text or "").strip()
    if not text:
        return today
    m = _DATE.match(text)
    if not m:
        raise InvalidInput(f"date must be YYYY-MM-DD, got {text!r}")
    try:
        return date(int(m[1]), int(m[2]), int(m[3]))
    except ValueError as e:
        raise InvalidInput(str(e)) from None


def clean_tags(tags):
    """Tags arrive as a JSON list. A tag that looks like a timew hint or
    keyword would change the command's meaning, so those are refused."""
    if not isinstance(tags, list) or not all(isinstance(t, str) for t in tags):
        raise InvalidInput("tags must be a list of strings")
    out = [t.strip() for t in tags if t.strip()]
    for t in out:
        if t.startswith(":") or t.startswith("@") or t == "-":
            raise InvalidInput(f"tag not allowed: {t!r}")
    return out


def start_args(tags, at=None):
    return ["start"] + ([at] if at else []) + tags


def track_args(day, start_hm, end_hm, tags, adjust=False):
    """timew reads these bare datetimes as local time."""
    if end_hm <= start_hm:
        raise InvalidInput("end must be after start")
    d = day.isoformat()
    args = ["track", f"{d}T{start_hm}:00", "-", f"{d}T{end_hm}:00"] + tags
    return args + ([":adjust"] if adjust else [])


def find_id(intervals, start):
    """timew renumbers @ids whenever anything changes, so entries are named
    by their start timestamp and the id is looked up just before use."""
    for iv in intervals:
        if iv["start"] == start:
            return iv["id"]
    return None


def is_overlap(message):
    return "overlap" in (message or "").lower()


def public(iv):
    """What the API returns for an interval: no ids, which go stale."""
    return {
        "start": iv["start"],
        "end": iv.get("end"),
        "tags": iv.get("tags", []),
        "annotation": iv.get("annotation", ""),
    }


def tz_label(day):
    off = datetime.combine(day, time(12), TZ).utcoffset()
    return "Irish time (IST, UTC+1)" if off else "Irish time (GMT, UTC+0)"


def weekly_report(intervals, monday, project=""):
    """The plain-text weekly report, ready to paste into a timesheet tool.

    Each day lists its start–end ranges and tracked h:mm. Billing rounds each
    day down to whole hours; that figure is shown alongside, never instead.
    """
    days = {}
    for iv in sorted(intervals, key=lambda i: i["start"]):
        if not iv.get("end"):
            continue
        d = local_day(iv)
        if week_start(d) == monday:
            days.setdefault(d, []).append(iv)

    lines, total, billed = [], 0, 0
    for d in sorted(days):
        ivs = days[d]
        secs = sum(duration(iv, None) for iv in ivs)
        total += secs
        billed += secs // 3600
        ranges = " and ".join(
            f"{parse_timew_date(i['start']).astimezone(TZ):%H:%M}–{parse_timew_date(i['end']).astimezone(TZ):%H:%M}"
            for i in ivs
        )
        notes = [i.get("annotation", "").strip().rstrip(".") for i in ivs]
        notes = ", ".join(n for n in notes if n) or "No notes"
        lines.append(f"{d:%a} {d.day} {d:%b}, {ranges} ({hm(secs)}): {notes}.")

    if not lines:
        return {"text": "", "tracked": 0, "billed_hours": 0}

    first, last = min(days), max(days)
    span = f"{first.day}–{last.day} {last:%b %Y}" if first.month == last.month else f"{first.day} {first:%b} – {last.day} {last:%b %Y}"
    label = f" ({project})" if project else ""
    head = [f"Week of {span}{label}, {hm(total)} h total", f"All times {tz_label(first)}", ""]
    return {"text": "\n".join(head + lines), "tracked": total, "billed_hours": billed}
