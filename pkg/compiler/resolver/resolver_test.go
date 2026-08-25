package resolver

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/registry"

	// Imported for their registration side effects, so the tests drive the
	// front ends the way a real binary does.
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	return reg
}

// resolveQuery runs the real pipeline: parse the DSL text, then resolve it.
// Testing against parsed input rather than hand-built ASTs is what makes these
// tests catch a mismatch between what a parser produces and what the resolver
// expects.
func resolveQuery(t *testing.T, dsl, query string) *ir.Query {
	t.Helper()
	p, err := parser.Get(dsl)
	if err != nil {
		t.Fatalf("parser.Get(%q): %v", dsl, err)
	}
	node, err := p.Parse(query)
	if err != nil {
		t.Fatalf("parsing %s: %v", query, err)
	}
	resolved, err := Resolve(node, dsl, testRegistry(t))
	if err != nil {
		t.Fatalf("resolving %s: %v", query, err)
	}
	return resolved
}

func aggAt(t *testing.T, q *ir.Query, i int) *ir.AggregationStage {
	t.Helper()
	if i >= len(q.Pipeline) {
		t.Fatalf("pipeline has %d stages, wanted an aggregation at %d: %s", len(q.Pipeline), i, q)
	}
	stage, ok := q.Pipeline[i].(*ir.AggregationStage)
	if !ok {
		t.Fatalf("stage %d is %T, want *ir.AggregationStage: %s", i, q.Pipeline[i], q)
	}
	return stage
}

func fnAt(t *testing.T, q *ir.Query, i int) *ir.FunctionStage {
	t.Helper()
	if i >= len(q.Pipeline) {
		t.Fatalf("pipeline has %d stages, wanted a function at %d: %s", len(q.Pipeline), i, q)
	}
	stage, ok := q.Pipeline[i].(*ir.FunctionStage)
	if !ok {
		t.Fatalf("stage %d is %T, want *ir.FunctionStage: %s", i, q.Pipeline[i], q)
	}
	return stage
}

func unaryAt(t *testing.T, q *ir.Query, i int) *ir.UnaryOpStage {
	t.Helper()
	if i >= len(q.Pipeline) {
		t.Fatalf("pipeline has %d stages, wanted a unary operator at %d: %s", len(q.Pipeline), i, q)
	}
	stage, ok := q.Pipeline[i].(*ir.UnaryOpStage)
	if !ok {
		t.Fatalf("stage %d is %T, want *ir.UnaryOpStage: %s", i, q.Pipeline[i], q)
	}
	return stage
}

func binAt(t *testing.T, q *ir.Query, i int) *ir.BinaryOpStage {
	t.Helper()
	if i >= len(q.Pipeline) {
		t.Fatalf("pipeline has %d stages, wanted a binary operator at %d: %s", len(q.Pipeline), i, q)
	}
	stage, ok := q.Pipeline[i].(*ir.BinaryOpStage)
	if !ok {
		t.Fatalf("stage %d is %T, want *ir.BinaryOpStage: %s", i, q.Pipeline[i], q)
	}
	return stage
}

func filterAt(t *testing.T, q *ir.Query, i int) *ir.FilterStage {
	t.Helper()
	if i >= len(q.Pipeline) {
		t.Fatalf("pipeline has %d stages, wanted a filter at %d: %s", len(q.Pipeline), i, q)
	}
	stage, ok := q.Pipeline[i].(*ir.FilterStage)
	if !ok {
		t.Fatalf("stage %d is %T, want *ir.FilterStage: %s", i, q.Pipeline[i], q)
	}
	return stage
}

// matcherOf unwraps a filter stage holding a single comparison.
func matcherOf(t *testing.T, stage *ir.FilterStage) *ir.LabelMatcher {
	t.Helper()
	predicate, ok := stage.Predicate.(*ir.MatchPredicate)
	if !ok {
		t.Fatalf("predicate is %T, want *ir.MatchPredicate", stage.Predicate)
	}
	return predicate.Matcher
}

// stageKinds names each stage's concrete type, for asserting pipeline shape.
func stageKinds(q *ir.Query) []string {
	kinds := make([]string, 0, len(q.Pipeline))
	for _, stage := range q.Pipeline {
		switch stage.(type) {
		case *ir.AggregationStage:
			kinds = append(kinds, "aggregation")
		case *ir.FunctionStage:
			kinds = append(kinds, "function")
		case *ir.FilterStage:
			kinds = append(kinds, "filter")
		case *ir.JoinStage:
			kinds = append(kinds, "join")
		case *ir.BinaryOpStage:
			kinds = append(kinds, "binary")
		case *ir.UnaryOpStage:
			kinds = append(kinds, "unary")
		default:
			kinds = append(kinds, "unknown")
		}
	}
	return kinds
}

func assertKinds(t *testing.T, q *ir.Query, want ...string) {
	t.Helper()
	got := stageKinds(q)
	if len(got) != len(want) {
		t.Fatalf("pipeline = %v, want %v\n  %s", got, want, q)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pipeline = %v, want %v\n  %s", got, want, q)
		}
	}
}

func step(t *testing.T, q *ir.Query) time.Duration {
	t.Helper()
	if q.Output == nil || q.Output.Window == nil {
		t.Fatalf("query has no window: %s", q)
	}
	return q.Output.Window.Step.Duration()
}

