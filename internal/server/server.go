// Package server is the HTTP layer: one small JSON API and the web page that
// uses it, both on the same port.
package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lurraca/timew-api/internal/auth"
	"github.com/lurraca/timew-api/internal/model"
	"github.com/lurraca/timew-api/internal/timew"
)

//go:embed index.html
var page []byte

const recentDays = 35

type Server struct {
	Timew    *timew.Runner
	Identity auth.Identity
	Project  string // label in the weekly report header
	Now      func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /{$}", s.authed(func(w http.ResponseWriter, _ *http.Request) error {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		_, err := w.Write(page)
		return err
	}))
	mux.Handle("GET /manifest.webmanifest", s.authed(s.manifest))
	mux.Handle("GET /icon.png", s.authed(s.icon))
	mux.Handle("GET /api/status", s.api(s.status))
	mux.Handle("GET /api/intervals", s.api(s.intervals))
	mux.Handle("GET /api/report", s.api(s.report))
	mux.Handle("POST /api/start", s.write(s.start))
	mux.Handle("POST /api/stop", s.write(s.simple("stop")))
	mux.Handle("POST /api/continue", s.write(s.simple("continue")))
	mux.Handle("POST /api/annotate", s.write(s.annotate))
	mux.Handle("POST /api/track", s.write(s.track))
	mux.Handle("DELETE /api/intervals/{start}", s.write(s.delete))
	return headers(mux)
}

// ── plumbing ────────────────────────────────────────────────────────────

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) authed(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := s.Identity.Who(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
			return
		}
		if err := h(w, r); err != nil {
			s.fail(w, err)
			return
		}
		if r.Method != http.MethodGet {
			log.Printf("%s %s by %s", r.Method, r.URL.Path, who)
		}
	})
}

// api wraps a handler that returns a value to send as JSON.
func (s *Server) api(fn func(r *http.Request) (any, error)) http.Handler {
	return s.authed(func(w http.ResponseWriter, r *http.Request) error {
		v, err := fn(r)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, v)
		return nil
	})
}

// write is api for requests that change something. They must be JSON (a
// cross-site form can't send that without a CORS preflight, which this
// server never answers) and, when the browser says where it came from, from
// this host. DELETE has no body and always needs a preflight, so only the
// origin check applies to it.
func (s *Server) write(fn func(r *http.Request) (any, error)) http.Handler {
	inner := s.api(fn)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "send application/json"})
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request"})
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		inner.ServeHTTP(w, r)
	})
}

type notFound struct{ msg string }

func (e notFound) Error() string { return e.msg }

func (s *Server) fail(w http.ResponseWriter, err error) {
	var inv *model.Invalid
	var nf notFound
	var te *timew.Error
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &inv), errors.As(err, &syn), errors.As(err, &typ):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	case errors.As(err, &nf):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.As(err, &te) && model.IsOverlap(te.Msg):
		// Overlaps have a standard fix: `adjust: true` trims the neighbour.
		writeJSON(w, http.StatusConflict, map[string]any{"error": te.Msg, "overlap": true})
	case errors.As(err, &te):
		// timew refusing a command is nearly always about the input
		// ("does not accept ranges", "end must be after start").
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": te.Msg})
	default:
		log.Printf("error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(v)
}

// ── reads ───────────────────────────────────────────────────────────────

type status struct {
	Now        string         `json:"now"`
	Running    *model.Public  `json:"running"`
	Totals     model.Totals   `json:"totals"`
	Today      []model.Public `json:"today"`
	RecentTags [][]string     `json:"recent_tags"`
}

func (s *Server) status(*http.Request) (any, error) {
	ivs, err := s.Timew.Export(s.since())
	if err != nil {
		return nil, err
	}
	return s.statusOf(ivs), nil
}

