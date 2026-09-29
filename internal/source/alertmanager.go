package source

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ricoberger/tower/internal/config"
)

const (
	// FetchTimeout is the end-to-end budget of one source poll, including
	// credential resolution, the HTTP request and reading the response.
	FetchTimeout = 15 * time.Second
	// CredentialWaitDelay bounds how long a credential command's process and
	// output pipe may keep Wait busy after cancellation or exit. It is a
	// cleanup safeguard, not an additional execution budget.
	CredentialWaitDelay = 250 * time.Millisecond
	// ExcerptBytes is the maximum number of response bytes a diagnostic
	// excerpt is drawn from.
	ExcerptBytes = 512
	// RejectedHint is added to the source error of HTTP 401 and 403
	// responses.
	RejectedHint = "credentials rejected (token expired?)"

	maxResponseBytes = 64 << 20
	maxCommandOutput = 1 << 20
	redacted         = "[REDACTED]"
)

// Alertmanager polls the GET /api/v2/alerts API of a plain or Grafana-managed
// Alertmanager.
type Alertmanager struct {
	name     string
	baseURL  string
	managed  string
	filter   []string
	receiver string
	auth     *config.Auth

	// client and timeout are replaced in tests.
	client  *http.Client
	timeout time.Duration
}

// NewAlertmanager creates the source of a configured alertmanager source,
// using its effective polling URL and auth (see config.Source.PollURL and
// config.Source.PollAuth). Credentials are only resolved during Fetch.
func NewAlertmanager(s config.Source) *Alertmanager {
	return &Alertmanager{
		name:     s.Name,
		baseURL:  s.PollURL(),
		managed:  s.GrafanaAlertmanager,
		filter:   append([]string(nil), s.Filter...),
		receiver: s.Receiver,
		auth:     s.PollAuth(),
		client:   &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		timeout:  FetchTimeout,
	}
}

// Name returns the configured source name.
func (a *Alertmanager) Name() string { return a.name }

