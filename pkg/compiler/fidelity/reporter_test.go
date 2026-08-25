package fidelity

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ir"
)

// flag marks a node with a verdict, standing in for the validator.
func flag(node ir.Node, f ir.TranslatabilityFlag, reason string) ir.Node {
	node.Base().SetTranslatability(f, reason)
	return node
}

func matcher(key string, op ir.MatchOp, value string) *ir.LabelMatcher {
	return &ir.LabelMatcher{Key: key, Op: op, Value: value}
}

// simpleQuery builds a selector query: Query, DataSource, Selector, LabelMatcher
// and Output — five nodes.
func simpleQuery() *ir.Query {
	return &ir.Query{
		Signal: ir.SignalMetric,
		Source: &ir.DataSource{
			Name:      "up",
			Selectors: []*ir.Selector{{Matchers: []*ir.LabelMatcher{matcher("job", ir.MatchEQ, "api")}}},
		},
		Output: &ir.Output{},
	}
}

func TestAllFullReport(t *testing.T) {
	report := Generate(simpleQuery(), "promql", "promql")

	if report.FidelityScore != 1.0 {
		t.Errorf("FidelityScore = %v, want 1.0", report.FidelityScore)
	}
	if len(report.Nodes) != 0 {
		t.Errorf("Nodes = %v, want empty", report.Nodes)
	}
	if report.TotalNodes != report.FullCount {
		t.Errorf("%d of %d nodes full, want all", report.FullCount, report.TotalNodes)
	}
	if !report.IsFullyTranslated() {
		t.Error("IsFullyTranslated should be true")
	}
	if !strings.Contains(report.Summary, "all constructs translated fully") {
		t.Errorf("Summary = %q", report.Summary)
	}
	if report.WorstFlag != ir.TranslatabilityFull {
		t.Errorf("WorstFlag = %s, want FULL", report.WorstFlag)
	}

	text := report.ToText()
	if !strings.Contains(text, "✓ All constructs translated fully.") {
		t.Errorf("ToText():\n%s", text)
	}
	// A clean report should read as clean, not as a list of absences.
	if strings.Contains(text, "Partial:") || strings.Contains(text, "Unsupported:") {
		t.Errorf("a clean report should not list empty counts:\n%s", text)
	}

	markdown := report.ToMarkdown()
	if strings.Contains(markdown, "**Partial:**") || strings.Contains(markdown, "**Unsupported:**") {
		t.Errorf("ToMarkdown() should have no findings sections:\n%s", markdown)
	}
}

// mixedQuery builds a query with two approximations and one refusal.
func mixedQuery() *ir.Query {
	regexFilter := &ir.FilterStage{Predicate: &ir.MatchPredicate{
		Matcher: matcher("path", ir.MatchRegex, "/api/.*"),
	}}
	flag(regexFilter.Predicate.(*ir.MatchPredicate), ir.TranslatabilityPartial,
		"regex dialect may differ between promql and logql")

	boolFilter := &ir.FilterStage{
		Predicate:   &ir.MatchPredicate{Matcher: matcher(ir.FieldValue, ir.MatchGT, "5")},
		ReturnsBool: true,
	}
	flag(boolFilter, ir.TranslatabilityPartial,
		"bool modifier (returns 0/1) not supported in logql; emitted as filter")

	join := &ir.JoinStage{JoinType: ir.JoinInner, OnLabels: []string{"job"}}
	flag(join, ir.TranslatabilityUnsupported, "joins not supported in logql")

	return &ir.Query{
		Signal:   ir.SignalMetric,
		Source:   &ir.DataSource{Name: "up"},
		Pipeline: ir.Pipeline{&ir.AggregationStage{Op: ir.AggRate, Scope: ir.AggScopeTemporal}, regexFilter, boolFilter, join},
		Output:   &ir.Output{Window: &ir.Window{Step: ir.NewInterval(5 * time.Minute)}},
	}
}