func TestResolvePromQL(t *testing.T) {
	t.Run("bare selector", func(t *testing.T) {
		q := resolveQuery(t, "promql", `up`)

		if q.Signal != ir.SignalMetric {
			t.Errorf("Signal = %s, want METRIC", q.Signal)
		}
		if q.Source == nil || q.Source.Name != "up" {
			t.Fatalf("Source = %+v, want the metric name up", q.Source)
		}
		// PromQL has one flat label namespace, so nothing is scoped.
		if q.Source.Scope != ir.ScopeUnscoped {
			t.Errorf("Scope = %s, want UNSCOPED", q.Source.Scope)
		}
		if len(q.Source.Selectors) != 0 {
			t.Errorf("got %d selectors, want none", len(q.Source.Selectors))
		}
		if len(q.Pipeline) != 0 {
			t.Errorf("pipeline = %v, want empty", stageKinds(q))
		}
		if q.Output == nil {
			t.Error("Output should be present even when nothing sets a window")
		}
	})

	t.Run("label matcher", func(t *testing.T) {
		q := resolveQuery(t, "promql", `http_requests_total{status="500"}`)

		if len(q.Source.Selectors) != 1 {
			t.Fatalf("got %d selectors, want 1", len(q.Source.Selectors))
		}
		matchers := q.Source.Selectors[0].Matchers
		if len(matchers) != 1 {
			t.Fatalf("got %d matchers, want 1", len(matchers))
		}
		m := matchers[0]
		if m.Key != "status" || m.Op != ir.MatchEQ || m.Value != "500" {
			t.Errorf("matcher = %+v, want status EQ 500", m)
		}
	})

	t.Run("regex matcher stays a regex", func(t *testing.T) {
		// An alternation is not decomposed into an IN list: that rewrite would
		// change what the query matches when the alternation is not fully
		// literal, and would hide itself from the fidelity report.
		q := resolveQuery(t, "promql", `http_requests_total{status=~"5..|4.."}`)

		m := q.Source.Selectors[0].Matchers[0]
		if m.Op != ir.MatchRegex {
			t.Errorf("Op = %s, want REGEX", m.Op)
		}
		if m.Value != "5..|4.." {
			t.Errorf("Value = %q, want the pattern verbatim", m.Value)
		}
		if len(m.Values) != 0 {
			t.Errorf("Values = %v, want empty; the regex was not decomposed", m.Values)
		}
	})

	t.Run("rate over a range", func(t *testing.T) {
		q := resolveQuery(t, "promql", `rate(http_requests_total{status="500"}[5m])`)

		assertKinds(t, q, "aggregation")
		agg := aggAt(t, q, 0)
		if agg.Op != ir.AggRate {
			t.Errorf("Op = %s, want RATE", agg.Op)
		}
		if agg.Scope != ir.AggScopeTemporal {
			t.Errorf("Scope = %s, want TEMPORAL", agg.Scope)
		}
		// The range belongs to the temporal aggregation's window, not to the
		// data source.
		if got := step(t, q); got != 5*time.Minute {
			t.Errorf("Window.Step = %s, want 5m", got)
		}
		if q.Source.Name != "http_requests_total" {
			t.Errorf("Source.Name = %q", q.Source.Name)
		}
	})

	t.Run("group aggregation over a temporal one", func(t *testing.T) {
		q := resolveQuery(t, "promql", `sum by (job) (rate(http_requests_total[5m]))`)

		// PromQL's nesting flattens into an ordered pipeline, innermost first.
		assertKinds(t, q, "aggregation", "aggregation")

		inner := aggAt(t, q, 0)
		if inner.Op != ir.AggRate || inner.Scope != ir.AggScopeTemporal {
			t.Errorf("stage 0 = %s/%s, want RATE/TEMPORAL", inner.Op, inner.Scope)
		}
		outer := aggAt(t, q, 1)
		if outer.Op != ir.AggSum || outer.Scope != ir.AggScopeGroup {
			t.Errorf("stage 1 = %s/%s, want SUM/GROUP", outer.Op, outer.Scope)
		}
		if len(outer.GroupBy) != 1 || outer.GroupBy[0] != "job" {
			t.Errorf("GroupBy = %v, want [job]", outer.GroupBy)
		}
		if len(outer.Without) != 0 {
			t.Errorf("Without = %v, want empty for a by clause", outer.Without)
		}
		if got := step(t, q); got != 5*time.Minute {
			t.Errorf("Window.Step = %s, want 5m", got)
		}
	})

	t.Run("without clause", func(t *testing.T) {
		q := resolveQuery(t, "promql", `sum without (pod) (rate(x[5m]))`)

		outer := aggAt(t, q, 1)
		if len(outer.Without) != 1 || outer.Without[0] != "pod" {
			t.Errorf("Without = %v, want [pod]", outer.Without)
		}
		if len(outer.GroupBy) != 0 {
			t.Errorf("GroupBy = %v, want empty for a without clause", outer.GroupBy)
		}
	})

	t.Run("parameterised aggregation", func(t *testing.T) {
		q := resolveQuery(t, "promql", `topk(5, http_requests_total)`)

		assertKinds(t, q, "aggregation")
		agg := aggAt(t, q, 0)
		if agg.Op != ir.AggTopK || agg.Scope != ir.AggScopeGroup {
			t.Errorf("stage = %s/%s, want TOPK/GROUP", agg.Op, agg.Scope)
		}
		literal, ok := agg.Parameter.(*ir.LiteralExpr)
		if !ok {
			t.Fatalf("Parameter is %T, want *ir.LiteralExpr", agg.Parameter)
		}
		if literal.Type != ir.DataTypeDouble {
			t.Errorf("parameter type = %s, want DOUBLE", literal.Type)
		}
		if value, ok := literal.Value.(float64); !ok || value != 5 {
			t.Errorf("parameter = %v, want the number 5", literal.Value)
		}
	})

	t.Run("three nested aggregations", func(t *testing.T) {
		q := resolveQuery(t, "promql",
			`histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`)

		assertKinds(t, q, "aggregation", "aggregation", "aggregation")

		want := []struct {
			op    ir.AggOp
			scope ir.AggScope
		}{
			{ir.AggRate, ir.AggScopeTemporal},
			{ir.AggSum, ir.AggScopeGroup},
			{ir.AggHistogramQuantile, ir.AggScopeGroup},
		}
		for i, w := range want {
			agg := aggAt(t, q, i)
			if agg.Op != w.op || agg.Scope != w.scope {
				t.Errorf("stage %d = %s/%s, want %s/%s", i, agg.Op, agg.Scope, w.op, w.scope)
			}
		}
		if by := aggAt(t, q, 1).GroupBy; len(by) != 1 || by[0] != "le" {
			t.Errorf("GroupBy = %v, want [le]", by)
		}
		param, ok := aggAt(t, q, 2).Parameter.(*ir.LiteralExpr)
		if !ok || param.Value != 0.99 {
			t.Errorf("histogram_quantile parameter = %v, want 0.99", aggAt(t, q, 2).Parameter)
		}
	})

	t.Run("binary operator between two selectors", func(t *testing.T) {
		q := resolveQuery(t, "promql", `http_requests_total / http_requests_failed`)

		assertKinds(t, q, "binary")
		bin := binAt(t, q, 0)
		if bin.Op != ir.ArithDiv {
			t.Errorf("Op = %s, want DIV", bin.Op)
		}
		// Both operands are sub-queries, so nothing about the operator is
		// carried as text.
		if bin.Left == nil || bin.Left.Source.Name != "http_requests_total" {
			t.Errorf("Left = %v, want the left selector", bin.Left)
		}
		if bin.Right == nil || bin.Right.Source.Name != "http_requests_failed" {
			t.Errorf("Right = %v, want the right selector", bin.Right)
		}
		// An operator over two sources has no single data source, and the IR
		// says so rather than picking one.
		if q.Source != nil {
			t.Errorf("Source = %+v, want nil for a two-source expression", q.Source)
		}
	})

	t.Run("offset", func(t *testing.T) {
		q := resolveQuery(t, "promql", `rate(http_requests_total[5m] offset 1h)`)

		if got := q.Output.Window.Offset.Duration(); got != time.Hour {
			t.Errorf("Window.Offset = %s, want 1h", got)
		}
		if got := step(t, q); got != 5*time.Minute {
			t.Errorf("Window.Step = %s, want 5m", got)
		}
	})

	t.Run("at modifier pins the time range", func(t *testing.T) {
		q := resolveQuery(t, "promql", `up @ 1609746000`)

		if q.Output.Range == nil {
			t.Fatal("Output.Range is nil")
		}
		want := time.Unix(1609746000, 0).UTC()
		if got := q.Output.Range.Start.Time(); !got.Equal(want) {
			t.Errorf("Range.Start = %s, want %s", got, want)
		}
		if !q.Output.Range.End.Time().Equal(want) {
			t.Error("an @ modifier pins an instant, so start and end should match")
		}
	})

	t.Run("subquery", func(t *testing.T) {
		q := resolveQuery(t, "promql", `rate(http_requests_total[5m])[30m:1m]`)

		if !q.Output.IsSubquery() {
			t.Fatal("Output should report a subquery")
		}
		// All three durations survive: the inner aggregation keeps its own
		// window, and the subquery's outer range and resolution sit beside it.
		if got := step(t, q); got != 5*time.Minute {
			t.Errorf("Window.Step = %s, want the inner 5m window", got)
		}
		if got := q.Output.SubqueryRange.Duration(); got != 30*time.Minute {
			t.Errorf("SubqueryRange = %s, want 30m", got)
		}
		if q.Output.SubqueryStep == nil || q.Output.SubqueryStep.Duration() != time.Minute {
			t.Errorf("SubqueryStep = %v, want 1m", q.Output.SubqueryStep)
		}
		assertKinds(t, q, "aggregation")
	})

	t.Run("function with no IR aggregation stays a function", func(t *testing.T) {
		// abs has no ir_kind in the registry, which is how it avoids claiming
		// to be an aggregation.
		q := resolveQuery(t, "promql", `abs(x)`)

		assertKinds(t, q, "function")
		fn := fnAt(t, q, 0)
		if fn.Name != "abs" {
			t.Errorf("Name = %q, want abs", fn.Name)
		}
		if fn.ReturnType != ir.DataTypeDouble {
			t.Errorf("ReturnType = %s, want DOUBLE from the registry", fn.ReturnType)
		}
		if q.Source.Name != "x" {
			t.Errorf("the function should fold into its operand's query, got source %+v", q.Source)
		}
	})

	t.Run("function arguments become typed expressions", func(t *testing.T) {
		q := resolveQuery(t, "promql", `label_replace(up, "dst", "$1", "src", "(.*)")`)

		fn := fnAt(t, q, 0)
		if fn.Name != "label_replace" {
			t.Errorf("Name = %q", fn.Name)
		}
		if len(fn.Args) != 4 {
			t.Fatalf("got %d args, want the four strings", len(fn.Args))
		}
		for i, want := range []string{"dst", "$1", "src", "(.*)"} {
			literal, ok := fn.Args[i].(*ir.LiteralExpr)
			if !ok {
				t.Fatalf("args[%d] is %T, want *ir.LiteralExpr", i, fn.Args[i])
			}
			if literal.Type != ir.DataTypeString || literal.Value != want {
				t.Errorf("args[%d] = %v, want the string %q", i, literal.Value, want)
			}
		}
	})

	t.Run("comparison against a scalar filters on the metric value", func(t *testing.T) {
		q := resolveQuery(t, "promql", `up > 5`)

		assertKinds(t, q, "filter")
		m := matcherOf(t, filterAt(t, q, 0))
		if m.Key != FieldValue {
			t.Errorf("Key = %q, want the QLS metric value field %q", m.Key, FieldValue)
		}
		if m.Op != ir.MatchGT || m.Value != "5" {
			t.Errorf("matcher = %+v, want value GT 5", m)
		}
		if filterAt(t, q, 0).ReturnsBool {
			t.Error("no bool modifier was written, so ReturnsBool should be false")
		}
	})

	t.Run("bool modifier is recorded rather than applied", func(t *testing.T) {
		// With bool the comparison stops filtering and yields 0/1. The IR has
		// only the filter form, so the difference is recorded for the fidelity
		// report instead of being silently lost.
		q := resolveQuery(t, "promql", `up > bool 5`)

		if !filterAt(t, q, 0).ReturnsBool {
			t.Error("ReturnsBool should record the modifier")
		}
	})
}

