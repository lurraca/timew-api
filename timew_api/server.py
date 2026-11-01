"""HTTP wrapper around the timew CLI.

Standard library only (plus `cryptography` for the Access token). One small
JSON API and the web page that uses it, both served from the same port.
"""

import argparse
import json
import os
import sys
from datetime import datetime, timedelta, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from importlib import resources
from urllib.parse import parse_qs, unquote, urlparse

from . import model
from .auth import HEADER, AccessVerifier, Unauthorized, who
from .timew import Timew, TimewError

RECENT_DAYS = 35


class App:
    def __init__(self, timew, verifier=None, project="", clock=None):
        self.timew = timew
        self.verifier = verifier
        self.project = project
        self.clock = clock or (lambda: datetime.now(timezone.utc))
        self.page = resources.files("timew_api").joinpath("static/index.html").read_bytes()

    def today(self):
        return self.clock().astimezone(model.TZ).date()

    # ── reads ────────────────────────────────────────────────────────────

    def status(self, _q, _body):
        now, today = self.clock(), self.today()
        since = min(today.replace(day=1), model.week_start(today), today - timedelta(days=RECENT_DAYS))
        ivs = self.timew.export(since)
        run = model.running(ivs)
        recent, seen = [], set()
        for iv in sorted(ivs, key=lambda i: i["start"], reverse=True):
            key = tuple(iv.get("tags", []))
            if key and key not in seen and len(recent) < 5:
                seen.add(key)
                recent.append(list(key))
        return {
            "now": model.to_timew_date(now),
            "running": dict(model.public(run), seconds=model.duration(run, now)) if run else None,
            "totals": model.totals(ivs, now),
            "today": [model.public(iv) for iv in ivs if model.local_day(iv) == today],
            "recent_tags": recent,
        }

    def intervals(self, q, _body):
        since = model.parse_day(q.get("from", ""), self.today() - timedelta(days=7))
        return [model.public(iv) for iv in self.timew.export(since)]

    def report(self, q, _body):
        monday = model.week_start(model.parse_day(q.get("date", ""), self.today()))
        ivs = self.timew.export(monday)
        return dict(model.weekly_report(ivs, monday, self.project), week=monday.isoformat())

    # ── writes ───────────────────────────────────────────────────────────

    def start(self, _q, body):
        tags = model.clean_tags(body.get("tags", []))
        at = model.parse_hm(body["at"]) if body.get("at") else None
        with self.timew.lock:
            self.timew.run(model.start_args(tags, at))
            self._check(lambda ivs: (model.running(ivs) or {}).get("tags", []) == tags, "a tag", self.today())
        return self.status({}, {})

    def stop(self, _q, _body):
        self.timew.run(["stop"])
        return self.status({}, {})

    def resume(self, _q, _body):
        self.timew.run(["continue"])
        return self.status({}, {})

    def annotate(self, _q, body):
        start, text = body.get("start", ""), str(body.get("text", ""))
        model.parse_timew_date(start)
        with self.timew.lock:
            iid = model.find_id(self.timew.export(self._day_of(start)), start)
            if iid is None:
                raise LookupError("no interval starts at " + start)
            self._annotate(iid, start, text)
        return self.status({}, {})

    def track(self, _q, body):
        day = model.parse_day(body.get("date", ""), self.today())
        s, e = model.parse_hm(body.get("from", "")), model.parse_hm(body.get("to", ""))
        tags = model.clean_tags(body.get("tags", []))
        note = str(body.get("note", "")).strip()
        start = model.to_timew_date(datetime.combine(day, datetime.strptime(s, "%H:%M").time(), model.TZ))
        with self.timew.lock:
            self.timew.run(model.track_args(day, s, e, tags, adjust=bool(body.get("adjust"))))
            self._check(lambda ivs: any(i["start"] == start and i.get("tags", []) == tags for i in ivs), "a tag", day)
            if note:
                iid = model.find_id(self.timew.export(day), start)
                if iid is None:
                    raise LookupError("tracked, but could not find it to add the note")
                self._annotate(iid, start, note)
        return self.status({}, {})

    def delete(self, start):
        model.parse_timew_date(start)
        with self.timew.lock:
            iid = model.find_id(self.timew.export(self._day_of(start)), start)
            if iid is None:
                raise LookupError("no interval starts at " + start)
            self.timew.run(["delete", f"@{iid}"])
        return self.status({}, {})

    def _annotate(self, iid, start, text):
        self.timew.run(["annotate", f"@{iid}", text])
        self._check(lambda ivs: any(i["start"] == start and i.get("annotation", "") == text for i in ivs),
                    "the note", self._day_of(start))

    def _check(self, ok, what, since):
        """timew parses any argument that looks like a date or duration as one
        ("sunday", "second", "now"), even in a tag or note, and still exits 0.
        So read back what it stored, and if it isn't what was asked for, put
        everything back with `timew undo` and say why."""
        if not ok(self.timew.export(since - timedelta(days=1))):
            self.timew.run(["undo"])
            raise model.InvalidInput(f"timew read {what} as a date or duration, so nothing was changed. "
                                     "Use more than one word, or a different word")

    def _day_of(self, start):
        # Export from the day before: a local date can be the previous UTC day.
        return model.parse_timew_date(start).astimezone(model.TZ).date() - timedelta(days=1)


GET = {"/api/status": App.status, "/api/intervals": App.intervals, "/api/report": App.report}
POST = {"/api/start": App.start, "/api/stop": App.stop, "/api/continue": App.resume,
        "/api/annotate": App.annotate, "/api/track": App.track}


