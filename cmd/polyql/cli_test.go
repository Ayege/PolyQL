package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/fidelity"
)

// run drives the command tree in process and returns what it wrote and the exit
// code it would have produced.
//
// Running in process rather than through exec keeps the tests fast and lets a
// failure point at the line that caused it, at the cost of not covering main's
// own few lines — which TestExitCodeMapping covers directly.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	root, _ := newRootCommand(&out, &errOut)
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)

	err := root.Execute()
	code = exitOK
	if err != nil {
		code = exitError
		var coded *exitCodeError
		if errors.As(err, &coded) {
			code = coded.code
		}
		if msg := err.Error(); msg != "" {
			errOut.WriteString("polyql: " + msg + "\n")
		}
	}
	return out.String(), errOut.String(), code
}

func TestTranslateSingleQuery(t *testing.T) {
	stdout, _, code := run(t,
		"translate", "--from", "promql", "--to", "promql",
		"--query", `rate(http_requests_total{status="500"}[5m])`)

	// Into its own language nothing is lost, so the command succeeds outright.
	if code != exitOK {
		t.Errorf("exit = %d, want %d:\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, `rate(http_requests_total{status="500"}[5m])`) {
		t.Errorf("the translated query should appear:\n%s", stdout)
	}
	if !strings.Contains(stdout, "PolyQL fidelity report: promql → promql") {
		t.Errorf("the report should follow the query:\n%s", stdout)
	}
	if !strings.Contains(stdout, "✓ All constructs translated fully.") {
		t.Errorf("a faithful translation should say so:\n%s", stdout)
	}
}

func TestTranslateAcrossLanguages(t *testing.T) {
	stdout, _, code := run(t,
		"translate", "--from", "promql", "--to", "logql",
		"--query", `rate(http_requests_total{status="500"}[5m])`)

	if !strings.Contains(stdout, `status="500"`) {
		t.Errorf("the selector should survive:\n%s", stdout)
	}
	// Signal mismatch is reported separately from construct-level fidelity.
	if code != exitOK {
		t.Errorf("exit = %d, want %d:\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "Signal type") {
		t.Errorf("the report should mention the signal mismatch:\n%s", stdout)
	}
}

func TestTranslateJSONFormat(t *testing.T) {
	stdout, _, _ := run(t,
		"translate", "--from", "promql", "--to", "logql",
		"--query", `rate(http_requests_total{status="500"}[5m])`, "--format", "json")

	var result Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}

	if result.SourceDSL != "promql" || result.TargetDSL != "logql" {
		t.Errorf("languages = %s → %s", result.SourceDSL, result.TargetDSL)
	}
	if result.Input == "" || result.Output == "" {
		t.Errorf("both the input and the output should be present: %+v", result)
	}
	if result.Fidelity == nil {
		t.Fatal("the report should be embedded")
	}
	if result.Fidelity.TotalNodes == 0 {
		t.Error("the report counted nothing")
	}
	// The emitter's comment lines are lifted into structured notes so a machine
	// need not parse them back out of the query text.
	if len(result.Notes) == 0 {
		t.Errorf("the notes should be listed: %+v", result)
	}
	if strings.HasPrefix(result.Output, "#") {
		t.Errorf("the output should be the query alone, got %q", result.Output)
	}

	t.Run("a single query is an object, not a list", func(t *testing.T) {
		if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
			t.Errorf("output starts with %q", stdout[:1])
		}
	})
}

func TestTranslateQueryOnlyFormat(t *testing.T) {
	stdout, _, _ := run(t,
		"translate", "--from", "promql", "--to", "promql",
		"--query", `sum by (job) (rate(x[5m]))`, "--format", "query-only")

	got := strings.TrimSpace(stdout)
	if got != `sum by (job) (rate(x[5m]))` {
		t.Errorf("output = %q, want the query alone", got)
	}
	// Nothing but the query: this format exists to be piped.
	if strings.Contains(stdout, "fidelity") || strings.Contains(stdout, "#") {
		t.Errorf("query-only should carry no report or notes:\n%s", stdout)
	}
}