func TestMixedReport(t *testing.T) {
	report := Generate(mixedQuery(), "promql", "logql")

	if len(report.Nodes) != 3 {
		t.Fatalf("got %d findings, want 3:\n%s", len(report.Nodes), report.ToText())
	}
	if report.PartialCount != 2 || report.UnsupportedCount != 1 {
		t.Errorf("counts = %d partial, %d unsupported; want 2 and 1",
			report.PartialCount, report.UnsupportedCount)
	}
	if report.FullCount+report.PartialCount+report.UnsupportedCount != report.TotalNodes {
		t.Errorf("the counts do not add up to %d", report.TotalNodes)
	}

	want := float64(report.FullCount) / float64(report.TotalNodes)
	if math.Abs(report.FidelityScore-want) > 1e-9 {
		t.Errorf("FidelityScore = %v, want %v", report.FidelityScore, want)
	}
	if report.WorstFlag != ir.TranslatabilityUnsupported {
		t.Errorf("WorstFlag = %s, want UNSUPPORTED", report.WorstFlag)
	}

	// Approximations come before refusals, in every rendering.
	if report.Nodes[0].Flag != ir.TranslatabilityPartial ||
		report.Nodes[1].Flag != ir.TranslatabilityPartial ||
		report.Nodes[2].Flag != ir.TranslatabilityUnsupported {
		t.Errorf("findings are out of order: %+v", report.Nodes)
	}

	wantPaths := []string{
		"Query.Pipeline[1].FilterStage.Predicate",
		"Query.Pipeline[2].FilterStage",
		"Query.Pipeline[3].JoinStage",
	}
	for i, path := range wantPaths {
		if report.Nodes[i].Path != path {
			t.Errorf("Nodes[%d].Path = %q, want %q", i, report.Nodes[i].Path, path)
		}
	}
	wantTypes := []string{"MatchPredicate", "FilterStage", "JoinStage"}
	for i, nodeType := range wantTypes {
		if report.Nodes[i].NodeType != nodeType {
			t.Errorf("Nodes[%d].NodeType = %q, want %q", i, report.Nodes[i].NodeType, nodeType)
		}
	}

	t.Run("text groups partial before unsupported", func(t *testing.T) {
		text := report.ToText()
		partialAt := strings.Index(text, "⚠ Partial translations:")
		unsupportedAt := strings.Index(text, "✗ Unsupported translations:")
		if partialAt < 0 || unsupportedAt < 0 {
			t.Fatalf("both sections should appear:\n%s", text)
		}
		if partialAt > unsupportedAt {
			t.Errorf("partial should come first:\n%s", text)
		}
	})

	t.Run("markdown groups partial before unsupported", func(t *testing.T) {
		markdown := report.ToMarkdown()
		partialAt := strings.Index(markdown, "**Partial:**")
		unsupportedAt := strings.Index(markdown, "**Unsupported:**")
		if partialAt < 0 || unsupportedAt < 0 {
			t.Fatalf("both sections should appear:\n%s", markdown)
		}
		if partialAt > unsupportedAt {
			t.Errorf("partial should come first:\n%s", markdown)
		}
	})
}

func TestAllUnsupportedReport(t *testing.T) {
	query := simpleQuery()
	ir.Inspect(query, func(n ir.Node) bool {
		flag(n, ir.TranslatabilityUnsupported, "nothing here translates")
		return true
	})

	report := Generate(query, "promql", "logql")

	if report.FidelityScore != 0.0 {
		t.Errorf("FidelityScore = %v, want 0.0", report.FidelityScore)
	}
	if report.FullCount != 0 {
		t.Errorf("FullCount = %d, want 0", report.FullCount)
	}
	if report.UnsupportedCount != report.TotalNodes {
		t.Errorf("%d of %d unsupported, want all", report.UnsupportedCount, report.TotalNodes)
	}
	if !strings.Contains(report.Summary, "0 full") {
		t.Errorf("Summary = %q", report.Summary)
	}
	if len(report.Partials()) != 0 {
		t.Errorf("Partials() = %v, want none", report.Partials())
	}
}

func TestEmptyQueryReport(t *testing.T) {
	query := &ir.Query{Signal: ir.SignalMetric, Output: &ir.Output{}}

	report := Generate(query, "promql", "promql")

	// A query with nothing to lose has lost nothing.
	if report.FidelityScore != 1.0 {
		t.Errorf("FidelityScore = %v, want 1.0", report.FidelityScore)
	}
	// The query and its output are still nodes.
	if report.TotalNodes != 2 {
		t.Errorf("TotalNodes = %d, want the query and its output", report.TotalNodes)
	}
}

