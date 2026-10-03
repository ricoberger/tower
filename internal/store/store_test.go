package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func alert(fp string) Incoming {
	return Incoming{Key: "alert:dev:" + fp, CreatedAt: t0, Title: "title " + fp, Description: "description " + fp, Details: "# " + fp, URL: "https://example.com/" + fp}
}

func items(t *testing.T, s *Store) map[int64]Item {
	t.Helper()
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]Item{}
	for _, it := range list {
		m[it.ID] = it
	}
	return m
}

func sync(t *testing.T, s *Store, now time.Time, fps ...string) []int64 {
	t.Helper()
	var in []Incoming
	for _, fp := range fps {
		in = append(in, alert(fp))
	}
	added, err := s.Sync(context.Background(), Update{Kind: "alert", Source: "src", Items: in, At: now, ReopenWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return added
}

func TestSyncLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	added := sync(t, s, t0, "a", "b")
	if len(added) != 2 {
		t.Fatalf("added = %v", added)
	}
	a, b := added[0], added[1]
	if got := items(t, s)[a]; got.State != StateTodo || got.Title != "title a" || got.Description != "description a" || got.Details != "# a" || got.URL != "https://example.com/a" || !got.CreatedAt.Equal(t0) {
		t.Fatalf("a = %+v", got)
	}

	// Still firing: no new items.
	if added := sync(t, s, t0.Add(time.Minute), "a", "b"); len(added) != 0 {
		t.Fatalf("added = %v", added)
	}

	// b resolves → DONE with resolved_at.
	sync(t, s, t0.Add(2*time.Minute), "a")
	got := items(t, s)[b]
	if got.State != StateDone || got.ResolvedAt == nil {
		t.Fatalf("b = %+v", got)
	}

	// b fires again within the reopen window → same item back in TO DO.
	if added := sync(t, s, t0.Add(30*time.Minute), "a", "b"); len(added) != 1 || added[0] != b {
		t.Fatalf("reopen added = %v", added)
	}
	if got := items(t, s)[b]; got.State != StateTodo || got.ResolvedAt != nil {
		t.Fatalf("reopened b = %+v", got)
	}

	// b resolves and fires again after the reopen window → new item.
	sync(t, s, t0.Add(31*time.Minute), "a")
	added = sync(t, s, t0.Add(3*time.Hour), "a", "b")
	if len(added) != 1 || added[0] == b {
		t.Fatalf("after window added = %v", added)
	}
	if got := items(t, s)[b]; got.State != StateDone {
		t.Fatalf("old b = %+v", got)
	}

	// a moved to DONE by the user while firing stays DONE, and gets
	// resolved_at once it stops firing.
	if err := s.Move(ctx, a, StateDone, t0); err != nil {
		t.Fatal(err)
	}
	sync(t, s, t0.Add(4*time.Hour), "a")
	if got := items(t, s)[a]; got.State != StateDone || got.ResolvedAt != nil {
		t.Fatalf("done a = %+v", got)
	}
	sync(t, s, t0.Add(5*time.Hour))
	if got := items(t, s)[a]; got.State != StateDone || got.ResolvedAt == nil {
		t.Fatalf("resolved a = %+v", got)
	}
}

func TestSyncResolvedItemMovedBackToTodo(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id := sync(t, s, t0, "a")[0]
	sync(t, s, t0.Add(time.Minute))
	if err := s.Move(ctx, id, StateTodo, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	sync(t, s, t0.Add(3*time.Minute))
	if got := items(t, s)[id]; got.State != StateTodo || got.ResolvedAt == nil {
		t.Fatalf("item = %+v", got)
	}
}

func TestSyncInProgressResolved(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id := sync(t, s, t0, "a")[0]
	if err := s.Start(ctx, id, "sess", t0); err != nil {
		t.Fatal(err)
	}
	sync(t, s, t0.Add(time.Minute))
	if got := items(t, s)[id]; got.State != StateDone {
		t.Fatalf("item = %+v", got)
	}
	// The agent finishing later only records whether it failed.
	moved, err := s.Finish(ctx, id, "sess", false, t0.Add(2*time.Minute))
	if err != nil || moved {
		t.Fatalf("moved = %v, err = %v", moved, err)
	}
	got := items(t, s)[id]
	if got.State != StateDone || got.Failed {
		t.Fatalf("item = %+v", got)
	}
}

func TestSyncIsolatedByKindAndSource(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sync(t, s, t0, "a")
	for _, u := range []Update{
		{Kind: "alert", Source: "other", At: t0},
		{Kind: "pr", Source: "src", At: t0},
	} {
		if _, err := s.Sync(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for _, it := range items(t, s) {
		if it.State != StateTodo {
			t.Fatalf("item = %+v", it)
		}
	}
}

func TestGet(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id := sync(t, s, t0, "a")[0]
	if got, err := s.Get(ctx, id); err != nil || got.ID != id || got.Title != "title a" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, id+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item = %v", err)
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id, err := s.Create(ctx, Item{Key: "task:t", Kind: "task", Source: "task", CreatedAt: t0, Title: "Fix", Description: "the build"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx, id, "s1", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx, id, "s2", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second start err = %v", err)
	}
	if err := s.SetPID(ctx, id, "s1", 42); err != nil {
		t.Fatal(err)
	}
	if got := items(t, s)[id]; got.State != StateInProgress || got.PID != 42 || got.SessionID != "s1" {
		t.Fatalf("item = %+v", got)
	}
	// A stale session does not touch the item.
	if moved, err := s.Finish(ctx, id, "old", true, t0); err != nil || moved {
		t.Fatalf("stale finish = %v, %v", moved, err)
	}
	moved, err := s.Finish(ctx, id, "s1", true, t0.Add(time.Minute))
	if err != nil || !moved {
		t.Fatalf("finish = %v, %v", moved, err)
	}
	got := items(t, s)[id]
	if got.State != StateWaiting || got.PID != 0 || !got.Failed {
		t.Fatalf("item = %+v", got)
	}
	// A PID recorded after the wrapper finished is ignored.
	if err := s.SetPID(ctx, id, "s1", 43); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late SetPID err = %v", err)
	}
}

func TestAbort(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id := sync(t, s, t0, "a")[0]
	if err := s.Start(ctx, id, "s1", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Abort(ctx, id, "s1", t0); err != nil {
		t.Fatal(err)
	}
	if got := items(t, s)[id]; got.State != StateTodo || got.SessionID != "" {
		t.Fatalf("item = %+v", got)
	}
}

func TestDeleteDone(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	ids := sync(t, s, t0, "a", "b", "c")
	_ = s.Move(ctx, ids[0], StateDone, t0)
	_ = s.Move(ctx, ids[1], StateDone, t0.Add(2*time.Hour))
	deleted, err := s.DeleteDone(ctx, t0.Add(time.Hour))
	if err != nil || len(deleted) != 1 || deleted[0] != ids[0] {
		t.Fatalf("deleted = %v, %v", deleted, err)
	}
	if len(items(t, s)) != 2 {
		t.Fatal("wrong item count")
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tower.lock")
	f, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	// flock locks are per open file description, so a second open in the
	// same process conflicts too.
	if _, err := Lock(path); err == nil {
		t.Fatal("second lock succeeded")
	}
	_ = f.Close()
	f, err = Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestSyncRejectsFailedPoll(t *testing.T) {
	s := open(t)
	if _, err := s.Sync(context.Background(), Update{Kind: "alert", Source: "src", At: t0, Err: errors.New("boom")}); err == nil {
		t.Fatal("failed poll was synced")
	}
}

func TestSyncUpdatesFields(t *testing.T) {
	for _, state := range States {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			it := startedItem(t, s)
			if state != StateInProgress {
				if _, err := s.Finish(ctx, it.ID, it.SessionID, true, t0.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				if state != StateWaiting {
					if err := s.Move(ctx, it.ID, state, t0.Add(2*time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := items(t, s)[it.ID]
			in := alert("a")
			in.Title, in.Description, in.Details, in.URL = "new title", "new description", "# new", ""
			added, err := s.Sync(ctx, Update{Kind: "alert", Source: "src", Items: []Incoming{in}, At: t0.Add(3 * time.Minute), ReopenWindow: time.Hour})
			if err != nil || len(added) != 0 {
				t.Fatalf("sync = %v, %v", added, err)
			}
			got := items(t, s)[it.ID]
			if got.Title != in.Title || got.Description != in.Description || got.Details != in.Details || got.URL != in.URL {
				t.Fatalf("item fields = %+v", got)
			}
			if got.State != before.State || !got.UpdatedAt.Equal(before.UpdatedAt) || !got.CreatedAt.Equal(before.CreatedAt) ||
				got.SessionID != before.SessionID || got.PID != before.PID || got.Failed != before.Failed || got.ResolvedAt != nil {
				t.Fatalf("sync changed lifecycle metadata: %+v", got)
			}
		})
	}
}
