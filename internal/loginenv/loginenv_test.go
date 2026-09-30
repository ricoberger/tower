package loginenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeShell writes a POSIX script that behaves like a login shell whose
// interactive startup file exports PATH and a secret-bearing variable. It
// fails unless called as `-l -i -c <script>`.
func fakeShell(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake shell")
	if body == "" {
		body = `[ "$1" = "-l" ] && [ "$2" = "-i" ] && [ "$3" = "-c" ] || { echo "bad args: $*" >&2; exit 64; }
echo "startup noise on stdout"
echo "startup noise on stderr" >&2
printf 'tower-env-fake:begin\0KEY=looks like a record\0'
export PATH=/login/bin:/usr/bin:/bin
export GRAFANA_INSTANCES='[{"name":"dev"}]'
if read -r line; then echo "stdin was readable: $line" >&2; exit 65; fi
exec /bin/sh -c "$4"
`
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	return path
}

func testEnviron() []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "CALLER=kept"}
}

func TestCapture(t *testing.T) {
	sh := fakeShell(t, "")
	env, err := Capture(context.Background(), sh, Options{Environ: testEnviron})
	if err != nil {
		t.Fatal(err)
	}
	if env.Vars["PATH"] != "/login/bin:/usr/bin:/bin" {
		t.Fatalf("PATH %q", env.Vars["PATH"])
	}
	if env.Vars["GRAFANA_INSTANCES"] != `[{"name":"dev"}]` || env.Vars["CALLER"] != "kept" {
		t.Fatalf("vars %v", env.Vars)
	}
	if _, ok := env.Vars["KEY"]; ok {
		t.Fatal("noise before the marker parsed as a record")
	}
}

func TestCaptureMultilineValues(t *testing.T) {
	sh := fakeShell(t, `export MULTI="$(printf 'a\nb=c')"
exec /bin/sh -c "$4"
`)
	env, err := Capture(context.Background(), sh, Options{Environ: testEnviron})
	if err != nil {
		t.Fatal(err)
	}
	if env.Vars["MULTI"] != "a\nb=c" {
		t.Fatalf("MULTI %q", env.Vars["MULTI"])
	}
}

func TestCaptureMalformed(t *testing.T) {
	// The capture script starts with `printf '%s:begin\0' <marker>;`.
	marker := "m=$(printf '%s' \"$4\" | awk '{print $3}' | tr -d ';')\n"
	for name, tc := range map[string]struct{ body, want string }{
		"no markers":     {"echo 'SECRET=super-secret-value'\n", "start marker"},
		"exit failure":   {"echo 'SECRET=super-secret-value'\nexit 3\n", "failed"},
		"missing end":    {marker + "printf '%s:begin\\0SECRET=super-secret-value\\0' \"$m\"\n", "end marker"},
		"not terminated": {marker + "printf '%s:begin\\0SECRET=super-secret-value%s:end\\0' \"$m\" \"$m\"\n", "not NUL terminated"},
		"invalid record": {marker + "printf '%s:begin\\0SECRET-super-secret-value\\0%s:end\\0' \"$m\" \"$m\"\n", "invalid environment record"},
	} {
		t.Run(name, func(t *testing.T) {
			sh := fakeShell(t, tc.body)
			_, err := Capture(context.Background(), sh, Options{Environ: testEnviron})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "super-secret-value") {
				t.Fatalf("content leaked: %v", err)
			}
		})
	}
}

func TestParse(t *testing.T) {
	b, e := "M:begin\x00", "M:end\x00"
	for name, tc := range map[string]struct {
		out string
		ok  bool
	}{
		"valid":          {"noise" + b + "A=1\x00B=\x00" + e + "noise", true},
		"no begin":       {"A=1\x00" + e, false},
		"no end":         {b + "A=1\x00", false},
		"unterminated":   {b + "A=1" + e, false},
		"invalid record": {b + "SECRET-no-equals\x00" + e, false},
		"empty key":      {b + "=v\x00" + e, false},
		"empty":          {b + e, false},
	} {
		vars, err := parse([]byte(tc.out), b, e)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: leaked content", name)
		}
		if tc.ok && (vars["A"] != "1" || vars["B"] != "") {
			t.Errorf("%s: vars %v", name, vars)
		}
	}
}

func TestCaptureTimeoutKillsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	sh := fakeShell(t, "sleep 30 &\necho $! > '"+pidFile+"'\nwait\n")
	start := time.Now()
	_, err := Capture(context.Background(), sh, Options{Environ: testEnviron, Timeout: 300 * time.Millisecond, WaitDelay: 100 * time.Millisecond})
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
	for exec.CommandContext(t.Context(), "/bin/kill", "-0", pid).Run() == nil { // #nosec G204 -- test pid
		if time.Now().After(deadline) {
			t.Fatal("descendant survived")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCaptureCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Capture(ctx, fakeShell(t, ""), Options{Environ: testEnviron}); err == nil {
		t.Fatal("no error")
	}
}

func TestSelectShell(t *testing.T) {
	sh := fakeShell(t, "")
	lookup := func(v string, ok bool) func(string) (string, bool) {
		return func(string) (string, bool) { return v, ok }
	}
	userShell := func(s string, err error) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return s, err }
	}
	called := false
	for name, tc := range map[string]struct {
		opts Options
		want string
	}{
		"SHELL wins": {Options{Lookup: lookup("/custom/shell", true), UserShell: func(context.Context) (string, error) {
			called = true
			return sh, nil
		}}, "/custom/shell"},
		"empty SHELL uses the password database": {Options{Lookup: lookup("", true), UserShell: userShell(sh, nil)}, sh},
		"unset SHELL uses the password database": {Options{Lookup: lookup("", false), UserShell: userShell(sh, nil)}, sh},
		"lookup failure falls back":              {Options{Lookup: lookup("", false), UserShell: userShell("", errors.New("dscl failed"))}, FallbackShell},
		"relative shell falls back":              {Options{Lookup: lookup("", false), UserShell: userShell("zsh", nil)}, FallbackShell},
		"missing shell falls back":               {Options{Lookup: lookup("", false), UserShell: userShell("/nonexistent/zsh", nil)}, FallbackShell},
	} {
		if got := SelectShell(context.Background(), tc.opts); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
	if called {
		t.Error("password database queried although SHELL was set")
	}
}

func TestMerge(t *testing.T) {
	captured := Env{Vars: map[string]string{
		"PATH": "/login/bin", "GRAFANA_INSTANCES": "login", "EMPTY": "login", "ONLY_LOGIN": "x",
	}}
	got := Merge([]string{"PATH=/usr/bin:/bin", "GRAFANA_INSTANCES=caller", "EMPTY=", "ONLY_CALLER=y", "malformed", "=v"}, captured)
	want := map[string]string{
		"PATH": "/login/bin", "GRAFANA_INSTANCES": "caller", "EMPTY": "", "ONLY_LOGIN": "x", "ONLY_CALLER": "y",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q, want %q", k, got[k], v)
		}
	}
	// Without a captured PATH the caller's PATH is kept.
	if got := Merge([]string{"PATH=/usr/bin"}, Env{Vars: map[string]string{"A": "1"}}); got["PATH"] != "/usr/bin" {
		t.Fatalf("PATH %q", got["PATH"])
	}
	if l := List(map[string]string{"B": "2", "A": "1"}); !slices.Equal(l, []string{"A=1", "B=2"}) {
		t.Fatalf("List %q", l)
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LookPath("tool", "relative:"+dir); err != nil || got != exe {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := LookPath("tool", "/usr/bin"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if _, err := LookPath("plain", dir); err == nil {
		t.Fatal("non-executable accepted")
	}
	if got, err := LookPath(exe, ""); err != nil || got != exe {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := LookPath(filepath.Join(dir, "missing"), ""); err == nil {
		t.Fatal("missing path accepted")
	}
}
