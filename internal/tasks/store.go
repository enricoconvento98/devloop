// Package tasks is devloop's local task queue: a SQLite file shared by the
// TUI, the web dashboard and the CLI, plus the worker that runs the tasks.
package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Status string

const (
	Queued          Status = "queued"
	Running         Status = "running"
	CancelRequested Status = "cancel_requested"
	Succeeded       Status = "succeeded"
	Failed          Status = "failed"
	Canceled        Status = "canceled"
)

// Done reports whether the task has finished, one way or another.
func (s Status) Done() bool { return s == Succeeded || s == Failed || s == Canceled }

// Kinds are the devloop commands a task may run.
var Kinds = []string{"plan", "implement", "review", "triage"}

type Task struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	Args       []string   `json:"args"`
	Dir        string     `json:"dir"`
	Status     Status     `json:"status"`
	Summary    string     `json:"summary"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Title is the command line the task runs, for display.
func (t Task) Title() string { return Join(append([]string{t.Kind}, t.Args...)) }

// Duration is how long the task ran, or has been running.
func (t Task) Duration() time.Duration {
	if t.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	return end.Sub(*t.StartedAt).Round(time.Second)
}

// Elapsed is Duration for display: "" before the task starts, "<1s" for instant runs.
func (t Task) Elapsed() string {
	if t.StartedAt == nil {
		return ""
	}
	if d := t.Duration(); d > 0 {
		return d.String()
	}
	return "<1s"
}

type Store struct {
	db  *sql.DB
	dir string
}

// Home is devloop's data directory: $DEVLOOP_HOME, else ~/.devloop.
func Home() (string, error) {
	if h := os.Getenv("DEVLOOP_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".devloop"), nil
}

// Open opens (creating if needed) the task store in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		return nil, err
	}
	// WAL + busy timeout let the TUI, the web server and the CLI share the file.
	dsn := "file:" + filepath.Join(dir, "devloop.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		kind        TEXT NOT NULL,
		args        TEXT NOT NULL,
		dir         TEXT NOT NULL DEFAULT '',
		status      TEXT NOT NULL,
		summary     TEXT NOT NULL DEFAULT '',
		error       TEXT NOT NULL DEFAULT '',
		worker_pid  INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL,
		started_at  INTEGER,
		finished_at INTEGER
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dir: dir}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// LogPath is the combined stdout/stderr of a task.
func (s *Store) LogPath(id int64) string {
	return filepath.Join(s.dir, "logs", fmt.Sprintf("%d.log", id))
}

// OutputPath is a task's stdout alone: the command's result.
func (s *Store) OutputPath(id int64) string {
	return filepath.Join(s.dir, "logs", fmt.Sprintf("%d.out", id))
}

func validKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Add queues a task that runs in dir (relative paths in args resolve against it).
func (s *Store) Add(ctx context.Context, kind string, args []string, dir string) (int64, error) {
	if !validKind(kind) {
		return 0, fmt.Errorf("unknown task kind %q (want one of %v)", kind, Kinds)
	}
	argsJSON, _ := json.Marshal(args)
	res, err := s.db.ExecContext(ctx, `INSERT INTO tasks (kind, args, dir, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		kind, string(argsJSON), dir, Queued, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const columns = `id, kind, args, dir, status, summary, error, created_at, started_at, finished_at`

func scan(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var args string
	var created int64
	var started, finished sql.NullInt64
	if err := row.Scan(&t.ID, &t.Kind, &args, &t.Dir, &t.Status, &t.Summary, &t.Error, &created, &started, &finished); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(args), &t.Args)
	t.CreatedAt = time.UnixMilli(created)
	if started.Valid {
		v := time.UnixMilli(started.Int64)
		t.StartedAt = &v
	}
	if finished.Valid {
		v := time.UnixMilli(finished.Int64)
		t.FinishedAt = &v
	}
	return &t, nil
}

var ErrNotFound = errors.New("task not found")

func (s *Store) Get(ctx context.Context, id int64) (*Task, error) {
	t, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// List returns the most recent tasks first.
func (s *Store) List(ctx context.Context, limit int) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Claim atomically marks the oldest queued task as running for this worker.
// It returns nil when the queue is empty.
func (s *Store) Claim(ctx context.Context) (*Task, error) {
	for {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE status = ? ORDER BY id LIMIT 1`, Queued).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, worker_pid = ?, started_at = ?, error = '', summary = ''
			WHERE id = ? AND status = ?`, Running, os.Getpid(), time.Now().UnixMilli(), id, Queued)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return s.Get(ctx, id)
		}
		// Another worker claimed it first; try the next one.
	}
}

// Finish records a task's outcome.
func (s *Store) Finish(ctx context.Context, id int64, status Status, summary, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, summary = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, summary, errMsg, time.Now().UnixMilli(), id)
	return err
}

// Cancel dequeues a queued task, or asks the worker running it to stop.
func (s *Store) Cancel(ctx context.Context, id int64) error {
	t, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	switch t.Status {
	case Queued:
		return s.Finish(ctx, id, Canceled, "", "canceled before it started")
	case Running:
		_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ? AND status = ?`, CancelRequested, id, Running)
		return err
	default:
		return fmt.Errorf("task %d is already %s", id, t.Status)
	}
}

// Retry queues a new task with the same command as a finished one.
func (s *Store) Retry(ctx context.Context, id int64) (int64, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if !t.Status.Done() {
		return 0, fmt.Errorf("task %d is still %s", id, t.Status)
	}
	return s.Add(ctx, t.Kind, t.Args, t.Dir)
}

// IsCancelRequested reports whether someone asked to cancel a running task.
func (s *Store) IsCancelRequested(ctx context.Context, id int64) bool {
	var st Status
	err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&st)
	return err == nil && st == CancelRequested
}

// RecoverOrphans fails tasks left running by a worker process that no longer exists.
func (s *Store) RecoverOrphans(ctx context.Context, alive func(pid int) bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, worker_pid FROM tasks WHERE status IN (?, ?)`, Running, CancelRequested)
	if err != nil {
		return err
	}
	var orphans []int64
	for rows.Next() {
		var id int64
		var pid int
		if err := rows.Scan(&id, &pid); err == nil && !alive(pid) {
			orphans = append(orphans, id)
		}
	}
	rows.Close()
	for _, id := range orphans {
		if err := s.Finish(ctx, id, Failed, "", "worker stopped while the task was running"); err != nil {
			return err
		}
	}
	return nil
}
