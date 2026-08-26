package validator

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/registry"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	return reg
}

// queryBuilder assembles IR trees directly, so the validator is exercised in
// isolation from the parsers and the resolver.
type queryBuilder struct{ query *ir.Query }

func newBuilder(signal ir.SignalType, sourceDSL string) *queryBuilder {
	q := &ir.Query{
		Signal: signal,
		Source: &ir.DataSource{Name: "target", Scope: ir.ScopeUnscoped},
		Output: &ir.Output{},
	}
	if sourceDSL != "" {
		q.Hints = map[string]string{ir.HintSourceDSL: sourceDSL}
	}
	return &queryBuilder{query: q}
}

func (b *queryBuilder) stage(s ir.PipelineStage) *queryBuilder {
	b.query.Pipeline = append(b.query.Pipeline, s)
	return b
}

func (b *queryBuilder) subquery(outerRange, resolution time.Duration) *queryBuilder {
	r := ir.NewInterval(outerRange)
	step := ir.NewInterval(resolution)
	b.query.Output.SubqueryRange = &r
	b.query.Output.SubqueryStep = &step
	return b
}

func (b *queryBuilder) window(step time.Duration, alignment ir.WindowAlignment) *queryBuilder {
	b.query.Output.Window = &ir.Window{Step: ir.NewInterval(step), Alignment: alignment}
	return b
}

func (b *queryBuilder) build() *ir.Query { return b.query }

func agg(op ir.AggOp, scope ir.AggScope) *ir.AggregationStage {
	return &ir.AggregationStage{Op: op, Scope: scope}
}

func fn(name string) *ir.FunctionStage {
	return &ir.FunctionStage{Name: name, ReturnType: ir.DataTypeDouble}
}

func filter(key string, op ir.MatchOp, value string) *ir.FilterStage {
	return &ir.FilterStage{Predicate: &ir.MatchPredicate{
		Matcher: &ir.LabelMatcher{Key: key, Op: op, Value: value},
	}}
}

// findIssue returns the first issue whose reason contains substr.
func findIssue(issues []ValidationIssue, substr string) (ValidationIssue, bool) {
	for _, issue := range issues {
		if strings.Contains(issue.Reason, substr) {
			return issue, true
		}
	}
	return ValidationIssue{}, false
}

func assertFlag(t *testing.T, node ir.Node, want ir.TranslatabilityFlag) {
	t.Helper()
	if got, reason := node.Base().Translatability(); got != want {
		t.Errorf("%T flag = %s (%q), want %s", node, got, reason, want)
	}
}

// assertNoIssues fails with the findings listed, since a surprise issue is much
// easier to diagnose when its reason is visible.
func assertNoIssues(t *testing.T, issues []ValidationIssue) {
	t.Helper()
	if len(issues) == 0 {
		return
	}
	var b strings.Builder
	for _, issue := range issues {
		b.WriteString("\n  " + issue.String())
	}
	t.Errorf("expected no issues, got %d:%s", len(issues), b.String())
}

// TestCleanTreeIsUnchanged covers the base case: a query the target can express
// keeps every flag FULL and produces no findings.
func TestCleanTreeIsUnchanged(t *testing.T) {
	query := newBuilder(ir.SignalLog, "logql").
		stage(fn("parse_json")).
		stage(filter("status", ir.MatchGTE, "400")).
		stage(agg(ir.AggCount, ir.AggScopeTemporal)).
		stage(agg(ir.AggSum, ir.AggScopeGroup)).
		window(5*time.Minute, ir.WindowUTCNormalized).
		build()

	got, issues, _ := Validate(query, "logql", testRegistry(t))

	if got != query {
		t.Error("Validate should return the same tree it was given")
	}
	assertNoIssues(t, issues)

	ir.Inspect(got, func(n ir.Node) bool {
		assertFlag(t, n, ir.TranslatabilityFull)
		return true
	})
	if worst, _ := ir.WorstTranslatability(got); worst != ir.TranslatabilityFull {
		t.Errorf("worst flag = %s, want FULL", worst)
	}
}

func TestSignalTypeCompatibility(t *testing.T) {
	t.Run("metric query into logql", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").build()
		got, issues, mismatch := Validate(query, "logql", testRegistry(t))

		assertFlag(t, got, ir.TranslatabilityPartial)
		if mismatch == nil {
			t.Fatal("expected a signal mismatch record")
		}
		if _, ok := findIssue(issues, "does not support metric queries"); ok {
			t.Fatalf("signal mismatch should not create a construct-level issue: %v", issues)
		}
		if _, ok := findIssue(issues, "NaN-as-sentinel semantics differ"); !ok {
			t.Fatalf("expected the root-level semantic mismatch to remain visible: %v", issues)
		}
	})

	t.Run("log query into promql", func(t *testing.T) {
		query := newBuilder(ir.SignalLog, "logql").build()
		got, issues, mismatch := Validate(query, "promql", testRegistry(t))

		assertFlag(t, got, ir.TranslatabilityPartial)
		if mismatch == nil {
			t.Fatal("expected a signal mismatch record")
		}
		if _, ok := findIssue(issues, "does not support log queries"); ok {
			t.Fatalf("signal mismatch should not create a construct-level issue: %v", issues)
		}
		if _, ok := findIssue(issues, "NaN-as-sentinel semantics differ"); !ok {
			t.Fatalf("expected the root-level semantic mismatch to remain visible: %v", issues)
		}
	})

	t.Run("matching signal passes", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").build()
		_, issues, mismatch := Validate(query, "promql", testRegistry(t))
		assertNoIssues(t, issues)
		if mismatch != nil {
			t.Fatalf("matching signal should not create a mismatch: %+v", mismatch)
		}
	})
}