func TestNilQueryReport(t *testing.T) {
	report := Generate(nil, "promql", "logql")
	if report == nil {
		t.Fatal("Generate should never return nil")
	}
	if report.TotalNodes != 0 || report.FidelityScore != 1.0 {
		t.Errorf("report = %+v, want an empty perfect score", report)
	}
	if report.ToText() == "" || report.ToMarkdown() == "" {
		t.Error("an empty report should still render")
	}
}

// TestBinaryOpOperandsAreReported covers a verdict inside a binary operator's
// sub-query, which only the shared traversal reaches.
func TestBinaryOpOperandsAreReported(t *testing.T) {
	inner := &ir.FunctionStage{Name: "histogram_quantile", ReturnType: ir.DataTypeDouble}
	flag(inner, ir.TranslatabilityPartial, "approximated in the target")

	left := &ir.Query{Signal: ir.SignalMetric, Output: &ir.Output{}, Pipeline: ir.Pipeline{inner}}
	right := &ir.Query{Signal: ir.SignalMetric, Source: &ir.DataSource{Name: "b"}, Output: &ir.Output{}}

	query := &ir.Query{
		Signal:   ir.SignalMetric,
		Output:   &ir.Output{},
		Pipeline: ir.Pipeline{&ir.BinaryOpStage{Op: ir.ArithDiv, Left: left, Right: right}},
	}

	report := Generate(query, "promql", "logql")

	if len(report.Nodes) != 1 {
		t.Fatalf("got %d findings, want 1:\n%s", len(report.Nodes), report.ToText())
	}
	want := "Query.Pipeline[0].BinaryOpStage.Left.Pipeline[0].FunctionStage"
	if report.Nodes[0].Path != want {
		t.Errorf("Path = %q, want %q", report.Nodes[0].Path, want)
	}
}

// TestNestedPredicateIsReported covers a verdict on a leaf of a predicate tree
// while the filter wrapping it stays clean.
func TestNestedPredicateIsReported(t *testing.T) {
	leaf := &ir.MatchPredicate{Matcher: matcher("path", ir.MatchRegex, "/a.*")}
	flag(leaf, ir.TranslatabilityPartial, "regex dialect may differ")

	filter := &ir.FilterStage{Predicate: &ir.LogicalPredicate{
		Op: ir.LogicalAnd,
		Operands: []ir.Predicate{
			&ir.MatchPredicate{Matcher: matcher("level", ir.MatchEQ, "error")},
			leaf,
		},
	}}

	query := &ir.Query{Signal: ir.SignalLog, Output: &ir.Output{}, Pipeline: ir.Pipeline{filter}}
	report := Generate(query, "logql", "promql")

	if len(report.Nodes) != 1 {
		t.Fatalf("got %d findings, want 1:\n%s", len(report.Nodes), report.ToText())
	}
	want := "Query.Pipeline[0].FilterStage.Predicate.Operands[1]"
	if report.Nodes[0].Path != want {
		t.Errorf("Path = %q, want %q", report.Nodes[0].Path, want)
	}
	// The filter's own verdict is untouched; only the leaf was flagged.
	if got, _ := filter.Translatability(); got != ir.TranslatabilityFull {
		t.Errorf("the filter stage should stay FULL, got %s", got)
	}
}

// TestJoinRightSideIsReported covers a verdict inside the joined query.
func TestJoinRightSideIsReported(t *testing.T) {
	unsupported := &ir.FunctionStage{Name: "parse_json", ReturnType: ir.DataTypeString}
	flag(unsupported, ir.TranslatabilityUnsupported, "function \"parse_json\" is not available in promql")

	right := &ir.Query{Signal: ir.SignalLog, Output: &ir.Output{}, Pipeline: ir.Pipeline{unsupported}}
	query := &ir.Query{
		Signal:   ir.SignalMetric,
		Output:   &ir.Output{},
		Pipeline: ir.Pipeline{&ir.JoinStage{JoinType: ir.JoinInner, RightSide: right}},
	}

	report := Generate(query, "logql", "promql")

	want := "Query.Pipeline[0].JoinStage.RightSide.Pipeline[0].FunctionStage"
	found := false
	for _, node := range report.Nodes {
		if node.Path == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no finding at %q:\n%s", want, report.ToText())
	}
}

