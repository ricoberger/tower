package source

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config"
)

// Synthetic credentials only.
const (
	testToken    = "syn-token-4f1e"      // #nosec G101 -- synthetic test value
	testPassword = "syn-password-9c2b"   // #nosec G101 -- synthetic test value
	cmdMarker    = "SYN-CMD-TEXT-MARKER" // appears only in command text
	cmdOutput    = "SYN-CMD-OUTPUT-7a1d" // printed by failing commands
)

func ptr(s string) *string { return &s }

// fakeAM is a fake Alertmanager recording the requests it receives.
type fakeAM struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	handler  http.HandlerFunc
}

func newFakeAM(t *testing.T, handler http.HandlerFunc) *fakeAM {
	t.Helper()
	f := &fakeAM{handler: handler}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		h := f.handler
		f.mu.Unlock()
		if h == nil {
			_, _ = w.Write([]byte("[]"))
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAM) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request received")
	}
	return f.requests[len(f.requests)-1]
}

func plainSource(url string, auth *config.Auth) config.Source {
	return config.Source{Name: "am", Type: config.SourceAlertmanager, URL: &url, Auth: auth}
}

func assertNoLeak(t *testing.T, s string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(s, secret) {
			t.Errorf("%q leaks %q", s, secret)
		}
	}
}

func TestAlertmanagerEndpoints(t *testing.T) {
	srv := newFakeAM(t, nil)
	tests := []struct {
		name     string
		src      config.Source
		path     string
		filter   []string
		receiver string
	}{
		{
			name: "plain with prefix and trailing slash",
			src: config.Source{
				Name: "am", Type: config.SourceAlertmanager, URL: ptr(srv.URL + "/prefix/am/"),
				Filter:   []string{`team="core"`, `severity=~"crit.*|warn"`, `env!=dev`},
				Receiver: `^(team-a|team b)$`,
			},
			path:     "/prefix/am/api/v2/alerts",
			filter:   []string{`team="core"`, `severity=~"crit.*|warn"`, `env!=dev`},
			receiver: `^(team-a|team b)$`,
		},
		{
			name: "grafana managed",
			src: config.Source{
				Name: "g", Type: config.SourceAlertmanager, GrafanaAlertmanager: "grafana",
				Instance:        &config.GrafanaInstance{URL: srv.URL + "/grafana/", TokenCommand: "echo " + testToken},
				GrafanaInstance: "prod",
				Filter:          []string{`a="b&c=d"`},
			},
			path:   "/grafana/api/alertmanager/grafana/api/v2/alerts",
			filter: []string{`a="b&c=d"`},
		},
		{
			name: "grafana managed with explicit url",
			src: config.Source{
				Name: "g", Type: config.SourceAlertmanager, GrafanaAlertmanager: "my-am",
				URL: ptr(srv.URL), Receiver: ".*",
			},
			path:     "/api/alertmanager/my-am/api/v2/alerts",
			receiver: ".*",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alerts, err := NewAlertmanager(tt.src).Fetch(context.Background())
			if err != nil || alerts == nil || len(alerts) != 0 {
				t.Fatalf("Fetch = %v, %v", alerts, err)
			}
			r := srv.last(t)
			if r.Method != http.MethodGet {
				t.Errorf("method = %s", r.Method)
			}
			if r.URL.Path != tt.path {
				t.Errorf("path = %q, want %q", r.URL.Path, tt.path)
			}
			q := r.URL.Query()
			for k, want := range map[string]string{"active": "true", "silenced": "true", "inhibited": "true", "unprocessed": "false"} {
				if got := q[k]; len(got) != 1 || got[0] != want {
					t.Errorf("%s = %v, want %s", k, got, want)
				}
			}
			if got := q["filter"]; strings.Join(got, "\x00") != strings.Join(tt.filter, "\x00") {
				t.Errorf("filter = %q, want %q", got, tt.filter)
			}
			if tt.receiver == "" {
				if _, ok := q["receiver"]; ok {
					t.Errorf("receiver must be omitted, got %q", q["receiver"])
				}
			} else if got := q["receiver"]; len(got) != 1 || got[0] != tt.receiver {
				t.Errorf("receiver = %q, want %q", got, tt.receiver)
			}
		})
	}
}

