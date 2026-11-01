"""End to end: real HTTP requests, real `timew`, a throwaway database."""

import json
import time
import shutil
import tempfile
import threading
import unittest
import urllib.request
from datetime import datetime, timedelta, timezone

from timew_api import model
from timew_api.auth import HEADER, AccessVerifier
from timew_api.server import App, make_server
from timew_api.timew import Timew

from .test_auth import AUD, TEAM, Keys


class Client:
    def __init__(self, base, headers=None):
        self.base, self.headers = base, headers or {}

    def call(self, method, path, body=None, ctype="application/json", origin=None):
        data = json.dumps(body).encode() if body is not None else None
        h = dict(self.headers)
        if data is not None:
            h["Content-Type"] = ctype
        if origin:
            h["Origin"] = origin
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=h)
        try:
            with urllib.request.urlopen(req) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def json(self, method, path, body=None, **kw):
        status, data = self.call(method, path, body, **kw)
        return status, json.loads(data)


@unittest.skipUnless(shutil.which("timew"), "timew not installed")
class Server(unittest.TestCase):
    verifier = None

    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir)
        self.app = App(Timew(self.dir), self.verifier)
        self.srv = make_server(self.app, "127.0.0.1", 0)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)
        self.base = f"http://127.0.0.1:{self.srv.server_address[1]}"
        self.c = Client(self.base)

    def today(self):
        return datetime.now(timezone.utc).astimezone(model.TZ).date()


