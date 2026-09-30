package notify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ricoberger/tower/internal/item"
)

const itemID = "alert-dev-abc123-1"

// fakeScript is a fake delivery executable. It records its arguments
// NUL-separated to $FAKE_NOTIFY_LOG/<name>.args and behaves according to
// $FAKE_NOTIFY_MODE: ok, fail, hang (sleeps with a descendant) or pipe
// (exits zero while a descendant keeps stdout open). Descendant PIDs are
// written to <name>.child.
const fakeScript = `#!/bin/sh
PATH=/bin:/usr/bin
name=$(basename "$0")
printf '%s\0' "$@" >"$FAKE_NOTIFY_LOG/$name.args"
case "$FAKE_NOTIFY_MODE" in
fail) exit 3 ;;
hang)
	(trap '' TERM; sleep 60) &
	echo $! >"$FAKE_NOTIFY_LOG/$name.child"
	sleep 60 ;;
pipe)
	(trap '' TERM; sleep 60) &
	echo $! >"$FAKE_NOTIFY_LOG/$name.child"
	exit 0 ;;
esac
exit 0
`

// fakeBin creates a PATH directory containing the named fake executables and
// isolates PATH to it.
func fakeBin(t *testing.T, names ...string) (logDir string) {
	t.Helper()
	bin := t.TempDir()
	logDir = t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(fakeScript), 0o700); err != nil { // #nosec G306 -- executable test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_NOTIFY_LOG", logDir)
	t.Setenv("FAKE_NOTIFY_MODE", "ok")
	return logDir
}