func TestAlertmanagerAuth(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	passwordFile := filepath.Join(dir, "password")
	if err := os.WriteFile(tokenFile, []byte("  "+testToken+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordFile, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newFakeAM(t, nil)
	basic := func(user, pw string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw))
	}
	tests := []struct {
		name string
		auth *config.Auth
		want string
	}{
		{"no auth", nil, ""},
		{"explicit none", &config.Auth{Type: config.AuthNone}, ""},
		// Direct values are used unchanged, including whitespace.
		{"basic direct", &config.Auth{Type: config.AuthBasic, Username: "tower", Password: ptr(" " + testPassword + " ")}, basic("tower", " "+testPassword+" ")},
		{"bearer direct", &config.Auth{Type: config.AuthBearer, Token: ptr(testToken)}, "Bearer " + testToken},
		{"bearer file", &config.Auth{Type: config.AuthBearer, TokenFile: ptr(tokenFile)}, "Bearer " + testToken},
		{"basic file", &config.Auth{Type: config.AuthBasic, Username: "tower", PasswordFile: ptr(passwordFile)}, basic("tower", testPassword)},
		{"bearer command", &config.Auth{Type: config.AuthBearer, TokenCommand: ptr("printf '  %s\\n' " + testToken)}, "Bearer " + testToken},
		{"basic command", &config.Auth{Type: config.AuthBasic, Username: "tower", PasswordCommand: ptr("echo " + testPassword)}, basic("tower", testPassword)},
		{"instance-derived bearer", nil, "Bearer " + testToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := plainSource(srv.URL, tt.auth)
			if tt.name == "instance-derived bearer" {
				src = config.Source{
					Name: "g", Type: config.SourceAlertmanager, GrafanaAlertmanager: "grafana", GrafanaInstance: "prod",
					Instance: &config.GrafanaInstance{URL: srv.URL, TokenCommand: "echo " + testToken},
				}
			}
			if _, err := NewAlertmanager(src).Fetch(context.Background()); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got := srv.last(t).Header.Get("Authorization"); got != tt.want {
				t.Errorf("Authorization = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlainSourceDoesNotUseInstanceCredentials(t *testing.T) {
	srv := newFakeAM(t, nil)
	marker := filepath.Join(t.TempDir(), "ran")
	src := plainSource(srv.URL, nil)
	src.GrafanaInstance = "prod"
	src.Instance = &config.GrafanaInstance{URL: "https://grafana.example.com", TokenCommand: "touch " + marker}
	if _, err := NewAlertmanager(src).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := srv.last(t).Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the instance token command must not run for plain sources")
	}
}

func TestCredentialsResolvedPerPoll(t *testing.T) {
	dir := t.TempDir()
	srv := newFakeAM(t, nil)

	tokenFile := filepath.Join(dir, "token")
	am := NewAlertmanager(plainSource(srv.URL, &config.Auth{Type: config.AuthBearer, TokenFile: ptr(tokenFile)}))
	for _, tok := range []string{"syn-first", "syn-second"} {
		if err := os.WriteFile(tokenFile, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := am.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := srv.last(t).Header.Get("Authorization"); got != "Bearer "+tok {
			t.Errorf("Authorization = %q, want token %q", got, tok)
		}
	}

	// The command runs at request time on every poll (and never before).
	counter := filepath.Join(dir, "counter")
	cmd := `n=$(cat "` + counter + `" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "` + counter + `"; echo syn-pw-$n`
	am = NewAlertmanager(plainSource(srv.URL, &config.Auth{Type: config.AuthBasic, Username: "u", PasswordCommand: &cmd}))
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatal("the command must not run when the source is created")
	}
	for i := 1; i <= 2; i++ {
		if _, err := am.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, pw, _ := srv.last(t).BasicAuth()
		if want := "syn-pw-" + strconv.Itoa(i); pw != want {
			t.Errorf("password = %q, want %q", pw, want)
		}
	}
}

func TestCredentialFailuresSendNoRequest(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failing := "echo " + cmdOutput + "; echo " + cmdOutput + " >&2; : " + cmdMarker + "; exit 3"
	tests := []struct {
		name string
		auth *config.Auth
		want string
	}{
		{"missing file", &config.Auth{Type: config.AuthBearer, TokenFile: ptr(filepath.Join(dir, "missing-"+cmdMarker))}, "token_file: read file"},
		{"empty file", &config.Auth{Type: config.AuthBasic, Username: "u", PasswordFile: ptr(empty)}, "password_file: file is empty"},
		{"failing command", &config.Auth{Type: config.AuthBearer, TokenCommand: ptr(failing)}, "token_command: command failed with exit status 3"},
		{"empty output", &config.Auth{Type: config.AuthBasic, Username: "u", PasswordCommand: ptr(": " + cmdMarker + "; printf ' \\n'")}, "password_command: command produced empty output"},
		{"killed command", &config.Auth{Type: config.AuthBearer, TokenCommand: ptr("echo " + cmdOutput + "; kill -9 $$")}, "token_command: command terminated by signal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeAM(t, nil)
			_, err := NewAlertmanager(plainSource(srv.URL, tt.auth)).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if n := srv.count(); n != 0 {
				t.Errorf("%d requests sent", n)
			}
			assertNoLeak(t, err.Error(), cmdMarker, cmdOutput, dir)
		})
	}
}

func TestHTTPStatusErrors(t *testing.T) {
	for _, tt := range []struct {
		status int
		hint   bool
	}{
		{http.StatusInternalServerError, false},
		{http.StatusNotFound, false},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
	} {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			srv := newFakeAM(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				// A valid snapshot body does not turn a failure into success.
				_, _ = w.Write([]byte(`[{"fingerprint":"abc","labels":{"alertname":"A"},"status":{"state":"active"}}]`))
			})
			alerts, err := NewAlertmanager(plainSource(srv.URL, nil)).Fetch(context.Background())
			if err == nil || alerts != nil {
				t.Fatalf("Fetch = %v, %v; want failure", alerts, err)
			}
			if !strings.HasPrefix(err.Error(), "HTTP "+strconv.Itoa(tt.status)) {
				t.Errorf("err = %v", err)
			}
			if got := strings.Contains(err.Error(), "credentials rejected (token expired?)"); got != tt.hint {
				t.Errorf("hint present = %v, want %v: %v", got, tt.hint, err)
			}
		})
	}
}