// TestSubqueryVerdictIsReported covers the subquery refusal.
//
// The verdict lives on the Output node: SubqueryRange is an ir.Interval, a plain
// value with no flag of its own, so Output is the nearest node that can carry
// one.
func TestSubqueryVerdictIsReported(t *testing.T) {
	outer := ir.NewIntervalFromSource(30*time.Minute, "30m")
	step := ir.NewIntervalFromSource(time.Minute, "1m")
	output := &ir.Output{SubqueryRange: &outer, SubqueryStep: &step}
	flag(output, ir.TranslatabilityUnsupported, "subqueries are not supported in logql")

	query := &ir.Query{Signal: ir.SignalMetric, Output: output}
	report := Generate(query, "promql", "logql")

	if len(report.Nodes) != 1 {
		t.Fatalf("got %d findings, want 1:\n%s", len(report.Nodes), report.ToText())
	}
	if report.Nodes[0].Path != "Query.Output" {
		t.Errorf("Path = %q, want Query.Output", report.Nodes[0].Path)
	}
	if report.Nodes[0].NodeType != "Output" {
		t.Errorf("NodeType = %q", report.Nodes[0].NodeType)
	}
	if !strings.Contains(report.Nodes[0].Reason, "subquer") {
		t.Errorf("Reason = %q", report.Nodes[0].Reason)
	}
}

