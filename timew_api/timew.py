"""The only place that runs `timew`. Everything goes through the CLI so timew
keeps doing its own validation, and `timew undo` keeps working."""

import json
import os
import subprocess
import threading


class TimewError(Exception):
    def __init__(self, message):
        super().__init__(message)
        self.message = message


class Timew:
    def __init__(self, db, binary="timew", timeout=15):
        self.db = db
        self.binary = binary
        self.timeout = timeout
        # One command at a time: two clients writing at once could race on
        # the data files, and ids looked up by one call must still be valid
        # when the next call uses them.
        self.lock = threading.RLock()
        self._ensure_db()

    def _ensure_db(self):
        # timew prompts "Create new config?" on a missing database, which
        # would hang a service. An existing data/ dir skips the prompt.
        os.makedirs(os.path.join(self.db, "data"), exist_ok=True)
        cfg = os.path.join(self.db, "timewarrior.cfg")
        if not os.path.exists(cfg):
            with open(cfg, "w") as f:
                f.write("# Written by timew-api.\nconfirmation = off\nverbose = off\n")

    def run(self, args):
        env = dict(os.environ, TIMEWARRIORDB=self.db)
        with self.lock:
            try:
                # No shell, and stdin closed: `confirmation = off` in the
                # config answers timew's prompts, so nothing can block.
                p = subprocess.run([self.binary, *args], capture_output=True, text=True, env=env,
                                   timeout=self.timeout, stdin=subprocess.DEVNULL)
            except subprocess.TimeoutExpired:
                raise TimewError(f"timew {args[0]} timed out") from None
        if p.returncode != 0:
            raise TimewError((p.stderr or p.stdout).strip() or f"timew {args[0]} exited {p.returncode}")
        return p.stdout

    def export(self, since=None):
        out = self.run(["export"] + (["from", since.isoformat()] if since else []))
        return json.loads(out or "[]")