func TestMalformedResponsesFail(t *testing.T) {
	for _, body := range []string{``, `{`, `{}`, `null`, `[{"fingerprint": "abc"`, `[1]`, `[{"fingerprint": "../x"}]`} {
		t.Run(body, func(t *testing.T) {
			srv := newFakeAM(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			alerts, err := NewAlertmanager(plainSource(srv.URL, nil)).Fetch(context.Background())
			if err == nil || alerts != nil {
				t.Fatalf("Fetch = %v, %v; want failure", alerts, err)
			}
			if !strings.HasPrefix(err.Error(), "HTTP 200: ") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestSnapshotDecoded(t *testing.T) {
	srv := newFakeAM(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sample)) })
	alerts, err := NewAlertmanager(plainSource(srv.URL, nil)).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Unprocessed alerts are kept so that reconciliation treats them as neutral.
	if len(alerts) != 5 || alerts[0].Source != "am" || !alerts[2].Unprocessed() {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestDiagnosticsRedactSecrets(t *testing.T) {
	const tail = "SYN-TAIL-BEYOND-EXCERPT"
	basicValue := base64.StdEncoding.EncodeToString([]byte("tower:" + testPassword))
	for _, tt := range []struct {
		name     string
		auth     *config.Auth
		status   int
		straddle bool
		secrets  []string
	}{
		{"bearer 401", &config.Auth{Type: config.AuthBearer, Token: ptr(testToken)}, http.StatusUnauthorized, false, []string{testToken}},
		{"bearer 500 straddling", &config.Auth{Type: config.AuthBearer, TokenCommand: ptr("echo " + testToken)}, http.StatusInternalServerError, true, []string{testToken}},
		{"basic 403", &config.Auth{Type: config.AuthBasic, Username: "tower", Password: ptr(testPassword)}, http.StatusForbidden, true, []string{testPassword, basicValue}},
		{"basic malformed 200", &config.Auth{Type: config.AuthBasic, Username: "tower", PasswordCommand: ptr("echo " + testPassword)}, http.StatusOK, true, []string{testPassword, basicValue}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeAM(t, func(w http.ResponseWriter, r *http.Request) {
				body := "echo: " + r.Header.Get("Authorization") + " | " + strings.Join(tt.secrets, " | ") + " | " +
					strings.Trim(strconv.Quote(tt.secrets[0]), `"`) + " "
				if tt.straddle {
					// Put a secret across the 512-byte excerpt boundary.
					body += strings.Repeat("x", ExcerptBytes-len(body)-5) + tt.secrets[0]
				}
				body += strings.Repeat("y", ExcerptBytes) + tail
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(body)) // #nosec G705 -- test server echoing synthetic values
			})
			_, err := NewAlertmanager(plainSource(srv.URL, tt.auth)).Fetch(context.Background())
			if err == nil {
				t.Fatal("want error")
			}
			msg := err.Error()
			if !strings.Contains(msg, redacted) || !strings.Contains(msg, "(truncated)") {
				t.Errorf("err = %s", msg)
			}
			assertNoLeak(t, msg, append([]string{tail, "Bearer " + testToken, "Basic " + basicValue}, tt.secrets...)...)
			// No prefix of the secret cut by the boundary survives either.
			if tt.straddle {
				assertNoLeak(t, msg, "x"+tt.secrets[0][:5])
			}
		})
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt(nil, nil); got != "" {
		t.Errorf("empty body excerpt = %q", got)
	}
	if got := excerpt([]byte{0xff, 0xfe, 'a'}, nil); strings.Contains(got, "a") || !strings.Contains(got, "omitted") {
		t.Errorf("binary excerpt = %q", got)
	}
	// A multi-byte rune cut at the boundary is dropped, not garbled.
	body := strings.Repeat("a", ExcerptBytes-1) + "é" + "rest"
	got := excerpt([]byte(body), nil)
	if !strings.Contains(got, strings.Repeat("a", ExcerptBytes-1)+`"`) || strings.Contains(got, "rest") {
		t.Errorf("rune boundary excerpt = %q", got)
	}
	if got := excerpt([]byte("short"), nil); got != `: body "short"` {
		t.Errorf("short excerpt = %q", got)
	}
}

func TestTransportErrorOmitsURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := strings.Replace(srv.URL, "http://", "http://syn-user:"+testPassword+"@", 1)
	srv.Close()
	_, err := NewAlertmanager(plainSource(u, nil)).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("err = %v", err)
	}
	assertNoLeak(t, err.Error(), "syn-user", testPassword, "http://")

	_, err = NewAlertmanager(plainSource("::syn-user:"+testPassword+"@bad", nil)).Fetch(context.Background())
	if err == nil {
		t.Fatal("want invalid url error")
	}
	assertNoLeak(t, err.Error(), "syn-user", testPassword)
}

func TestProductionBudget(t *testing.T) {
	if FetchTimeout != 15*time.Second {
		t.Fatalf("FetchTimeout = %s", FetchTimeout)
	}
	if CredentialWaitDelay <= 0 || CredentialWaitDelay > time.Second {
		t.Fatalf("CredentialWaitDelay = %s", CredentialWaitDelay)
	}
	am := NewAlertmanager(plainSource("http://am.invalid", &config.Auth{Type: config.AuthBearer, TokenCommand: ptr("sleep 0.3; echo " + testToken)}))
	if am.timeout != FetchTimeout {
		t.Fatalf("timeout = %s", am.timeout)
	}
	// The HTTP stage receives what is left of the same 15s budget after the
	// credential command, not a fresh one. No live server is involved.
	var remaining time.Duration
	am.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			return nil, errors.New("no deadline")
		}
		remaining = time.Until(deadline)
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	})}
	_, _ = am.Fetch(context.Background())
	if remaining <= 0 || remaining > FetchTimeout-300*time.Millisecond {
		t.Errorf("remaining HTTP budget = %s, want < %s", remaining, FetchTimeout-300*time.Millisecond)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// blockingAM blocks every request until the request is cancelled or release
// is closed.
func blockingAM(t *testing.T) (*fakeAM, chan struct{}) {
	release := make(chan struct{})
	srv := newFakeAM(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
			_, _ = w.Write([]byte("[]"))
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return srv, release
}

func TestSharedBudgetAcrossStages(t *testing.T) {
	srv, _ := blockingAM(t)
	am := NewAlertmanager(plainSource(srv.URL, &config.Auth{Type: config.AuthBearer, TokenCommand: ptr("sleep 0.5; echo " + testToken)}))
	am.timeout = time.Second
	start := time.Now()
	_, err := am.Fetch(context.Background())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out while requesting alerts") {
		t.Fatalf("err = %v", err)
	}
	if srv.count() != 1 {
		t.Errorf("requests = %d", srv.count())
	}
	// Two sequential budgets would take at least 1.5s.
	if elapsed > 1400*time.Millisecond {
		t.Errorf("elapsed = %s, want about one budget", elapsed)
	}
}

func TestCredentialStageExpirySendsNoRequest(t *testing.T) {
	srv := newFakeAM(t, nil)
	pidFile := filepath.Join(t.TempDir(), "pid")
	am := NewAlertmanager(plainSource(srv.URL, &config.Auth{Type: config.AuthBearer, TokenCommand: ptr("echo $$ > " + pidFile + "; sleep 30; echo " + testToken)}))
	am.timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := am.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out while resolving credentials") {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("elapsed = %s", elapsed)
	}
	if srv.count() != 0 {
		t.Errorf("requests = %d", srv.count())
	}
	assertDead(t, readPID(t, pidFile))
}

