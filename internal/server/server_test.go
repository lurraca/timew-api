package server

// End to end: real HTTP requests, real `timew`, a throwaway database.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lurraca/timew-api/internal/auth"
	"github.com/lurraca/timew-api/internal/model"
	"github.com/lurraca/timew-api/internal/timew"
)

type env struct {
	t   *testing.T
	url string
}

func setup(t *testing.T, id auth.Identity) *env {
	t.Helper()
	if _, err := exec.LookPath("timew"); err != nil {
		t.Skip("timew not installed")
	}
	runner, err := timew.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if id == nil {
		id = auth.Dev{}
	}
	srv := httptest.NewServer((&Server{Timew: runner, Identity: id, Project: "Client – Project"}).Handler())
	t.Cleanup(srv.Close)
	return &env{t, srv.URL}
}

type opt func(*http.Request)

func ctype(v string) opt  { return func(r *http.Request) { r.Header.Set("Content-Type", v) } }
func origin(v string) opt { return func(r *http.Request) { r.Header.Set("Origin", v) } }
func token(v string) opt  { return func(r *http.Request) { r.Header.Set(auth.Header, v) } }

func (e *env) do(method, path string, body any, opts ...opt) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (e *env) json(method, path string, body any, opts ...opt) (int, map[string]any) {
	e.t.Helper()
	code, b := e.do(method, path, body, opts...)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s %s: %d %s", method, path, code, b)
	}
	return code, out
}

func (e *env) list(path string) []map[string]any {
	e.t.Helper()
	_, b := e.do("GET", path, nil)
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s: %s", path, b)
	}
	return out
}

func yesterday() string {
	return model.Day(time.Now()).AddDate(0, 0, -1).Format("2006-01-02")
}

func running(st map[string]any) map[string]any {
	r, _ := st["running"].(map[string]any)
	return r
}

func TestPageAndHealth(t *testing.T) {
	e := setup(t, nil)
	if code, _ := e.do("GET", "/healthz", nil); code != 200 {
		t.Fatal(code)
	}
	code, page := e.do("GET", "/", nil)
	if code != 200 || !bytes.Contains(page, []byte("<title>timew</title>")) {
		t.Fatal(code)
	}
	if code, _ := e.do("GET", "/nope", nil); code != 404 {
		t.Fatal(code)
	}
}

func TestStartNoteStopResume(t *testing.T) {
	e := setup(t, nil)
	code, st := e.json("POST", "/api/start", map[string]any{"tags": []string{"client.project", "two words"}})
	if code != 200 || running(st) == nil {
		t.Fatal(code, st)
	}
	start := running(st)["start"].(string)

	note := `Training 22/56, "quotes" & a ; semicolon`
	if code, st = e.json("POST", "/api/annotate", map[string]any{"start": start, "text": note}); code != 200 || running(st)["annotation"] != note {
		t.Fatal(code, st)
	}
	time.Sleep(1100 * time.Millisecond) // timew refuses a zero-length interval
	if code, st = e.json("POST", "/api/stop", map[string]any{}); code != 200 || running(st) != nil {
		t.Fatal(code, st)
	}
	if code, st = e.json("POST", "/api/continue", map[string]any{}); code != 200 || running(st)["annotation"] != note {
		t.Fatal("continue should copy the note:", code, st)
	}
}

func TestTrackOverlapAndAdjust(t *testing.T) {
	e := setup(t, nil)
	day := yesterday()
	block := map[string]any{"date": day, "from": "11:00", "to": "13:00", "tags": []string{"t"}, "note": "first"}
	if code, st := e.json("POST", "/api/track", block); code != 200 {
		t.Fatal(code, st)
	}
	block["from"], block["to"], block["note"] = "12:00", "14:00", "second block"
	code, err := e.json("POST", "/api/track", block)
	if code != 409 || err["overlap"] != true {
		t.Fatal(code, err)
	}
	block["adjust"] = true
	if code, st := e.json("POST", "/api/track", block); code != 200 {
		t.Fatal(code, st)
	}
	ivs := e.list("/api/intervals?from=" + day)
	if len(ivs) != 2 || ivs[0]["annotation"] != "first" || ivs[1]["annotation"] != "second block" || ivs[0]["end"] != ivs[1]["start"] {
		t.Fatal("the first should be trimmed to meet the second:", ivs)
	}
}

func TestDeleteByStart(t *testing.T) {
	e := setup(t, nil)
	day := yesterday()
	e.json("POST", "/api/track", map[string]any{"date": day, "from": "09:00", "to": "10:00", "tags": []string{"t"}})
	start := e.list("/api/intervals?from=" + day)[0]["start"].(string)
	if code, _ := e.json("DELETE", "/api/intervals/"+start, nil); code != 200 {
		t.Fatal(code)
	}
	if ivs := e.list("/api/intervals?from=" + day); len(ivs) != 0 {
		t.Fatal(ivs)
	}
	if code, _ := e.json("DELETE", "/api/intervals/"+start, nil); code != 404 {
		t.Fatal(code)
	}
}