func TestResolvePromQLJoin(t *testing.T) {
	q := resolveQuery(t, "promql",
		`sum(rate(a[5m])) / on (job) group_left (env) sum(rate(b[5m]))`)

	assertKinds(t, q, "aggregation", "aggregation", "join", "binary")

	join, ok := q.Pipeline[2].(*ir.JoinStage)
	if !ok {
		t.Fatalf("stage 2 is %T, want *ir.JoinStage", q.Pipeline[2])
	}
	// group_left keeps every series on the many side, which QLS §Joins calls a
	// left outer equi-join.
	if join.JoinType != ir.JoinLeftOuter {
		t.Errorf("JoinType = %s, want LEFT_OUTER for group_left", join.JoinType)
	}
	if len(join.OnLabels) != 1 || join.OnLabels[0] != "job" {
		t.Errorf("OnLabels = %v, want [job]", join.OnLabels)
	}
	if len(join.IgnoreLabels) != 0 {
		t.Errorf("IgnoreLabels = %v, want empty when on(...) was written", join.IgnoreLabels)
	}
	if join.RightSide == nil || join.RightSide.Source.Name != "b" {
		t.Errorf("RightSide = %v, want the resolved right-hand query", join.RightSide)
	}
	// group_left(env) copies env onto the result; dropping the list would
	// change which labels the joined series carry.
	if got := join.IncludeLabels; len(got) != 1 || got[0] != "env" {
		t.Errorf("IncludeLabels = %v, want [env]", got)
	}
	// The operator that applies to the joined series follows as its own stage,
	// with no operands of its own.
	bin := binAt(t, q, 3)
	if bin.Op != ir.ArithDiv {
		t.Errorf("Op = %s, want DIV", bin.Op)
	}
	if bin.Left != nil || bin.Right != nil {
		t.Error("a joined operator takes its operands from the join, not from the stage")
	}

	t.Run("ignoring becomes IgnoreLabels", func(t *testing.T) {
		q := resolveQuery(t, "promql", `a / ignoring (env) b`)
		join := q.Pipeline[0].(*ir.JoinStage)
		if join.JoinType != ir.JoinInner {
			t.Errorf("JoinType = %s, want INNER without a group modifier", join.JoinType)
		}
		if len(join.IgnoreLabels) != 1 || join.IgnoreLabels[0] != "env" {
			t.Errorf("IgnoreLabels = %v, want [env]", join.IgnoreLabels)
		}
	})

	t.Run("group_right becomes a right outer join", func(t *testing.T) {
		q := resolveQuery(t, "promql", `a / on (job) group_right b`)
		if got := q.Pipeline[0].(*ir.JoinStage).JoinType; got != ir.JoinRightOuter {
			t.Errorf("JoinType = %s, want RIGHT_OUTER for group_right", got)
		}
	})
}

