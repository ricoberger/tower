// Package notify shows macOS notifications via osascript.
package notify

import (
	"context"
	"os/exec"
	"time"
)

const script = `on run argv
	display notification (item 3 of argv) with title (item 1 of argv) subtitle (item 2 of argv)
end run`

type Notify struct{}

func New() *Notify {
	return &Notify{}
}

// Send shows a notification. The texts are passed as arguments, never
// interpolated into the script.
func (n *Notify) Send(title, subtitle, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "osascript", "-e", script, title, subtitle, message).Run()
}
