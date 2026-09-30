package ghostty

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func request() Request {
	return Request{
		Helper:     "/bin/true",
		Placement:  PlacementSplit,
		Direction:  "right",
		WorkingDir: "/state dir/items/abc",
		Title:      "High latency",
		Command:    "/opt/bin/copilot",
		SessionID:  "sess-1",
		ResumeArgs: []string{"--allow-all-tools"},
	}
}

func TestArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Request)
		want   []string
	}{
		{"split with direction", nil, []string{
			"--placement", "split", "--direction", "right",
			"--working-dir", "/state dir/items/abc", "--title", "tower: High latency",
			"--command", "'/opt/bin/copilot' '--resume' 'sess-1' '--allow-all-tools'",
		}},
		{"split without direction", func(r *Request) { r.Direction = "" }, []string{
			"--placement", "split",
			"--working-dir", "/state dir/items/abc", "--title", "tower: High latency",
			"--command", "'/opt/bin/copilot' '--resume' 'sess-1' '--allow-all-tools'",
		}},
		{"tab ignores direction", func(r *Request) { r.Placement = PlacementTab }, []string{
			"--placement", "tab",
			"--working-dir", "/state dir/items/abc", "--title", "tower: High latency",
			"--command", "'/opt/bin/copilot' '--resume' 'sess-1' '--allow-all-tools'",
		}},
		{"window without resume args", func(r *Request) { r.Placement = PlacementWindow; r.ResumeArgs = nil }, []string{
			"--placement", "window",
			"--working-dir", "/state dir/items/abc", "--title", "tower: High latency",
			"--command", "'/opt/bin/copilot' '--resume' 'sess-1'",
		}},
		{"title control characters", func(r *Request) { r.Title = "a\nb\x1bc" }, []string{
			"--placement", "split", "--direction", "right",
			"--working-dir", "/state dir/items/abc", "--title", "tower: a b c",
			"--command", "'/opt/bin/copilot' '--resume' 'sess-1' '--allow-all-tools'",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			got, err := Args(r)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestQuoteSpecialCharacters(t *testing.T) {
	got, err := TypedCommand("/Apps/My Tools/it's copilot", "s'1", []string{"a b", "!!", "$(touch x)"})
	if err != nil {
		t.Fatal(err)
	}
	want := `'/Apps/My Tools/it'\''s copilot' '--resume' 's'\''1' 'a b' '!!' '$(touch x)'`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

// TestTypedCommandRoundTrip types the command into real POSIX shells and
// checks that a fake program receives every argument literally and nothing
// else runs.
func TestTypedCommandRoundTrip(t *testing.T) {
	dir := t.TempDir()
	exeDir := filepath.Join(dir, "it's a dir")
	if err := os.Mkdir(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(exeDir, "fake copilot")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	args := []string{
		"!!", "!$", "$(touch " + marker + ")", "`touch " + marker + "`", "a; touch " + marker,
		"# comment", "x'y", `back\slash`, "$HOME", "*", "~", "{a,b}", "", "日本",
	}
	typed, err := TypedCommand(exe, "session-1", args)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"--resume", "session-1"}, args...)
	for _, sh := range [][]string{
		{"/bin/sh", "-c"},
		{"/bin/zsh", "-f", "-i", "-c"},
		{"/bin/bash", "--norc", "--noprofile", "-i", "-c"},
	} {
		if _, err := os.Stat(sh[0]); err != nil {
			continue
		}
		cmd := exec.CommandContext(t.Context(), sh[0], append(sh[1:], typed)...) // #nosec G204 -- test shells with a fixed command
		cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
		cmd.Stdin = nil
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", sh[0], err)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if !slices.Equal(got, want) {
			t.Fatalf("%s:\ngot  %q\nwant %q", sh[0], got, want)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s executed an argument", sh[0])
		}
	}
}

func TestControlCharactersRejected(t *testing.T) {
	secret := "tok\nsecret-value"
	for _, tc := range []struct {
		name      string
		mutate    func(*Request)
		component string
	}{
		{"command newline", func(r *Request) { r.Command = "/bin/" + secret }, "runs.command"},
		{"session CR", func(r *Request) { r.SessionID = "s\rsecret-value" }, "session ID"},
		{"resume arg NUL", func(r *Request) { r.ResumeArgs = []string{"ok", "a\x00secret-value"} }, "runs.resume_args[1]"},
		{"resume arg newline", func(r *Request) { r.ResumeArgs = []string{secret} }, "runs.resume_args[0]"},
		{"working dir", func(r *Request) { r.WorkingDir = "/x\nsecret-value" }, "item directory"},
		{"direction", func(r *Request) { r.Direction = "right\nsecret-value" }, "ghostty.direction"},
		{"invalid UTF-8", func(r *Request) { r.ResumeArgs = []string{"\xffsecret-value"} }, "runs.resume_args[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "launched")
			helper := filepath.Join(dir, "helper")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil { // #nosec G306 -- test executable
				t.Fatal(err)
			}
			r := request()
			r.Helper = helper
			tc.mutate(&r)
			err := (&Launcher{}).Launch(context.Background(), r)
			if err == nil || !strings.Contains(err.Error(), tc.component) {
				t.Fatalf("err %v", err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("value leaked: %v", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("helper launched")
			}
		})
	}
}

func TestArgsValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"placement":  func(r *Request) { r.Placement = "pane" },
		"workingdir": func(r *Request) { r.WorkingDir = "" },
		"command":    func(r *Request) { r.Command = "" },
		"session":    func(r *Request) { r.SessionID = "" },
	} {
		r := request()
		mutate(&r)
		if _, err := Args(r); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	r := request()
	r.Helper = ""
	if err := (&Launcher{}).Launch(context.Background(), r); err == nil {
		t.Fatal("empty helper accepted")
	}
}

func TestLauncher(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + out + "'\nprintf 'X=%s\\n' \"$X\" >> '" + out + "'\necho noise; echo noise >&2\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	r := request()
	r.Helper = helper
	if err := (&Launcher{Env: []string{"X=from-login", "PATH=/usr/bin:/bin"}}).Launch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	args, _ := Args(r)
	want := strings.Join(args, "\n") + "\nX=from-login\n"
	if string(data) != want {
		t.Fatalf("got %q\nwant %q", data, want)
	}

	// A failing helper is an error.
	failing := filepath.Join(dir, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	r.Helper = failing
	if err := (&Launcher{}).Launch(context.Background(), r); err == nil || strings.Contains(err.Error(), "--resume") {
		t.Fatalf("err %v", err)
	}
}

func TestLauncherTimeoutKillsGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nsleep 30 &\necho $! > '" + pidFile + "'\nwait\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	r := request()
	r.Helper = helper
	start := time.Now()
	err := (&Launcher{Timeout: 300 * time.Millisecond, WaitDelay: 100 * time.Millisecond}).Launch(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not enforced")
	}
	data, _ := os.ReadFile(pidFile) // #nosec G304 -- test file
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		t.Fatal("no descendant pid")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := exec.CommandContext(t.Context(), "/bin/kill", "-0", pid).Run(); err != nil { // #nosec G204 -- test pid
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant survived")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