class Flows(Server):
    def test_page_and_health(self):
        self.assertEqual(self.c.call("GET", "/healthz")[0], 200)
        status, page = self.c.call("GET", "/")
        self.assertEqual(status, 200)
        self.assertIn(b"<title>timew</title>", page)

    def test_start_note_stop_resume(self):
        s, st = self.c.json("POST", "/api/start", {"tags": ["client.project", "two words"]})
        self.assertEqual(s, 200)
        self.assertEqual(st["running"]["tags"], ["client.project", "two words"])

        note = 'Training 22/56, "quotes" & a ; semicolon'
        s, st = self.c.json("POST", "/api/annotate", {"start": st["running"]["start"], "text": note})
        self.assertEqual((s, st["running"]["annotation"]), (200, note))

        time.sleep(1.1)  # timew refuses a zero-length interval
        s, st = self.c.json("POST", "/api/stop", {})
        self.assertIsNone(st["running"])
        s, st = self.c.json("POST", "/api/continue", {})
        self.assertEqual(st["running"]["annotation"], note)  # continue copies the note

    def test_start_at_earlier_time(self):
        start = (datetime.now(model.TZ) - timedelta(minutes=30)).strftime("%H:%M")
        if start > datetime.now(model.TZ).strftime("%H:%M"):
            self.skipTest("too close to midnight")
        s, st = self.c.json("POST", "/api/start", {"tags": ["t"], "at": start})
        self.assertEqual(s, 200)
        self.assertGreaterEqual(st["running"]["seconds"], 29 * 60)

    def test_track_with_note_overlap_and_adjust(self):
        day = (self.today() - timedelta(days=1)).isoformat()
        block = {"date": day, "from": "11:00", "to": "13:00", "tags": ["t"], "note": "first"}
        self.assertEqual(self.c.json("POST", "/api/track", block)[0], 200)

        s, err = self.c.json("POST", "/api/track", dict(block, **{"from": "12:00", "to": "14:00", "note": "second"}))
        self.assertEqual(s, 409)
        self.assertTrue(err["overlap"])


        s, _ = self.c.json("POST", "/api/track", dict(block, **{"from": "12:00", "to": "14:00", "note": "second block", "adjust": True}))
        self.assertEqual(s, 200)
        ivs = self.c.json("GET", f"/api/intervals?from={day}")[1]
        got = [(i["annotation"], i["start"], i["end"]) for i in ivs]
        self.assertEqual([g[0] for g in got], ["first", "second block"])
        self.assertEqual(got[0][2], got[1][1])  # the first was trimmed to meet the second

    def test_delete_by_start(self):
        day = (self.today() - timedelta(days=1)).isoformat()
        self.c.json("POST", "/api/track", {"date": day, "from": "09:00", "to": "10:00", "tags": ["t"]})
        start = self.c.json("GET", f"/api/intervals?from={day}")[1][0]["start"]
        self.assertEqual(self.c.json("DELETE", f"/api/intervals/{start}")[0], 200)
        self.assertEqual(self.c.json("GET", f"/api/intervals?from={day}")[1], [])
        self.assertEqual(self.c.json("DELETE", f"/api/intervals/{start}")[0], 404)

    def test_report(self):
        monday = model.week_start(self.today()) - timedelta(days=7)
        self.c.json("POST", "/api/track", {"date": monday.isoformat(), "from": "09:00", "to": "17:10", "tags": ["t"], "note": "work"})
        s, r = self.c.json("GET", f"/api/report?date={monday.isoformat()}")
        self.assertEqual(s, 200)
        self.assertIn("09:00–17:10 (8:10): work.", r["text"])
        self.assertEqual((r["tracked"], r["billed_hours"]), (8 * 3600 + 600, 8))

    def test_words_timew_reads_as_dates_are_undone(self):
        day = (self.today() - timedelta(days=1)).isoformat()
        block = {"date": day, "from": "09:00", "to": "10:00", "tags": ["t"]}
        self.assertEqual(self.c.json("POST", "/api/track", dict(block, note="ok note"))[0], 200)
        start = self.c.json("GET", f"/api/intervals?from={day}")[1][0]["start"]

        s, err = self.c.json("POST", "/api/annotate", {"start": start, "text": "second"})
        self.assertEqual(s, 400)
        self.assertIn("as a date", err["error"])
        self.assertEqual(self.c.json("GET", f"/api/intervals?from={day}")[1][0]["annotation"], "ok note")

        # timew refuses this one itself; either way nothing may change.
        s, _ = self.c.json("POST", "/api/start", {"tags": ["sunday"]})
        self.assertIn(s, (400, 422))
        self.assertIsNone(self.c.json("GET", "/api/status")[1]["running"])
        self.assertEqual(len(self.c.json("GET", f"/api/intervals?from={day}")[1]), 1)

    def test_bad_input_is_400_and_changes_nothing(self):
        for body in ({"tags": [":yes"]}, {"tags": "a"}, {"tags": ["t"], "at": "25:00"}):
            self.assertEqual(self.c.json("POST", "/api/start", body)[0], 400)
        self.assertEqual(self.c.json("POST", "/api/track", {"date": "2026-02-30", "from": "1", "to": "2", "tags": []})[0], 400)
        self.assertIsNone(self.c.json("GET", "/api/status")[1]["running"])

    def test_writes_need_json_and_same_origin(self):
        self.assertEqual(self.c.call("POST", "/api/stop", {}, ctype="text/plain")[0], 415)
        self.assertEqual(self.c.call("POST", "/api/stop", {}, origin="https://evil.example")[0], 403)


class WithAccess(Server):
    def setUp(self):
        self.keys = Keys()
        self.verifier = AccessVerifier(TEAM, AUD, fetch=lambda _url: {"keys": [self.keys.jwk()]})
        super().setUp()

    def test_no_token_no_data(self):
        for path in ("/", "/api/status", "/api/report"):
            self.assertEqual(self.c.call("GET", path)[0], 401, path)
        self.assertEqual(self.c.call("POST", "/api/stop", {})[0], 401)
        self.assertEqual(self.c.call("GET", "/healthz")[0], 200)  # liveness stays open

    def test_valid_token(self):
        c = Client(self.base, {HEADER: self.keys.token()})
        self.assertEqual(c.call("GET", "/api/status")[0], 200)
        bad = Client(self.base, {HEADER: self.keys.token(aud=["other"])})
        self.assertEqual(bad.call("GET", "/api/status")[0], 401)


if __name__ == "__main__":
    unittest.main()
