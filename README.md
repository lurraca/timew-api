# timew-api

A small HTTP API and web page for [timewarrior](https://timewarrior.net). It runs
`timew` and nothing else: every action is a normal timew command, so timew's own
checks still apply and `timew undo` still works.

Go, standard library only, one static binary. The page is embedded in it.

```
 browser (any machine) ─┐   HTTPS          ┌────────────┐  argv   ┌──────┐
 scripts, a tray app ───┼─▶ Cloudflare ───▶│ timew-api  │───────▶│ timew │
                        │   Access         │ 127.0.0.1  │        └──────┘
                        └─ (login / token) └────────────┘
```

## API

All JSON. Entries are identified by their **start timestamp** (`20261026T110000Z`),
never by timew's `@1` ids, which renumber whenever anything changes.

| Method | Path | Body / query | |
|---|---|---|---|
| GET | `/api/status` | | running entry, today's entries, today/week/month totals, recent tags |
| GET | `/api/intervals` | `?from=YYYY-MM-DD` | entries since a date (default: a week back) |
| GET | `/api/report` | `?date=YYYY-MM-DD` | plain-text weekly report for that date's week |
| POST | `/api/start` | `{"tags": [...], "at": "HH:MM"?}` | |
| POST | `/api/stop` | `{}` | |
| POST | `/api/continue` | `{}` | resume the last entry, note included |
| POST | `/api/annotate` | `{"start": "…Z", "text": "…"}` | replaces the note |
| POST | `/api/track` | `{"date", "from", "to", "tags", "note"?, "adjust"?}` | a past block |
| DELETE | `/api/intervals/{start}` | | |
| GET | `/healthz` | | liveness, no auth |

Errors are `{"error": "…"}`: 400 bad input, 401 no valid Access token, 404 no
such entry, **409 overlap** (`"overlap": true`; retry with `"adjust": true` to trim
the neighbour), 422 timew refused the command.

**Words timew reads as dates.** timew parses any argument that looks like a date
or duration (`sunday`, `second`, `now`) as one, even inside a tag or note, and
still exits 0. After every write the server reads back what timew stored; if it
differs from what was asked, it runs `timew undo` and returns 400. Multi-word
notes are never affected.

## Auth

Cloudflare Access sits in front: a one-time email code for browsers, a service
token (`CF-Access-Client-Id` / `CF-Access-Client-Secret` headers) for scripts.
The server also verifies Access's signed JWT (`Cf-Access-Jwt-Assertion`) against
the team's public keys, checking signature, audience, issuer and expiry, so
nothing reaches timew without it.

Writes must be `application/json` and same-origin, so another site can't post a
form at the page.

## Run

```bash
go build ./cmd/timew-api

# development, no auth (refused on anything but loopback)
./timew-api --no-auth --port 8097 --db /tmp/timew-test

# real
TIMEW_API_CF_TEAM_DOMAIN=myteam.cloudflareaccess.com \
TIMEW_API_CF_AUD=<aud tag> \
TIMEW_API_PROJECT="Client – Project" \
./timew-api --port 8097 --db ~/.local/share/timewarrior
```

| Path | |
|---|---|
| `cmd/timew-api` | flags and startup |
| `internal/model` | pure logic: parsing, totals, the weekly report |
| `internal/timew` | runs `timew`, one command at a time |
| `internal/auth` | Cloudflare Access token check (from splice) |
| `internal/server` | HTTP handlers and the embedded page |

On NixOS, import `nixosModules.default` and set `services.timew-api.{enable,dataDir,environmentFile}`.

## Tests

```bash
go test ./...
```

The server tests run real `timew` against a throwaway database (skipped if
timew isn't installed). `nix build` runs them too.
