package prep

import (
	"strings"
	"testing"

	"github.com/ricoberger/tower/internal/result"
)

func sampleData() Data {
	return Data{
		RunDir:        "/s/items/src-0123456789abcdef/runs/2",
		AlertFile:     "/s/items/src-0123456789abcdef/alert.md",
		AlertMarkdown: "# Alert\n\n## Summary\n\nsomething broke\n",
		SourceName:    "src",
		Fingerprint:   "0123456789abcdef",
		ResultSchema:  result.Schema,
	}
}

func TestDefaultTemplateContract(t *testing.T) {
	if err := Validate(DefaultText); err != nil {
		t.Fatalf("default template invalid: %v", err)
	}
	out, err := Render(Default(), sampleData())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"You are running unattended as a preparation run for the user's work harness\n\"tower\".",
		"Task: investigate the alert below using the `sre-analyze-alert` skill.",
		"1. Follow the skill exactly as written. Do not skip phases.",
		"2. This run is strictly READ-ONLY. Do not change anything anywhere: no\n   kubectl/helm/flux mutations, no silences, no git push, no GitHub or Jira\n   writes, no Grafana writes, no file changes outside the run directory below.",
		"3. Stop at the skill's FIRST point where it would ask the user anything",
		"The request to \"restate the alert so the user can confirm\" is NOT a stop\n     point in this run: write the restatement into the report instead.",
		"stop\n   with status \"blocked\" and put the exact question into \"gate.question\".",
		"   - /s/items/src-0123456789abcdef/runs/2/report.md   — the full report in the skill's report format\n",
		"   - /s/items/src-0123456789abcdef/runs/2/result.json — machine-readable summary, schema below\n",
		"   Write result.json last. Both files are mandatory, also when blocked.",
		"6. Then end the session with a one-line summary.\n\nresult.json schema:\n" + result.Schema + "\n",
		"Alert (from src, fingerprint 0123456789abcdef), also available at\n/s/items/src-0123456789abcdef/alert.md:\n\n# Alert\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(s, "Context:") || strings.Contains(s, "<no value>") {
		t.Errorf("unexpected content:\n%s", s)
	}
	if !strings.HasSuffix(s, "something broke\n") || strings.HasSuffix(s, "\n\n") {
		t.Errorf("prompt must end with alert.md verbatim: %q", s[len(s)-30:])
	}
}

func TestDefaultTemplatePreviousReports(t *testing.T) {
	d := sampleData()
	d.PreviousReports = []string{"/r/2/report.md", "/r/1/report.md", "/p/runs/4/report.md"}
	out, err := Render(Default(), d)
	if err != nil {
		t.Fatal(err)
	}
	want := "6. Then end the session with a one-line summary.\n\n" +
		"Context: this alert (same fingerprint) was investigated before. Read these\n" +
		"earlier reports first and state in your report whether the situation is the\n" +
		"same, and what changed:\n" +
		"- /r/2/report.md\n- /r/1/report.md\n- /p/runs/4/report.md\n\nresult.json schema:\n"
	if !strings.Contains(string(out), want) {
		t.Errorf("prompt =\n%s", out)
	}
}

func TestValidate(t *testing.T) {
	valid := []string{
		"",
		"static text",
		"{{.RunDir}} {{.AlertFile}} {{.AlertMarkdown}} {{.SourceName}} {{.Fingerprint}} {{.ResultSchema}}",
		"{{range $i, $r := .PreviousReports}}{{$i}} {{$r}}{{end}}{{len .PreviousReports}}",
		"{{if .PreviousReports}}{{index .PreviousReports 0}}{{end}}",
	}
	for _, text := range valid {
		if err := Validate(text); err != nil {
			t.Errorf("Validate(%q): %v", text, err)
		}
	}
	invalid := map[string]string{
		"unclosed action":           "{{.RunDir",
		"unknown function":          "{{shell .RunDir}}",
		"unknown field":             "{{.Nope}}",
		"field of string":           "{{.RunDir.Nope}}",
		"unknown field in range":    "{{range .PreviousReports}}{{.Path}}{{end}}",
		"unknown field in if":       "{{if .PreviousReports}}{{.Secret}}{{end}}",
		"index beyond populated":    "{{if .PreviousReports}}{{index .PreviousReports 7}}{{end}}",
		"index on empty":            "{{index .PreviousReports 0}}",
		"wrong type in conditional": "{{if .PreviousReports}}{{range .Fingerprint}}{{end}}{{end}}",
		"unterminated range":        "{{range .PreviousReports}}",
	}
	for name, text := range invalid {
		if err := Validate(text); err == nil {
			t.Errorf("%s: Validate(%q) accepted", name, text)
		}
	}
}

func TestRenderError(t *testing.T) {
	tmpl, err := Parse("{{index .PreviousReports 2}}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render(tmpl, sampleData()); err == nil || !strings.Contains(err.Error(), "render prompt") {
		t.Fatalf("err = %v", err)
	}
}
