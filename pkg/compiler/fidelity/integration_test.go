package fidelity_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/fidelity"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/compiler/resolver"
	"github.com/polyql/polyql/pkg/compiler/validator"
	"github.com/polyql/polyql/pkg/registry"

	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
)

// translate runs the pipeline as a real caller would and reports on the result.
func translate(t *testing.T, sourceDSL, query, targetDSL string) *fidelity.Report {
	t.Helper()

	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	p, err := parser.Get(sourceDSL)
	if err != nil {
		t.Fatalf("parser.Get: %v", err)
	}
	node, err := p.Parse(query)
	if err != nil {
		t.Fatalf("parsing %s: %v", query, err)
	}
	resolved, err := resolver.Resolve(node, sourceDSL, reg)
	if err != nil {
		t.Fatalf("resolving %s: %v", query, err)
	}

	// The reporter reads flags, so validation must have run first.
	if worst, _ := ir.WorstTranslatability(resolved); worst != ir.TranslatabilityFull {
		t.Fatalf("the resolver should hand over an all-FULL tree, got %s", worst)
	}
	_, issues, _ := validator.Validate(resolved, targetDSL, reg)

	// The validator may reach several verdicts about one node, and a node keeps
	// only the worst. Handing over its list is what lets the report show the
	// rest.
	findings := make([]fidelity.Finding, 0, len(issues))
	for _, issue := range issues {
		findings = append(findings, fidelity.Finding{
			Path: issue.Path, Flag: issue.Flag, Reason: issue.Reason,
		})
	}
	return fidelity.GenerateWithIssues(resolved, findings, sourceDSL, targetDSL)
}

// findByReason returns the first finding whose reason mentions substr.
func findByReason(report *fidelity.Report, substr string) (fidelity.NodeReport, bool) {
	for _, node := range report.Nodes {
		if strings.Contains(node.Reason, substr) {
			return node, true
		}
	}
	return fidelity.NodeReport{}, false
}

// TestReportOnARealTranslation drives a query that mixes what LogQL can express
// with what it cannot, and checks the report tells the truth about both.
func TestReportOnARealTranslation(t *testing.T) {
	const query = `sum by (job) (rate(x[5m])) / on (job) group_left (env) ` +
		`histogram_quantile(0.99, y)`

	report := translate(t, "promql", query, "logql")

	if report.SourceDSL != "promql" || report.TargetDSL != "logql" {
		t.Errorf("report is for %s → %s", report.SourceDSL, report.TargetDSL)
	}
	if report.TotalNodes == 0 {
		t.Fatal("the report visited nothing")
	}

	t.Run("the untranslatable constructs are named", func(t *testing.T) {
		for _, want := range []struct{ reason, describe string }{
			{"histogram_quantile", "the aggregation LogQL lacks"},
			{"joins are not supported", "the join LogQL lacks"},
		} {
			node, ok := findByReason(report, want.reason)
			if !ok {
				t.Errorf("no finding for %s:\n%s", want.describe, report.ToText())
				continue
			}
			if node.Flag != ir.TranslatabilityUnsupported {
				t.Errorf("%s is %s, want UNSUPPORTED", want.describe, node.Flag)
			}
			if node.Path == "" {
				t.Errorf("%s has no path", want.describe)
			}
		}
	})

	t.Run("the absent-data warning is an approximation", func(t *testing.T) {
		node, ok := findByReason(report, "NaN-as-sentinel")
		if !ok {
			t.Fatalf("the sentinel difference should be reported:\n%s", report.ToText())
		}
		if node.Flag != ir.TranslatabilityPartial {
			t.Errorf("Flag = %s, want PARTIAL", node.Flag)
		}
	})

	t.Run("what does translate is not flagged", func(t *testing.T) {
		// rate and sum both exist in LogQL on the same axes, so neither should
		// appear among the findings.
		for _, node := range report.Nodes {
			if node.NodeType != "AggregationStage" {
				continue
			}
			if strings.Contains(node.Reason, `"rate"`) || strings.Contains(node.Reason, `"sum"`) {
				t.Errorf("%s should have translated cleanly: %s", node.NodeType, node.Reason)
			}
		}
		// And the aggregation stages are among the nodes counted as full.
		if report.FullCount == 0 {
			t.Errorf("nothing counted as full:\n%s", report.ToText())
		}
	})

	t.Run("the copied join labels do not break the report", func(t *testing.T) {
		// group_left(env) is carried on the join, which is already unsupported;
		// the labels must not cause a second finding or a panic.
		count := 0
		for _, node := range report.Nodes {
			if node.NodeType == "JoinStage" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("got %d join findings, want exactly one", count)
		}
	})

	t.Run("the score reflects the loss", func(t *testing.T) {
		if report.FidelityScore >= 1.0 {
			t.Errorf("FidelityScore = %v, want less than 1.0", report.FidelityScore)
		}
		if report.FidelityScore < 0 || report.FidelityScore > 1 {
			t.Errorf("FidelityScore = %v, outside [0, 1]", report.FidelityScore)
		}
		if report.IsFullyTranslated() {
			t.Error("IsFullyTranslated should be false")
		}
		if report.WorstFlag != ir.TranslatabilityUnsupported {
			t.Errorf("WorstFlag = %s, want UNSUPPORTED", report.WorstFlag)
		}
	})

	t.Run("every rendering is usable", func(t *testing.T) {
		text := report.ToText()
		if text == "" || !strings.Contains(text, "promql → logql") {
			t.Errorf("ToText():\n%s", text)
		}
		if !strings.Contains(text, "✗ Unsupported translations:") {
			t.Errorf("ToText() should list the refusals:\n%s", text)
		}

		markdown := report.ToMarkdown()
		if markdown == "" || !strings.Contains(markdown, "| **Total** |") {
			t.Errorf("ToMarkdown():\n%s", markdown)
		}

		data, err := report.ToJSON()
		if err != nil {
			t.Fatalf("ToJSON: %v", err)
		}
		var restored fidelity.Report
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatalf("the JSON should parse back: %v\n%s", err, data)
		}
		if restored.TotalNodes != report.TotalNodes || len(restored.Nodes) != len(report.Nodes) {
			t.Errorf("the JSON lost detail: %+v", restored)
		}
	})
}

