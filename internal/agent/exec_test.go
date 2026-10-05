package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/store"
)

func TestExecDoesNotRunRecoveredSession(t *testing.T) {
	for _, state := range []store.State{store.StateWaiting, store.StateTodo, store.StateDone} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			_, s, dir := setup(t, "true {{.Prompt}} {{.SessionID}}", "true {{.SessionID}}")
			it := createTask(t, s)
			if err := s.Start(ctx, it.ID, "session", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverStarts(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := s.Move(ctx, it.ID, state, time.Now()); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "command-ran")
			code := Exec(s, it.ID, "session", []string{"sh", "-c", `printf ran > "$0"`, marker})
			if code != 127 {
				t.Fatalf("exit code = %d", code)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("recovered session ran its command: %v", err)
			}
			got, err := s.Get(ctx, it.ID)
			if err != nil || got.State != state || got.PID != 0 || !got.Failed {
				t.Fatalf("item = %+v, err = %v", got, err)
			}
		})
	}
}

func TestExecContinuesClaimedResolvedSession(t *testing.T) {
	ctx := context.Background()
	_, s, _ := setup(t, "true {{.Prompt}} {{.SessionID}}", "true {{.SessionID}}")
	it := createTask(t, s)
	if err := s.Start(ctx, it.ID, "session", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPID(ctx, it.ID, "session", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(ctx, it.ID, store.StateDone, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := Exec(s, it.ID, "session", []string{"sh", "-c", "exit 3"}); code != 3 {
		t.Fatalf("exit code = %d", code)
	}
	got, err := s.Get(ctx, it.ID)
	if err != nil || got.State != store.StateDone || got.PID != 0 || !got.Failed {
		t.Fatalf("item = %+v, err = %v", got, err)
	}
}