func TestReasonsAreCarriedThrough(t *testing.T) {
	cases := []struct {
		name    string
		build   func() (*ir.Query, ir.Node)
		reason  string
		wantSub string
	}{
		{
			name: "bool modifier",
			build: func() (*ir.Query, ir.Node) {
				stage := &ir.FilterStage{
					Predicate:   &ir.MatchPredicate{Matcher: matcher(ir.FieldValue, ir.MatchGT, "5")},
					ReturnsBool: true,
				}
				return &ir.Query{Signal: ir.SignalMetric, Output: &ir.Output{},
					Pipeline: ir.Pipeline{stage}}, stage
			},
			reason:  "logql has no bool modifier: the comparison will drop the records that fail it",
			wantSub: "bool",
		},
		{
			name: "containment",
			build: func() (*ir.Query, ir.Node) {
				predicate := &ir.MatchPredicate{Matcher: matcher(ir.FieldBody, ir.MatchContains, "error")}
				stage := &ir.FilterStage{Predicate: predicate}
				return &ir.Query{Signal: ir.SignalLog, Output: &ir.Output{},
					Pipeline: ir.Pipeline{stage}}, predicate
			},
			reason:  "promql has no containment operator; the test will be written as a regular expression",
			wantSub: "containment",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			query, node := c.build()
			flag(node, ir.TranslatabilityPartial, c.reason)

			report := Generate(query, "promql", "logql")

			if len(report.Nodes) != 1 {
				t.Fatalf("got %d findings, want 1", len(report.Nodes))
			}
			if !strings.Contains(report.Nodes[0].Reason, c.wantSub) {
				t.Errorf("Reason = %q, want it to mention %q", report.Nodes[0].Reason, c.wantSub)
			}
			// The reason must survive into what a person reads.
			if !strings.Contains(report.ToText(), c.wantSub) {
				t.Errorf("ToText() lost the reason:\n%s", report.ToText())
			}
			if !strings.Contains(report.ToMarkdown(), c.wantSub) {
				t.Errorf("ToMarkdown() lost the reason:\n%s", report.ToMarkdown())
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	report := Generate(mixedQuery(), "promql", "logql")

	data, err := report.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}

	// Field names are snake_case, and enums are symbols rather than ordinals.
	for _, want := range []string{
		`"source_dsl"`, `"target_dsl"`, `"total_nodes"`, `"fidelity_score"`,
		`"node_type"`, `"PARTIAL"`, `"UNSUPPORTED"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("JSON should contain %s:\n%s", want, data)
		}
	}

	var restored Report
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if restored.SourceDSL != report.SourceDSL || restored.TargetDSL != report.TargetDSL {
		t.Errorf("DSLs changed: %+v", restored)
	}
	if restored.TotalNodes != report.TotalNodes || restored.FullCount != report.FullCount ||
		restored.PartialCount != report.PartialCount ||
		restored.UnsupportedCount != report.UnsupportedCount {
		t.Errorf("counts changed: %+v", restored)
	}
	if math.Abs(restored.FidelityScore-report.FidelityScore) > 1e-9 {
		t.Errorf("FidelityScore = %v, want %v", restored.FidelityScore, report.FidelityScore)
	}
	if restored.WorstFlag != report.WorstFlag || restored.Summary != report.Summary {
		t.Errorf("summary fields changed: %+v", restored)
	}
	if len(restored.Nodes) != len(report.Nodes) {
		t.Fatalf("got %d nodes, want %d", len(restored.Nodes), len(report.Nodes))
	}
	for i := range report.Nodes {
		if restored.Nodes[i] != report.Nodes[i] {
			t.Errorf("Nodes[%d] = %+v, want %+v", i, restored.Nodes[i], report.Nodes[i])
		}
	}
}

func TestTextFormatting(t *testing.T) {
	text := Generate(mixedQuery(), "promql", "logql").ToText()

	if !strings.HasPrefix(text, "PolyQL fidelity report: promql → logql\n") {
		t.Errorf("heading:\n%s", text)
	}
	if !strings.Contains(text, strings.Repeat("─", 40)) {
		t.Errorf("the separator should be a rule:\n%s", text)
	}
	for _, want := range []string{"Total nodes:", "Full:", "Partial:", "Unsupported:", "Fidelity score:"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "⚠") || !strings.Contains(text, "✗") {
		t.Errorf("both markers should appear:\n%s", text)
	}

	// A finding is two lines: the location, then the reason indented under it.
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.Contains(line, "JoinStage)") {
			if !strings.HasPrefix(line, "  ") {
				t.Errorf("a finding should be indented: %q", line)
			}
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "    ") {
				t.Errorf("a reason should be indented under its finding: %q", lines[i+1])
			}
		}
	}
}

func TestMarkdownFormatting(t *testing.T) {
	markdown := Generate(mixedQuery(), "promql", "logql").ToMarkdown()

	if !strings.HasPrefix(markdown, "### PolyQL fidelity report: promql → logql") {
		t.Errorf("heading:\n%s", markdown)
	}
	if !strings.Contains(markdown, "| Metric | Count | % |") ||
		!strings.Contains(markdown, "|--------|-------|---|") {
		t.Errorf("the table header should be well formed:\n%s", markdown)
	}

	// Every table row has the same number of cells as the header.
	for _, line := range strings.Split(markdown, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if got := strings.Count(line, "|"); got != 4 {
			t.Errorf("row %q has %d pipes, want 4", line, got)
		}
	}

	// Paths are written as code, since they hold brackets and dots.
	if !strings.Contains(markdown, "- `Query.Pipeline[3].JoinStage` (JoinStage):") {
		t.Errorf("findings should wrap the path in backticks:\n%s", markdown)
	}
}

// TestScorePrecision covers the split between the stored value and the
// displayed one.
func TestScorePrecision(t *testing.T) {
	// Three nodes, one of them refused.
	query := &ir.Query{Signal: ir.SignalMetric, Output: &ir.Output{}}
	stage := &ir.AggregationStage{Op: ir.AggSum, Scope: ir.AggScopeGroup}
	flag(stage, ir.TranslatabilityUnsupported, "no equivalent")
	query.Pipeline = ir.Pipeline{stage}

	report := Generate(query, "promql", "logql")

	if report.TotalNodes != 3 {
		t.Fatalf("TotalNodes = %d, want 3", report.TotalNodes)
	}
	// The field keeps full precision.
	if math.Abs(report.FidelityScore-2.0/3.0) > 1e-12 {
		t.Errorf("FidelityScore = %v, want 2/3 exactly", report.FidelityScore)
	}
	// What a person reads is rounded.
	for _, rendering := range []string{report.Summary, report.ToText(), report.ToMarkdown()} {
		if !strings.Contains(rendering, "0.67") {
			t.Errorf("the displayed score should be 0.67:\n%s", rendering)
		}
		if strings.Contains(rendering, "0.6666") {
			t.Errorf("the displayed score should be rounded:\n%s", rendering)
		}
	}
}

// TestWorstFlagMatchesTheIR covers the report agreeing with the IR's own rollup
// rather than recomputing one that could drift.
func TestWorstFlagMatchesTheIR(t *testing.T) {
	cases := []struct {
		name  string
		query *ir.Query
	}{
		{"all full", simpleQuery()},
		{"mixed", mixedQuery()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantFlag, wantReason := ir.WorstTranslatability(c.query)
			report := Generate(c.query, "promql", "logql")

			if report.WorstFlag != wantFlag {
				t.Errorf("WorstFlag = %s, want %s", report.WorstFlag, wantFlag)
			}
			if report.WorstReason != wantReason {
				t.Errorf("WorstReason = %q, want %q", report.WorstReason, wantReason)
			}
		})
	}
}

// TestTraversalIsComplete is the guard that matters most: a node type added to
// the IR but not to the shared traversal would be silently unreported, and a
// translation would look cleaner than it is.
//
// The count is derived from ir.Inspect, which walks the tree by its own
// independent switch. The two agreeing is what proves neither has drifted.
func TestTraversalIsComplete(t *testing.T) {
	query := everyNodeTypeQuery()

	var walked int
	ir.Inspect(query, func(ir.Node) bool {
		walked++
		return true
	})

	report := Generate(query, "promql", "logql")

	if report.TotalNodes != walked {
		t.Errorf("the reporter visited %d nodes, ir.Inspect visited %d; "+
			"a node type is missing from one of the two traversals",
			report.TotalNodes, walked)
	}

	// Every node type in the fixture should be reachable by name.
	seen := map[string]bool{}
	ir.InspectPath(query, "Query", func(_ string, node ir.Node) bool {
		seen[ir.NodeTypeName(node)] = true
		return true
	})
	for _, want := range []string{
		"Query", "DataSource", "Selector", "LabelMatcher", "Output", "Window", "TimeRange",
		"AggregationStage", "FunctionStage", "FilterStage", "JoinStage", "BinaryOpStage",
		"MatchPredicate", "LogicalPredicate", "LiteralExpr", "RefExpr", "QueryExpr",
	} {
		if !seen[want] {
			t.Errorf("the traversal never reached a %s", want)
		}
	}
}

// everyNodeTypeQuery builds a tree containing one of every IR node type.
func everyNodeTypeQuery() *ir.Query {
	nested := &ir.Query{
		Signal: ir.SignalMetric,
		Source: &ir.DataSource{Name: "nested"},
		Output: &ir.Output{},
	}
	right := &ir.Query{
		Signal:   ir.SignalMetric,
		Source:   &ir.DataSource{Name: "right"},
		Output:   &ir.Output{},
		Pipeline: ir.Pipeline{&ir.FunctionStage{Name: "abs", ReturnType: ir.DataTypeDouble}},
	}
	operand := &ir.Query{
		Signal: ir.SignalMetric,
		Source: &ir.DataSource{Name: "operand"},
		Output: &ir.Output{},
	}

	outer := ir.NewIntervalFromSource(30*time.Minute, "30m")
	step := ir.NewIntervalFromSource(time.Minute, "1m")

	return &ir.Query{
		Signal: ir.SignalMetric,
		Source: &ir.DataSource{
			Name:      "metric",
			Selectors: []*ir.Selector{{Matchers: []*ir.LabelMatcher{matcher("job", ir.MatchEQ, "api")}}},
		},
		Pipeline: ir.Pipeline{
			&ir.AggregationStage{
				Op: ir.AggTopK, Scope: ir.AggScopeGroup, Parameter: ir.NewNumberLiteral(5),
			},
			&ir.FunctionStage{
				Name: "label_replace",
				Args: []ir.IRExpr{
					ir.NewStringLiteral("dst"),
					&ir.RefExpr{Name: "src", Type: ir.DataTypeString},
					&ir.QueryExpr{Query: nested},
				},
				ReturnType: ir.DataTypeDouble,
			},
			&ir.FilterStage{Predicate: &ir.LogicalPredicate{
				Op: ir.LogicalAnd,
				Operands: []ir.Predicate{
					&ir.MatchPredicate{Matcher: matcher("a", ir.MatchEQ, "1")},
					&ir.MatchPredicate{Matcher: matcher("b", ir.MatchRegex, "2")},
				},
			}},
			&ir.JoinStage{JoinType: ir.JoinLeftOuter, IncludeLabels: []string{"env"}, RightSide: right},
			&ir.BinaryOpStage{Op: ir.ArithDiv, Left: operand, Right: right},
		},
		Output: &ir.Output{
			Range:         &ir.TimeRange{Start: ir.NewTimestamp(time.Unix(0, 0))},
			Window:        &ir.Window{Step: ir.NewIntervalFromSource(5*time.Minute, "5m")},
			SubqueryRange: &outer,
			SubqueryStep:  &step,
		},
	}
}

// TestGenerateWithIssuesRecoversLostVerdicts covers the case that flags alone
// cannot express: one node judged twice keeps only the worse verdict, and the
// milder one would otherwise vanish from the report.
func TestGenerateWithIssuesRecoversLostVerdicts(t *testing.T) {
	query := simpleQuery()
	// The query is rejected outright, and separately warned about.
	flag(query, ir.TranslatabilityUnsupported, "logql does not support metric queries")

	findings := []Finding{
		{Path: "Query", Flag: ir.TranslatabilityUnsupported,
			Reason: "logql does not support metric queries"},
		{Path: "Query", Flag: ir.TranslatabilityPartial,
			Reason: "NaN-as-sentinel semantics differ between promql and logql"},
	}

	report := GenerateWithIssues(query, findings, "promql", "logql")

	if len(report.Nodes) != 2 {
		t.Fatalf("got %d findings, want both verdicts:\n%s", len(report.Nodes), report.ToText())
	}
	// The milder verdict is listed first, and both name the same node.
	if report.Nodes[0].Flag != ir.TranslatabilityPartial {
		t.Errorf("Nodes[0].Flag = %s, want the approximation first", report.Nodes[0].Flag)
	}
	if !strings.Contains(report.Nodes[0].Reason, "NaN-as-sentinel") {
		t.Errorf("the warning should survive: %q", report.Nodes[0].Reason)
	}
	for _, node := range report.Nodes {
		if node.Path != "Query" || node.NodeType != "Query" {
			t.Errorf("finding should name the query: %+v", node)
		}
	}

	// The census still counts nodes, not findings, so it stays consistent.
	if report.FullCount+report.PartialCount+report.UnsupportedCount != report.TotalNodes {
		t.Errorf("counts no longer sum to TotalNodes")
	}
	if report.UnsupportedCount != 1 {
		t.Errorf("UnsupportedCount = %d, want 1; the node was judged once", report.UnsupportedCount)
	}

	t.Run("a verdict already visible is not repeated", func(t *testing.T) {
		again := GenerateWithIssues(query, findings, "promql", "logql")
		if len(again.Nodes) != len(report.Nodes) {
			t.Errorf("got %d findings, want %d", len(again.Nodes), len(report.Nodes))
		}
	})

	t.Run("full verdicts are not findings", func(t *testing.T) {
		clean := GenerateWithIssues(simpleQuery(),
			[]Finding{{Path: "Query", Flag: ir.TranslatabilityFull, Reason: "fine"}},
			"promql", "promql")
		if !clean.IsFullyTranslated() {
			t.Errorf("a FULL verdict is not a finding:\n%s", clean.ToText())
		}
	})

	t.Run("no findings is the same as Generate", func(t *testing.T) {
		plain := Generate(mixedQuery(), "promql", "logql")
		merged := GenerateWithIssues(mixedQuery(), nil, "promql", "logql")
		if len(plain.Nodes) != len(merged.Nodes) || plain.Summary != merged.Summary {
			t.Errorf("merging nothing should change nothing:\n%+v\n%+v", plain, merged)
		}
	})
}