func TestAggregationCompatibility(t *testing.T) {
	t.Run("histogram_quantile is unavailable in logql", func(t *testing.T) {
		stage := agg(ir.AggHistogramQuantile, ir.AggScopeGroup)
		query := newBuilder(ir.SignalLog, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		issue, ok := findIssue(issues, "histogram_quantile")
		if !ok {
			t.Fatalf("expected an aggregation issue, got %v", issues)
		}
		if issue.Path != "Query.Pipeline[0].AggregationStage" {
			t.Errorf("Path = %q", issue.Path)
		}
		if issue.SourceConstruct != "HISTOGRAM_QUANTILE" {
			t.Errorf("SourceConstruct = %q", issue.SourceConstruct)
		}
	})

	t.Run("rate on the temporal axis exists in both", func(t *testing.T) {
		stage := agg(ir.AggRate, ir.AggScopeTemporal)
		query := newBuilder(ir.SignalLog, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityFull)
		assertNoIssues(t, issues)
	})

	t.Run("sum on the group axis exists in both", func(t *testing.T) {
		// A range aggregation comes first, because LogQL's sum reduces samples
		// and a log stream yields lines. Without one this is not a LogQL query
		// at all, which TestGroupAggregationNeedsSamples covers separately.
		stage := agg(ir.AggSum, ir.AggScopeGroup)
		query := newBuilder(ir.SignalLog, "logql").
			stage(agg(ir.AggCount, ir.AggScopeTemporal)).
			stage(stage).
			build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityFull)
		assertNoIssues(t, issues)
	})

	t.Run("an operator available only on the other axis is partial", func(t *testing.T) {
		// IR IRATE is PromQL's irate, a temporal operator. LogQL has no
		// equivalent on any axis, so this is the wrong shape for the test; use
		// COUNT_DISTINCT, which PromQL offers as count_values on the group axis
		// only, and ask for it on the temporal one.
		stage := agg(ir.AggCountDistinct, ir.AggScopeTemporal)
		query := newBuilder(ir.SignalMetric, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityPartial)
		issue, ok := findIssue(issues, "different aggregation scope")
		if !ok {
			t.Fatalf("expected a scope issue, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityPartial {
			t.Errorf("Flag = %s, want PARTIAL", issue.Flag)
		}
	})

	t.Run("an operator absent from the target entirely is unsupported", func(t *testing.T) {
		stage := agg(ir.AggIrate, ir.AggScopeTemporal)
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		if _, ok := findIssue(issues, "is not available in logql"); !ok {
			t.Errorf("expected an unsupported aggregation, got %v", issues)
		}
	})
}

func TestFunctionCompatibility(t *testing.T) {
	t.Run("a logql parser stage is unavailable in promql", func(t *testing.T) {
		stage := fn("parse_json")
		query := newBuilder(ir.SignalLog, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		issue, ok := findIssue(issues, `function "parse_json" is not available in promql`)
		if !ok {
			t.Fatalf("expected a function issue, got %v", issues)
		}
		if issue.Path != "Query.Pipeline[0].FunctionStage" {
			t.Errorf("Path = %q", issue.Path)
		}
	})

	t.Run("line_format is unavailable in promql", func(t *testing.T) {
		stage := fn("line_format")
		query := newBuilder(ir.SignalLog, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		if _, ok := findIssue(issues, "line_format"); !ok {
			t.Errorf("expected a function issue, got %v", issues)
		}
	})

	t.Run("logql stages are available in logql", func(t *testing.T) {
		for _, name := range []string{
			"parse_json", "parse_logfmt", "parse_regexp", "parse_pattern", "parse_unpack",
			"line_format", "label_format", "drop_labels", "keep_labels", "decolorize", "unwrap",
		} {
			t.Run(name, func(t *testing.T) {
				stage := fn(name)
				query := newBuilder(ir.SignalLog, "logql").stage(stage).build()
				_, issues, _ := Validate(query, "logql", testRegistry(t))
				assertFlag(t, stage, ir.TranslatabilityFull)
				assertNoIssues(t, issues)
			})
		}
	})

	t.Run("structural operations need no registry entry", func(t *testing.T) {
		// Arithmetic and literals describe IR structure rather than a DSL's
		// vocabulary, so no definition names them and every target can write
		// them.
		for _, name := range []string{ir.FuncUnaryOp, ir.FuncLiteral} {
			t.Run(name, func(t *testing.T) {
				stage := fn(name)
				query := newBuilder(ir.SignalMetric, "promql").stage(stage).build()
				_, issues, _ := Validate(query, "promql", testRegistry(t))
				assertFlag(t, stage, ir.TranslatabilityFull)
				assertNoIssues(t, issues)
			})
		}
	})

	t.Run("a promql-only function is unavailable in logql", func(t *testing.T) {
		stage := fn("histogram_count")
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()
		_, issues, _ := Validate(query, "logql", testRegistry(t))
		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		if len(issues) == 0 {
			t.Error("expected an issue")
		}
	})
}

// TestJoinSupportReadsCapabilities covers the check being data-driven: the
// verdict comes from the target's capabilities block, not from its name.
func TestJoinSupportReadsCapabilities(t *testing.T) {
	t.Run("logql cannot join", func(t *testing.T) {
		stage := &ir.JoinStage{
			JoinType:  ir.JoinInner,
			OnLabels:  []string{"job"},
			RightSide: &ir.Query{Signal: ir.SignalLog, Output: &ir.Output{}},
		}
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		issue, ok := findIssue(issues, "joins are not supported in logql")
		if !ok {
			t.Fatalf("expected a join issue, got %v", issues)
		}
		if issue.Path != "Query.Pipeline[0].JoinStage" {
			t.Errorf("Path = %q", issue.Path)
		}
	})

	t.Run("promql can join", func(t *testing.T) {
		stage := &ir.JoinStage{JoinType: ir.JoinLeftOuter, OnLabels: []string{"job"}}
		query := newBuilder(ir.SignalMetric, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityFull)
		assertNoIssues(t, issues)
	})

	t.Run("a join type the target lacks is partial", func(t *testing.T) {
		// PromQL's vector matching covers inner and the two outer forms, but it
		// has no cross join.
		stage := &ir.JoinStage{JoinType: ir.JoinCross}
		query := newBuilder(ir.SignalMetric, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityPartial)
		if _, ok := findIssue(issues, "cannot express a CROSS join"); !ok {
			t.Errorf("expected a join type issue, got %v", issues)
		}
	})

	t.Run("the joined query is validated too", func(t *testing.T) {
		right := &ir.Query{
			Signal:   ir.SignalMetric,
			Output:   &ir.Output{},
			Pipeline: ir.Pipeline{fn("parse_json")},
		}
		stage := &ir.JoinStage{JoinType: ir.JoinInner, RightSide: right}
		query := newBuilder(ir.SignalMetric, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		issue, ok := findIssue(issues, "parse_json")
		if !ok {
			t.Fatalf("the right-hand query should be validated, got %v", issues)
		}
		want := "Query.Pipeline[0].JoinStage.RightSide.Pipeline[0].FunctionStage"
		if issue.Path != want {
			t.Errorf("Path = %q, want %q", issue.Path, want)
		}
	})
}

func TestPredicateAndOperatorCompatibility(t *testing.T) {
	t.Run("regex across DSLs is a partial warning", func(t *testing.T) {
		stage := filter("path", ir.MatchRegex, "/api/.*")
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		issue, ok := findIssue(issues, "regex dialect may differ")
		if !ok {
			t.Fatalf("expected a regex dialect warning, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityPartial {
			t.Errorf("Flag = %s, want PARTIAL", issue.Flag)
		}
		if issue.Path != "Query.Pipeline[0].FilterStage.Predicate" {
			t.Errorf("Path = %q", issue.Path)
		}
	})

	t.Run("regex within one DSL is not warned about", func(t *testing.T) {
		// Translating a query back into its own language crosses no dialect.
		stage := filter("path", ir.MatchRegex, "/api/.*")
		query := newBuilder(ir.SignalLog, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertNoIssues(t, issues)
		assertFlag(t, stage.Predicate.(*ir.MatchPredicate), ir.TranslatabilityFull)
	})

	t.Run("not-regex is warned about too", func(t *testing.T) {
		stage := filter("path", ir.MatchNotRegex, "/health")
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()
		_, issues, _ := Validate(query, "logql", testRegistry(t))
		if _, ok := findIssue(issues, "regex dialect may differ"); !ok {
			t.Errorf("expected a regex warning, got %v", issues)
		}
	})

	t.Run("an operator the target cannot spell is unsupported", func(t *testing.T) {
		// Neither DSL can write an IN predicate; QLS defines one, but PromQL and
		// LogQL both express set membership as a regex alternation instead.
		stage := &ir.FilterStage{Predicate: &ir.MatchPredicate{
			Matcher: &ir.LabelMatcher{Key: "status", Op: ir.MatchIn, Values: []string{"500", "503"}},
		}}
		query := newBuilder(ir.SignalMetric, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		issue, ok := findIssue(issues, "has no operator for IN")
		if !ok {
			t.Fatalf("expected an operator issue, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityUnsupported {
			t.Errorf("Flag = %s, want UNSUPPORTED", issue.Flag)
		}
	})

	t.Run("a predicate tree is walked to its leaves", func(t *testing.T) {
		predicate := &ir.LogicalPredicate{
			Op: ir.LogicalAnd,
			Operands: []ir.Predicate{
				&ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: "level", Op: ir.MatchEQ, Value: "error"}},
				&ir.LogicalPredicate{
					Op: ir.LogicalOr,
					Operands: []ir.Predicate{
						&ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: "path", Op: ir.MatchRegex, Value: "/a.*"}},
						&ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: "code", Op: ir.MatchIn, Values: []string{"1"}}},
					},
				},
			},
		}
		stage := &ir.FilterStage{Predicate: predicate}
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		regex, ok := findIssue(issues, "regex dialect")
		if !ok {
			t.Fatalf("the nested regex leaf should be found, got %v", issues)
		}
		wantRegexPath := "Query.Pipeline[0].FilterStage.Predicate.Operands[1].Operands[0]"
		if regex.Path != wantRegexPath {
			t.Errorf("regex path = %q, want %q", regex.Path, wantRegexPath)
		}

		in, ok := findIssue(issues, "operator for IN")
		if !ok {
			t.Fatalf("the nested IN leaf should be found, got %v", issues)
		}
		wantInPath := "Query.Pipeline[0].FilterStage.Predicate.Operands[1].Operands[1]"
		if in.Path != wantInPath {
			t.Errorf("IN path = %q, want %q", in.Path, wantInPath)
		}
	})

	t.Run("selector matchers are checked", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").build()
		query.Source.Selectors = []*ir.Selector{{
			Matchers: []*ir.LabelMatcher{
				{Key: "job", Op: ir.MatchEQ, Value: "api"},
				{Key: "status", Op: ir.MatchIn, Values: []string{"500"}},
			},
		}}

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		issue, ok := findIssue(issues, "operator for IN")
		if !ok {
			t.Fatalf("expected a selector operator issue, got %v", issues)
		}
		want := "Query.Source.Selectors[0].Matchers[1]"
		if issue.Path != want {
			t.Errorf("Path = %q, want %q", issue.Path, want)
		}
	})
}

func TestSubquerySupport(t *testing.T) {
	t.Run("logql has no subqueries", func(t *testing.T) {
		query := newBuilder(ir.SignalLog, "promql").subquery(30*time.Minute, time.Minute).build()

		got, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, got, ir.TranslatabilityUnsupported)
		// The finding lands on the Output that carries the subquery as well as
		// on the query as a whole.
		assertFlag(t, got.Output, ir.TranslatabilityUnsupported)
		issue, ok := findIssue(issues, "subqueries are not supported in logql")
		if !ok {
			t.Fatalf("expected a subquery issue, got %v", issues)
		}
		if issue.Path != "Query.Output" {
			t.Errorf("Path = %q, want Query.Output", issue.Path)
		}
	})

	t.Run("promql has subqueries", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").subquery(30*time.Minute, time.Minute).build()
		_, issues, _ := Validate(query, "promql", testRegistry(t))
		assertNoIssues(t, issues)
	})
}