func TestResolveLogQL(t *testing.T) {
	t.Run("stream selector", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{app="frontend"}`)

		if q.Signal != ir.SignalLog {
			t.Errorf("Signal = %s, want LOG", q.Signal)
		}
		// A LogQL stream is identified entirely by its labels; it has no name.
		if q.Source == nil || q.Source.Name != "" {
			t.Fatalf("Source = %+v, want an unnamed source", q.Source)
		}
		if q.Source.Scope != ir.ScopeUnscoped {
			t.Errorf("Scope = %s, want UNSCOPED", q.Source.Scope)
		}
		if len(q.Source.Selectors) != 1 {
			t.Fatalf("got %d selectors, want 1", len(q.Source.Selectors))
		}
		m := q.Source.Selectors[0].Matchers[0]
		if m.Key != "app" || m.Op != ir.MatchEQ || m.Value != "frontend" {
			t.Errorf("matcher = %+v, want app EQ frontend", m)
		}
	})

	t.Run("line filter", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{app="frontend"} |= "error"`)

		assertKinds(t, q, "filter")
		m := matcherOf(t, filterAt(t, q, 0))
		// A line filter tests the whole line, so the predicate is over the QLS
		// log body field rather than a label.
		if m.Key != FieldBody {
			t.Errorf("Key = %q, want the QLS log body field %q", m.Key, FieldBody)
		}
		if m.Value != "error" {
			t.Errorf("Value = %q, want error", m.Value)
		}
		// "|=" tests for a literal substring, which the IR names outright
		// rather than lowering to a regex.
		if m.Op != ir.MatchContains {
			t.Errorf("Op = %s, want CONTAINS", m.Op)
		}
	})

	t.Run("all four line filter operators", func(t *testing.T) {
		cases := []struct {
			query string
			want  ir.MatchOp
		}{
			{`{a="b"} |= "x"`, ir.MatchContains},
			{`{a="b"} != "x"`, ir.MatchNotContains},
			{`{a="b"} |~ "x.*"`, ir.MatchRegex},
			{`{a="b"} !~ "x.*"`, ir.MatchNotRegex},
		}
		for _, c := range cases {
			t.Run(c.query, func(t *testing.T) {
				q := resolveQuery(t, "logql", c.query)
				if got := matcherOf(t, filterAt(t, q, 0)).Op; got != c.want {
					t.Errorf("Op = %s, want %s", got, c.want)
				}
			})
		}
	})

	t.Run("containment keeps a metacharacter literal", func(t *testing.T) {
		// Under the old lowering to a regex, the dot in "error.log" would have
		// become "any character" and matched "errorXlog" too.
		q := resolveQuery(t, "logql", `{app="x"} |= "error.log"`)

		m := matcherOf(t, filterAt(t, q, 0))
		if m.Op != ir.MatchContains {
			t.Errorf("Op = %s, want CONTAINS", m.Op)
		}
		if m.Value != "error.log" {
			t.Errorf("Value = %q, want the text verbatim", m.Value)
		}
	})

	t.Run("parser stage then numeric label filter", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{app="frontend"} | json | status >= 400`)

		assertKinds(t, q, "function", "filter")
		if got := fnAt(t, q, 0).Name; got != FuncParseJSON {
			t.Errorf("Name = %q, want %q", got, FuncParseJSON)
		}
		m := matcherOf(t, filterAt(t, q, 1))
		if m.Key != "status" || m.Op != ir.MatchGTE || m.Value != "400" {
			t.Errorf("matcher = %+v, want status GTE 400", m)
		}
	})

	t.Run("range aggregation over a filtered pipeline", func(t *testing.T) {
		q := resolveQuery(t, "logql", `rate({app="frontend"} |= "error" [5m])`)

		assertKinds(t, q, "filter", "aggregation")
		agg := aggAt(t, q, 1)
		if agg.Op != ir.AggRate || agg.Scope != ir.AggScopeTemporal {
			t.Errorf("stage = %s/%s, want RATE/TEMPORAL", agg.Op, agg.Scope)
		}
		if got := step(t, q); got != 5*time.Minute {
			t.Errorf("Window.Step = %s, want 5m", got)
		}
	})

	t.Run("vector aggregation over a range aggregation", func(t *testing.T) {
		q := resolveQuery(t, "logql", `sum by (level) (count_over_time({app="frontend"}[1h]))`)

		assertKinds(t, q, "aggregation", "aggregation")
		inner := aggAt(t, q, 0)
		if inner.Op != ir.AggCount || inner.Scope != ir.AggScopeTemporal {
			t.Errorf("stage 0 = %s/%s, want COUNT/TEMPORAL", inner.Op, inner.Scope)
		}
		outer := aggAt(t, q, 1)
		if outer.Op != ir.AggSum || outer.Scope != ir.AggScopeGroup {
			t.Errorf("stage 1 = %s/%s, want SUM/GROUP", outer.Op, outer.Scope)
		}
		if by := outer.GroupBy; len(by) != 1 || by[0] != "level" {
			t.Errorf("GroupBy = %v, want [level]", by)
		}
		if got := step(t, q); got != time.Hour {
			t.Errorf("Window.Step = %s, want 1h", got)
		}
	})

	t.Run("unwrap becomes its own stage before the aggregation", func(t *testing.T) {
		q := resolveQuery(t, "logql", `avg_over_time({app="frontend"} | json | unwrap duration [5m])`)

		// The unwrap belongs to the range, not to the pipeline, and it is metric
		// coercion rather than a temporal aggregation — so it lands between the
		// parser and the aggregation.
		assertKinds(t, q, "function", "function", "aggregation")

		if got := fnAt(t, q, 0).Name; got != FuncParseJSON {
			t.Errorf("stage 0 = %q, want %q", got, FuncParseJSON)
		}
		unwrap := fnAt(t, q, 1)
		if unwrap.Name != FuncUnwrap {
			t.Errorf("stage 1 = %q, want %q", unwrap.Name, FuncUnwrap)
		}
		if len(unwrap.Args) != 1 {
			t.Fatalf("unwrap has %d args, want the label reference", len(unwrap.Args))
		}
		ref, ok := unwrap.Args[0].(*ir.RefExpr)
		if !ok {
			t.Fatalf("unwrap arg is %T, want *ir.RefExpr", unwrap.Args[0])
		}
		if ref.Name != "duration" {
			t.Errorf("unwrapped label = %q, want duration", ref.Name)
		}

		agg := aggAt(t, q, 2)
		if agg.Op != ir.AggAvg || agg.Scope != ir.AggScopeTemporal {
			t.Errorf("stage 2 = %s/%s, want AVG/TEMPORAL", agg.Op, agg.Scope)
		}
		if got := step(t, q); got != 5*time.Minute {
			t.Errorf("Window.Step = %s, want 5m", got)
		}
	})

	t.Run("unwrap conversion is recorded", func(t *testing.T) {
		q := resolveQuery(t, "logql", `sum_over_time({a="b"} | unwrap bytes(size) [5m])`)

		unwrap := fnAt(t, q, 0)
		if len(unwrap.Args) != 2 {
			t.Fatalf("got %d args, want the reference and the conversion", len(unwrap.Args))
		}
		conversion, ok := unwrap.Args[1].(*ir.LiteralExpr)
		if !ok || conversion.Value != "bytes" {
			t.Errorf("conversion = %v, want bytes", unwrap.Args[1])
		}
		if unwrap.ReturnType != ir.DataTypeUnsignedInt {
			t.Errorf("ReturnType = %s, want UNSIGNED_INT for a byte conversion", unwrap.ReturnType)
		}
	})

	t.Run("multi-stage pipeline keeps its order", func(t *testing.T) {
		q := resolveQuery(t, "logql",
			`{app="frontend"} |= "error" | json | line_format "{{.message}}"`)

		assertKinds(t, q, "filter", "function", "function")
		if got := fnAt(t, q, 1).Name; got != FuncParseJSON {
			t.Errorf("stage 1 = %q, want %q", got, FuncParseJSON)
		}
		format := fnAt(t, q, 2)
		if format.Name != FuncLineFormat {
			t.Errorf("stage 2 = %q, want %q", format.Name, FuncLineFormat)
		}
		template, ok := format.Args[0].(*ir.LiteralExpr)
		if !ok || template.Value != "{{.message}}" {
			t.Errorf("template = %v, want the Go template verbatim", format.Args[0])
		}
	})

	t.Run("all parser and formatter stages map to function stages", func(t *testing.T) {
		cases := []struct{ query, want string }{
			{`{a="b"} | json`, FuncParseJSON},
			{`{a="b"} | logfmt`, FuncParseLogfmt},
			{`{a="b"} | regexp "(?P<x>.*)"`, FuncParseRegexp},
			{`{a="b"} | pattern "<x>"`, FuncParsePattern},
			{`{a="b"} | unpack`, FuncParseUnpack},
			{`{a="b"} | line_format "x"`, FuncLineFormat},
			{`{a="b"} | label_format dst=src`, FuncLabelFormat},
			{`{a="b"} | drop level`, FuncDropLabels},
			{`{a="b"} | keep level`, FuncKeepLabels},
			{`{a="b"} | decolorize`, FuncDecolorize},
		}
		for _, c := range cases {
			t.Run(c.want, func(t *testing.T) {
				q := resolveQuery(t, "logql", c.query)
				if got := fnAt(t, q, 0).Name; got != c.want {
					t.Errorf("Name = %q, want %q", got, c.want)
				}
			})
		}
	})

	t.Run("parameterised vector aggregation", func(t *testing.T) {
		q := resolveQuery(t, "logql", `topk(3, sum by (x) (rate({a="b"}[5m])))`)

		assertKinds(t, q, "aggregation", "aggregation", "aggregation")
		top := aggAt(t, q, 2)
		if top.Op != ir.AggTopK || top.Scope != ir.AggScopeGroup {
			t.Errorf("stage = %s/%s, want TOPK/GROUP", top.Op, top.Scope)
		}
		param, ok := top.Parameter.(*ir.LiteralExpr)
		if !ok || param.Value != float64(3) {
			t.Errorf("Parameter = %v, want 3", top.Parameter)
		}
	})

	t.Run("function with no IR aggregation stays a function", func(t *testing.T) {
		// bytes_rate measures payload size, not entry count, so the registry
		// deliberately gives it no ir_kind.
		q := resolveQuery(t, "logql", `bytes_rate({a="b"}[5m])`)

		assertKinds(t, q, "function")
		if got := fnAt(t, q, 0).Name; got != "bytes_rate" {
			t.Errorf("Name = %q, want bytes_rate", got)
		}
		if got := step(t, q); got != 5*time.Minute {
			t.Errorf("Window.Step = %s, want 5m even for a non-aggregation range", got)
		}
	})

	t.Run("offset", func(t *testing.T) {
		q := resolveQuery(t, "logql", `count_over_time({a="b"}[5m] offset 1h)`)
		if got := q.Output.Window.Offset.Duration(); got != time.Hour {
			t.Errorf("Window.Offset = %s, want 1h", got)
		}
	})

	t.Run("a duration keeps the units it was written with", func(t *testing.T) {
		q := resolveQuery(t, "logql", `count_over_time({a="b"}[90m])`)

		window := q.Output.Window
		if got := window.Step.Duration(); got != 90*time.Minute {
			t.Errorf("Step = %s, want 90m", got)
		}
		// Ninety minutes and one hour thirty are the same length, but only one
		// of them is what the author typed.
		if got := window.Step.SourceText; got != "90m" {
			t.Errorf("Step.SourceText = %q, want 90m", got)
		}
	})

	t.Run("range aggregation grouping partitions the stage", func(t *testing.T) {
		q := resolveQuery(t, "logql", `max_over_time({a="b"} | unwrap y [5m]) by (pod)`)

		agg := aggAt(t, q, 1)
		if agg.Scope != ir.AggScopeTemporal {
			t.Errorf("Scope = %s, want TEMPORAL", agg.Scope)
		}
		// The clause partitions the streams before reducing each over time, so
		// it belongs on this stage rather than becoming an invented second one.
		if by := agg.GroupBy; len(by) != 1 || by[0] != "pod" {
			t.Errorf("GroupBy = %v, want [pod]", by)
		}
	})
}

// TestResolveLabelFilterPredicateTree covers boolean label filters becoming a
// predicate tree rather than being flattened.
func TestResolveLabelFilterPredicateTree(t *testing.T) {
	q := resolveQuery(t, "logql", `{a="b"} | duration > 1m and size > 20MB`)

	stage := filterAt(t, q, 0)
	logical, ok := stage.Predicate.(*ir.LogicalPredicate)
	if !ok {
		t.Fatalf("predicate is %T, want *ir.LogicalPredicate", stage.Predicate)
	}
	if logical.Op != ir.LogicalAnd {
		t.Errorf("Op = %s, want AND", logical.Op)
	}
	if len(logical.Operands) != 2 {
		t.Fatalf("got %d operands, want 2", len(logical.Operands))
	}

	// The source spelling of a duration or byte size is kept, so a translated
	// query still reads the way it was written.
	want := []struct{ key, value string }{{"duration", "1m"}, {"size", "20MB"}}
	for i, w := range want {
		leaf, ok := logical.Operands[i].(*ir.MatchPredicate)
		if !ok {
			t.Fatalf("operand %d is %T, want *ir.MatchPredicate", i, logical.Operands[i])
		}
		if leaf.Matcher.Key != w.key || leaf.Matcher.Value != w.value {
			t.Errorf("operand %d = %+v, want %s %s", i, leaf.Matcher, w.key, w.value)
		}
		if leaf.Matcher.Op != ir.MatchGT {
			t.Errorf("operand %d op = %s, want GT", i, leaf.Matcher.Op)
		}
	}

	t.Run("or maps to the OR operator", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | x="1" or y="2"`)
		logical := filterAt(t, q, 0).Predicate.(*ir.LogicalPredicate)
		if logical.Op != ir.LogicalOr {
			t.Errorf("Op = %s, want OR", logical.Op)
		}
	})

	t.Run("comma is a synonym for and", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | x="1", y="2"`)
		logical := filterAt(t, q, 0).Predicate.(*ir.LogicalPredicate)
		if logical.Op != ir.LogicalAnd {
			t.Errorf("Op = %s, want AND for a comma", logical.Op)
		}
	})

	t.Run("nesting is preserved", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | x="1" or (y="2" and z="3")`)
		top := filterAt(t, q, 0).Predicate.(*ir.LogicalPredicate)
		if top.Op != ir.LogicalOr {
			t.Fatalf("top-level op = %s, want OR", top.Op)
		}
		nested, ok := top.Operands[1].(*ir.LogicalPredicate)
		if !ok || nested.Op != ir.LogicalAnd {
			t.Errorf("right operand = %v, want a nested AND", top.Operands[1])
		}
	})
}

