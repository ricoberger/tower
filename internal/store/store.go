// Package store keeps all items in one SQLite database. The TUI and the
// `tower exec` wrappers of running agents write to it concurrently (WAL mode
// with a busy timeout).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // SQLite driver
)

// State is the board column of an item.
type State string

// Item states.
const (
	StateTodo       State = "todo"
	StateInProgress State = "in_progress"
	StateWaiting    State = "waiting"
	StateDone       State = "done"
)

// States lists the states in board order.
var States = []State{StateTodo, StateInProgress, StateWaiting, StateDone}

// ErrNotFound is returned for unknown items or when a conditional update did
// not apply.
var ErrNotFound = errors.New("item not found or not in the expected state")

// Item is one unit of work.
type Item struct {
	ID int64
	// Key identifies the item across all sources, e.g.
	// "alert:prod-de1:<fingerprint>". It is set by the source.
	Key string
	// Kind is the item kind of the provider that owns the item.
	Kind      string
	Source    string
	State     State
	CreatedAt time.Time
	UpdatedAt time.Time
	// ResolvedAt is set when the source stopped reporting the item.
	ResolvedAt *time.Time
	SessionID  string
	// PID is the wrapper's PID; zero means unclaimed or finished.
	PID int
	// Failed is set when the last run exited non-zero, its wrapper disappeared,
	// or an unclaimed start was recovered.
	Failed bool
	// Title is the one-line title shown on the board and used in messages,
	// notifications and commands.
	Title string
	// Description is the short text shown below the title on the board.
	Description string
	// Details is the item as Markdown for the agent prompt.
	Details string
	// URL is opened with the `o` key; it can be empty.
	URL string
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	key         TEXT    NOT NULL,
	kind        TEXT    NOT NULL,
	source      TEXT    NOT NULL,
	state       TEXT    NOT NULL,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	resolved_at INTEGER,
	session_id  TEXT    NOT NULL DEFAULT '',
	pid         INTEGER NOT NULL DEFAULT 0,
	failed      INTEGER NOT NULL DEFAULT 0,
	title       TEXT    NOT NULL DEFAULT '',
	description TEXT    NOT NULL DEFAULT '',
	details     TEXT    NOT NULL DEFAULT '',
	url         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS items_key ON items (key);
CREATE INDEX IF NOT EXISTS items_kind_source ON items (kind, source);
CREATE INDEX IF NOT EXISTS items_state ON items (state);
`

// Store is the item database.
type Store struct {
	db *sql.DB
}

// New opens (and creates) tower.db in stateDir.
func New(stateDir string) (*Store, error) {
	path, err := filepath.Abs(filepath.Join(stateDir, "tower.db"))
	if err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate",
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return &Store{
		db: db,
	}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// List returns all items.
func (s *Store) List(ctx context.Context) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, key, kind, source, state, created_at, updated_at, resolved_at, session_id, pid, failed, title, description, details, url FROM items ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { rows.Close() }()

	var items []Item
	for rows.Next() {
		it, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}

	return items, rows.Err()
}

// Get looks up an item by ID, returning ErrNotFound if it does not exist.
// It is the single-item lookup API, also used by integration tests.
func (s *Store) Get(ctx context.Context, id int64) (Item, error) {
	it, err := scan(s.db.QueryRowContext(ctx, "SELECT id, key, kind, source, state, created_at, updated_at, resolved_at, session_id, pid, failed, title, description, details, url FROM items WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}

	return it, err
}

// Create inserts a new item in TO DO and returns its ID.
func (s *Store) Create(ctx context.Context, it Item, now time.Time) (int64, error) {
	if it.Key == "" {
		return 0, errors.New("item has no key")
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO items (key, kind, source, state, created_at, updated_at, title, description, details, url) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, it.Key, it.Kind, it.Source, StateTodo, it.CreatedAt.Unix(), now.Unix(), it.Title, it.Description, it.Details, it.URL)
	if err != nil {
		return 0, err
	}

	return res.LastInsertId()
}

// Move changes the state of an item by user action. Moving to TO DO keeps
// resolved_at, so a resolved alert that the user wants to work on is not
// resolved again by the next poll.
func (s *Store) Move(ctx context.Context, id int64, to State, now time.Time) error {
	return s.exec(ctx, "UPDATE items SET state = ?, updated_at = ? WHERE id = ?", to, now.Unix(), id)
}

// Start moves a TO DO item to IN PROGRESS with a new session. The PID is
// recorded separately once the agent process exists.
func (s *Store) Start(ctx context.Context, id int64, sessionID string, now time.Time) error {
	return s.exec(ctx, `UPDATE items SET state = ?, session_id = ?, pid = 0, failed = 0, updated_at = ? WHERE id = ? AND state = ?`, StateInProgress, sessionID, now.Unix(), id, StateTodo)
}

// SetPID claims a pending IN PROGRESS session or confirms the same wrapper's
// existing claim. A recovered or finished session cannot be claimed again.
func (s *Store) SetPID(ctx context.Context, id int64, sessionID string, pid int) error {
	if pid <= 0 {
		return errors.New("wrapper PID must be positive")
	}
	return s.exec(ctx, `UPDATE items SET pid = ? WHERE id = ? AND session_id = ? AND ((state = ? AND pid = 0) OR pid = ?)`, pid, id, sessionID, StateInProgress, pid)
}

// Abort reverts a failed start back to TO DO.
func (s *Store) Abort(ctx context.Context, id int64, sessionID string, now time.Time) error {
	return s.exec(ctx, `UPDATE items SET state = ?, session_id = '', pid = 0, updated_at = ? WHERE id = ? AND session_id = ? AND state = ?`, StateTodo, now.Unix(), id, sessionID, StateInProgress)
}

// Finish atomically records the end of a session and whether it failed. An
// IN PROGRESS item moves to WAITING and Finish reports true; an item the user
// or a source moved meanwhile keeps its state. Already WAITING sessions are
// left alone.
func (s *Store) Finish(ctx context.Context, id int64, sessionID string, failed bool, now time.Time) (bool, error) {
	var state State
	err := s.db.QueryRowContext(ctx, `UPDATE items SET state = CASE WHEN state = ? THEN ? ELSE state END, updated_at = CASE WHEN state = ? THEN ? ELSE updated_at END, pid = 0, failed = ? WHERE id = ? AND session_id = ? AND state != ? RETURNING state`, StateInProgress, StateWaiting, StateInProgress, now.Unix(), failed, id, sessionID, StateWaiting).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return state == StateWaiting, err
}

// Recover marks an observed dead wrapper as failed and moves its item to
// WAITING only if the state, session and PID still match the observation.
func (s *Store) Recover(ctx context.Context, id int64, sessionID string, pid int, now time.Time) (bool, error) {
	err := s.exec(ctx, `UPDATE items SET state = ?, updated_at = ?, pid = 0, failed = 1 WHERE id = ? AND session_id = ? AND pid = ? AND state = ?`,
		StateWaiting, now.Unix(), id, sessionID, pid, StateInProgress)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// RecoverStarts marks unclaimed starts as failed (→ WAITING). Call it once
// with the TUI lock held, before accepting new starts. Wrappers must claim
// their PID before running the agent, so either their claim or recovery wins.
func (s *Store) RecoverStarts(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE items SET state = ?, updated_at = ?, failed = 1 WHERE state = ? AND pid = 0`, StateWaiting, now.Unix(), StateInProgress)
	return err
}

// DeleteDone removes DONE items not updated since before and returns their
// IDs.
func (s *Store) DeleteDone(ctx context.Context, before time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, "DELETE FROM items WHERE state = ? AND updated_at < ? RETURNING id", StateDone, before.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	return ids, rows.Err()
}

func (s *Store) exec(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}

	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}

	return nil
}
