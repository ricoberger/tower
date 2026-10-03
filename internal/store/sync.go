package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Update is the result of one poll of one source. Every source module sends
// its updates to the same channel.
type Update struct {
	// Kind and Source identify the snapshot: Sync only resolves items with
	// the same kind and source, and new items get this kind.
	Kind   string
	Source string
	// Items is the complete snapshot of the source.
	Items []Incoming
	// Err is set when the poll failed; Items is then nil and must not be
	// synced.
	Err error
	At  time.Time
	// ReopenWindow is passed to Sync.
	ReopenWindow time.Duration
}

// Incoming is an item as currently reported by a source.
type Incoming struct {
	// Key must be unique across all sources (see Item.Key).
	Key       string
	CreatedAt time.Time
	// Title, Description, Details and URL are copied to the item on every
	// sync (see Item).
	Title       string
	Description string
	Details     string
	URL         string
}

// Sync reconciles the items of one kind and source with the complete,
// successful snapshot in u (u.Err must be nil):
//
//   - an unknown key becomes a new TO DO item;
//   - an item that is not DONE gets fresh details and is no longer resolved;
//   - a DONE item that was resolved within u.ReopenWindow goes back to TO DO,
//     one resolved longer ago is left alone and a new item is created;
//   - a DONE item the user closed while it was still reported gets fresh
//     details but stays DONE, without resetting its retention timestamp;
//   - items missing from the snapshot are resolved: moved to DONE unless the
//     user moved them back to TO DO after they were resolved.
//
// Sync returns the IDs of newly created or reopened items.
func (s *Store) Sync(ctx context.Context, u Update) ([]int64, error) {
	if u.Err != nil {
		return nil, fmt.Errorf("sync %s/%s: failed poll: %w", u.Kind, u.Source, u.Err)
	}
	if u.Kind == "" || u.Source == "" {
		return nil, errors.New("sync: update without kind or source")
	}
	now := u.At
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	seen := map[string]bool{}
	var added []int64
	for _, in := range u.Items {
		if in.Key == "" {
			return nil, fmt.Errorf("sync %s/%s: item without key", u.Kind, u.Source)
		}
		if seen[in.Key] {
			continue
		}
		seen[in.Key] = true
		id, isNew, err := syncOne(ctx, tx, u, in)
		if err != nil {
			return nil, fmt.Errorf("sync %s: %w", in.Key, err)
		}
		if isNew {
			added = append(added, id)
		}
	}

	rows, err := tx.QueryContext(ctx, "SELECT id, key, state FROM items WHERE kind = ? AND source = ? AND resolved_at IS NULL", u.Kind, u.Source)
	if err != nil {
		return nil, err
	}
	type gone struct {
		id    int64
		state State
	}
	var missing []gone
	for rows.Next() {
		var (
			g   gone
			key string
		)
		if err := rows.Scan(&g.id, &key, &g.state); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !seen[key] {
			missing = append(missing, g)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range missing {
		q := "UPDATE items SET resolved_at = ?, state = ?, updated_at = ? WHERE id = ?"
		args := []any{now.Unix(), StateDone, now.Unix(), g.id}
		if g.state == StateDone {
			q, args = "UPDATE items SET resolved_at = ? WHERE id = ?", []any{now.Unix(), g.id}
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return nil, err
		}
	}
	return added, tx.Commit()
}

func syncOne(ctx context.Context, tx *sql.Tx, u Update, in Incoming) (int64, bool, error) {
	now := u.At
	var (
		id       int64
		state    State
		resolved sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, "SELECT id, state, resolved_at FROM items WHERE key = ? ORDER BY id DESC LIMIT 1",
		in.Key).Scan(&id, &state, &resolved)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return insert(ctx, tx, u, in)
	case err != nil:
		return 0, false, err
	}

	update := "UPDATE items SET title = ?, description = ?, details = ?, url = ?, resolved_at = NULL"
	args := []any{in.Title, in.Description, in.Details, in.URL}
	reopened := false
	if state == StateDone && resolved.Valid {
		if now.Sub(time.Unix(resolved.Int64, 0)) > u.ReopenWindow {
			return insert(ctx, tx, u, in)
		}
		update += ", state = ?, updated_at = ?"
		args = append(args, StateTodo, now.Unix())
		reopened = true
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, update+" WHERE id = ?", args...); err != nil {
		return 0, false, err
	}
	return id, reopened, nil
}

func insert(ctx context.Context, tx *sql.Tx, u Update, in Incoming) (int64, bool, error) {
	created := in.CreatedAt
	if created.IsZero() {
		created = u.At
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO items (key, kind, source, state, created_at, updated_at, title, description, details, url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Key, u.Kind, u.Source, StateTodo, created.Unix(), u.At.Unix(), in.Title, in.Description, in.Details, in.URL)
	if err != nil {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	return id, true, err
}