// TestCrossDSLStructuralEquivalence is the point of the whole IR: the same
// operation written in two languages must resolve to the same shape, differing
// only where the languages genuinely differ.
func TestCrossDSLStructuralEquivalence(t *testing.T) {
	prom := resolveQuery(t, "promql", `rate(x[5m])`)
	log := resolveQuery(t, "logql", `rate({a="b"}[5m])`)

	for _, q := range []*ir.Query{prom, log} {
		if len(q.Pipeline) != 1 {
			t.Fatalf("pipeline = %v, want a single aggregation\n  %s", stageKinds(q), q)
		}
	}

	promAgg, logAgg := aggAt(t, prom, 0), aggAt(t, log, 0)
	if promAgg.Op != logAgg.Op {
		t.Errorf("Op differs: promql %s, logql %s", promAgg.Op, logAgg.Op)
	}
	if promAgg.Op != ir.AggRate {
		t.Errorf("Op = %s, want RATE", promAgg.Op)
	}
	if promAgg.Scope != logAgg.Scope {
		t.Errorf("Scope differs: promql %s, logql %s", promAgg.Scope, logAgg.Scope)
	}
	if promAgg.Scope != ir.AggScopeTemporal {
		t.Errorf("Scope = %s, want TEMPORAL", promAgg.Scope)
	}
	if step(t, prom) != step(t, log) {
		t.Errorf("Window.Step differs: promql %s, logql %s", step(t, prom), step(t, log))
	}

	// What legitimately differs is the signal and how the source is identified:
	// PromQL names a metric, LogQL selects a stream by its labels.
	if prom.Signal == log.Signal {
		t.Error("the two should differ in signal type")
	}
	if prom.Signal != ir.SignalMetric || log.Signal != ir.SignalLog {
		t.Errorf("signals = %s and %s, want METRIC and LOG", prom.Signal, log.Signal)
	}
	if prom.Source.Name == "" {
		t.Error("a PromQL source is named")
	}
	if log.Source.Name != "" {
		t.Error("a LogQL stream has no name")
	}
}

