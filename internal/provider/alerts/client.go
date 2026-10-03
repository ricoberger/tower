// Package alerts polls Grafana-managed Alertmanagers and turns their alerts
// into items.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Alert is one alert of a source snapshot.
type Alert struct {
	Fingerprint  string            `json:"fingerprint"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Status       struct {
		State string `json:"state"`
	} `json:"status"`
	Receivers []struct {
		Name string `json:"name"`
	} `json:"receivers"`
}

var fingerprintPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

// Decode decodes a GET /api/v2/alerts response. Unprocessed alerts are
// skipped; silenced and inhibited alerts are kept.
func Decode(data []byte) ([]Alert, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil || raws == nil {
		return nil, errors.New("response is not a JSON array of alerts")
	}
	out := make([]Alert, 0, len(raws))
	for i, raw := range raws {
		var a Alert
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("alert %d: invalid alert", i)
		}
		if !fingerprintPattern.MatchString(a.Fingerprint) {
			return nil, fmt.Errorf("alert %d: missing or invalid fingerprint", i)
		}
		if a.Status.State == "unprocessed" {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Client fetches the alerts of one source.
type Client struct {
	source SourceConfig
	http   *http.Client
}

// NewClient creates the client of a configured source.
func NewClient(s SourceConfig) *Client {
	return &Client{source: s, http: &http.Client{}}
}

// Name returns the source name.
func (c *Client) Name() string {
	return c.source.Name
}

// Fetch returns the complete current snapshot.
func (c *Client) Fetch(ctx context.Context) ([]Alert, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	endpoint, err := c.endpoint()
	if err != nil {
		return nil, err
	}
	token, err := RunTokenCommand(ctx, c.source.TokenCommand)
	if err != nil {
		return nil, fmt.Errorf("token command: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("invalid request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := "HTTP " + strconv.Itoa(resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += " (token expired?)"
		}
		return nil, errors.New(msg)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return Decode(body)
}

func (c *Client) endpoint() (string, error) {
	u, err := url.Parse(c.source.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid Grafana url")
	}
	u.User = nil
	u = u.JoinPath("api", "alertmanager", url.PathEscape(c.source.GrafanaAlertmanager), "api", "v2", "alerts")
	q := url.Values{}
	q.Set("active", "true")
	q.Set("silenced", "true")
	q.Set("inhibited", "true")
	q.Set("unprocessed", "false")
	for _, f := range c.source.Filter {
		q.Add("filter", f)
	}
	if c.source.Receiver != "" {
		q.Set("receiver", c.source.Receiver)
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}

// RunTokenCommand runs command with sh -c in its own process group and
// returns its trimmed stdout. The whole group is killed when ctx ends.
// Errors never contain the command or its output.
func RunTokenCommand(ctx context.Context, command string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", command) // #nosec G204 -- configured by the user
	cmd.Stdout = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return "", errors.New("timed out")
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("exit status %d", ee.ExitCode())
		}
		return "", errors.New("failed")
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" || strings.ContainsAny(tok, "\r\n") {
		return "", errors.New("output must be a single non-empty line")
	}
	return tok, nil
}