// Fetch resolves the credentials and requests the full alert snapshot within
// one end-to-end budget. Every failure is returned as an error that never
// contains credentials; unprocessed alerts are kept in the snapshot so that
// they stay neutral during reconciliation.
func (a *Alertmanager) Fetch(ctx context.Context) ([]Alert, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	endpoint, err := a.endpoint()
	if err != nil {
		return nil, err
	}

	header, secrets, err := a.authorization(ctx)
	if err != nil {
		return nil, a.stageError(ctx, "resolving credentials", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, a.stageError(ctx, "resolving credentials", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("build request: invalid url")
	}
	req.Header.Set("Accept", "application/json")
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, a.stageError(ctx, "requesting alerts", redactErr(transportError(err), secrets))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, a.stageError(ctx, "reading the response", redactErr(transportError(err), secrets))
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("HTTP %d: response exceeds %d bytes", resp.StatusCode, maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := "HTTP " + strconv.Itoa(resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += ": " + RejectedHint
		}
		return nil, errors.New(msg + excerpt(body, secrets))
	}
	alerts, err := DecodeSnapshot(a.name, body)
	if err != nil {
		return nil, fmt.Errorf("HTTP %d: %s%s", resp.StatusCode, redact(err.Error(), secrets), excerpt(body, secrets))
	}
	return alerts, nil
}

// endpoint builds the request URL. Errors never include the configured URL,
// which may contain userinfo.
func (a *Alertmanager) endpoint() (string, error) {
	if a.baseURL == "" {
		return "", errors.New("no polling url configured")
	}
	u, err := url.Parse(a.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "", errors.New("invalid polling url (expected an absolute http(s) URL)")
	}
	if a.managed != "" {
		u = u.JoinPath("api", "alertmanager", url.PathEscape(a.managed), "api", "v2", "alerts")
	} else {
		u = u.JoinPath("api", "v2", "alerts")
	}
	q := url.Values{}
	q.Set("active", "true")
	q.Set("silenced", "true")
	q.Set("inhibited", "true")
	q.Set("unprocessed", "false")
	for _, f := range a.filter {
		q.Add("filter", f)
	}
	if a.receiver != "" {
		q.Set("receiver", a.receiver)
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

// stageError reports budget expiry and cancellation as such, independent of
// the stage's own error.
func (a *Alertmanager) stageError(ctx context.Context, stage string, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("source timed out while %s (poll budget %s or an earlier deadline)", stage, a.timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		return fmt.Errorf("source poll cancelled while %s", stage)
	}
	return err
}

// authorization resolves the Authorization header value and the values that
// must never appear in diagnostics.
func (a *Alertmanager) authorization(ctx context.Context) (string, []string, error) {
	if a.auth == nil {
		return "", nil, nil
	}
	switch a.auth.Type {
	case config.AuthNone:
		return "", nil, nil
	case config.AuthBearer:
		tok, err := credential(ctx, "token", a.auth.Token, a.auth.TokenFile, a.auth.TokenCommand)
		if err != nil {
			return "", nil, err
		}
		h := "Bearer " + tok
		return h, []string{tok, h}, nil
	case config.AuthBasic:
		pw, err := credential(ctx, "password", a.auth.Password, a.auth.PasswordFile, a.auth.PasswordCommand)
		if err != nil {
			return "", nil, err
		}
		enc := base64.StdEncoding.EncodeToString([]byte(a.auth.Username + ":" + pw))
		h := "Basic " + enc
		return h, []string{pw, enc, h}, nil
	}
	return "", nil, errors.New("unsupported auth type")
}

// credential resolves exactly one configured credential variant. Direct
// values are used unchanged; file contents and command output are trimmed
// and must not be empty. Errors name the variant, never its value, path
// contents, command text or output.
func credential(ctx context.Context, kind string, direct, file, command *string) (string, error) {
	switch {
	case direct != nil:
		return *direct, nil
	case file != nil:
		v, err := readCredentialFile(ctx, *file)
		if err != nil {
			return "", fmt.Errorf("%s_file: %w", kind, err)
		}
		if v = strings.TrimSpace(v); v == "" {
			return "", fmt.Errorf("%s_file: file is empty", kind)
		}
		return v, nil
	case command != nil:
		v, err := RunCredentialCommand(ctx, *command)
		if err != nil {
			return "", fmt.Errorf("%s_command: %w", kind, err)
		}
		return v, nil
	}
	return "", fmt.Errorf("no %s configured", kind)
}

// readCredentialFile reads a credential file anew, respecting ctx.
func readCredentialFile(ctx context.Context, path string) (string, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(path) // #nosec G304 -- path comes from the user's configuration
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			var pe *fs.PathError
			if errors.As(r.err, &pe) {
				return "", fmt.Errorf("read file: %w", pe.Err)
			}
			return "", errors.New("read file failed")
		}
		return string(r.data), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// RunCredentialCommand runs command verbatim with sh -c in its own process
// group and returns its trimmed stdout. Stderr is discarded. On cancellation
// or deadline expiry the whole process group is killed, and CredentialWaitDelay
// bounds waiting for processes or inherited output pipes. Errors never
// include the command text or its output.
func RunCredentialCommand(ctx context.Context, command string) (string, error) {
	var out limitedBuffer
	cmd := exec.CommandContext(ctx, "sh", "-c", command) // #nosec G204 -- the credential command is configured by the user
	cmd.Stdout = &out
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = CredentialWaitDelay

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("command could not be started")
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		switch {
		case errors.Is(err, exec.ErrWaitDelay):
			// The shell exited but descendants kept its output open: the
			// output is ambiguous and the leftovers are killed.
			_ = killGroup(cmd)
			return "", errors.New("command left background processes holding its output")
		case errors.As(err, &ee):
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return "", fmt.Errorf("command terminated by signal %d", ws.Signal())
			}
			return "", fmt.Errorf("command failed with exit status %d", ee.ExitCode())
		default:
			return "", errors.New("command failed")
		}
	}
	if out.overflow {
		return "", errors.New("command output too large")
	}
	v := strings.TrimSpace(out.buf.String())
	if v == "" {
		return "", errors.New("command produced empty output")
	}
	return v, nil
}

// killGroup kills the credential command's whole process group.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// limitedBuffer collects up to maxCommandOutput bytes and discards the rest.
type limitedBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxCommandOutput - b.buf.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// transportError removes the request URL (which may contain userinfo or
// be long) from HTTP client errors.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("request failed: %w", err)
}

// secretVariants returns the forms in which a secret may be echoed.
func secretVariants(secrets []string) []string {
	var out []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		out = append(out, s, url.QueryEscape(s), url.PathEscape(s), strings.Trim(strconv.Quote(s), `"`))
	}
	return out
}

func redact(s string, secrets []string) string {
	for _, v := range secretVariants(secrets) {
		s = strings.ReplaceAll(s, v, redacted)
	}
	return s
}

func redactErr(err error, secrets []string) error {
	return errors.New(redact(err.Error(), secrets))
}

// excerpt returns a quoted diagnostic excerpt of at most the first
// ExcerptBytes response bytes, prefixed with ": body ". Every occurrence of a
// secret that overlaps the excerpt is replaced, including one cut by the
// excerpt boundary. The excerpt is omitted when it is empty or not valid
// UTF-8 text.
func excerpt(body []byte, secrets []string) string {
	n := min(len(body), ExcerptBytes)
	if n == 0 {
		return ""
	}
	masked := make([]bool, n)
	for _, v := range secretVariants(secrets) {
		sb := []byte(v)
		for off := 0; off < n; {
			i := bytes.Index(body[off:], sb)
			if i < 0 || off+i >= n {
				break
			}
			start := off + i
			for j := start; j < min(start+len(sb), n); j++ {
				masked[j] = true
			}
			off = start + 1
		}
	}
	// Drop an incomplete trailing rune cut by the boundary.
	cut := n
	for cut > 0 && n < len(body) && !utf8.Valid(body[:cut]) && n-cut < utf8.UTFMax {
		cut--
	}
	if !utf8.Valid(body[:cut]) {
		return ": body excerpt omitted (not UTF-8 text)"
	}
	var b strings.Builder
	for i := 0; i < cut; {
		if masked[i] {
			b.WriteString(redacted)
			for i < cut && masked[i] {
				i++
			}
			continue
		}
		b.WriteByte(body[i])
		i++
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return ""
	}
	suffix := ""
	if n < len(body) {
		suffix = " (truncated)"
	}
	return ": body " + strconv.Quote(text) + suffix
}