// TestSentinelSemantics covers the blanket warning about absent-data handling,
// which is a property of the translation rather than of any one node.
func TestSentinelSemantics(t *testing.T) {
	t.Run("promql to logql differs", func(t *testing.T) {
		query := newBuilder(ir.SignalLog, "promql").build()

		got, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, got, ir.TranslatabilityPartial)
		issue, ok := findIssue(issues, "NaN-as-sentinel semantics differ")
		if !ok {
			t.Fatalf("expected a sentinel issue, got %v", issues)
		}
		if issue.Path != "Query" {
			t.Errorf("Path = %q, want the root", issue.Path)
		}
		if !strings.Contains(issue.Reason, "promql") || !strings.Contains(issue.Reason, "logql") {
			t.Errorf("reason %q should name both DSLs", issue.Reason)
		}
	})

	t.Run("logql to promql differs the other way", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "logql").build()
		_, issues, _ := Validate(query, "promql", testRegistry(t))
		if _, ok := findIssue(issues, "NaN-as-sentinel semantics differ"); !ok {
			t.Errorf("expected a sentinel issue, got %v", issues)
		}
	})

	t.Run("same DSL does not warn", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").build()
		_, issues, _ := Validate(query, "promql", testRegistry(t))
		assertNoIssues(t, issues)
	})

	t.Run("an unknown source DSL skips the check", func(t *testing.T) {
		// A hand-built tree may not name a source, and the comparison simply
		// cannot be made.
		query := newBuilder(ir.SignalMetric, "").build()
		_, issues, _ := Validate(query, "promql", testRegistry(t))
		assertNoIssues(t, issues)
	})
}

func TestWindowValidation(t *testing.T) {
	t.Run("a supported alignment passes", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").
			window(5*time.Minute, ir.WindowUTCNormalized).build()
		_, issues, _ := Validate(query, "promql", testRegistry(t))
		assertNoIssues(t, issues)
	})

	t.Run("calendar alignment is partial", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").
			window(24*time.Hour, ir.WindowCalendarAligned).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		issue, ok := findIssue(issues, "cannot align windows as CALENDAR_ALIGNED")
		if !ok {
			t.Fatalf("expected an alignment issue, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityPartial {
			t.Errorf("Flag = %s, want PARTIAL", issue.Flag)
		}
		if issue.Path != "Query.Output.Window" {
			t.Errorf("Path = %q", issue.Path)
		}
		assertFlag(t, query.Output.Window, ir.TranslatabilityPartial)
	})
}

// logqlDef returns the LogQL definition, which describeOrder needs in order to
// recognize a range aggregation written as a plain call.
func logqlDef(t *testing.T) *registry.DSLDefinition {
	t.Helper()
	def, err := testRegistry(t).Get("logql")
	if err != nil {
		t.Fatalf("Get(\"logql\"): %v", err)
	}
	return def
}

