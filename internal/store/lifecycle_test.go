package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func startedItem(t *testing.T, s *Store) Item {
	t.Helper()
	ctx := context.Background()
	id := sync(t, s, t0, "a")[0]
	if err := s.Start(ctx, id, "session", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPID(ctx, id, "session", 42); err != nil {
		t.Fatal(err)
	}
	return items(t, s)[id]
}

func TestFinishIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	before := startedItem(t, s)
	_, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_finish BEFORE UPDATE OF state ON items
		WHEN NEW.state = 'waiting' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := s.Finish(ctx, before.ID, before.SessionID, true, t0.Add(time.Minute)); err == nil || moved {
		t.Fatalf("finish = %v, %v", moved, err)
	}
	if got := items(t, s)[before.ID]; !reflect.DeepEqual(got, before) {
		t.Fatalf("failed finish changed item: %+v", got)
	}
}

func TestFinishPreservesUserMoves(t *testing.T) {
	for _, state := range []State{StateTodo, StateDone} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			it := startedItem(t, s)
			movedAt := t0.Add(time.Minute)
			if err := s.Move(ctx, it.ID, state, movedAt); err != nil {
				t.Fatal(err)
			}
			moved, err := s.Finish(ctx, it.ID, it.SessionID, true, t0.Add(2*time.Minute))
			if err != nil || moved {
				t.Fatalf("finish = %v, %v", moved, err)
			}
			got := items(t, s)[it.ID]
			if got.State != state || !got.UpdatedAt.Equal(movedAt) || got.PID != 0 || !got.Failed || got.SessionID != it.SessionID {
				t.Fatalf("item = %+v", got)
			}
		})
	}
}

func TestFinishDoesNotOverwriteCompletedSession(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	it := startedItem(t, s)
	if _, err := s.Finish(ctx, it.ID, it.SessionID, false, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := items(t, s)[it.ID]
	if moved, err := s.Finish(ctx, it.ID, it.SessionID, true, t0.Add(2*time.Minute)); err != nil || moved {
		t.Fatalf("second finish = %v, %v", moved, err)
	}
	if got := items(t, s)[it.ID]; !reflect.DeepEqual(got, before) {
		t.Fatalf("second finish changed completed session: %+v", got)
	}
}

func TestRecoverRequiresMatchingObservation(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(context.Context, *Store, Item) error
		pid     int
		session string
		moved   bool
	}{
		{name: "dead wrapper", pid: 42, session: "session", moved: true},
		{name: "different PID", pid: 41, session: "session"},
		{name: "claimed since snapshot", pid: 0, session: "session"},
		{name: "different session", pid: 42, session: "old"},
		{name: "wrapper finished", pid: 42, session: "session", prepare: func(ctx context.Context, s *Store, it Item) error {
			_, err := s.Finish(ctx, it.ID, it.SessionID, false, t0.Add(time.Minute))
			return err
		}},
		{name: "moved to done", pid: 42, session: "session", prepare: func(ctx context.Context, s *Store, it Item) error {
			return s.Move(ctx, it.ID, StateDone, t0.Add(time.Minute))
		}},
		{name: "moved to todo", pid: 42, session: "session", prepare: func(ctx context.Context, s *Store, it Item) error {
			return s.Move(ctx, it.ID, StateTodo, t0.Add(time.Minute))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			other, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = other.Close() }()
			it := startedItem(t, s)
			if tt.prepare != nil {
				if err := tt.prepare(ctx, other, it); err != nil {
					t.Fatal(err)
				}
			}
			before := items(t, s)[it.ID]
			moved, err := s.Recover(ctx, it.ID, tt.session, tt.pid, t0.Add(2*time.Minute))
			if err != nil || moved != tt.moved {
				t.Fatalf("recover = %v, %v", moved, err)
			}
			got := items(t, s)[it.ID]
			if tt.moved {
				if got.State != StateWaiting || got.PID != 0 || !got.Failed || got.SessionID != it.SessionID {
					t.Fatalf("recovered item = %+v", got)
				}
			} else if !reflect.DeepEqual(got, before) {
				t.Fatalf("stale observation changed item: %+v", got)
			}
		})
	}
}

func TestRecoverStartsCompetesWithWrapperClaim(t *testing.T) {
	for _, claimFirst := range []bool{false, true} {
		name := "recovery first"
		if claimFirst {
			name = "claim first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			id := sync(t, s, t0, "a")[0]
			if err := s.Start(ctx, id, "session", t0); err != nil {
				t.Fatal(err)
			}
			if claimFirst {
				if err := s.SetPID(ctx, id, "session", 42); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RecoverStarts(ctx, t0.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			err := s.SetPID(ctx, id, "session", 42)
			got := items(t, s)[id]
			if claimFirst {
				if err != nil || got.State != StateInProgress || got.PID != 42 || got.Failed {
					t.Fatalf("claimed item = %+v, err = %v", got, err)
				}
			} else {
				if !errors.Is(err, ErrNotFound) || got.State != StateWaiting || got.PID != 0 || !got.Failed {
					t.Fatalf("recovered item = %+v, err = %v", got, err)
				}
				for _, state := range []State{StateTodo, StateDone} {
					if err := s.Move(ctx, id, state, t0.Add(2*time.Minute)); err != nil {
						t.Fatal(err)
					}
					if err := s.SetPID(ctx, id, "session", 42); !errors.Is(err, ErrNotFound) {
						t.Fatalf("late claim on %s = %v", state, err)
					}
				}
			}
		})
	}
}

func TestSetPIDPreservesExistingClaim(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	it := startedItem(t, s)
	if err := s.SetPID(ctx, it.ID, it.SessionID, 43); !errors.Is(err, ErrNotFound) {
		t.Fatalf("different wrapper claim = %v", err)
	}
	if err := s.Move(ctx, it.ID, StateDone, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Resolution does not stop a wrapper that already claimed its session.
	if err := s.SetPID(ctx, it.ID, it.SessionID, it.PID); err != nil {
		t.Fatalf("confirm claim after resolution: %v", err)
	}
}