// since covers this month, this week and enough history for recent tags.
func (s *Server) since() time.Time {
	today := model.Day(s.now())
	first := today.AddDate(0, 0, 1-today.Day())
	week := model.WeekStart(today)
	back := today.AddDate(0, 0, -recentDays)
	for _, d := range []time.Time{first, week} {
		if d.Before(back) {
			back = d
		}
	}
	return back
}

func (s *Server) statusOf(ivs []model.Interval) status {
	now := s.now()
	today := model.Day(now)
	st := status{Now: model.FormatTimew(now), Totals: model.ComputeTotals(ivs, now), Today: []model.Public{}, RecentTags: [][]string{}}
	if run := model.Running(ivs); run != nil {
		p := model.ToPublic(*run)
		secs := model.Duration(*run, now)
		p.Seconds = &secs
		st.Running = &p
	}
	for _, iv := range ivs {
		if model.LocalDay(iv).Equal(today) {
			st.Today = append(st.Today, model.ToPublic(iv))
		}
	}
	sorted := append([]model.Interval(nil), ivs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start > sorted[j].Start })
	seen := map[string]bool{}
	for _, iv := range sorted {
		key := strings.Join(iv.Tags, "\x00")
		if len(iv.Tags) == 0 || seen[key] || len(st.RecentTags) == 5 {
			continue
		}
		seen[key] = true
		st.RecentTags = append(st.RecentTags, iv.Tags)
	}
	return st
}

func (s *Server) intervals(r *http.Request) (any, error) {
	since, err := model.ParseDay(r.URL.Query().Get("from"), s.now().AddDate(0, 0, -7))
	if err != nil {
		return nil, err
	}
	ivs, err := s.Timew.Export(since)
	if err != nil {
		return nil, err
	}
	out := make([]model.Public, 0, len(ivs))
	for _, iv := range ivs {
		out = append(out, model.ToPublic(iv))
	}
	return out, nil
}

func (s *Server) report(r *http.Request) (any, error) {
	d, err := model.ParseDay(r.URL.Query().Get("date"), s.now())
	if err != nil {
		return nil, err
	}
	monday := model.WeekStart(d)
	ivs, err := s.Timew.Export(monday)
	if err != nil {
		return nil, err
	}
	rep := model.WeeklyReport(ivs, monday, s.Project)
	return struct {
		model.Report
		Week string `json:"week"`
	}{rep, monday.Format("2006-01-02")}, nil
}

// ── writes ──────────────────────────────────────────────────────────────

// after re-exports once the write is done, inside the same lock, so the
// response shows the state that write produced.
func (s *Server) after(tx *timew.Tx) (any, error) {
	ivs, err := tx.Export(s.since())
	if err != nil {
		return nil, err
	}
	return s.statusOf(ivs), nil
}

// check reads back what timew stored. timew parses any argument that looks
// like a date or duration ("sunday", "second", "now") as one, even in a tag
// or note, and still exits 0. If what's stored isn't what was asked for, put
// everything back with `timew undo` and say why.
func check(tx *timew.Tx, since time.Time, what string, ok func([]model.Interval) bool) error {
	ivs, err := tx.Export(since.AddDate(0, 0, -1))
	if err != nil {
		return err
	}
	if ok(ivs) {
		return nil
	}
	if _, err := tx.Run("undo"); err != nil {
		return err
	}
	return &model.Invalid{Msg: "timew read " + what + " as a date or duration, so nothing was changed. Use more than one word, or a different word"}
}

func equalTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Server) simple(cmd string) func(*http.Request) (any, error) {
	return func(*http.Request) (out any, err error) {
		err = s.Timew.Do(func(tx *timew.Tx) error {
			if _, err := tx.Run(cmd); err != nil {
				return err
			}
			out, err = s.after(tx)
			return err
		})
		return
	}
}