// TestPipelineReordering covers the target whose syntax fixes stage order.
func TestPipelineReordering(t *testing.T) {
	t.Run("an already-ordered pipeline is left alone", func(t *testing.T) {
		query := newBuilder(ir.SignalLog, "logql").
			stage(filter(ir.FieldBody, ir.MatchRegex, "error")).
			stage(fn("parse_json")).
			stage(filter("status", ir.MatchGTE, "400")).
			stage(agg(ir.AggCount, ir.AggScopeTemporal)).
			build()
		before := describeOrder(query.Pipeline, logqlDef(t))

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		if after := describeOrder(query.Pipeline, logqlDef(t)); after != before {
			t.Errorf("order changed from %q to %q", before, after)
		}
		if _, ok := findIssue(issues, "stage order adjusted"); ok {
			t.Errorf("no reordering was needed, got %v", issues)
		}
	})

	t.Run("a label filter before its parser is reordered and flagged", func(t *testing.T) {
		labelFilter := filter("status", ir.MatchGTE, "400")
		parser := fn("parse_json")
		query := newBuilder(ir.SignalLog, "logql").
			stage(labelFilter).
			stage(parser).
			build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		// LogQL cannot write the original order at all, so the pipeline is
		// rewritten — but the report says the meaning may have changed rather
		// than presenting it as equivalent.
		if got := describeOrder(query.Pipeline, logqlDef(t)); got != "parser -> label filter" {
			t.Errorf("order = %q, want the parser first", got)
		}
		issue, ok := findIssue(issues, "stage order adjusted")
		if !ok {
			t.Fatalf("expected a reordering issue, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityPartial {
			t.Errorf("Flag = %s, want PARTIAL", issue.Flag)
		}
		if !strings.Contains(issue.Reason, "verify semantic equivalence") {
			t.Errorf("reason %q should ask for confirmation", issue.Reason)
		}
		assertFlag(t, labelFilter, ir.TranslatabilityPartial)
	})

	t.Run("a reorder crossing no producer is transparent", func(t *testing.T) {
		// An aggregation written before a line filter moves after it, but
		// neither changes which attributes exist, so nothing is flagged.
		lineFilter := filter(ir.FieldBody, ir.MatchRegex, "error")
		aggregation := agg(ir.AggCount, ir.AggScopeTemporal)
		query := newBuilder(ir.SignalLog, "logql").
			stage(aggregation).
			stage(lineFilter).
			build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		if got := describeOrder(query.Pipeline, logqlDef(t)); got != "line filter -> aggregation" {
			t.Errorf("order = %q, want the filter first", got)
		}
		if _, ok := findIssue(issues, "stage order adjusted"); ok {
			t.Errorf("this reorder crosses no producer and should be transparent, got %v", issues)
		}
		assertFlag(t, aggregation, ir.TranslatabilityFull)
		assertFlag(t, lineFilter, ir.TranslatabilityFull)
	})

	t.Run("stages of one kind keep their written order", func(t *testing.T) {
		first := filter(ir.FieldBody, ir.MatchRegex, "first")
		second := filter(ir.FieldBody, ir.MatchRegex, "second")
		query := newBuilder(ir.SignalLog, "logql").
			stage(fn("parse_json")).
			stage(first).
			stage(second).
			build()

		Validate(query, "logql", testRegistry(t))

		// LogQL puts line filters ahead of the parser, and two line filters
		// compose, so swapping them would be a change nobody asked for.
		if query.Pipeline[0] != first || query.Pipeline[1] != second {
			t.Errorf("the two line filters should keep their relative order: %s",
				describeOrder(query.Pipeline, logqlDef(t)))
		}
	})

	t.Run("a value filter stays after the aggregation that produces it", func(t *testing.T) {
		// A filter on the aggregated value is not a label filter: moving it
		// ahead of the aggregation would leave it filtering on something that
		// does not exist yet.
		aggregation := agg(ir.AggRate, ir.AggScopeTemporal)
		valueFilter := filter(ir.FieldValue, ir.MatchGT, "5")
		query := newBuilder(ir.SignalLog, "logql").
			stage(aggregation).
			stage(valueFilter).
			build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		if query.Pipeline[0] != aggregation || query.Pipeline[1] != valueFilter {
			t.Errorf("order changed to %q, want the aggregation first",
				describeOrder(query.Pipeline, logqlDef(t)))
		}
		if _, ok := findIssue(issues, "stage order adjusted"); ok {
			t.Errorf("no reordering was needed, got %v", issues)
		}
	})

	t.Run("promql imposes no order", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").
			stage(agg(ir.AggSum, ir.AggScopeGroup)).
			stage(filter(ir.FieldValue, ir.MatchGT, "5")).
			build()
		before := describeOrder(query.Pipeline, logqlDef(t))

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		if after := describeOrder(query.Pipeline, logqlDef(t)); after != before {
			t.Errorf("PromQL nests rather than chaining, so order should be untouched: %q -> %q",
				before, after)
		}
		assertNoIssues(t, issues)
	})
}

// TestWorstTranslatabilityOnMixedTree covers the rollup the fidelity reporter
// will use: the worst verdict anywhere in the subtree.
func TestWorstTranslatabilityOnMixedTree(t *testing.T) {
	partial := filter("path", ir.MatchRegex, "/a.*")
	unsupported := agg(ir.AggHistogramQuantile, ir.AggScopeGroup)
	clean := agg(ir.AggSum, ir.AggScopeGroup)

	// The range aggregation is what makes the sum expressible; see the note in
	// TestAggregationCompatibility.
	query := newBuilder(ir.SignalLog, "promql").
		stage(partial).
		stage(agg(ir.AggCount, ir.AggScopeTemporal)).
		stage(clean).
		stage(unsupported).
		build()

	_, issues, _ := Validate(query, "logql", testRegistry(t))

	assertFlag(t, clean, ir.TranslatabilityFull)
	assertFlag(t, unsupported, ir.TranslatabilityUnsupported)

	worst, reason := ir.WorstTranslatability(query)
	if worst != ir.TranslatabilityUnsupported {
		t.Errorf("worst = %s, want UNSUPPORTED", worst)
	}
	if reason == "" {
		t.Error("the worst verdict should carry its reason")
	}

	// Every finding is in the list, even where a node ended up flagged worse.
	if len(issues) < 3 {
		t.Errorf("got %d issues, want the regex, the sentinel warning and the aggregation", len(issues))
	}
}

// TestNodeKeepsTheWorstVerdict covers a node drawing more than one finding: the
// flag is the worst of them, and neither finding is dropped.
func TestNodeKeepsTheWorstVerdict(t *testing.T) {
	// This matcher draws both an unsupported operator and, were it a regex, a
	// dialect warning; IN is unsupported, so the node must end UNSUPPORTED.
	stage := &ir.FilterStage{Predicate: &ir.MatchPredicate{
		Matcher: &ir.LabelMatcher{Key: "status", Op: ir.MatchIn, Values: []string{"500"}},
	}}
	query := newBuilder(ir.SignalLog, "promql").stage(stage).build()

	_, issues, _ := Validate(query, "logql", testRegistry(t))

	// A matcher inside a predicate is an implementation detail of that
	// predicate, so the verdict lands on the predicate — which is the node a
	// fidelity report names.
	assertFlag(t, stage.Predicate.(*ir.MatchPredicate), ir.TranslatabilityUnsupported)
	assertFlag(t, stage.Predicate.(*ir.MatchPredicate).Matcher, ir.TranslatabilityFull)

	// The root carries the sentinel warning independently.
	assertFlag(t, query, ir.TranslatabilityPartial)
	if len(issues) < 2 {
		t.Errorf("got %d issues, want both the operator and the sentinel finding", len(issues))
	}
}