// TestAllFlagsDefaultToFull covers the resolver's contract with the validator:
// it never downgrades a flag, so every node arrives FULL and any other value can
// only have come from validation.
//
// Walking every node also proves the tree is acyclic — a query that referenced
// itself through a QueryExpr would not terminate here.
func TestAllFlagsDefaultToFull(t *testing.T) {
	queries := []struct{ dsl, query string }{
		{"promql", `up`},
		{"promql", `sum by (job) (rate(http_requests_total{status="500"}[5m] offset 1h))`},
		{"promql", `histogram_quantile(0.99, sum by (le) (rate(bucket[5m])))`},
		{"promql", `a / on (job) group_left (env) b`},
		{"promql", `http_requests_total / http_requests_failed`},
		{"promql", `rate(x[5m])[30m:1m]`},
		{"promql", `up > bool 5`},
		{"logql", `{app="frontend"}`},
		{"logql", `{app="frontend"} |= "error" | json | status >= 400 | line_format "{{.m}}"`},
		{"logql", `avg_over_time({a="b"} | json | unwrap duration [5m])`},
		{"logql", `topk(3, sum by (x) (rate({a="b"}[5m])))`},
		{"logql", `{a="b"} | x="1" or (y="2" and z="3")`},
	}

	for _, c := range queries {
		t.Run(c.dsl+" "+c.query, func(t *testing.T) {
			q := resolveQuery(t, c.dsl, c.query)

			nodes := 0
			ir.Inspect(q, func(n ir.Node) bool {
				nodes++
				flag, reason := n.Base().Translatability()
				if flag != ir.TranslatabilityFull {
					t.Errorf("%T carries flag %s (%q); the resolver must leave every node FULL",
						n, flag, reason)
				}
				if reason != "" {
					t.Errorf("%T carries reason %q; only the validator sets one", n, reason)
				}
				return true
			})
			if nodes == 0 {
				t.Fatal("walked no nodes")
			}
			if worst, _ := ir.WorstTranslatability(q); worst != ir.TranslatabilityFull {
				t.Errorf("subtree fidelity = %s, want FULL before validation", worst)
			}
		})
	}
}

// TestResolveRecordsSourceDSL covers the hint that tells a later stage which
// language the tree came from.
func TestResolveRecordsSourceDSL(t *testing.T) {
	for _, dsl := range []string{"promql", "logql"} {
		query := `up`
		if dsl == "logql" {
			query = `{a="b"}`
		}
		q := resolveQuery(t, dsl, query)
		if got := q.Hints[HintSourceDSL]; got != dsl {
			t.Errorf("%s: hint = %q, want %q", dsl, got, dsl)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	reg := testRegistry(t)

	t.Run("nil node", func(t *testing.T) {
		if _, err := Resolve(nil, "promql", reg); err == nil {
			t.Error("expected an error for a nil node")
		}
	})

	t.Run("nil registry", func(t *testing.T) {
		p, _ := parser.Get("promql")
		node, _ := p.Parse(`up`)
		if _, err := Resolve(node, "promql", nil); err == nil {
			t.Error("expected an error for a nil registry")
		}
	})

	t.Run("empty DSL name", func(t *testing.T) {
		p, _ := parser.Get("promql")
		node, _ := p.Parse(`up`)
		if _, err := Resolve(node, "", reg); err == nil {
			t.Error("expected an error for an empty DSL name")
		}
	})

	t.Run("tree paired with the wrong DSL", func(t *testing.T) {
		p, _ := parser.Get("promql")
		node, _ := p.Parse(`up`)
		_, err := Resolve(node, "logql", reg)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "promql AST as logql") {
			t.Errorf("error %q should name both DSLs", err)
		}
	})

	t.Run("unregistered DSL", func(t *testing.T) {
		p, _ := parser.Get("promql")
		node, _ := p.Parse(`up`)
		if _, err := Resolve(node, "traceql", reg); err == nil {
			t.Error("expected an error for a DSL with no registry definition")
		}
	})

	t.Run("registry missing a function", func(t *testing.T) {
		// A definition the parser accepts but the registry does not describe
		// must fail loudly here rather than resolve to something empty.
		defs, err := registry.LoadEmbedded()
		if err != nil {
			t.Fatal(err)
		}
		stripped := *defs["promql"]
		stripped.Functions = map[string]*registry.FunctionDef{}
		partial := registry.New(map[string]*registry.DSLDefinition{"promql": &stripped})

		p, _ := parser.Get("promql")
		node, _ := p.Parse(`rate(x[5m])`)
		_, err = Resolve(node, "promql", partial)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "rate") {
			t.Errorf("error %q should name the missing function", err)
		}
	})
}