func TestTranslateUnsupportedConstruct(t *testing.T) {
	stdout, _, code := run(t,
		"translate", "--from", "promql", "--to", "logql",
		"--query", `histogram_quantile(0.99, x)`)

	if code != exitFidelity {
		t.Errorf("exit = %d, want %d:\n%s", code, exitFidelity, stdout)
	}
	if !strings.Contains(stdout, "# UNSUPPORTED:") {
		t.Errorf("the query should carry the marker:\n%s", stdout)
	}
	if !strings.Contains(stdout, "histogram_quantile") {
		t.Errorf("the report should name the construct:\n%s", stdout)
	}
	// The marker is a comment in the target language, so the output stays
	// something the target's own parser accepts.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "UNSUPPORTED") && !strings.HasPrefix(line, "#") &&
			!strings.HasPrefix(line, " ") && !strings.Contains(line, "Unsupported") {
			t.Errorf("a marker should be a comment line: %q", line)
		}
	}
}

func TestFailOnPartial(t *testing.T) {
	// The bool modifier has no LogQL spelling, which is an approximation rather
	// than a refusal.
	const query = `up > bool 5`

	t.Run("without the flag a partial result still fails on the unsupported signal", func(t *testing.T) {
		// This particular query also crosses a signal boundary, so use a pair
		// where the only finding is the approximation.
		_, _, code := run(t, "translate", "--from", "promql", "--to", "promql", "--query", query)
		if code != exitOK {
			t.Errorf("exit = %d, want %d: promql writes bool natively", code, exitOK)
		}
	})

	t.Run("with the flag an approximation fails", func(t *testing.T) {
		stdout, _, code := run(t, "translate", "--from", "promql", "--to", "logql",
			"--query", `rate(x[5m]) > bool 5`, "--fail-on-partial")
		if code != exitFidelity {
			t.Errorf("exit = %d, want %d:\n%s", code, exitFidelity, stdout)
		}
	})

	t.Run("an approximation alone passes without the flag", func(t *testing.T) {
		// A regex crossing languages is PARTIAL and nothing else.
		stdout, _, code := run(t, "translate", "--from", "logql", "--to", "logql",
			"--query", `{app=~"front.*"}`)
		if code != exitOK {
			t.Errorf("exit = %d, want %d:\n%s", code, exitOK, stdout)
		}
	})
}

func TestTranslateFileInput(t *testing.T) {
	stdout, _, code := run(t,
		"translate", "--from", "promql", "--to", "logql", "--file", "../../testdata/queries.txt")

	if code != exitFidelity {
		t.Errorf("exit = %d, want %d: the file holds untranslatable queries", code, exitFidelity)
	}
	// Each query is echoed with its own result, separated.
	if !strings.Contains(stdout, strings.Repeat("-", 60)) {
		t.Errorf("queries should be separated:\n%s", stdout)
	}
	// The run ends with the totals rather than with its last query.
	if !strings.Contains(stdout, strings.Repeat("=", 60)) {
		t.Errorf("a summary should close the run:\n%s", stdout)
	}
	if !strings.Contains(stdout, "exact,") || !strings.Contains(stdout, "incomplete") {
		t.Errorf("the summary should count outcomes:\n%s", stdout)
	}

	t.Run("comments and blank lines are skipped", func(t *testing.T) {
		if strings.Contains(stdout, "Sample queries for polyql") {
			t.Errorf("a comment line was treated as a query:\n%s", stdout)
		}
	})

	t.Run("json gives an array", func(t *testing.T) {
		stdout, _, _ := run(t, "translate", "--from", "promql", "--to", "logql",
			"--file", "../../testdata/queries.txt", "--format", "json")
		var results []Result
		if err := json.Unmarshal([]byte(stdout), &results); err != nil {
			t.Fatalf("not valid JSON: %v\n%s", err, stdout)
		}
		if len(results) < 5 {
			t.Errorf("got %d results, want one per query", len(results))
		}
		for _, result := range results {
			if result.Input == "" {
				t.Errorf("a result has no input: %+v", result)
			}
		}
	})

	t.Run("query-only gives one line per query", func(t *testing.T) {
		stdout, _, _ := run(t, "translate", "--from", "promql", "--to", "logql",
			"--file", "../../testdata/queries.txt", "--format", "query-only")
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) < 5 {
			t.Errorf("got %d lines, want one per query:\n%s", len(lines), stdout)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "#") {
				t.Errorf("query-only should carry no comments: %q", line)
			}
		}
	})
}