func TestValidateEdgeCases(t *testing.T) {
	t.Run("nil query", func(t *testing.T) {
		got, issues, _ := Validate(nil, "promql", testRegistry(t))
		if got != nil || issues != nil {
			t.Errorf("got %v, %v; want nil, nil", got, issues)
		}
	})

	t.Run("nil registry", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").build()
		got, issues, _ := Validate(query, "promql", nil)
		assertFlag(t, got, ir.TranslatabilityUnsupported)
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want one", len(issues))
		}
		if !strings.Contains(issues[0].Reason, "no language registry") {
			t.Errorf("reason = %q", issues[0].Reason)
		}
	})

	t.Run("unknown target DSL", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").build()
		// A name no definition claims. It has to be one the registry genuinely
		// does not hold, so this cannot be a real DSL that might be added later.
		got, issues, _ := Validate(query, "nonsuchql", testRegistry(t))

		// Validate returns no error, so an unusable target is reported the same
		// way as anything else the translation cannot do.
		assertFlag(t, got, ir.TranslatabilityUnsupported)
		if _, ok := findIssue(issues, `no registry definition for target "nonsuchql"`); !ok {
			t.Errorf("expected a target issue, got %v", issues)
		}
	})

	t.Run("target name is normalised", func(t *testing.T) {
		query := newBuilder(ir.SignalMetric, "promql").build()
		_, issues, _ := Validate(query, "  PromQL ", testRegistry(t))
		assertNoIssues(t, issues)
	})
}

// TestWalkReachesEveryNode pins the validator's path-tracking traversal to the
// same node set as ir.Walk.
//
// The validator cannot use ir.Walk directly, because the Visitor contract hands
// over a node with no indication of the field or index it was reached through,
// and the issue paths need exactly that. This test is what keeps the two from
// drifting: a node type added to ir.Walk but not here would show up as a count
// mismatch rather than as silently unvalidated output.
func TestWalkReachesEveryNode(t *testing.T) {
	query := newBuilder(ir.SignalLog, "promql").
		stage(filter(ir.FieldBody, ir.MatchRegex, "error")).
		stage(fn("parse_json")).
		stage(&ir.FilterStage{Predicate: &ir.LogicalPredicate{
			Op: ir.LogicalAnd,
			Operands: []ir.Predicate{
				&ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: "a", Op: ir.MatchEQ, Value: "1"}},
				&ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: "b", Op: ir.MatchRegex, Value: "2"}},
			},
		}}).
		stage(agg(ir.AggSum, ir.AggScopeGroup)).
		stage(&ir.JoinStage{JoinType: ir.JoinInner, RightSide: &ir.Query{
			Signal:   ir.SignalLog,
			Source:   &ir.DataSource{Selectors: []*ir.Selector{{Matchers: []*ir.LabelMatcher{{Key: "x", Op: ir.MatchEQ, Value: "y"}}}}},
			Output:   &ir.Output{},
			Pipeline: ir.Pipeline{fn("parse_logfmt")},
		}}).
		window(time.Minute, ir.WindowUTCNormalized).
		build()
	query.Source.Selectors = []*ir.Selector{{
		Matchers: []*ir.LabelMatcher{{Key: "app", Op: ir.MatchEQ, Value: "frontend"}},
	}}

	// Collect the node kinds the validator's traversal reaches, by flagging
	// everything it visits through a target that supports nothing.
	visited := map[string]int{}
	v := &validator{targetDSL: "probe", sourceDSL: "promql"}
	v.target = probeDefinition()
	v.run(query)
	for _, issue := range v.issues {
		visited[issue.Path]++
	}

	// Every matcher, every stage, the window and the root should have been
	// reached; ir.Walk agrees on the node set.
	var walkNodes int
	ir.Inspect(query, func(ir.Node) bool {
		walkNodes++
		return true
	})
	if walkNodes == 0 {
		t.Fatal("ir.Inspect walked nothing")
	}

	for _, want := range []string{
		"Query.Source.Selectors[0].Matchers[0]",
		"Query.Pipeline[0].FilterStage.Predicate",
		"Query.Pipeline[1].FunctionStage",
		"Query.Pipeline[2].FilterStage.Predicate.Operands[0]",
		"Query.Pipeline[2].FilterStage.Predicate.Operands[1]",
		"Query.Pipeline[3].AggregationStage",
		"Query.Pipeline[4].JoinStage",
		"Query.Pipeline[4].JoinStage.RightSide.Source.Selectors[0].Matchers[0]",
		"Query.Pipeline[4].JoinStage.RightSide.Pipeline[0].FunctionStage",
		"Query.Output.Window",
	} {
		if visited[want] == 0 {
			t.Errorf("the traversal never reached %s", want)
		}
	}
}

// probeDefinition is a definition that can express nothing, so that every node
// the traversal reaches produces a finding.
func probeDefinition() *registry.DSLDefinition {
	return &registry.DSLDefinition{
		DSL:                  "probe",
		SupportedSignalTypes: []ir.SignalType{ir.SignalProfile},
		Functions:            map[string]*registry.FunctionDef{},
		Operators:            map[string]*registry.OperatorDef{},
		Capabilities: registry.Capabilities{
			Joins:            false,
			WindowAlignments: []ir.WindowAlignment{ir.WindowCalendarAligned},
		},
		SourcePath: "probe",
	}
}

// TestIssuePathsAreWellFormed covers the path format itself.
func TestIssuePathsAreWellFormed(t *testing.T) {
	query := newBuilder(ir.SignalMetric, "promql").
		stage(agg(ir.AggHistogramQuantile, ir.AggScopeGroup)).
		build()

	_, issues, _ := Validate(query, "logql", testRegistry(t))

	if len(issues) == 0 {
		t.Fatal("expected issues")
	}
	for _, issue := range issues {
		if issue.Path == "" {
			t.Errorf("issue %q has an empty path", issue.Reason)
		}
		if !strings.HasPrefix(issue.Path, "Query") {
			t.Errorf("path %q should be rooted at Query", issue.Path)
		}
		if strings.Contains(issue.Path, "..") {
			t.Errorf("path %q has an empty segment", issue.Path)
		}
		if issue.Flag == ir.TranslatabilityFull {
			t.Errorf("issue %q is flagged FULL; only downgrades are findings", issue.Reason)
		}
	}
}

// TestBinaryOpOperandsAreValidated covers Fix 1's contract: the operator is
// always expressible, but what it combines may not be.
func TestBinaryOpOperandsAreValidated(t *testing.T) {
	left := &ir.Query{Signal: ir.SignalLog, Output: &ir.Output{},
		Pipeline: ir.Pipeline{agg(ir.AggRate, ir.AggScopeTemporal)}}
	right := &ir.Query{Signal: ir.SignalLog, Output: &ir.Output{},
		Pipeline: ir.Pipeline{fn("parse_json")}}
	binary := &ir.BinaryOpStage{Op: ir.ArithDiv, Left: left, Right: right}

	query := newBuilder(ir.SignalLog, "logql").stage(binary).build()

	_, issues, _ := Validate(query, "promql", testRegistry(t))

	// The operator itself is fine; the right operand's parser stage is not.
	assertFlag(t, binary, ir.TranslatabilityFull)
	issue, ok := findIssue(issues, "parse_json")
	if !ok {
		t.Fatalf("the right operand should be validated, got %v", issues)
	}
	want := "Query.Pipeline[0].BinaryOpStage.Right.Pipeline[0].FunctionStage"
	if issue.Path != want {
		t.Errorf("Path = %q, want %q", issue.Path, want)
	}
}

