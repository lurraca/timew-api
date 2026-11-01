// Package timew is the only place that runs `timew`. Everything goes through
// the CLI so timew keeps doing its own validation and `timew undo` works.
package timew

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lurraca/timew-api/internal/model"
)

// Error is timew refusing a command, with its own message.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

type Runner struct {
	DB      string
	Bin     string
	Timeout time.Duration

	// One command at a time: two clients writing at once could race on the
	// data files, and an id looked up by one call must still be valid when
	// the next call uses it.
	mu sync.Mutex
}

func New(db string) (*Runner, error) {
	r := &Runner{DB: db, Bin: "timew", Timeout: 15 * time.Second}
	// timew prompts "Create new config?" on a missing database, which would
	// hang a service. An existing data/ dir skips the prompt.
	if err := os.MkdirAll(filepath.Join(db, "data"), 0o750); err != nil {
		return nil, err
	}
	cfg := filepath.Join(db, "timewarrior.cfg")
	if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfg, []byte("# Written by timew-api.\nconfirmation = off\nverbose = off\n"), 0o640); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Tx runs several commands with nothing else in between.
type Tx struct{ r *Runner }

// Do holds the lock for the whole of fn.
func (r *Runner) Do(fn func(tx *Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn(&Tx{r})
}

func (tx *Tx) Run(args ...string) (string, error) {
	r := tx.r
	ctx, cancel := context.WithTimeout(context.Background(), r.Timeout)
	defer cancel()
	// No shell, and stdin closed: `confirmation = off` answers timew's
	// prompts, so nothing can block.
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	// timew turns bare times like 09:00 into dates in its local zone, and
	// everything here reads them back in Irish time, so the two must agree
	// regardless of how the process was started.
	cmd.Env = append(os.Environ(), "TIMEWARRIORDB="+r.DB, "TZ="+model.Dublin.String())
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", &Error{fmt.Sprintf("timew %s timed out", args[0])}
		}
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = fmt.Sprintf("timew %s failed: %v", args[0], err)
		}
		return "", &Error{msg}
	}
	return out.String(), nil
}

// Export lists entries from a local date on, or all of them for a zero time.
func (tx *Tx) Export(since time.Time) ([]model.Interval, error) {
	args := []string{"export"}
	if !since.IsZero() {
		args = append(args, "from", since.Format("2006-01-02"))
	}
	out, err := tx.Run(args...)
	if err != nil {
		return nil, err
	}
	var ivs []model.Interval
	if strings.TrimSpace(out) == "" {
		return ivs, nil
	}
	if err := json.Unmarshal([]byte(out), &ivs); err != nil {
		return nil, fmt.Errorf("parse timew export: %w", err)
	}
	return ivs, nil
}

func (r *Runner) Export(since time.Time) (ivs []model.Interval, err error) {
	err = r.Do(func(tx *Tx) error { ivs, err = tx.Export(since); return err })
	return
}