func TestCallerDeadlineAndCancellation(t *testing.T) {
	srv, _ := blockingAM(t)
	am := NewAlertmanager(plainSource(srv.URL, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := am.Fetch(ctx)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start = time.Now()
	_, err = am.Fetch(ctx)
	if err == nil || !strings.Contains(err.Error(), "cancelled while requesting alerts") || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	eventuallyTrue(t, func() bool {
		data, err := os.ReadFile(path) // #nosec G304 -- test file
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	})
	return pid
}

func assertDead(t *testing.T, pid int) {
	t.Helper()
	eventuallyTrue(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

func eventuallyTrue(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCredentialCommandProcessGroup(t *testing.T) {
	seen := map[string]bool{}
	for range 2 {
		out, err := RunCredentialCommand(context.Background(), `echo $$ $(ps -o pgid= -p $$)`)
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(out)
		if len(f) != 2 {
			t.Fatalf("output = %q", out)
		}
		if f[0] != f[1] {
			t.Errorf("pgid %s != shell pid %s", f[1], f[0])
		}
		if f[1] == strconv.Itoa(syscall.Getpgrp()) {
			t.Error("the command runs in tower's process group")
		}
		if seen[f[1]] {
			t.Error("process group reused across invocations")
		}
		seen[f[1]] = true
	}
}

func TestCredentialCommandGroupKilled(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			shellPID, childPID := filepath.Join(dir, "shell"), filepath.Join(dir, "child")
			cmd := "sleep 30 & echo $! > " + childPID + "; echo $$ > " + shellPID + "; wait"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "timeout" {
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			} else {
				go func() {
					readPID(t, childPID)
					cancel()
				}()
			}
			start := time.Now()
			_, err := RunCredentialCommand(ctx, cmd)
			if err == nil {
				t.Fatal("want error")
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("elapsed = %s", elapsed)
			}
			assertDead(t, readPID(t, shellPID))
			assertDead(t, readPID(t, childPID))
		})
	}
}

func TestCredentialCommandInheritedPipe(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child")
	// The shell exits at once, but its background child keeps stdout open.
	start := time.Now()
	_, err := RunCredentialCommand(context.Background(), "sleep 30 & echo $! > "+childPID+"; echo "+testToken)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "background processes") {
		t.Fatalf("err = %v", err)
	}
	assertNoLeak(t, err.Error(), testToken)
	if elapsed > CredentialWaitDelay+2*time.Second {
		t.Errorf("elapsed = %s, want bounded by WaitDelay", elapsed)
	}
	assertDead(t, readPID(t, childPID))
}

func TestConcurrentFetchesIndependent(t *testing.T) {
	var started atomic.Int32
	srv, release := blockingAM(t)
	fast := newFakeAM(t, nil)
	slowHTTP := NewAlertmanager(plainSource(srv.URL, nil))
	slowCred := NewAlertmanager(plainSource(fast.URL, &config.Auth{Type: config.AuthBearer, TokenCommand: ptr("sleep 30")}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	for _, s := range []*Alertmanager{slowHTTP, slowCred} {
		go func() {
			started.Add(1)
			_, err := s.Fetch(ctx)
			done <- err
		}()
	}
	eventuallyTrue(t, func() bool { return srv.count() == 1 })
	if _, err := NewAlertmanager(plainSource(fast.URL, nil)).Fetch(context.Background()); err != nil {
		t.Fatalf("fast source blocked: %v", err)
	}
	cancel()
	close(release)
	for range 2 {
		<-done
	}
	if started.Load() != 2 {
		t.Fatal("not all fetches started")
	}
}

func TestURLUserinfoNeverSentOrLeaked(t *testing.T) {
	const user = "syn-url-user"
	basicValue := base64.StdEncoding.EncodeToString([]byte(user + ":" + testPassword))
	for _, tt := range []struct {
		name string
		auth *config.Auth
		want string
	}{
		{"omitted auth", nil, ""},
		{"explicit none", &config.Auth{Type: config.AuthNone}, ""},
		{"bearer", &config.Auth{Type: config.AuthBearer, Token: ptr(testToken)}, "Bearer " + testToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeAM(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				// Echo the header and the URL login details in every form.
				_, _ = w.Write([]byte("auth=" + r.Header.Get("Authorization") + " user=" + user + " pw=" + testPassword + " basic=" + basicValue)) // #nosec G705 -- test server echoing synthetic values
			})
			u := strings.Replace(srv.URL, "http://", "http://"+user+":"+testPassword+"@", 1)
			_, err := NewAlertmanager(plainSource(u, tt.auth)).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), RejectedHint) {
				t.Fatalf("err = %v", err)
			}
			if got := srv.last(t).Header.Get("Authorization"); got != tt.want {
				t.Errorf("Authorization = %q, want %q", got, tt.want)
			}
			assertNoLeak(t, err.Error(), user, testPassword, basicValue, testToken)
		})
	}
}