// unknownNode is an ast.Node from no DSL the resolver knows.
type unknownNode struct{}

func (unknownNode) String() string { return "unknown" }
func (unknownNode) DSL() string    { return "mysteryql" }

// TestResolveRejectsUnknownNodeType covers the dispatch failing loudly, matching
// ir.Walk's refusal to silently skip a node type it does not handle.
func TestResolveRejectsUnknownNodeType(t *testing.T) {
	defs, err := registry.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	// Register the unknown DSL so the failure is the node type, not the lookup.
	stub := *defs["promql"]
	stub.DSL = "mysteryql"
	reg := registry.New(map[string]*registry.DSLDefinition{"mysteryql": &stub})

	_, err = Resolve(unknownNode{}, "mysteryql", reg)
	if err == nil {
		t.Fatal("expected an error for an unhandled node type")
	}
	if !strings.Contains(err.Error(), "no resolver handles") {
		t.Errorf("error %q should say the node type is unhandled", err)
	}
}

// TestResolveBinaryExpressionsInBothDSLs covers the binary family for LogQL as
// well as PromQL. LogQL borrows PromQL's operator set, so both resolve through
// the same three shapes — join, value filter, or function stage.
func TestResolveBinaryExpressionsInBothDSLs(t *testing.T) {
	t.Run("logql arithmetic", func(t *testing.T) {
		q := resolveQuery(t, "logql",
			`sum(rate({app="frontend"}[5m])) / sum(rate({app="backend"}[5m]))`)

		assertKinds(t, q, "binary")
		bin := binAt(t, q, 0)
		if bin.Op != ir.ArithDiv {
			t.Errorf("Op = %s, want DIV", bin.Op)
		}
		if bin.Left == nil || bin.Right == nil {
			t.Fatal("both operands should be sub-queries")
		}
		if q.Signal != ir.SignalLog {
			t.Errorf("Signal = %s, want LOG", q.Signal)
		}
	})

	t.Run("logql scalar operand becomes a literal query", func(t *testing.T) {
		q := resolveQuery(t, "logql", `2 * sum(rate({a="b"}[5m]))`)

		bin := binAt(t, q, 0)
		if bin.Op != ir.ArithMul {
			t.Errorf("Op = %s, want MUL", bin.Op)
		}
		// A bare scalar has no data source, so it becomes a query holding just
		// the literal.
		literalStage := fnAt(t, bin.Left, 0)
		if literalStage.Name != ir.FuncLiteral {
			t.Fatalf("left operand stage = %q, want %q", literalStage.Name, ir.FuncLiteral)
		}
		literal, ok := literalStage.Args[0].(*ir.LiteralExpr)
		if !ok || literal.Value != float64(2) || literal.Type != ir.DataTypeDouble {
			t.Errorf("left operand = %v, want the DOUBLE 2", literalStage.Args[0])
		}
	})

	t.Run("logql join", func(t *testing.T) {
		q := resolveQuery(t, "logql",
			`sum(rate({a="b"}[5m])) / on (x) group_left (y) sum(rate({c="d"}[5m]))`)

		assertKinds(t, q, "aggregation", "aggregation", "join", "binary")
		join := q.Pipeline[2].(*ir.JoinStage)
		if join.JoinType != ir.JoinLeftOuter {
			t.Errorf("JoinType = %s, want LEFT_OUTER for group_left", join.JoinType)
		}
		if len(join.OnLabels) != 1 || join.OnLabels[0] != "x" {
			t.Errorf("OnLabels = %v, want [x]", join.OnLabels)
		}
		if got := join.IncludeLabels; len(got) != 1 || got[0] != "y" {
			t.Errorf("IncludeLabels = %v, want [y]", got)
		}
		if join.RightSide == nil {
			t.Error("RightSide should hold the resolved right-hand query")
		}
	})

	t.Run("logql comparison against a scalar filters on the value", func(t *testing.T) {
		q := resolveQuery(t, "logql", `rate({a="b"}[5m]) > 5`)

		assertKinds(t, q, "aggregation", "filter")
		m := matcherOf(t, filterAt(t, q, 1))
		if m.Key != FieldValue || m.Op != ir.MatchGT || m.Value != "5" {
			t.Errorf("matcher = %+v, want value GT 5", m)
		}
	})

	t.Run("logql bool modifier is recorded", func(t *testing.T) {
		q := resolveQuery(t, "logql", `rate({a="b"}[5m]) > bool 5`)
		if !filterAt(t, q, 1).ReturnsBool {
			t.Error("ReturnsBool should record the modifier")
		}
	})

	t.Run("operator names are DSL-neutral", func(t *testing.T) {
		// The same operator written in either language must reach the IR under
		// the same name, or an emitter could not map it back.
		cases := []struct {
			promql, logql string
			want          ir.ArithOp
		}{
			{`a + b`, `sum(rate({x="1"}[5m])) + sum(rate({y="2"}[5m]))`, ir.ArithAdd},
			{`a - b`, `sum(rate({x="1"}[5m])) - sum(rate({y="2"}[5m]))`, ir.ArithSub},
			{`a * b`, `sum(rate({x="1"}[5m])) * sum(rate({y="2"}[5m]))`, ir.ArithMul},
			{`a % b`, `sum(rate({x="1"}[5m])) % sum(rate({y="2"}[5m]))`, ir.ArithMod},
			{`a ^ b`, `sum(rate({x="1"}[5m])) ^ sum(rate({y="2"}[5m]))`, ir.ArithPow},
			{`a and b`, `sum(rate({x="1"}[5m])) and sum(rate({y="2"}[5m]))`, ir.ArithAnd},
			{`a unless b`, `sum(rate({x="1"}[5m])) unless sum(rate({y="2"}[5m]))`, ir.ArithUnless},
		}
		for _, c := range cases {
			t.Run(c.want.String(), func(t *testing.T) {
				for dsl, query := range map[string]string{"promql": c.promql, "logql": c.logql} {
					q := resolveQuery(t, dsl, query)
					bin := binAt(t, q, len(q.Pipeline)-1)
					if bin.Op != c.want {
						t.Errorf("%s: operator = %s, want %s", dsl, bin.Op, c.want)
					}
				}
			})
		}
	})
}