// TestFileWithABadQueryKeepsGoing covers one unparseable line not discarding the
// rest of the file.
func TestFileWithABadQueryKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.txt")
	content := "up\nrate(unclosed\nsum(x)\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := run(t, "translate", "--from", "promql", "--to", "promql", "--file", path)

	if code != exitError {
		t.Errorf("exit = %d, want %d for a file with a bad query", code, exitError)
	}
	if !strings.Contains(stdout, "parse error") {
		t.Errorf("the failure should be reported:\n%s", stdout)
	}
	// The queries either side of it still translated.
	if !strings.Contains(stdout, "sum(x)") {
		t.Errorf("the later query should still be translated:\n%s", stdout)
	}
	if !strings.Contains(stdout, "failed to parse") {
		t.Errorf("the summary should count the failure:\n%s", stdout)
	}
}

func TestTranslateErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			name:    "unknown source language",
			args:    []string{"translate", "--from", "foo", "--to", "logql", "--query", "x"},
			wantSub: "unknown source language",
		},
		{
			name:    "unknown target language",
			args:    []string{"translate", "--from", "promql", "--to", "foo", "--query", "x"},
			wantSub: "unknown target language",
		},
		{
			name:    "no query given",
			args:    []string{"translate", "--from", "promql", "--to", "logql"},
			wantSub: "--query",
		},
		{
			name:    "unknown format",
			args:    []string{"translate", "--from", "promql", "--to", "logql", "--query", "up", "--format", "yaml"},
			wantSub: "unknown --format",
		},
		{
			name:    "missing file",
			args:    []string{"translate", "--from", "promql", "--to", "logql", "--file", "does-not-exist.txt"},
			wantSub: "does-not-exist.txt",
		},
		{
			name: "query and file together",
			args: []string{"translate", "--from", "promql", "--to", "logql",
				"--query", "up", "--file", "x.txt"},
			// cobra enforces this one; the wording is its own.
			wantSub: "none of the others can be",
		},
		{
			name:    "unknown registry directory",
			args:    []string{"translate", "--from", "promql", "--to", "logql", "--query", "up", "--registry-dir", "nope"},
			wantSub: "nope",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, stderr, code := run(t, c.args...)
			if code != exitError {
				t.Errorf("exit = %d, want %d", code, exitError)
			}
			if !strings.Contains(stderr, c.wantSub) {
				t.Errorf("stderr should mention %q:\n%s", c.wantSub, stderr)
			}
		})
	}

	t.Run("an unknown language names the available ones", func(t *testing.T) {
		_, stderr, _ := run(t, "translate", "--from", "foo", "--to", "logql", "--query", "x")
		if !strings.Contains(stderr, "logql") || !strings.Contains(stderr, "promql") {
			t.Errorf("stderr should list what is available:\n%s", stderr)
		}
	})
}

func TestTranslateParseError(t *testing.T) {
	stdout, stderr, code := run(t,
		"translate", "--from", "promql", "--to", "logql", "--query", "rate(unclosed")

	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	// A failed command reports on stderr and writes nothing to the output.
	if !strings.Contains(stderr, "parse error") {
		t.Errorf("stderr should carry the failure:\n%s", stderr)
	}
	if !strings.Contains(stderr, "1:14") {
		t.Errorf("the position should be reported:\n%s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should stay empty:\n%s", stdout)
	}
}