// TestBoolModifierIsFlagged covers Fix 5: a target without the modifier can
// still write the filter, but the result set differs.
func TestBoolModifierIsFlagged(t *testing.T) {
	t.Run("logql has no bool modifier", func(t *testing.T) {
		stage := filter(ir.FieldValue, ir.MatchGT, "5")
		stage.ReturnsBool = true
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityPartial)
		issue, ok := findIssue(issues, "no bool modifier")
		if !ok {
			t.Fatalf("expected a bool modifier issue, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityPartial {
			t.Errorf("Flag = %s, want PARTIAL", issue.Flag)
		}
	})

	t.Run("promql has one", func(t *testing.T) {
		stage := filter(ir.FieldValue, ir.MatchGT, "5")
		stage.ReturnsBool = true
		query := newBuilder(ir.SignalMetric, "promql").stage(stage).build()
		_, issues, _ := Validate(query, "promql", testRegistry(t))
		assertNoIssues(t, issues)
	})

	t.Run("a plain filter is never flagged", func(t *testing.T) {
		query := newBuilder(ir.SignalLog, "logql").
			stage(filter(ir.FieldValue, ir.MatchGT, "5")).build()
		_, issues, _ := Validate(query, "logql", testRegistry(t))
		assertNoIssues(t, issues)
	})
}

// TestContainmentIsApproximated covers Fix 3: containment is the one predicate
// with a faithful fallback, so a target lacking it approximates rather than
// refuses.
func TestContainmentIsApproximated(t *testing.T) {
	t.Run("promql approximates it on an attribute", func(t *testing.T) {
		// An attribute can be matched with an escaped pattern, so containment
		// is an approximation rather than a refusal.
		stage := filter("path", ir.MatchContains, "error.log")
		query := newBuilder(ir.SignalMetric, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		issue, ok := findIssue(issues, "no containment operator")
		if !ok {
			t.Fatalf("expected a containment issue, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityPartial {
			t.Errorf("Flag = %s, want PARTIAL rather than a refusal", issue.Flag)
		}
	})

	t.Run("a body filter has nowhere to land", func(t *testing.T) {
		// PromQL has no log body, so there is no field for even an escaped
		// pattern to address. Promising an approximation here would be a
		// promise the emitter cannot keep.
		stage := filter(ir.FieldBody, ir.MatchContains, "error.log")
		query := newBuilder(ir.SignalMetric, "logql").stage(stage).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		issue, ok := findIssue(issues, "no log body to filter on")
		if !ok {
			t.Fatalf("expected a body issue, got %v", issues)
		}
		if issue.Flag != ir.TranslatabilityUnsupported {
			t.Errorf("Flag = %s, want UNSUPPORTED", issue.Flag)
		}
	})

	t.Run("logql expresses it natively", func(t *testing.T) {
		for _, op := range []ir.MatchOp{ir.MatchContains, ir.MatchNotContains} {
			query := newBuilder(ir.SignalLog, "logql").
				stage(filter(ir.FieldBody, op, "error")).build()
			_, issues, _ := Validate(query, "logql", testRegistry(t))
			assertNoIssues(t, issues)
		}
	})
}

// spanBuilder starts a span query whose source carries a boolean filter, which
// is the shape only a TraceQL source produces.
func spanBuilder(sourceDSL string, filter ir.Predicate) *queryBuilder {
	q := &ir.Query{
		Signal: ir.SignalSpan,
		Source: &ir.DataSource{
			Scope:   ir.ScopeSpan,
			Spanset: &ir.SpansetSelector{Filters: filter},
		},
		Output: &ir.Output{},
	}
	if sourceDSL != "" {
		q.Hints = map[string]string{ir.HintSourceDSL: sourceDSL}
	}
	return &queryBuilder{query: q}
}

func spanMatch(key string, op ir.MatchOp, value string) ir.Predicate {
	return &ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: key, Op: op, Value: value}}
}

// TestSpansetFilterCompatibility covers the two independent ways a span set
// filter can outrun a target's selector.
func TestSpansetFilterCompatibility(t *testing.T) {
	t.Run("a conjunction lowers into a conjunctive selector", func(t *testing.T) {
		query := spanBuilder("traceql", &ir.LogicalPredicate{
			Op: ir.LogicalAnd,
			Operands: []ir.Predicate{
				spanMatch("service", ir.MatchEQ, "web"),
				spanMatch("method", ir.MatchEQ, "GET"),
			},
		}).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		if _, ok := findIssue(issues, "implicit \"and\""); ok {
			t.Errorf("an AND-tree lowers exactly and should not be reported: %v", issues)
		}
	})

	t.Run("a disjunction over one attribute folds to set membership", func(t *testing.T) {
		// Several alternatives for a single attribute are not a general
		// disjunction: they are what the IR's IN predicate means, and every
		// conjunctive target writes one as an anchored regex alternation.
		spanset := &ir.SpansetSelector{Filters: &ir.LogicalPredicate{
			Op: ir.LogicalOr,
			Operands: []ir.Predicate{
				spanMatch("service", ir.MatchEQ, "web"),
				spanMatch("service", ir.MatchEQ, "api"),
			},
		}}
		query := &ir.Query{
			Signal: ir.SignalSpan,
			Source: &ir.DataSource{Spanset: spanset},
			Output: &ir.Output{},
			Hints:  map[string]string{ir.HintSourceDSL: "traceql"},
		}

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, spanset, ir.TranslatabilityFull)
		if _, ok := findIssue(issues, "implicit \"and\""); ok {
			t.Errorf("a foldable disjunction should not be reported: %v", issues)
		}
	})

	t.Run("a disjunction across two attributes cannot be written at all", func(t *testing.T) {
		spanset := &ir.SpansetSelector{Filters: &ir.LogicalPredicate{
			Op: ir.LogicalOr,
			Operands: []ir.Predicate{
				spanMatch("service", ir.MatchEQ, "web"),
				spanMatch("region", ir.MatchEQ, "eu"),
			},
		}}
		query := &ir.Query{
			Signal: ir.SignalSpan,
			Source: &ir.DataSource{Spanset: spanset},
			Output: &ir.Output{},
			Hints:  map[string]string{ir.HintSourceDSL: "traceql"},
		}

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, spanset, ir.TranslatabilityUnsupported)
		if _, ok := findIssue(issues, "implicit \"and\""); !ok {
			t.Errorf("expected a disjunction issue, got %v", issues)
		}
	})

	t.Run("traceql keeps its own disjunction", func(t *testing.T) {
		// The same tree translated into TraceQL is exact, since its braces hold
		// a full boolean expression.
		spanset := &ir.SpansetSelector{Filters: &ir.LogicalPredicate{
			Op: ir.LogicalOr,
			Operands: []ir.Predicate{
				spanMatch("span.a", ir.MatchEQ, "1"),
				spanMatch("span.b", ir.MatchEQ, "2"),
			},
		}}
		query := &ir.Query{
			Signal: ir.SignalSpan,
			Source: &ir.DataSource{Spanset: spanset},
			Output: &ir.Output{},
			Hints:  map[string]string{ir.HintSourceDSL: "traceql"},
		}

		_, issues, _ := Validate(query, "traceql", testRegistry(t))

		assertFlag(t, spanset, ir.TranslatabilityFull)
		assertNoIssues(t, issues)
	})

	t.Run("an ordered comparison moves to a label filter where there is one", func(t *testing.T) {
		// A LogQL stream selector takes =, !=, =~ and !~ and nothing else, so
		// the comparison cannot go between the braces — but it is still
		// writable, as a label filter stage after them. The emitter moves it
		// there, so reporting it lost would put the score at odds with the
		// output.
		query := spanBuilder("traceql", spanMatch("duration", ir.MatchGT, "100ms")).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		if _, ok := findIssue(issues, "inside a selector"); ok {
			t.Errorf("LogQL has label filters, so this is writable: %v", issues)
		}
	})

	t.Run("a target with no label filters still reports it", func(t *testing.T) {
		// PromQL's ordered comparisons operate on a series' value rather than a
		// label, so there is no stage to move this to and nothing can be
		// written.
		query := spanBuilder("traceql", spanMatch("duration", ir.MatchGT, "100ms")).build()

		_, issues, _ := Validate(query, "promql", testRegistry(t))

		if _, ok := findIssue(issues, "inside a selector"); !ok {
			t.Errorf("expected a selector-context issue, got %v", issues)
		}
	})
}