func recorded(t *testing.T, logDir, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, name+".args"))
	if err != nil {
		t.Fatalf("%s was not invoked: %v", name, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func invoked(logDir, name string) bool {
	_, err := os.Stat(filepath.Join(logDir, name+".args"))
	return err == nil
}

// waitGone waits until the descendant recorded by a fake has exited.
func waitGone(t *testing.T, logDir, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, name+".child"))
	if err != nil {
		t.Fatalf("no descendant recorded: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant %d survived the delivery", pid)
}

func newNotifier(exe, config string) *Notifier {
	return &Notifier{
		Sound:      "default",
		ConfigPath: config,
		Executable: func() (string, error) { return exe, nil },
		LookPath:   exec.LookPath,
		Timeout:    300 * time.Millisecond,
		WaitDelay:  100 * time.Millisecond,
	}
}

func TestDefaults(t *testing.T) {
	if Timeout != 10*time.Second || WaitDelay <= 0 || WaitDelay > time.Second || MaxMessage != 200 {
		t.Fatalf("defaults = %s %s %d", Timeout, WaitDelay, MaxMessage)
	}
}

func TestTitles(t *testing.T) {
	for o, want := range map[item.Outcome]string{
		item.OutcomeReady: "tower — report ready", item.OutcomeBlocked: "tower — blocked", item.OutcomeFailed: "tower — run failed",
	} {
		if got := (Notification{Outcome: o}).Title(); got != want {
			t.Errorf("%s: %q", o, got)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 200); got != "short" {
		t.Errorf("got %q", got)
	}
	exact := strings.Repeat("ä", 200)
	if got := Truncate(exact, 200); got != exact {
		t.Error("200 characters must not be cut")
	}
	long := strings.Repeat("🚀é", 150)
	got := Truncate(long, 200)
	if utf8.RuneCountInString(got) != 200 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") ||
		!strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Errorf("got %d runes, valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if got := Truncate("a\xffb", 200); !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 kept: %q", got)
	}
}

func TestTerminalNotifierPreferred(t *testing.T) {
	logDir := fakeBin(t, "terminal-notifier", "osascript")
	n := newNotifier("/opt/tower/bin/tower", "/etc/tower/config.yaml")
	x := Notification{ItemID: itemID, Outcome: item.OutcomeReady, Subtitle: "KubePodCrashLooping: core/api-1", Message: "pods crash-loop"}
	if err := n.Deliver(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-title", "tower — report ready", "-subtitle", "KubePodCrashLooping: core/api-1", "-message", "pods crash-loop",
		"-sound", "default", "-group", itemID,
		"-execute", "'/opt/tower/bin/tower' --config '/etc/tower/config.yaml' resume " + itemID,
	}
	if got := recorded(t, logDir, "terminal-notifier"); !slices.Equal(got, want) {
		t.Errorf("args =\n%q\nwant\n%q", got, want)
	}
	if invoked(logDir, "osascript") {
		t.Error("osascript invoked although terminal-notifier exists")
	}

	// Without sound; the message is truncated.
	n.Sound = ""
	x.Outcome = item.OutcomeFailed
	x.Message = strings.Repeat("ü", 250)
	if err := n.Deliver(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	got := recorded(t, logDir, "terminal-notifier")
	if slices.Contains(got, "-sound") || got[1] != "tower — run failed" || utf8.RuneCountInString(got[5]) != 200 {
		t.Errorf("args = %q", got)
	}
}

func TestResumeTargetQuoting(t *testing.T) {
	// The tower executable and config live in paths with spaces, quotes and
	// shell metacharacters; the -execute command must reproduce them
	// literally when run by a shell.
	base := filepath.Join(t.TempDir(), `it's $(touch pwned) & "x";`)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	exe := filepath.Join(base, "tower `id` 'q'")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" >\"" + filepath.Join(logDir, "tower.args") + "\"\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}
	config := filepath.Join(base, "my config's $HOME.yaml")
	cmd, err := ResumeCommand(exe, config, itemID)
	if err != nil {
		t.Fatal(err)
	}
	sh := exec.CommandContext(t.Context(), "/bin/sh", "-c", cmd) // #nosec G204 -- test executes the generated command
	sh.Dir = t.TempDir()
	sh.Env = []string{"PATH=/nonexistent"}
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("run %q: %v %s", cmd, err, out)
	}
	if got := recorded(t, logDir, "tower"); !slices.Equal(got, []string{"--config", config, "resume", itemID}) {
		t.Errorf("args = %q", got)
	}
	if _, err := os.Stat(filepath.Join(sh.Dir, "pwned")); err == nil {
		t.Error("shell metacharacters were executed")
	}

	for _, c := range []struct{ exe, config, id string }{
		{"tower", config, itemID},
		{exe, "config.yaml", itemID},
		{exe, config, "../x"},
	} {
		if _, err := ResumeCommand(c.exe, c.config, c.id); err == nil {
			t.Errorf("ResumeCommand(%q, %q, %q) accepted", c.exe, c.config, c.id)
		}
	}
}

func TestTargetResolutionFailure(t *testing.T) {
	logDir := fakeBin(t, "terminal-notifier", "osascript")
	x := Notification{ItemID: itemID, Outcome: item.OutcomeReady, Subtitle: "s", Message: "m"}
	n := newNotifier("", "/c.yaml")
	n.Executable = func() (string, error) { return "", errors.New("unsupported") }
	if err := n.Deliver(context.Background(), x); err == nil || !strings.Contains(err.Error(), "tower executable") {
		t.Fatalf("err = %v", err)
	}
	n = newNotifier("/bin/tower", "")
	if err := n.Deliver(context.Background(), x); err == nil || !strings.Contains(err.Error(), "configuration path") {
		t.Fatalf("err = %v", err)
	}
	if invoked(logDir, "terminal-notifier") || invoked(logDir, "osascript") {
		t.Error("delivered without a resolvable resume target")
	}
}

func TestOsascriptFallback(t *testing.T) {
	logDir := fakeBin(t, "osascript")
	n := newNotifier("/bin/tower", "/c.yaml")
	message := "say \"hi\" \\ back\nslash $(touch pwned); `id` ' end tell"
	subtitle := `KubePod"Crash" \ 'x'`
	x := Notification{ItemID: itemID, Outcome: item.OutcomeBlocked, Subtitle: subtitle, Message: message}
	if err := n.Deliver(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-e", "on run argv",
		"-e", "display notification (item 1 of argv) with title (item 2 of argv) subtitle (item 3 of argv) sound name (item 4 of argv)",
		"-e", "end run", "--", message, "tower — blocked", subtitle, "default",
	}
	if got := recorded(t, logDir, "osascript"); !slices.Equal(got, want) {
		t.Errorf("args =\n%q\nwant\n%q", got, want)
	}
	n.Sound = ""
	if err := n.Deliver(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	got := recorded(t, logDir, "osascript")
	if strings.Contains(got[3], "sound") || len(got) != 10 {
		t.Errorf("args = %q", got)
	}
}

// TestOsascriptArgumentsAreData runs the real osascript with the generated
// script prefix but a statement that returns the arguments instead of
// displaying them, proving hostile content stays data.
func TestOsascriptArgumentsAreData(t *testing.T) {
	path, err := exec.LookPath("osascript")
	if err != nil {
		t.Skip("osascript not available")
	}
	n := &Notifier{LookPath: func(name string) (string, error) {
		if name == "osascript" {
			return path, nil
		}
		return "", errors.New("not found")
	}}
	message := "-e \"x\" & (do shell script \"touch pwned\") \\ \n end run"
	_, args, err := n.Command(Notification{ItemID: itemID, Outcome: item.OutcomeFailed, Subtitle: "s", Message: message})
	if err != nil {
		t.Fatal(err)
	}
	args[3] = "return item 1 of argv"
	cmd := exec.CommandContext(t.Context(), path, args...) // #nosec G204 -- test executes the generated arguments
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(string(out), "\n") != message {
		t.Errorf("osascript returned %q", out)
	}
	if _, err := os.Stat(filepath.Join(cmd.Dir, "pwned")); err == nil {
		t.Error("content was executed")
	}
}

func TestDeliveryFailures(t *testing.T) {
	x := Notification{ItemID: itemID, Outcome: item.OutcomeReady, Subtitle: "s", Message: "m"}
	t.Run("nothing installed", func(t *testing.T) {
		fakeBin(t)
		if err := newNotifier("/bin/tower", "/c.yaml").Deliver(context.Background(), x); err == nil {
			t.Fatal("delivery without executables succeeded")
		}
	})
	t.Run("failing terminal-notifier does not fall back", func(t *testing.T) {
		logDir := fakeBin(t, "terminal-notifier", "osascript")
		t.Setenv("FAKE_NOTIFY_MODE", "fail")
		err := newNotifier("/bin/tower", "/c.yaml").Deliver(context.Background(), x)
		if err == nil || !strings.Contains(err.Error(), "terminal-notifier failed") {
			t.Fatalf("err = %v", err)
		}
		if invoked(logDir, "osascript") {
			t.Error("fell back to osascript")
		}
	})
}

func TestDeliveryDeadline(t *testing.T) {
	x := Notification{ItemID: itemID, Outcome: item.OutcomeReady, Subtitle: "s", Message: "m"}
	for _, name := range []string{"terminal-notifier", "osascript"} {
		t.Run(name, func(t *testing.T) {
			logDir := fakeBin(t, name)
			t.Setenv("FAKE_NOTIFY_MODE", "hang")
			n := newNotifier("/bin/tower", "/c.yaml")
			start := time.Now()
			err := n.Deliver(context.Background(), x)
			elapsed := time.Since(start)
			if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "300ms") {
				t.Fatalf("err = %v", err)
			}
			if elapsed < n.Timeout || elapsed > n.Timeout+n.WaitDelay+2*time.Second {
				t.Errorf("elapsed = %s", elapsed)
			}
			waitGone(t, logDir, name)
		})
	}
	t.Run("descendant holding output after success", func(t *testing.T) {
		logDir := fakeBin(t, "terminal-notifier")
		t.Setenv("FAKE_NOTIFY_MODE", "pipe")
		n := newNotifier("/bin/tower", "/c.yaml")
		n.Timeout = 5 * time.Second
		start := time.Now()
		if err := n.Deliver(context.Background(), x); err != nil {
			t.Fatalf("err = %v", err)
		}
		if elapsed := time.Since(start); elapsed > n.WaitDelay+2*time.Second {
			t.Errorf("elapsed = %s, want bounded by WaitDelay", elapsed)
		}
		waitGone(t, logDir, "terminal-notifier")
	})
}