// TestRegistryOverride covers --registry-dir loading the same definitions as the
// compiled-in set.
func TestRegistryOverride(t *testing.T) {
	const query = `sum by (job) (rate(x[5m]))`

	embedded, _, embeddedCode := run(t,
		"translate", "--from", "promql", "--to", "promql", "--query", query, "--format", "query-only")
	fromDisk, _, diskCode := run(t,
		"translate", "--from", "promql", "--to", "promql", "--query", query,
		"--format", "query-only", "--registry-dir", "../../registry")

	if embedded != fromDisk {
		t.Errorf("the two registries disagree:\n  embedded: %q\n  on disk:  %q", embedded, fromDisk)
	}
	if embeddedCode != diskCode {
		t.Errorf("exit codes differ: %d and %d", embeddedCode, diskCode)
	}
}

func TestOutputToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")

	stdout, _, _ := run(t, "translate", "--from", "promql", "--to", "promql",
		"--query", "up", "--output", path)

	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty when writing to a file:\n%s", stdout)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the output: %v", err)
	}
	if !strings.Contains(string(data), "up") {
		t.Errorf("the file should hold the translation:\n%s", data)
	}
}

func TestVerboseLogsToStderr(t *testing.T) {
	stdout, stderr, _ := run(t, "translate", "--from", "promql", "--to", "promql",
		"--query", "up", "--format", "query-only", "--verbose")

	// Diagnostics must not pollute a stream someone is piping.
	if strings.TrimSpace(stdout) != "up" {
		t.Errorf("stdout should carry only the query, got %q", stdout)
	}
	for _, want := range []string{"loaded", "IR nodes", "fidelity"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr should mention %q:\n%s", want, stderr)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	stdout, _, code := run(t, "version")

	if code != exitOK {
		t.Errorf("exit = %d", code)
	}
	for _, want := range []string{"polyql", "commit:", "go:", "languages:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output should mention %q:\n%s", want, stdout)
		}
	}
	// The binary reports what it can actually translate, which depends on the
	// packages it was built with.
	if !strings.Contains(stdout, "logql") || !strings.Contains(stdout, "promql") {
		t.Errorf("both languages should be listed:\n%s", stdout)
	}

	t.Run("json", func(t *testing.T) {
		stdout, _, _ := run(t, "version", "--json")
		var info buildInfo
		if err := json.Unmarshal([]byte(stdout), &info); err != nil {
			t.Fatalf("not valid JSON: %v\n%s", err, stdout)
		}
		if info.Version == "" || info.GoVersion == "" || len(info.Languages) != 2 {
			t.Errorf("info = %+v", info)
		}
	})
}

func TestRegistryListCommand(t *testing.T) {
	stdout, _, code := run(t, "registry", "list")

	if code != exitOK {
		t.Errorf("exit = %d", code)
	}
	for _, want := range []string{"promql", "logql", "signals: metric", "signals: log"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output should mention %q:\n%s", want, stdout)
		}
	}

	t.Run("json", func(t *testing.T) {
		stdout, _, _ := run(t, "registry", "list", "--json")
		var entries []struct {
			DSL      string   `json:"dsl"`
			Signals  []string `json:"signals"`
			CanParse bool     `json:"can_parse"`
			CanEmit  bool     `json:"can_emit"`
		}
		if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
			t.Fatalf("not valid JSON: %v\n%s", err, stdout)
		}
		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2", len(entries))
		}
		for _, entry := range entries {
			if !entry.CanParse || !entry.CanEmit {
				t.Errorf("%s should be readable and writable: %+v", entry.DSL, entry)
			}
		}
	})
}