// TestScopedAttributeIsPartial covers the rename a flat label namespace forces.
func TestScopedAttributeIsPartial(t *testing.T) {
	query := spanBuilder("traceql", spanMatch("span.http.status_code", ir.MatchEQ, "500")).build()

	_, issues, _ := Validate(query, "logql", testRegistry(t))

	issue, ok := findIssue(issues, "flat label namespace")
	if !ok {
		t.Fatalf("expected a scope-folding issue, got %v", issues)
	}
	if issue.Flag != ir.TranslatabilityPartial {
		t.Errorf("Flag = %s, want PARTIAL: the rename is faithful in meaning", issue.Flag)
	}
	// The reported name must be the one the emitter actually writes, or the note
	// describes something other than the output.
	if !strings.Contains(issue.Reason, "span_http_status_code") {
		t.Errorf("reason %q should name the rewritten key", issue.Reason)
	}

	t.Run("traceql keeps the scope", func(t *testing.T) {
		query := spanBuilder("traceql", spanMatch("span.http.status_code", ir.MatchEQ, "500")).build()
		_, issues, _ := Validate(query, "traceql", testRegistry(t))
		if _, ok := findIssue(issues, "flat label namespace"); ok {
			t.Errorf("TraceQL has scoped attributes and should not report a fold: %v", issues)
		}
	})
}

// TestStructuralCompatibility covers the relationship no non-span target has,
// including one that does have joins.
func TestStructuralCompatibility(t *testing.T) {
	for _, target := range []string{"logql", "promql"} {
		t.Run(target, func(t *testing.T) {
			stage := &ir.StructuralStage{Op: ir.StructuralDescendant}
			query := spanBuilder("traceql", nil).stage(stage).build()

			_, issues, _ := Validate(query, target, testRegistry(t))

			assertFlag(t, stage, ir.TranslatabilityUnsupported)
			issue, ok := findIssue(issues, "position in a trace")
			if !ok {
				t.Fatalf("expected a structural issue, got %v", issues)
			}
			if issue.SourceConstruct != "DESCENDANT" {
				t.Errorf("SourceConstruct = %q", issue.SourceConstruct)
			}
		})
	}

	// PromQL has joins, which is the point: a join correlates on values the
	// query names, and a descendant relationship on structure nothing records.
	// Having joins is therefore no help at all.
	t.Run("having joins does not help", func(t *testing.T) {
		defs := testRegistry(t)
		promql, err := defs.Get("promql")
		if err != nil {
			t.Fatal(err)
		}
		if !promql.Capabilities.Joins {
			t.Fatal("this test assumes PromQL has joins")
		}
	})

	t.Run("traceql expresses all three", func(t *testing.T) {
		for _, op := range []ir.StructuralOp{
			ir.StructuralChild, ir.StructuralDescendant, ir.StructuralSibling,
		} {
			stage := &ir.StructuralStage{Op: op}
			query := spanBuilder("traceql", nil).stage(stage).build()
			_, issues, _ := Validate(query, "traceql", testRegistry(t))
			assertFlag(t, stage, ir.TranslatabilityFull)
			assertNoIssues(t, issues)
		}
	})
}

// TestCoercionCompatibility covers the cast, which only a DSL declaring
// attribute_casts can write.
func TestCoercionCompatibility(t *testing.T) {
	stage := &ir.CoercionStage{Attribute: "span.http.status_code", TargetType: ir.DataTypeSignedInt}
	query := spanBuilder("traceql", nil).stage(stage).build()

	_, issues, _ := Validate(query, "logql", testRegistry(t))

	assertFlag(t, stage, ir.TranslatabilityUnsupported)
	if _, ok := findIssue(issues, "cannot reinterpret"); !ok {
		t.Errorf("expected a coercion issue, got %v", issues)
	}

	t.Run("traceql can", func(t *testing.T) {
		stage := &ir.CoercionStage{Attribute: "span.x", TargetType: ir.DataTypeSignedInt}
		query := spanBuilder("traceql", nil).stage(stage).build()
		_, issues, _ := Validate(query, "traceql", testRegistry(t))
		assertFlag(t, stage, ir.TranslatabilityFull)
		assertNoIssues(t, issues)
	})
}

// TestArithmeticCompatibility covers the capability that defaults to true, so
// only a language without arithmetic has to declare it.
func TestArithmeticCompatibility(t *testing.T) {
	t.Run("traceql has none", func(t *testing.T) {
		binary := &ir.BinaryOpStage{Op: ir.ArithDiv}
		unary := &ir.UnaryOpStage{Op: ir.ArithNeg}
		query := spanBuilder("promql", nil).stage(binary).stage(unary).build()

		_, issues, _ := Validate(query, "traceql", testRegistry(t))

		assertFlag(t, binary, ir.TranslatabilityUnsupported)
		assertFlag(t, unary, ir.TranslatabilityUnsupported)
		if _, ok := findIssue(issues, "no arithmetic between result sets"); !ok {
			t.Errorf("expected an arithmetic issue, got %v", issues)
		}
	})

	t.Run("the other targets keep it", func(t *testing.T) {
		for _, target := range []string{"promql", "logql"} {
			binary := &ir.BinaryOpStage{Op: ir.ArithDiv}
			query := newBuilder(ir.SignalMetric, "promql").stage(binary).build()
			_, issues, _ := Validate(query, target, testRegistry(t))
			assertFlag(t, binary, ir.TranslatabilityFull)
			if _, ok := findIssue(issues, "no arithmetic"); ok {
				t.Errorf("%s has arithmetic and should not report otherwise: %v", target, issues)
			}
		}
	})
}

// TestTemporalWindowCompatibility covers a target that cannot carry a time range
// inside the query at all, which is a difference in results rather than in
// spelling.
func TestTemporalWindowCompatibility(t *testing.T) {
	query := newBuilder(ir.SignalMetric, "promql").
		window(5*time.Minute, ir.WindowUTCNormalized).
		build()

	_, issues, _ := Validate(query, "traceql", testRegistry(t))

	assertFlag(t, query.Output.Window, ir.TranslatabilityUnsupported)
	issue, ok := findIssue(issues, "no range selector")
	if !ok {
		t.Fatalf("expected a window issue, got %v", issues)
	}
	if !strings.Contains(issue.Reason, "outside the query") {
		t.Errorf("reason %q should say where the range has to travel instead", issue.Reason)
	}

	t.Run("a query with no window is unaffected", func(t *testing.T) {
		query := spanBuilder("traceql", nil).build()
		_, issues, _ := Validate(query, "traceql", testRegistry(t))
		assertNoIssues(t, issues)
	})
}