func TestDecodeErrorsReportNoResponseContent(t *testing.T) {
	const marker = "SYN-BAD-TIME-MARKER"
	label := strings.Repeat("l", 600)
	for _, tt := range []struct {
		name, body, want string
	}{
		{"timestamp after byte 512", `[{"fingerprint":"abc","labels":{"x":"` + label + `"},"startsAt":"` + marker + `","status":{"state":"active"}}]`, "decode alert 0: invalid timestamp"},
		{"type after byte 512", `[{"fingerprint":"abc","labels":{"x":"` + label + `","` + marker + `":1},"status":{"state":"active"}}]`, "decode alert 0: unexpected JSON value type at byte offset"},
		{"syntax after byte 512", `[{"fingerprint":"abc","labels":{"x":"` + label + `"}` + marker + `}]`, "decode alerts: invalid JSON at byte offset"},
		{"state after byte 512", `[{"fingerprint":"abc","labels":{"x":"` + label + `"},"status":{"state":"` + marker + `"}}]`, "decode alert 0: unsupported status.state"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeAM(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.body)) })
			_, err := NewAlertmanager(plainSource(srv.URL, nil)).Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "(truncated)") {
				t.Fatalf("err = %v", err)
			}
			assertNoLeak(t, err.Error(), marker)
		})
	}
}