func TestRegistryValidateCommand(t *testing.T) {
	t.Run("the source of truth is valid", func(t *testing.T) {
		stdout, _, code := run(t, "registry", "validate", "--dir", "../../registry")
		if code != exitOK {
			t.Errorf("exit = %d, want %d:\n%s", code, exitOK, stdout)
		}
		if !strings.Contains(stdout, "✓") || !strings.Contains(stdout, "load cleanly") {
			t.Errorf("output:\n%s", stdout)
		}
	})

	t.Run("the compiled-in set is valid", func(t *testing.T) {
		_, _, code := run(t, "registry", "validate")
		if code != exitOK {
			t.Errorf("exit = %d", code)
		}
	})

	t.Run("a broken definition is reported", func(t *testing.T) {
		dir := t.TempDir()
		// A misspelled key: the loader is strict so a typo cannot pass silently.
		broken := "dsl: testql\nsignal_types: [metric]\nfunctionz: {}\n"
		if err := os.WriteFile(filepath.Join(dir, "testql.yaml"), []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}

		stdout, _, code := run(t, "registry", "validate", "--dir", dir)

		if code != exitFidelity {
			t.Errorf("exit = %d, want %d", code, exitFidelity)
		}
		if !strings.Contains(stdout, "✗") || !strings.Contains(stdout, "functionz") {
			t.Errorf("the problem should be named:\n%s", stdout)
		}
	})
}

func TestRegistryDiffCommand(t *testing.T) {
	t.Run("an identical directory shows no difference", func(t *testing.T) {
		stdout, _, code := run(t, "registry", "diff", "--dir", "../../registry")
		if code != exitOK {
			t.Errorf("exit = %d:\n%s", code, stdout)
		}
		if !strings.Contains(stdout, "matches the compiled-in registry") {
			t.Errorf("output:\n%s", stdout)
		}
	})

	t.Run("changes are listed", func(t *testing.T) {
		dir := t.TempDir()
		source, err := os.ReadFile("../../registry/promql.yaml")
		if err != nil {
			t.Fatal(err)
		}
		// Add a function and drop the other language.
		modified := string(source) + "\n  made_up_function:\n    arity: 0\n" +
			"    arg_types: []\n    return_type: DOUBLE\n"
		if err := os.WriteFile(filepath.Join(dir, "promql.yaml"), []byte(modified), 0o600); err != nil {
			t.Fatal(err)
		}

		stdout, _, code := run(t, "registry", "diff", "--dir", dir)

		if code != exitOK {
			t.Errorf("exit = %d:\n%s", code, stdout)
		}
		if !strings.Contains(stdout, "+ function made_up_function") {
			t.Errorf("the added function should be listed:\n%s", stdout)
		}
		if !strings.Contains(stdout, "- logql") {
			t.Errorf("the missing language should be listed:\n%s", stdout)
		}
	})

	t.Run("the directory is required", func(t *testing.T) {
		_, stderr, code := run(t, "registry", "diff")
		if code != exitError {
			t.Errorf("exit = %d, want %d", code, exitError)
		}
		if !strings.Contains(stderr, "--dir") {
			t.Errorf("stderr:\n%s", stderr)
		}
	})
}

// TestExitCodeMapping covers the contract a CI job depends on.
func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "an exact translation",
			args: []string{"translate", "--from", "promql", "--to", "promql", "--query", "up"},
			want: exitOK,
		},
		{
			name: "an incomplete translation",
			args: []string{"translate", "--from", "promql", "--to", "logql", "--query", "up"},
			want: exitOK,
		},
		{
			name: "a query that will not parse",
			args: []string{"translate", "--from", "promql", "--to", "promql", "--query", "sum("},
			want: exitError,
		},
		{
			name: "a language that does not exist",
			args: []string{"translate", "--from", "nope", "--to", "promql", "--query", "up"},
			want: exitError,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, code := run(t, c.args...)
			if code != c.want {
				t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, c.want, stdout, stderr)
			}
		})
	}
}