// TestGroupAggregationNeedsSamples covers the gap between what the validator
// scored and what the emitter wrote.
//
// LogQL's group aggregations reduce samples, and a stream selector yields log
// lines. The IR records only "collapse across groups", so a per-stage check
// finds COUNT on the group axis, sees that LogQL has count, and calls it FULL —
// while the emitter reaches the same stage, finds it applied to raw log lines,
// and drops it. That divergence is the one thing a fidelity report must never
// have, so the check reads the pipeline as a whole.
func TestGroupAggregationNeedsSamples(t *testing.T) {
	t.Run("a bare group aggregation over logs cannot be written", func(t *testing.T) {
		stage := agg(ir.AggSum, ir.AggScopeGroup)
		query := newBuilder(ir.SignalLog, "promql").stage(stage).build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		issue, ok := findIssue(issues, "nothing here has produced")
		if !ok {
			t.Fatalf("expected an operand issue, got %v", issues)
		}
		if issue.SourceConstruct != "SUM" {
			t.Errorf("SourceConstruct = %q", issue.SourceConstruct)
		}
	})

	t.Run("a temporal aggregation first makes it expressible", func(t *testing.T) {
		group := agg(ir.AggSum, ir.AggScopeGroup)
		query := newBuilder(ir.SignalLog, "promql").
			stage(agg(ir.AggRate, ir.AggScopeTemporal)).
			stage(group).
			build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, group, ir.TranslatabilityFull)
		if _, ok := findIssue(issues, "nothing here has produced"); ok {
			t.Errorf("a range aggregation ran first, so the sum is writable: %v", issues)
		}
	})

	t.Run("a range function counts as producing samples", func(t *testing.T) {
		// bytes_rate has no IR aggregation operator, so it reaches the IR as a
		// plain FunctionStage — but it is still a range aggregation, and the
		// registry says so through the argument type it consumes.
		group := agg(ir.AggSum, ir.AggScopeGroup)
		query := newBuilder(ir.SignalLog, "logql").
			stage(fn("bytes_rate")).
			stage(group).
			build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, group, ir.TranslatabilityFull)
		if _, ok := findIssue(issues, "nothing here has produced"); ok {
			t.Errorf("bytes_rate produces samples: %v", issues)
		}
	})

	t.Run("a filter does not produce samples", func(t *testing.T) {
		// Filters narrow the stream; they do not turn lines into numbers.
		stage := agg(ir.AggCount, ir.AggScopeGroup)
		query := newBuilder(ir.SignalLog, "logql").
			stage(filter(ir.FieldBody, ir.MatchContains, "error")).
			stage(stage).
			build()

		_, issues, _ := Validate(query, "logql", testRegistry(t))

		assertFlag(t, stage, ir.TranslatabilityUnsupported)
		if _, ok := findIssue(issues, "nothing here has produced"); !ok {
			t.Errorf("expected an operand issue, got %v", issues)
		}
	})

	t.Run("targets whose source is already samples are unaffected", func(t *testing.T) {
		// PromQL reads metrics, so its group aggregations need no conversion,
		// and the capability defaults to false for it.
		for _, target := range []string{"promql", "traceql"} {
			stage := agg(ir.AggCount, ir.AggScopeGroup)
			query := newBuilder(ir.SignalMetric, "promql").stage(stage).build()
			_, issues, _ := Validate(query, target, testRegistry(t))
			if _, ok := findIssue(issues, "nothing here has produced"); ok {
				t.Errorf("%s needs no sample conversion: %v", target, issues)
			}
		}
	})
}

// TestRangeFunctionKeepsItsPipelinePosition covers a reordering bug that was
// silently corrupting valid queries.
//
// A function with no IR aggregation operator — LogQL's bytes_rate — used to fall
// to the catch-all rank, which sorts after the aggregations. The reorder pass
// then moved it past the very aggregation that consumes its samples, and the
// emitter, finding a group aggregation applied to raw log lines, dropped it. The
// result was that "sum(bytes_rate(...))" translated into LogQL came back as
// "bytes_rate(...)" while still reporting full fidelity.
func TestRangeFunctionKeepsItsPipelinePosition(t *testing.T) {
	rangeFn := fn("bytes_rate")
	group := agg(ir.AggSum, ir.AggScopeGroup)
	query := newBuilder(ir.SignalLog, "logql").
		stage(rangeFn).
		stage(group).
		build()

	_, issues, _ := Validate(query, "logql", testRegistry(t))

	if len(query.Pipeline) != 2 {
		t.Fatalf("got %d stages, want two", len(query.Pipeline))
	}
	if query.Pipeline[0] != ir.PipelineStage(rangeFn) {
		t.Errorf("the range function moved; order is now %q",
			describeOrder(query.Pipeline, logqlDef(t)))
	}
	if query.Pipeline[1] != ir.PipelineStage(group) {
		t.Errorf("the aggregation moved; order is now %q",
			describeOrder(query.Pipeline, logqlDef(t)))
	}
	if _, ok := findIssue(issues, "stage order adjusted"); ok {
		t.Errorf("nothing needed reordering, got %v", issues)
	}

	t.Run("it ranks with the aggregations", func(t *testing.T) {
		def := logqlDef(t)
		rangeTypes := def.RangeOperandTypes()
		if got := rankOf(rangeFn, rangeTypes, def); got != rankAggregation {
			t.Errorf("rank = %v, want %v", got, rankAggregation)
		}
		// A function over an instant vector is not a range aggregation and
		// keeps the catch-all rank.
		if got := rankOf(fn("label_replace"), rangeTypes, def); got != rankOther {
			t.Errorf("label_replace rank = %v, want %v", got, rankOther)
		}
	})
}

// TestRangeOperandTypesAreDerived covers the registry lookup both the reorder
// pass and the operand check depend on. It is derived rather than declared: the
// DSL already says what a range is, through the type its temporal aggregations
// consume.
func TestRangeOperandTypesAreDerived(t *testing.T) {
	reg := testRegistry(t)

	logql, err := reg.Get("logql")
	if err != nil {
		t.Fatal(err)
	}
	got := logql.RangeOperandTypes()
	for _, want := range []string{"range_vector", "unwrapped_range"} {
		if !got[want] {
			t.Errorf("LogQL range operands = %v, want %q among them", got, want)
		}
	}
	// An instant vector is what a group aggregation consumes, never a range.
	if got["instant_vector"] {
		t.Errorf("instant_vector is not a range operand: %v", got)
	}

	t.Run("a language with no temporal aggregations has none", func(t *testing.T) {
		traceql, err := reg.Get("traceql")
		if err != nil {
			t.Fatal(err)
		}
		if got := traceql.RangeOperandTypes(); len(got) != 0 {
			t.Errorf("TraceQL has no temporal aggregations, got %v", got)
		}
	})
}
