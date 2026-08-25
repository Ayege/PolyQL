package dashboard

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/registry"

	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
)

const (
	promqlDashboard = "../../testdata/dashboards/sample_promql.json"
	logqlDashboard  = "../../testdata/dashboards/sample_logql.json"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

func translateFile(t *testing.T, path, from, to string) *TranslateResult {
	t.Helper()
	dash, err := ReadDashboard(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	result, err := Translate(dash, from, to, testRegistry(t))
	if err != nil {
		t.Fatalf("translating %s: %v", path, err)
	}
	return result
}

// reportFor finds the report for one panel id.
func reportFor(t *testing.T, result *TranslateResult, panelID int) PanelReport {
	t.Helper()
	for _, report := range result.PanelReports {
		if report.PanelID == panelID {
			return report
		}
	}
	t.Fatalf("no report for panel %d", panelID)
	return PanelReport{}
}

func TestTranslatePromQLDashboard(t *testing.T) {
	result := translateFile(t, promqlDashboard, "promql", "logql")

	// Eight expressions: seven top-level panels with a query, one inside a row.
	// The row panel itself has no target.
	if len(result.PanelReports) != 8 {
		t.Fatalf("got %d panel reports, want one per query:\n%+v",
			len(result.PanelReports), result.PanelReports)
	}

	t.Run("one bad expression does not stop the batch", func(t *testing.T) {
		broken := reportFor(t, result, 8)
		if !broken.Failed() {
			t.Errorf("panel 8 holds an unparseable expression and should have failed")
		}
		if !strings.Contains(broken.Error.Error(), "parse error") {
			t.Errorf("Error = %v", broken.Error)
		}
		// A half-translated query is worse than an untranslated one.
		if broken.TranslatedExpr != "" {
			t.Errorf("TranslatedExpr = %q, want it left empty", broken.TranslatedExpr)
		}
		// The panel keeps the expression it came with.
		var broken8 string
		for _, panel := range result.Dashboard.Panels {
			if panel.ID == 8 {
				broken8 = panel.Targets[0].Expr
			}
		}
		if broken8 != "rate(unclosed" {
			t.Errorf("the panel's expression = %q, want it unchanged", broken8)
		}
		// Everything after it was still translated.
		if result.TranslatedCount() != 7 {
			t.Errorf("translated %d of 8 queries, want the other seven", result.TranslatedCount())
		}
	})

	t.Run("the translatable panels are rewritten", func(t *testing.T) {
		for _, panelID := range []int{1, 2, 5} {
			report := reportFor(t, result, panelID)
			if report.Failed() {
				t.Errorf("panel %d: %v", panelID, report.Error)
				continue
			}
			if report.TranslatedExpr == "" || report.TranslatedExpr == report.OriginalExpr {
				t.Errorf("panel %d was not translated: %q -> %q",
					panelID, report.OriginalExpr, report.TranslatedExpr)
			}
		}
	})

	t.Run("histogram_quantile is reported as unsupported", func(t *testing.T) {
		report := reportFor(t, result, 3)
		if report.Fidelity == nil {
			t.Fatal("no report")
		}
		if report.Fidelity.FidelityScore >= 1.0 {
			t.Errorf("score = %v, want less than 1", report.Fidelity.FidelityScore)
		}
		if !findsReason(report, "histogram_quantile") {
			t.Errorf("the finding should name the aggregation:\n%s", report.Fidelity.ToText())
		}
	})

	t.Run("the join is reported as unsupported", func(t *testing.T) {
		report := reportFor(t, result, 6)
		if !findsReason(report, "joins are not supported") {
			t.Errorf("the finding should name the join:\n%s", report.Fidelity.ToText())
		}
		if report.Fidelity.WorstFlag != ir.TranslatabilityUnsupported {
			t.Errorf("WorstFlag = %s", report.Fidelity.WorstFlag)
		}
	})

	t.Run("a panel inside a row is translated", func(t *testing.T) {
		report := reportFor(t, result, 7)
		if report.Failed() {
			t.Fatalf("panel 7: %v", report.Error)
		}
		if !strings.Contains(report.PanelPath, "panels[") ||
			!strings.Contains(report.PanelPath, ".panels[") {
			t.Errorf("PanelPath = %q, want it to show the nesting", report.PanelPath)
		}
		if report.TranslatedExpr == "" {
			t.Error("the nested panel should have been translated")
		}
	})

	t.Run("the summary spans every panel", func(t *testing.T) {
		summary := result.Summary
		if summary == nil {
			t.Fatal("no summary")
		}
		if summary.TotalNodes == 0 {
			t.Fatal("the summary counted nothing")
		}
		if summary.FidelityScore <= 0 || summary.FidelityScore >= 1 {
			t.Errorf("score = %v, want between 0 and 1: some panels are clean and some are not",
				summary.FidelityScore)
		}
		// A finding says which panel it came from.
		var labeled bool
		for _, node := range summary.Nodes {
			if strings.Contains(node.Path, "Latency p99") {
				labeled = true
			}
		}
		if !labeled {
			t.Errorf("a combined finding should name its panel:\n%s", summary.ToText())
		}

		// The counts are the sum of the parts.
		var total, full int
		for _, report := range result.PanelReports {
			if report.Fidelity == nil {
				continue
			}
			total += report.Fidelity.TotalNodes
			full += report.Fidelity.FullCount
		}
		if summary.TotalNodes != total || summary.FullCount != full {
			t.Errorf("summary counts %d/%d, want %d/%d",
				summary.FullCount, summary.TotalNodes, full, total)
		}
	})

	t.Run("the output is valid JSON", func(t *testing.T) {
		data, err := Marshal(result.Dashboard)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var check map[string]any
		if err := json.Unmarshal(data, &check); err != nil {
			t.Fatalf("the output does not parse: %v", err)
		}
		if check["title"] != "Service overview" {
			t.Errorf("title = %v", check["title"])
		}
	})
}

func findsReason(report PanelReport, substr string) bool {
	if report.Fidelity == nil {
		return false
	}
	for _, node := range report.Fidelity.Nodes {
		if strings.Contains(node.Reason, substr) {
			return true
		}
	}
	return false
}

func TestTranslateLogQLDashboard(t *testing.T) {
	result := translateFile(t, logqlDashboard, "logql", "promql")

	if len(result.PanelReports) != 5 {
		t.Fatalf("got %d reports, want one per panel", len(result.PanelReports))
	}

	t.Run("parser stages are reported as unsupported", func(t *testing.T) {
		// Panel 3 carries | json, panel 4 carries | logfmt.
		for panelID, stage := range map[int]string{3: "parse_json", 4: "parse_logfmt"} {
			report := reportFor(t, result, panelID)
			if !findsReason(report, stage) {
				t.Errorf("panel %d should report %s:\n%s", panelID, stage, report.Fidelity.ToText())
			}
		}
	})

	t.Run("stream selectors survive", func(t *testing.T) {
		report := reportFor(t, result, 1)
		if !strings.Contains(report.TranslatedExpr, `app="frontend"`) {
			t.Errorf("the stream labels should carry across: %q", report.TranslatedExpr)
		}
	})

	t.Run("the metric layer translates", func(t *testing.T) {
		report := reportFor(t, result, 5)
		if !strings.Contains(report.TranslatedExpr, "topk(10,") {
			t.Errorf("TranslatedExpr = %q", report.TranslatedExpr)
		}
	})
}

// TestPromQLNameBecomesALabel covers the one structural change a PromQL query
// undergoes: LogQL has no metric name, so it becomes a matcher.
func TestPromQLNameBecomesALabel(t *testing.T) {
	result := translateFile(t, promqlDashboard, "promql", "logql")
	report := reportFor(t, result, 1)

	if !strings.Contains(report.TranslatedExpr, `__name__="http_requests_total"`) {
		t.Errorf("TranslatedExpr = %q", report.TranslatedExpr)
	}
	// The substitution is explained rather than done silently.
	var explained bool
	for _, note := range report.Notes {
		if strings.Contains(note, "__name__") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("Notes = %v, want the substitution explained", report.Notes)
	}
	// The notes stay in the report; the panel gets an expression Grafana can
	// run, not one with comments in it.
	if strings.Contains(report.TranslatedExpr, "#") {
		t.Errorf("the panel expression should carry no comments: %q", report.TranslatedExpr)
	}
}

// TestRoundTripPreservesEverythingElse is the property the whole design serves:
// a translated dashboard differs from its original only in its expressions.
func TestRoundTripPreservesEverythingElse(t *testing.T) {
	original, err := ReadDashboard(promqlDashboard)
	if err != nil {
		t.Fatal(err)
	}
	translated, err := ReadDashboard(promqlDashboard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Translate(translated, "promql", "logql", testRegistry(t)); err != nil {
		t.Fatal(err)
	}

	before := decodeGeneric(t, mustMarshal(t, original))
	after := decodeGeneric(t, mustMarshal(t, translated))

	stripExprs(before)
	stripExprs(after)

	if !reflect.DeepEqual(before, after) {
		t.Errorf("the translation changed something other than an expression:\n%s",
			describeDiff("", before, after))
	}
}

// TestMarshalIsIdentityWithoutTranslation covers the reader and writer alone:
// reading a dashboard and writing it back must not change it.
func TestMarshalIsIdentityWithoutTranslation(t *testing.T) {
	for _, path := range []string{promqlDashboard, logqlDashboard} {
		t.Run(path, func(t *testing.T) {
			dash, err := ReadDashboard(path)
			if err != nil {
				t.Fatal(err)
			}
			out := mustMarshal(t, dash)

			source, err := readFile(path)
			if err != nil {
				t.Fatal(err)
			}

			// Compared as values, since the input's own indentation is its
			// author's rather than something to reproduce.
			if !reflect.DeepEqual(decodeGeneric(t, source), decodeGeneric(t, out)) {
				t.Errorf("reading and writing changed the document:\n%s",
					describeDiff("", decodeGeneric(t, source), decodeGeneric(t, out)))
			}
		})
	}

	t.Run("key order is preserved", func(t *testing.T) {
		// Grafana does not care about key order, but a reviewer reading the
		// diff of a migrated dashboard does.
		dash, err := ReadDashboard(promqlDashboard)
		if err != nil {
			t.Fatal(err)
		}
		out := string(mustMarshal(t, dash))

		// "annotations" comes first in the source and "weekStart" last; sorting
		// would have moved both.
		annotations := strings.Index(out, `"annotations"`)
		editable := strings.Index(out, `"editable"`)
		weekStart := strings.Index(out, `"weekStart"`)
		if annotations < 0 || editable < 0 || weekStart < 0 {
			t.Fatalf("keys are missing from the output")
		}
		if !(annotations < editable && editable < weekStart) {
			t.Errorf("keys were reordered: annotations=%d editable=%d weekStart=%d",
				annotations, editable, weekStart)
		}
	})
}

func TestEmptyDashboard(t *testing.T) {
	var dash Dashboard
	if err := json.Unmarshal([]byte(`{"title":"Empty","panels":[]}`), &dash); err != nil {
		t.Fatal(err)
	}

	result, err := Translate(&dash, "promql", "logql", testRegistry(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	if len(result.PanelReports) != 0 {
		t.Errorf("got %d reports, want none", len(result.PanelReports))
	}
	// Nothing was lost because there was nothing to lose.
	if result.Summary.FidelityScore != 1.0 {
		t.Errorf("score = %v, want 1.0", result.Summary.FidelityScore)
	}

	data, err := Marshal(&dash)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("the output does not parse: %v", err)
	}
}

func TestDashboardOfOnlyBrokenExpressions(t *testing.T) {
	const source = `{"title":"Broken","panels":[
		{"id":1,"title":"a","type":"timeseries","targets":[{"expr":"rate(unclosed","refId":"A"}]},
		{"id":2,"title":"b","type":"timeseries","targets":[{"expr":"sum(","refId":"A"}]}
	]}`

	var dash Dashboard
	if err := json.Unmarshal([]byte(source), &dash); err != nil {
		t.Fatal(err)
	}
	result, err := Translate(&dash, "promql", "logql", testRegistry(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	if len(result.Failures()) != 2 {
		t.Errorf("got %d failures, want both", len(result.Failures()))
	}
	for _, panel := range dash.Panels {
		if panel.Targets[0].TranslatedOrOriginal() == "" {
			t.Errorf("panel %d lost its expression", panel.ID)
		}
	}
	// Nothing translated, so nothing was lost. The score says the translation
	// was faithful; the failure list says there was no translation.
	if result.Summary.FidelityScore != 1.0 {
		t.Errorf("score = %v, want 1.0 over zero nodes", result.Summary.FidelityScore)
	}
	if result.TranslatedCount() != 0 {
		t.Errorf("TranslatedCount = %d, want 0", result.TranslatedCount())
	}
}

func TestTranslateErrors(t *testing.T) {
	dash, err := ReadDashboard(promqlDashboard)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nil dashboard", func(t *testing.T) {
		if _, err := Translate(nil, "promql", "logql", testRegistry(t)); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("nil registry", func(t *testing.T) {
		if _, err := Translate(dash, "promql", "logql", nil); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("unknown source language", func(t *testing.T) {
		// A missing parser is a fault in the request, not in any panel, so it
		// fails once rather than eight times.
		if _, err := Translate(dash, "nope", "logql", testRegistry(t)); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("unknown target language", func(t *testing.T) {
		if _, err := Translate(dash, "promql", "nope", testRegistry(t)); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := ReadDashboard("does-not-exist.json"); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		var dash Dashboard
		if err := json.Unmarshal([]byte(`{"panels": [`), &dash); err == nil {
			t.Error("expected an error")
		}
	})
}

// TestTargetWithNoExpression covers a panel target that is not a query, which a
// dashboard may carry for its own reasons.
func TestTargetWithNoExpression(t *testing.T) {
	const source = `{"title":"Mixed","panels":[
		{"id":1,"title":"a","type":"timeseries","targets":[
			{"refId":"A","hide":true},
			{"expr":"up","refId":"B"}
		]}
	]}`

	var dash Dashboard
	if err := json.Unmarshal([]byte(source), &dash); err != nil {
		t.Fatal(err)
	}
	result, err := Translate(&dash, "promql", "promql", testRegistry(t))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.PanelReports) != 1 {
		t.Errorf("got %d reports, want only the target that holds a query", len(result.PanelReports))
	}
	if result.PanelReports[0].TargetRefID != "B" {
		t.Errorf("the report should be for target B, got %q", result.PanelReports[0].TargetRefID)
	}
	// The other target is untouched, including the field PolyQL never read.
	out := mustMarshal(t, &dash)
	if !strings.Contains(string(out), `"hide": true`) {
		t.Errorf("the other target lost a field:\n%s", out)
	}
}

// TestOutputDiffsOnlyInExpressions is the property a migration tool lives or
// dies by: the result has to be reviewable.
//
// A translated dashboard reaches a human as a pull request. If reading it means
// scanning a thousand reordered or re-escaped lines for the eight that matter,
// nobody will review it and nobody will trust the tool.
func TestOutputDiffsOnlyInExpressions(t *testing.T) {
	original, err := ReadDashboard(promqlDashboard)
	if err != nil {
		t.Fatal(err)
	}
	before := strings.Split(string(mustMarshal(t, original)), "\n")

	translated, err := ReadDashboard(promqlDashboard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Translate(translated, "promql", "logql", testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	after := strings.Split(string(mustMarshal(t, translated)), "\n")

	if len(before) != len(after) {
		t.Fatalf("the output has %d lines and the input %d; a translation should not "+
			"add or remove lines", len(after), len(before))
	}

	var changed []string
	for i := range before {
		if before[i] == after[i] {
			continue
		}
		changed = append(changed, fmt.Sprintf("line %d:\n  - %s\n  + %s", i+1, before[i], after[i]))
		if !strings.Contains(before[i], `"expr"`) {
			t.Errorf("a line other than an expression changed:\n  - %s\n  + %s", before[i], after[i])
		}
	}

	if len(changed) == 0 {
		t.Error("nothing changed at all; the translation did not happen")
	}
	t.Logf("%d expression line(s) changed, and nothing else", len(changed))

	t.Run("an ampersand survives", func(t *testing.T) {
		// encoding/json escapes &, < and > by default, which would rewrite
		// lines the translation never touched.
		out := string(mustMarshal(t, translated))
		if !strings.Contains(out, "Annotations & Alerts") {
			t.Errorf("an ampersand was escaped:\n%s", firstMatch(out, "Annotations"))
		}
		if strings.Contains(out, `\u0026`) {
			t.Errorf("the output holds an escaped ampersand")
		}
	})
}

func firstMatch(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
