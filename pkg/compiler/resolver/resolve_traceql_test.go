package resolver

import (
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser/traceql"
	"github.com/polyql/polyql/pkg/registry"
)

// resolveTraceQL parses and resolves a TraceQL query, or fails the test.
func resolveTraceQL(t *testing.T, query string) *ir.Query {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	node, err := traceql.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	resolved, err := Resolve(node, "traceql", reg)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", query, err)
	}
	// The resolver decides nothing about fidelity, so the tree it hands over
	// must be untouched. A flag here would mean a verdict reached too early.
	if worst, reason := ir.WorstTranslatability(resolved); worst != ir.TranslatabilityFull {
		t.Fatalf("the resolver flagged a node %s (%q); that is the validator's job",
			worst, reason)
	}
	return resolved
}

// TestResolveSpanset covers the source shape a TraceQL query produces.
func TestResolveSpanset(t *testing.T) {
	query := resolveTraceQL(t, `{span.http.status_code = 500}`)

	if query.Signal != ir.SignalSpan {
		t.Errorf("Signal = %s, want SPAN", query.Signal)
	}
	if query.Source == nil {
		t.Fatal("a span set becomes the query's data source")
	}
	// A TraceQL span set has no name of its own — it is identified entirely by
	// the predicate — so the name stays empty, as a LogQL stream's does.
	if query.Source.Name != "" {
		t.Errorf("Name = %q, want it empty", query.Source.Name)
	}
	if query.Source.Spanset == nil {
		t.Fatal("the filter should land in Source.Spanset")
	}
	if len(query.Source.Selectors) != 0 {
		t.Errorf("a spanset filter must not also populate Selectors: %v", query.Source.Selectors)
	}

	predicate, ok := query.Source.Spanset.Filters.(*ir.MatchPredicate)
	if !ok {
		t.Fatalf("Filters = %T, want *ir.MatchPredicate", query.Source.Spanset.Filters)
	}
	// The scope travels in the key rather than beside it, which is what keeps a
	// matcher comparable across DSLs that have no scoping at all.
	if got, want := predicate.Matcher.Key, "span.http.status_code"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
	if predicate.Matcher.Op != ir.MatchEQ {
		t.Errorf("Op = %s, want EQ", predicate.Matcher.Op)
	}
	if got, want := predicate.Matcher.Value, "500"; got != want {
		t.Errorf("Value = %q, want %q", got, want)
	}
}

// TestResolveEmptySpanset covers "{}", which selects rather than not selecting.
func TestResolveEmptySpanset(t *testing.T) {
	query := resolveTraceQL(t, `{}`)

	if query.Source.Spanset == nil {
		t.Fatal("an empty selector is a spanset with no predicate, not the absence of one")
	}
	if query.Source.Spanset.Filters != nil {
		t.Errorf("Filters = %v, want nil", query.Source.Spanset.Filters)
	}
}

// TestResolveBooleanFilter covers the tree a conjunctive matcher list cannot
// hold, which is why ir.SpansetSelector exists.
func TestResolveBooleanFilter(t *testing.T) {
	query := resolveTraceQL(t, `{(.a = 1 || .b = 2) && .c = 3}`)

	top, ok := query.Source.Spanset.Filters.(*ir.LogicalPredicate)
	if !ok {
		t.Fatalf("Filters = %T, want *ir.LogicalPredicate", query.Source.Spanset.Filters)
	}
	if top.Op != ir.LogicalAnd {
		t.Errorf("top operator = %s, want AND", top.Op)
	}
	if len(top.Operands) != 2 {
		t.Fatalf("got %d operands, want 2", len(top.Operands))
	}
	// The parentheses collapse into the tree's shape rather than becoming a
	// node, since the shape already records the grouping.
	left, ok := top.Operands[0].(*ir.LogicalPredicate)
	if !ok || left.Op != ir.LogicalOr {
		t.Errorf("Operands[0] = %v, want the OR grouped beneath the AND", top.Operands[0])
	}
}

// TestResolveNegation covers the connective, which is a different thing from a
// "!=" comparison.
func TestResolveNegation(t *testing.T) {
	query := resolveTraceQL(t, `{!(.error = true)}`)

	not, ok := query.Source.Spanset.Filters.(*ir.LogicalPredicate)
	if !ok || not.Op != ir.LogicalNot {
		t.Fatalf("Filters = %v, want a NOT predicate", query.Source.Spanset.Filters)
	}
	if len(not.Operands) != 1 {
		t.Errorf("NOT takes one operand, got %d", len(not.Operands))
	}
}

// TestResolveStructural covers the trace-tree relationship folding into the
// query its left operand produced.
func TestResolveStructural(t *testing.T) {
	cases := []struct {
		query string
		want  ir.StructuralOp
	}{
		{`{.a = 1} > {.b = 2}`, ir.StructuralChild},
		{`{.a = 1} >> {.b = 2}`, ir.StructuralDescendant},
		{`{.a = 1} ~ {.b = 2}`, ir.StructuralSibling},
	}

	for _, c := range cases {
		t.Run(c.want.String(), func(t *testing.T) {
			query := resolveTraceQL(t, c.query)

			if len(query.Pipeline) != 1 {
				t.Fatalf("got %d stages, want one", len(query.Pipeline))
			}
			stage, ok := query.Pipeline[0].(*ir.StructuralStage)
			if !ok {
				t.Fatalf("Pipeline[0] = %T, want *ir.StructuralStage", query.Pipeline[0])
			}
			if stage.Op != c.want {
				t.Errorf("Op = %s, want %s", stage.Op, c.want)
			}
			// The right-hand side is a whole query, since TraceQL admits a
			// filtered or aggregated span set there just as it does on the left.
			if stage.Right == nil || stage.Right.Source == nil {
				t.Fatal("the right-hand span set should resolve to its own query")
			}
			if stage.Right.Signal != ir.SignalSpan {
				t.Errorf("right Signal = %s, want SPAN", stage.Right.Signal)
			}
		})
	}
}