class Handler(BaseHTTPRequestHandler):
    app: App = None
    server_version = "timew-api"

    def _send(self, status, body, ctype="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "no-referrer")
        if ctype.startswith("text/html"):
            self.send_header("Content-Security-Policy",
                             "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
        self.end_headers()
        self.wfile.write(data)

    def _error(self, status, message, **extra):
        self._send(status, {"error": message, **extra})

    def _authorised(self):
        if self.app.verifier is None:
            return "local"
        try:
            return who(self.app.verifier.verify(self.headers.get(HEADER)))
        except Unauthorized as e:
            self._error(HTTPStatus.UNAUTHORIZED, str(e))
            return None

    def _same_origin(self, json_body=True):
        # Writes must be JSON (a cross-site form can't send that without a
        # CORS preflight, which this server never answers) and, when the
        # browser says where it came from, from this host. DELETE has no body
        # and always needs a preflight, so only the origin check applies.
        if json_body and not (self.headers.get("Content-Type") or "").startswith("application/json"):
            self._error(HTTPStatus.UNSUPPORTED_MEDIA_TYPE, "send application/json")
            return False
        origin = self.headers.get("Origin")
        if origin and urlparse(origin).netloc != self.headers.get("Host"):
            self._error(HTTPStatus.FORBIDDEN, "cross-origin request")
            return False
        return True

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n > 64_000:
            raise model.InvalidInput("body too large")
        data = json.loads(self.rfile.read(n) or b"{}")
        if not isinstance(data, dict):
            raise model.InvalidInput("body must be a JSON object")
        return data

    def _dispatch(self, fn, *args):
        try:
            self._send(HTTPStatus.OK, fn(self.app, *args))
        except (model.InvalidInput, json.JSONDecodeError) as e:
            self._error(HTTPStatus.BAD_REQUEST, str(e))
        except LookupError as e:
            self._error(HTTPStatus.NOT_FOUND, str(e).strip("'\""))
        except TimewError as e:
            # Overlaps have a standard fix (`adjust: true` trims the
            # neighbour); say so instead of a bare 500.
            if model.is_overlap(e.message):
                self._error(HTTPStatus.CONFLICT, e.message, overlap=True)
            else:
                # timew refusing a command is nearly always about the input
                # ("does not accept ranges", "end must be after start").
                self._error(HTTPStatus.UNPROCESSABLE_ENTITY, e.message)

    def do_GET(self):
        url = urlparse(self.path)
        if url.path == "/healthz":
            return self._send(HTTPStatus.OK, b"ok\n", "text/plain")
        if self._authorised() is None:
            return
        if url.path == "/":
            return self._send(HTTPStatus.OK, self.app.page, "text/html; charset=utf-8")
        fn = GET.get(url.path)
        if not fn:
            return self._error(HTTPStatus.NOT_FOUND, "not found")
        self._dispatch(fn, {k: v[0] for k, v in parse_qs(url.query).items()}, {})

    def do_POST(self):
        if self._authorised() is None or not self._same_origin():
            return
        fn = POST.get(urlparse(self.path).path)
        if not fn:
            return self._error(HTTPStatus.NOT_FOUND, "not found")
        try:
            body = self._body()
        except (model.InvalidInput, json.JSONDecodeError, ValueError) as e:
            return self._error(HTTPStatus.BAD_REQUEST, str(e))
        self._dispatch(fn, {}, body)

    def do_DELETE(self):
        if self._authorised() is None or not self._same_origin(json_body=False):
            return
        path = urlparse(self.path).path
        prefix = "/api/intervals/"
        if not path.startswith(prefix):
            return self._error(HTTPStatus.NOT_FOUND, "not found")
        self._dispatch(App.delete, unquote(path[len(prefix):]))

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))


def make_server(app, host, port):
    handler = type("BoundHandler", (Handler,), {"app": app})
    return ThreadingHTTPServer((host, port), handler)


def main(argv=None):
    p = argparse.ArgumentParser(description="HTTP API and web page for timewarrior")
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", type=int, default=int(os.environ.get("TIMEW_API_PORT", "8097")))
    p.add_argument("--db", default=os.environ.get("TIMEWARRIORDB") or os.path.expanduser("~/.local/share/timewarrior"))
    p.add_argument("--team-domain", default=os.environ.get("TIMEW_API_CF_TEAM_DOMAIN"),
                   help="Cloudflare Access team domain, e.g. myteam.cloudflareaccess.com")
    p.add_argument("--audience", default=os.environ.get("TIMEW_API_CF_AUD"), help="Access application AUD tag")
    p.add_argument("--project", default=os.environ.get("TIMEW_API_PROJECT", ""),
                   help="label shown in the weekly report header, e.g. a client and project name")
    p.add_argument("--no-auth", action="store_true", help="development only; refused unless bound to loopback")
    a = p.parse_args(argv)

    if a.no_auth:
        if a.host not in ("127.0.0.1", "::1", "localhost"):
            p.error("--no-auth is only allowed on a loopback address")
        verifier = None
    elif a.team_domain and a.audience:
        verifier = AccessVerifier(a.team_domain, a.audience)
    else:
        p.error("set --team-domain and --audience (or TIMEW_API_CF_*), or use --no-auth for local development")

    srv = make_server(App(Timew(a.db), verifier, a.project), a.host, a.port)
    print(f"timew-api on http://{a.host}:{a.port} (db {a.db}, auth {'Access' if verifier else 'OFF'})", file=sys.stderr)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
