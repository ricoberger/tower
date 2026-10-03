package store

import (
	"database/sql"
	"time"
)

type scanner interface{ Scan(...any) error }

func scan(r scanner) (Item, error) {
	var it Item
	var created int64
	var updated int64
	var resolved sql.NullInt64

	err := r.Scan(&it.ID, &it.Key, &it.Kind, &it.Source, &it.State, &created, &updated, &resolved, &it.SessionID, &it.PID, &it.Failed, &it.Title, &it.Description, &it.Details, &it.URL)
	if err != nil {
		return Item{}, err
	}

	it.CreatedAt = time.Unix(created, 0)
	it.UpdatedAt = time.Unix(updated, 0)
	it.ResolvedAt = timePtr(resolved)

	return it, nil
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}