// TestResolveAggregation covers metric extraction, and the scope every TraceQL
// aggregation has.
func TestResolveAggregation(t *testing.T) {
	query := resolveTraceQL(t, `count() over ({.error = true}) by (resource.service.name)`)

	stage, ok := query.Pipeline[0].(*ir.AggregationStage)
	if !ok {
		t.Fatalf("Pipeline[0] = %T, want *ir.AggregationStage", query.Pipeline[0])
	}
	if stage.Op != ir.AggCount {
		t.Errorf("Op = %s, want COUNT", stage.Op)
	}
	// TraceQL has no window to aggregate over, so every aggregation collapses
	// across spans. That is the group axis, and the scope comes from the
	// registry rather than being written into the resolver.
	if stage.Scope != ir.AggScopeGroup {
		t.Errorf("Scope = %s, want GROUP", stage.Scope)
	}
	if len(stage.GroupBy) != 1 || stage.GroupBy[0] != "resource.service.name" {
		t.Errorf("GroupBy = %v, want the qualified attribute", stage.GroupBy)
	}
	if stage.Parameter != nil {
		t.Errorf("count() counts spans and carries no attribute, got %v", stage.Parameter)
	}
	// The query still carries no window, since the range comes from the request.
	if query.Output != nil && query.Output.Window != nil {
		t.Errorf("a TraceQL query has no window in its text, got %v", query.Output.Window)
	}
}

// TestResolveAggregatedAttribute covers where the attribute an aggregate
// combines travels: the IR has no field naming one, so it becomes the stage's
// parameter.
func TestResolveAggregatedAttribute(t *testing.T) {
	query := resolveTraceQL(t, `sum(span.duration) over ({})`)

	stage := query.Pipeline[0].(*ir.AggregationStage)
	if stage.Op != ir.AggSum {
		t.Errorf("Op = %s, want SUM", stage.Op)
	}
	ref, ok := stage.Parameter.(*ir.RefExpr)
	if !ok {
		t.Fatalf("Parameter = %T, want *ir.RefExpr", stage.Parameter)
	}
	if got, want := ref.Name, "span.duration"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if ref.Scope != ir.ScopeSpan {
		t.Errorf("Scope = %s, want SPAN", ref.Scope)
	}
}

// TestResolveCoercion covers the cast becoming a stage of its own.
func TestResolveCoercion(t *testing.T) {
	query := resolveTraceQL(t, `{.x = 1} as (span.http.status_code: int)`)

	stage, ok := query.Pipeline[0].(*ir.CoercionStage)
	if !ok {
		t.Fatalf("Pipeline[0] = %T, want *ir.CoercionStage", query.Pipeline[0])
	}
	if got, want := stage.Attribute, "span.http.status_code"; got != want {
		t.Errorf("Attribute = %q, want %q", got, want)
	}
	if stage.TargetType != ir.DataTypeSignedInt {
		t.Errorf("TargetType = %s, want SIGNED_INT", stage.TargetType)
	}

	t.Run("every cast target maps to a QLS type", func(t *testing.T) {
		for _, c := range []struct {
			text string
			want ir.QlsDataType
		}{
			{"int", ir.DataTypeSignedInt},
			{"float", ir.DataTypeDouble},
			{"string", ir.DataTypeString},
			{"duration", ir.DataTypeInterval},
			{"bool", ir.DataTypeBoolean},
		} {
			query := resolveTraceQL(t, `{} as (.x: `+c.text+`)`)
			stage := query.Pipeline[0].(*ir.CoercionStage)
			if stage.TargetType != c.want {
				t.Errorf("%s → %s, want %s", c.text, stage.TargetType, c.want)
			}
		}
	})
}

// TestResolveValueSpellingSurvives covers the operand text, which is kept rather
// than the parsed value so a duration goes back out as written.
func TestResolveValueSpellingSurvives(t *testing.T) {
	query := resolveTraceQL(t, `{duration > 100ms}`)

	predicate := query.Source.Spanset.Filters.(*ir.MatchPredicate)
	if got, want := predicate.Matcher.Value, "100ms"; got != want {
		t.Errorf("Value = %q, want the source spelling %q", got, want)
	}
	// A bare intrinsic carries no scope prefix in the key.
	if got, want := predicate.Matcher.Key, "duration"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

// TestResolveTraceQLRecordsSourceDSL covers the hint later stages read to compare
// source and target semantics.
func TestResolveTraceQLRecordsSourceDSL(t *testing.T) {
	query := resolveTraceQL(t, `{}`)

	if got, ok := query.Hint(ir.HintSourceDSL); !ok || got != "traceql" {
		t.Errorf("source DSL hint = %q (present: %v), want traceql", got, ok)
	}
}

// TestResolveRejectsAMismatchedDSL covers the guard against pairing a tree with
// the wrong registry definition, which would otherwise surface as a stream of
// confusing "unknown function" errors.
func TestResolveRejectsAMismatchedDSL(t *testing.T) {
	reg, err := registry.Open("")
	if err != nil {
		t.Fatal(err)
	}
	node, err := traceql.ParseExpr(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(node, "promql", reg); err == nil {
		t.Error("resolving a TraceQL tree as PromQL should fail")
	} else if !strings.Contains(err.Error(), "traceql") {
		t.Errorf("error %q should name the tree's own DSL", err)
	}
}
