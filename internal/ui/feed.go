package ui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/snapshot"
)

// SnapshotMsg delivers a snapshot to the model.
type SnapshotMsg snapshot.Snapshot

// waitFeed returns a command that delivers the next published snapshot.
func waitFeed(ctx context.Context, f *snapshot.Feed) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.Updated():
			s, _ := f.Latest()
			return SnapshotMsg(s)
		case <-ctx.Done():
			return nil
		}
	}
}