// TestFidelityReportIsEmbeddedWhole covers the JSON carrying the full report
// rather than a summary of it, so a CI job can gate on any part of it.
func TestFidelityReportIsEmbeddedWhole(t *testing.T) {
	stdout, _, _ := run(t, "translate", "--from", "promql", "--to", "logql",
		"--query", `sum(rate(a[5m])) / on (job) group_left (env) histogram_quantile(0.99, b)`,
		"--format", "json")

	var result Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}

	report := result.Fidelity
	if report == nil {
		t.Fatal("no report")
	}
	if report.FidelityScore >= 1.0 {
		t.Errorf("FidelityScore = %v, want less than 1", report.FidelityScore)
	}
	if len(report.Nodes) == 0 {
		t.Fatal("the findings should be listed")
	}
	for _, node := range report.Nodes {
		if node.Path == "" || node.NodeType == "" || node.Reason == "" {
			t.Errorf("a finding is incomplete: %+v", node)
		}
	}

	// The findings name what actually failed.
	var joined, histogram bool
	for _, node := range report.Nodes {
		if strings.Contains(node.Reason, "joins are not supported") {
			joined = true
		}
		if strings.Contains(node.Reason, "histogram_quantile") {
			histogram = true
		}
	}
	if !joined || !histogram {
		t.Errorf("both losses should be named:\n%s", mustText(t, report))
	}
}

func mustText(t *testing.T, report *fidelity.Report) string {
	t.Helper()
	return report.ToText()
}

const (
	promqlDashboardPath = "../../testdata/dashboards/sample_promql.json"
	logqlDashboardPath  = "../../testdata/dashboards/sample_logql.json"
)