// TestReportOnAFaithfulTranslation covers the other end: a query translated back
// into its own language loses nothing, and the report says so plainly.
func TestReportOnAFaithfulTranslation(t *testing.T) {
	report := translate(t, "promql", `sum by (job) (rate(http_requests_total{status="500"}[5m]))`, "promql")

	if !report.IsFullyTranslated() {
		t.Errorf("a same-language translation should lose nothing:\n%s", report.ToText())
	}
	if report.FidelityScore != 1.0 {
		t.Errorf("FidelityScore = %v, want 1.0", report.FidelityScore)
	}
	if !strings.Contains(report.ToText(), "✓ All constructs translated fully.") {
		t.Errorf("ToText():\n%s", report.ToText())
	}
}

// TestReportAcrossBothDirections covers the reporter on each language pair, so a
// regression in either resolver or validator surfaces here.
func TestReportAcrossBothDirections(t *testing.T) {
	cases := []struct {
		name                string
		sourceDSL, query    string
		targetDSL           string
		wantFullyTranslated bool
	}{
		{"promql to promql", "promql", `rate(x[5m])`, "promql", true},
		{"logql to logql", "logql", `rate({app="a"} |= "err" [5m])`, "logql", true},
		{"promql to logql", "promql", `rate(x[5m])`, "logql", false},
		{"logql to promql", "logql", `rate({app="a"} |= "err" [5m])`, "promql", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report := translate(t, c.sourceDSL, c.query, c.targetDSL)

			if report.IsFullyTranslated() != c.wantFullyTranslated {
				t.Errorf("IsFullyTranslated = %v, want %v:\n%s",
					report.IsFullyTranslated(), c.wantFullyTranslated, report.ToText())
			}
			// The counts must always add up, whatever the verdict.
			if got := report.FullCount + report.PartialCount + report.UnsupportedCount; got != report.TotalNodes {
				t.Errorf("counts sum to %d, want %d", got, report.TotalNodes)
			}
			// Every finding names a node and gives a reason.
			for _, node := range report.Nodes {
				if node.Path == "" || node.NodeType == "" {
					t.Errorf("finding is unlocatable: %+v", node)
				}
				if node.Reason == "" {
					t.Errorf("finding at %s has no reason", node.Path)
				}
			}
		})
	}
}