func TestStatusKeptWhenBodyUnreadable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := newFakeAM(t, func(w http.ResponseWriter, _ *http.Request) {
				// Announce more bytes than are sent: reading fails with EOF.
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("short"))
			})
			_, err := NewAlertmanager(plainSource(srv.URL, nil)).Fetch(context.Background())
			if err == nil || !strings.HasPrefix(err.Error(), "HTTP "+strconv.Itoa(status)) || !strings.Contains(err.Error(), "request failed") {
				t.Fatalf("err = %v", err)
			}
			hint := status != http.StatusInternalServerError
			if got := strings.Contains(err.Error(), RejectedHint); got != hint {
				t.Errorf("hint present = %v, want %v: %v", got, hint, err)
			}
		})
	}
	t.Run("oversized 401", func(t *testing.T) {
		srv := newFakeAM(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			chunk := make([]byte, 1<<20)
			for range maxResponseBytes>>20 + 1 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		})
		_, err := NewAlertmanager(plainSource(srv.URL, nil)).Fetch(context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), "HTTP 401: "+RejectedHint) || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCredentialCommandCleanupAfterShellExit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		exit    string
		timeout time.Duration
		want    string
	}{
		// The shell exits at once; the budget expires while Wait still
		// drains the pipe inherited by the child.
		{"cancelled after shell exit", "exit 0", CredentialWaitDelay / 3, "deadline exceeded"},
		// A failing shell reports its exit status instead of ErrWaitDelay.
		{"failing shell with inherited stdout", "exit 3", time.Minute, "exit status 3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			childPID := filepath.Join(t.TempDir(), "child")
			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()
			_, err := RunCredentialCommand(ctx, "sleep 30 & echo $! > "+childPID+"; "+tt.exit)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v", err)
			}
			// Checked right after return: the child must already be gone.
			pid := readPID(t, childPID)
			assertDead(t, pid)
		})
	}
}