func TestResolveUnaryParenAndLiterals(t *testing.T) {
	cases := []struct{ dsl, query string }{
		{"promql", `-up`},
		{"logql", `-sum(rate({a="b"}[5m]))`},
	}
	for _, c := range cases {
		t.Run(c.dsl+" unary", func(t *testing.T) {
			q := resolveQuery(t, c.dsl, c.query)
			stage := unaryAt(t, q, len(q.Pipeline)-1)
			if stage.Op != ir.ArithNeg {
				t.Errorf("Op = %s, want %s", stage.Op, ir.ArithNeg)
			}
		})
	}

	t.Run("promql parentheses are recorded, not structural", func(t *testing.T) {
		q := resolveQuery(t, "promql", `(up)`)
		if q.Hints[HintParen] != "true" {
			t.Errorf("hints = %v, want the paren hint", q.Hints)
		}
		// The grouping is already in the tree's shape, so no stage is added.
		if len(q.Pipeline) != 0 {
			t.Errorf("pipeline = %v, want empty", stageKinds(q))
		}
		if q.Source.Name != "up" {
			t.Errorf("Source.Name = %q, want up", q.Source.Name)
		}
	})

	t.Run("logql parentheses", func(t *testing.T) {
		q := resolveQuery(t, "logql", `(sum(rate({a="b"}[5m])))`)
		if q.Hints[HintParen] != "true" {
			t.Errorf("hints = %v, want the paren hint", q.Hints)
		}
	})

	t.Run("promql bare literal", func(t *testing.T) {
		q := resolveQuery(t, "promql", `42`)
		fn := fnAt(t, q, 0)
		if fn.Name != FuncLiteral {
			t.Errorf("Name = %q, want %q", fn.Name, FuncLiteral)
		}
		if literal, ok := fn.Args[0].(*ir.LiteralExpr); !ok || literal.Value != float64(42) {
			t.Errorf("args[0] = %v, want 42", fn.Args[0])
		}
		if q.Source != nil {
			t.Errorf("Source = %+v, want nil for a bare scalar", q.Source)
		}
	})

	t.Run("logql bare literal", func(t *testing.T) {
		q := resolveQuery(t, "logql", `42`)
		if got := fnAt(t, q, 0).Name; got != FuncLiteral {
			t.Errorf("Name = %q, want %q", got, FuncLiteral)
		}
	})

	t.Run("promql function taking no series", func(t *testing.T) {
		q := resolveQuery(t, "promql", `time()`)
		fn := fnAt(t, q, 0)
		if fn.Name != "time" {
			t.Errorf("Name = %q, want time", fn.Name)
		}
		if fn.ReturnType != ir.DataTypeTimestamp {
			t.Errorf("ReturnType = %s, want TIMESTAMP from the registry", fn.ReturnType)
		}
		if q.Source != nil {
			t.Errorf("Source = %+v, want nil for a function over no series", q.Source)
		}
	})

	t.Run("logql label_replace", func(t *testing.T) {
		q := resolveQuery(t, "logql",
			`label_replace(rate({a="b"}[5m]), "dst", "$1", "src", "(.*)")`)

		fn := fnAt(t, q, len(q.Pipeline)-1)
		if fn.Name != "label_replace" {
			t.Errorf("Name = %q", fn.Name)
		}
		if len(fn.Args) != 4 {
			t.Fatalf("got %d args, want four strings", len(fn.Args))
		}
		if literal, ok := fn.Args[0].(*ir.LiteralExpr); !ok || literal.Value != "dst" {
			t.Errorf("args[0] = %v, want dst", fn.Args[0])
		}
	})
}

// TestResolveLogQLStageArguments covers the argument shapes of the stages that
// carry more than a name, where a label reference and a string literal mean
// different things.
func TestResolveLogQLStageArguments(t *testing.T) {
	t.Run("json extraction parameters", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | json first="servers[0]", second`)

		fn := fnAt(t, q, 0)
		if len(fn.Args) != 3 {
			t.Fatalf("got %d args, want the assigned pair and the bare name", len(fn.Args))
		}
		if name, ok := fn.Args[0].(*ir.LiteralExpr); !ok || name.Value != "first" {
			t.Errorf("args[0] = %v, want the label name", fn.Args[0])
		}
		if expr, ok := fn.Args[1].(*ir.LiteralExpr); !ok || expr.Value != "servers[0]" {
			t.Errorf("args[1] = %v, want the extraction expression", fn.Args[1])
		}
		// A bare name names an existing label, which is a reference.
		ref, ok := fn.Args[2].(*ir.RefExpr)
		if !ok {
			t.Fatalf("args[2] is %T, want *ir.RefExpr", fn.Args[2])
		}
		if ref.Name != "second" {
			t.Errorf("args[2] = %q, want second", ref.Name)
		}
	})

	t.Run("logfmt flags", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | logfmt --strict host`)
		fn := fnAt(t, q, 0)
		if fn.Name != FuncParseLogfmt {
			t.Errorf("Name = %q", fn.Name)
		}
		if flag, ok := fn.Args[0].(*ir.LiteralExpr); !ok || flag.Value != "--strict" {
			t.Errorf("args[0] = %v, want the flag", fn.Args[0])
		}
	})

	t.Run("regexp pattern", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | regexp "(?P<method>\\w+)"`)
		fn := fnAt(t, q, 0)
		if pattern, ok := fn.Args[0].(*ir.LiteralExpr); !ok || pattern.Value != `(?P<method>\w+)` {
			t.Errorf("args[0] = %v, want the pattern verbatim", fn.Args[0])
		}
	})

	t.Run("label_format distinguishes a rename from a template", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | label_format dst=src, other="{{.x}}"`)

		fn := fnAt(t, q, 0)
		if len(fn.Args) != 4 {
			t.Fatalf("got %d args, want two pairs", len(fn.Args))
		}
		// A rename names a source label, so it is a reference.
		if _, ok := fn.Args[1].(*ir.RefExpr); !ok {
			t.Errorf("args[1] is %T, want *ir.RefExpr for a rename", fn.Args[1])
		}
		// A template is opaque text.
		if template, ok := fn.Args[3].(*ir.LiteralExpr); !ok || template.Value != "{{.x}}" {
			t.Errorf("args[3] = %v, want the template literal", fn.Args[3])
		}
	})

	t.Run("drop with a matcher", func(t *testing.T) {
		q := resolveQuery(t, "logql", `{a="b"} | drop level, method="GET"`)

		fn := fnAt(t, q, 0)
		if fn.Name != FuncDropLabels {
			t.Errorf("Name = %q", fn.Name)
		}
		if len(fn.Args) != 4 {
			t.Fatalf("got %d args, want the bare name plus the matcher triple", len(fn.Args))
		}
		if ref, ok := fn.Args[0].(*ir.RefExpr); !ok || ref.Name != "level" {
			t.Errorf("args[0] = %v, want a reference to level", fn.Args[0])
		}
		if op, ok := fn.Args[2].(*ir.LiteralExpr); !ok || op.Value != "=" {
			t.Errorf("args[2] = %v, want the matcher operator", fn.Args[2])
		}
	})

	t.Run("post-unwrap filters run after the coercion", func(t *testing.T) {
		q := resolveQuery(t, "logql", `avg_over_time({a="b"} | unwrap x | __error__="" [5m])`)

		assertKinds(t, q, "function", "filter", "aggregation")
		if got := fnAt(t, q, 0).Name; got != FuncUnwrap {
			t.Errorf("stage 0 = %q, want the unwrap", got)
		}
		if m := matcherOf(t, filterAt(t, q, 1)); m.Key != "__error__" {
			t.Errorf("stage 1 filters on %q, want __error__", m.Key)
		}
	})
}