func TestReport(t *testing.T) {
	e := setup(t, nil)
	monday := model.WeekStart(time.Now()).AddDate(0, 0, -7).Format("2006-01-02")
	if code, st := e.json("POST", "/api/track", map[string]any{"date": monday, "from": "09:00", "to": "17:10", "tags": []string{"t"}, "note": "work"}); code != 200 {
		t.Fatal(code, st)
	}
	code, r := e.json("GET", "/api/report?date="+monday, nil)
	if code != 200 || !strings.Contains(r["text"].(string), "09:00–17:10 (8:10): work.") || r["billed_hours"] != 8.0 || r["week"] != monday {
		t.Fatal(code, r)
	}
}

// In summer, Irish time is UTC+1: 09:00 must be stored as 08:00Z and read
// back as 09:00, whatever zone the test process itself runs in.
func TestSummerTimeRoundTrip(t *testing.T) {
	t.Setenv("TZ", "UTC")
	e := setup(t, nil)
	code, st := e.json("POST", "/api/track", map[string]any{"date": "2026-06-01", "from": "09:00", "to": "17:00", "tags": []string{"t"}, "note": "summer"})
	if code != 200 {
		t.Fatal(code, st)
	}
	if ivs := e.list("/api/intervals?from=2026-06-01"); len(ivs) != 1 || ivs[0]["start"] != "20260601T080000Z" {
		t.Fatal(ivs)
	}
	_, r := e.json("GET", "/api/report?date=2026-06-01", nil)
	if !strings.Contains(r["text"].(string), "09:00–17:00 (8:00): summer.") || !strings.Contains(r["text"].(string), "IST, UTC+1") {
		t.Fatal(r["text"])
	}
}

// timew reads words like "second" or "sunday" as dates, even in a note or tag.
func TestWordsTimewReadsAsDatesAreUndone(t *testing.T) {
	e := setup(t, nil)
	day := yesterday()
	e.json("POST", "/api/track", map[string]any{"date": day, "from": "09:00", "to": "10:00", "tags": []string{"t"}, "note": "ok note"})
	start := e.list("/api/intervals?from=" + day)[0]["start"].(string)

	code, err := e.json("POST", "/api/annotate", map[string]any{"start": start, "text": "second"})
	if code != 400 || !strings.Contains(err["error"].(string), "as a date") {
		t.Fatal(code, err)
	}
	if got := e.list("/api/intervals?from=" + day)[0]["annotation"]; got != "ok note" {
		t.Fatal("the old note should be back:", got)
	}

	// timew refuses this one itself; either way nothing may change.
	if code, _ := e.json("POST", "/api/start", map[string]any{"tags": []string{"sunday"}}); code != 400 && code != 422 {
		t.Fatal(code)
	}
	if _, st := e.json("GET", "/api/status", nil); running(st) != nil {
		t.Fatal(st)
	}
	if ivs := e.list("/api/intervals?from=" + day); len(ivs) != 1 {
		t.Fatal(ivs)
	}
}

func TestBadInputIs400AndChangesNothing(t *testing.T) {
	e := setup(t, nil)
	for _, body := range []any{
		map[string]any{"tags": []string{":yes"}},
		map[string]any{"tags": "a"},
		map[string]any{"tags": []string{"t"}, "at": "25:00"},
	} {
		if code, _ := e.json("POST", "/api/start", body); code != 400 {
			t.Errorf("%v: %d", body, code)
		}
	}
	if code, _ := e.json("POST", "/api/track", map[string]any{"date": "2026-02-30", "from": "1", "to": "2"}); code != 400 {
		t.Error(code)
	}
	if _, st := e.json("GET", "/api/status", nil); running(st) != nil {
		t.Fatal(st)
	}
}

func TestWritesNeedJSONAndSameOrigin(t *testing.T) {
	e := setup(t, nil)
	if code, _ := e.do("POST", "/api/stop", map[string]any{}, ctype("text/plain")); code != 415 {
		t.Fatal(code)
	}
	if code, _ := e.do("POST", "/api/stop", map[string]any{}, origin("https://evil.example")); code != 403 {
		t.Fatal(code)
	}
	if code, _ := e.do("DELETE", "/api/intervals/20261026T110000Z", nil, origin("https://evil.example")); code != 403 {
		t.Fatal(code)
	}
}

// fakeIdentity lets a request through only with the header "good".
type fakeIdentity struct{}

func (fakeIdentity) Who(r *http.Request) (string, error) {
	if r.Header.Get(auth.Header) == "good" {
		return "me@example.com", nil
	}
	return "", errors.New("no")
}

func TestNoIdentityNoData(t *testing.T) {
	e := setup(t, fakeIdentity{})
	for _, path := range []string{"/", "/api/status", "/api/report"} {
		if code, _ := e.do("GET", path, nil); code != 401 {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _ := e.do("POST", "/api/stop", map[string]any{}); code != 401 {
		t.Error(code)
	}
	if code, _ := e.do("GET", "/healthz", nil); code != 200 {
		t.Error("liveness should stay open:", code)
	}
	if code, _ := e.do("GET", "/api/status", nil, token("good")); code != 200 {
		t.Error(code)
	}
}
