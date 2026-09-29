package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `[
  {
    "fingerprint": "abc123",
    "labels": {"alertname": "A", "severity": "critical"},
    "annotations": {"runbook_url": "https://rb"},
    "startsAt": "2026-01-01T00:00:00Z",
    "updatedAt": "2026-01-01T00:01:00Z",
    "endsAt": "2026-01-01T01:00:00Z",
    "generatorURL": "https://gen",
    "status": {"state": "active", "silencedBy": [], "inhibitedBy": []},
    "receivers": [{"name": "team"}]
  },
  {"fingerprint": "def", "status": {"state": "suppressed", "silencedBy": ["s1"]}},
  {"fingerprint": "ghi", "status": {"state": "unprocessed"}},
  {"fingerprint": "jkl", "status": {"state": "active", "inhibitedBy": ["x"]}},
  {"fingerprint": "mno", "status": {"state": "unprocessed", "silencedBy": ["s1"], "inhibitedBy": ["x"]}}
]`

func TestDecodeSnapshot(t *testing.T) {
	alerts, err := DecodeSnapshot("dev", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 5 {
		t.Fatalf("len = %d", len(alerts))
	}
	a := alerts[0]
	if a.Source != "dev" || a.Fingerprint != "abc123" || a.Labels["severity"] != "critical" ||
		a.Annotations["runbook_url"] != "https://rb" || a.GeneratorURL != "https://gen" ||
		a.StartsAt.IsZero() || a.UpdatedAt.IsZero() || a.EndsAt.IsZero() ||
		len(a.Receivers) != 1 || a.Receivers[0] != "team" || !a.Active() {
		t.Errorf("alert 0 = %+v", a)
	}
	if !strings.HasPrefix(string(a.Raw), "{") || !strings.Contains(string(a.Raw), `"receivers"`) {
		t.Errorf("raw = %s", a.Raw)
	}
	if !alerts[1].Suppressed() || alerts[1].Active() {
		t.Error("silenced alert must be suppressed")
	}
	if !alerts[2].Unprocessed() || alerts[2].Active() || alerts[2].Suppressed() {
		t.Error("unprocessed alert must be neutral")
	}
	if !alerts[3].Suppressed() {
		t.Error("inhibited alert must be suppressed even if state says active")
	}
	if !alerts[4].Unprocessed() || alerts[4].Active() || alerts[4].Suppressed() {
		t.Error("unprocessed state must take precedence over silencedBy/inhibitedBy")
	}
	if alerts[1].Labels == nil || alerts[1].Annotations == nil {
		t.Error("labels/annotations must be non-nil")
	}
}

func TestDecodeSnapshotEmpty(t *testing.T) {
	alerts, err := DecodeSnapshot("dev", []byte(" [] "))
	if err != nil || alerts == nil || len(alerts) != 0 {
		t.Fatalf("empty array must be an empty snapshot: %v %v", alerts, err)
	}
}

func TestDecodeSnapshotErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not json":         "{",
		"object":           `{"a": 1}`,
		"null":             "null",
		"empty":            "",
		"no fingerprint":   `[{"status": {"state": "active"}}]`,
		"bad fingerprint":  `[{"fingerprint": "../x", "status": {"state": "active"}}]`,
		"long fingerprint": `[{"fingerprint": "` + strings.Repeat("a", 129) + `", "status": {"state": "active"}}]`,
		"bad state":        `[{"fingerprint": "a", "status": {"state": "firing"}}]`,
		"missing state":    `[{"fingerprint": "a"}]`,
		"duplicate":        `[{"fingerprint": "a", "status": {"state": "active"}}, {"fingerprint": "a", "status": {"state": "active"}}]`,
		"bad element":      `[1]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSnapshot("dev", []byte(in)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestValidFingerprint(t *testing.T) {
	for fp, want := range map[string]bool{
		"a": true, "ABCdef0123": true, strings.Repeat("f", 128): true,
		"": false, strings.Repeat("f", 129): false, "a-b": false, "a/b": false, "..": false, "a b": false, "ä": false,
	} {
		if got := ValidFingerprint(fp); got != want {
			t.Errorf("ValidFingerprint(%q) = %v", fp, got)
		}
	}
}

func TestFileFetchRereads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	src := NewFile("dev", path)
	if src.Name() != "dev" {
		t.Fatal(src.Name())
	}
	ctx := context.Background()
	if _, err := src.Fetch(ctx); err == nil {
		t.Fatal("missing file must be an error, not an empty snapshot")
	}
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	alerts, err := src.Fetch(ctx)
	if err != nil || len(alerts) != 5 {
		t.Fatalf("fetch: %v %d", err, len(alerts))
	}
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	alerts, err = src.Fetch(ctx)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("fetch after change: %v %d", err, len(alerts))
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fetch(ctx); err == nil {
		t.Fatal("malformed file must be an error")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := src.Fetch(cancelled); err == nil {
		t.Fatal("cancelled context must be an error")
	}
}