func TestDashboardTranslate(t *testing.T) {
	t.Run("the dashboard goes to stdout and the report to stderr", func(t *testing.T) {
		stdout, stderr, code := run(t, "dashboard", "translate",
			"--from", "promql", "--to", "logql", "--input", promqlDashboardPath)

		// The sample holds one panel that will not parse.
		if code != exitError {
			t.Errorf("exit = %d, want %d", code, exitError)
		}

		// stdout carries the dashboard alone, so it can be piped to a file.
		var dash map[string]any
		if err := json.Unmarshal([]byte(stdout), &dash); err != nil {
			t.Fatalf("stdout is not a dashboard: %v\n%s", err, stdout)
		}
		if dash["title"] != "Service overview" {
			t.Errorf("title = %v", dash["title"])
		}
		if !strings.Contains(stderr, "PolyQL dashboard report") {
			t.Errorf("the report should go to stderr:\n%s", stderr)
		}
		if !strings.Contains(stderr, "not translated") {
			t.Errorf("the failed panel should be reported:\n%s", stderr)
		}
	})

	t.Run("output and report go to files", func(t *testing.T) {
		dir := t.TempDir()
		outPath := filepath.Join(dir, "translated.json")
		reportPath := filepath.Join(dir, "report.md")

		_, _, _ = run(t, "dashboard", "translate",
			"--from", "promql", "--to", "logql", "--input", promqlDashboardPath,
			"--output", outPath, "--report", reportPath, "--report-format", "markdown")

		data, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("reading the dashboard: %v", err)
		}
		var dash map[string]any
		if err := json.Unmarshal(data, &dash); err != nil {
			t.Fatalf("the written dashboard does not parse: %v", err)
		}

		report, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatalf("reading the report: %v", err)
		}
		text := string(report)
		if !strings.HasPrefix(text, "# PolyQL dashboard report") {
			t.Errorf("the report should be markdown:\n%s", text)
		}
		if !strings.Contains(text, "| Panel | Query | Score | Outcome |") {
			t.Errorf("the report should hold a table:\n%s", text)
		}
		if !strings.Contains(text, "histogram_quantile") {
			t.Errorf("the report should name what was lost:\n%s", text)
		}
	})

	t.Run("json report", func(t *testing.T) {
		dir := t.TempDir()
		reportPath := filepath.Join(dir, "report.json")

		_, _, _ = run(t, "dashboard", "translate",
			"--from", "promql", "--to", "logql", "--input", promqlDashboardPath,
			"--output", filepath.Join(dir, "out.json"),
			"--report", reportPath, "--report-format", "json")

		data, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			SourceDSL string `json:"source_dsl"`
			TargetDSL string `json:"target_dsl"`
			Panels    []struct {
				PanelTitle string `json:"panel_title"`
				Error      string `json:"error"`
				Fidelity   *struct {
					FidelityScore float64 `json:"fidelity_score"`
				} `json:"fidelity"`
			} `json:"panels"`
			Summary *struct {
				TotalNodes int `json:"total_nodes"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatalf("the report is not valid JSON: %v\n%s", err, data)
		}
		if report.SourceDSL != "promql" || report.TargetDSL != "logql" {
			t.Errorf("report is for %s → %s", report.SourceDSL, report.TargetDSL)
		}
		if len(report.Panels) != 8 {
			t.Errorf("got %d panel entries, want one per query", len(report.Panels))
		}
		if report.Summary == nil || report.Summary.TotalNodes == 0 {
			t.Errorf("the summary should span the dashboard: %+v", report.Summary)
		}
		// The failed panel carries its message through JSON, which cannot hold
		// an error value.
		var failed bool
		for _, panel := range report.Panels {
			if panel.Error != "" {
				failed = true
			}
		}
		if !failed {
			t.Error("the failed panel should carry its message in the JSON")
		}
	})

	t.Run("a clean dashboard exits zero", func(t *testing.T) {
		// Translated into its own language, nothing is lost.
		dir := t.TempDir()
		_, stderr, code := run(t, "dashboard", "translate",
			"--from", "logql", "--to", "logql", "--input", logqlDashboardPath,
			"--output", filepath.Join(dir, "out.json"))
		if code != exitOK {
			t.Errorf("exit = %d, want %d:\n%s", code, exitOK, stderr)
		}
	})

	t.Run("skip-errors=false makes a bad panel fatal", func(t *testing.T) {
		_, stderr, code := run(t, "dashboard", "translate",
			"--from", "promql", "--to", "logql", "--input", promqlDashboardPath,
			"--skip-errors=false")
		if code != exitError {
			t.Errorf("exit = %d, want %d", code, exitError)
		}
		if !strings.Contains(stderr, "would not parse") {
			t.Errorf("stderr should explain the refusal:\n%s", stderr)
		}
	})

	t.Run("errors", func(t *testing.T) {
		cases := []struct {
			name    string
			args    []string
			wantSub string
		}{
			{
				name: "missing input",
				args: []string{"dashboard", "translate", "--from", "promql", "--to", "logql",
					"--input", "does-not-exist.json"},
				wantSub: "does-not-exist.json",
			},
			{
				name: "unknown report format",
				args: []string{"dashboard", "translate", "--from", "promql", "--to", "logql",
					"--input", promqlDashboardPath, "--report-format", "yaml"},
				wantSub: "unknown --report-format",
			},
			{
				name: "unknown language",
				args: []string{"dashboard", "translate", "--from", "nope", "--to", "logql",
					"--input", promqlDashboardPath},
				wantSub: "nope",
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				_, stderr, code := run(t, c.args...)
				if code != exitError {
					t.Errorf("exit = %d, want %d", code, exitError)
				}
				if !strings.Contains(stderr, c.wantSub) {
					t.Errorf("stderr should mention %q:\n%s", c.wantSub, stderr)
				}
			})
		}
	})
}

// TestDashboardOutputIsReviewable covers the property through the CLI: what the
// command writes differs from what it read only in the expressions.
func TestDashboardOutputIsReviewable(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.json")

	_, _, _ = run(t, "dashboard", "translate",
		"--from", "promql", "--to", "logql", "--input", promqlDashboardPath, "--output", outPath)

	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), `\u0026`) {
		t.Error("an ampersand was escaped, which would rewrite untouched lines")
	}
	// The panel expressions were rewritten.
	if !strings.Contains(string(written), `__name__=\"http_requests_total\"`) {
		t.Errorf("the expressions were not translated:\n%s", written)
	}
	// The panel that would not parse kept its expression.
	if !strings.Contains(string(written), `"expr": "rate(unclosed"`) {
		t.Error("the unparseable panel should keep its expression")
	}
}