func (s *Server) start(r *http.Request) (out any, err error) {
	var body struct {
		Tags []string `json:"tags"`
		At   string   `json:"at"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	tags, err := model.CleanTags(body.Tags)
	if err != nil {
		return nil, err
	}
	at := ""
	if body.At != "" {
		if at, err = model.ParseHM(body.At); err != nil {
			return nil, err
		}
	}
	err = s.Timew.Do(func(tx *timew.Tx) error {
		if _, err := tx.Run(model.StartArgs(tags, at)...); err != nil {
			return err
		}
		if err := check(tx, model.Day(s.now()), "a tag", func(ivs []model.Interval) bool {
			run := model.Running(ivs)
			return run != nil && equalTags(run.Tags, tags)
		}); err != nil {
			return err
		}
		out, err = s.after(tx)
		return err
	})
	return
}

func (s *Server) annotate(r *http.Request) (out any, err error) {
	var body struct{ Start, Text string }
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if _, err := model.ParseTimew(body.Start); err != nil {
		return nil, err
	}
	err = s.Timew.Do(func(tx *timew.Tx) error {
		if err := annotate(tx, body.Start, body.Text); err != nil {
			return err
		}
		out, err = s.after(tx)
		return err
	})
	return
}

func dayOf(start string) time.Time {
	t, _ := model.ParseTimew(start)
	return model.Day(t)
}

// annotate replaces the note on the entry that starts at `start`.
func annotate(tx *timew.Tx, start, text string) error {
	ivs, err := tx.Export(dayOf(start).AddDate(0, 0, -1))
	if err != nil {
		return err
	}
	id, ok := model.FindID(ivs, start)
	if !ok {
		return notFound{"no entry starts at " + start}
	}
	if _, err := tx.Run("annotate", "@"+itoa(id), text); err != nil {
		return err
	}
	return check(tx, dayOf(start), "the note", func(ivs []model.Interval) bool {
		for _, iv := range ivs {
			if iv.Start == start {
				return iv.Annotation == text
			}
		}
		return false
	})
}

func (s *Server) track(r *http.Request) (out any, err error) {
	var body struct {
		Date, From, To, Note string
		Tags                 []string
		Adjust               bool
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	day, err := model.ParseDay(body.Date, s.now())
	if err != nil {
		return nil, err
	}
	from, err := model.ParseHM(body.From)
	if err != nil {
		return nil, err
	}
	to, err := model.ParseHM(body.To)
	if err != nil {
		return nil, err
	}
	tags, err := model.CleanTags(body.Tags)
	if err != nil {
		return nil, err
	}
	args, err := model.TrackArgs(day, from, to, tags, body.Adjust)
	if err != nil {
		return nil, err
	}
	hh, mm := atoi(from[:2]), atoi(from[3:])
	start := model.FormatTimew(time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, model.Dublin))
	note := strings.TrimSpace(body.Note)

	err = s.Timew.Do(func(tx *timew.Tx) error {
		if _, err := tx.Run(args...); err != nil {
			return err
		}
		if err := check(tx, day, "a tag", func(ivs []model.Interval) bool {
			for _, iv := range ivs {
				if iv.Start == start && equalTags(iv.Tags, tags) {
					return true
				}
			}
			return false
		}); err != nil {
			return err
		}
		if note != "" {
			if err := annotate(tx, start, note); err != nil {
				return err
			}
		}
		out, err = s.after(tx)
		return err
	})
	return
}

func (s *Server) delete(r *http.Request) (out any, err error) {
	start := r.PathValue("start")
	if _, err := model.ParseTimew(start); err != nil {
		return nil, err
	}
	err = s.Timew.Do(func(tx *timew.Tx) error {
		ivs, err := tx.Export(dayOf(start).AddDate(0, 0, -1))
		if err != nil {
			return err
		}
		id, ok := model.FindID(ivs, start)
		if !ok {
			return notFound{"no entry starts at " + start}
		}
		if _, err := tx.Run("delete", "@"+itoa(id)); err != nil {
			return err
		}
		out, err = s.after(tx)
		return err
	})
	return
}

func itoa(i int) string { return strconv.Itoa(i) }

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
