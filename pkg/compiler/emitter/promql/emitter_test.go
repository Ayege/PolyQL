package promql

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/emitter"
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

func emit(t *testing.T, query *ir.Query) string {
	t.Helper()
	text, err := Emitter{}.Emit(query, testRegistry(t))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return text
}

// selector builds a query over one named metric with the given matchers.
func selector(name string, matchers ...*ir.LabelMatcher) *ir.Query {
	q := &ir.Query{
		Signal: ir.SignalMetric,
		Source: &ir.DataSource{Name: name, Scope: ir.ScopeUnscoped},
		Output: &ir.Output{},
	}
	if len(matchers) > 0 {
		q.Source.Selectors = []*ir.Selector{{Matchers: matchers}}
	}
	return q
}

func matcher(key string, op ir.MatchOp, value string) *ir.LabelMatcher {
	return &ir.LabelMatcher{Key: key, Op: op, Value: value}
}

func withWindow(q *ir.Query, step time.Duration) *ir.Query {
	q.Output.Window = &ir.Window{Step: ir.NewInterval(step)}
	return q
}

func stage(q *ir.Query, stages ...ir.PipelineStage) *ir.Query {
	q.Pipeline = append(q.Pipeline, stages...)
	return q
}

// binary builds a query applying an operator to two sub-queries.
func binary(op ir.ArithOp, left, right *ir.Query) *ir.Query {
	q := &ir.Query{Signal: ir.SignalMetric, Output: &ir.Output{}}
	return stage(q, &ir.BinaryOpStage{Op: op, Left: left, Right: right})
}

// literalQuery builds a query holding a bare scalar.
func literalQuery(v float64) *ir.Query {
	q := &ir.Query{Signal: ir.SignalMetric, Output: &ir.Output{}}
	return stage(q, &ir.FunctionStage{
		Name: ir.FuncLiteral, Args: []ir.IRExpr{ir.NewNumberLiteral(v)},
		ReturnType: ir.DataTypeDouble,
	})
}

func agg(op ir.AggOp, scope ir.AggScope) *ir.AggregationStage {
	return &ir.AggregationStage{Op: op, Scope: scope}
}

// TestEmitStructures covers rendering IR built by hand, so the emitter is
// exercised without the parsers or the resolver.
func TestEmitStructures(t *testing.T) {
	cases := []struct {
		name  string
		query *ir.Query
		want  string
	}{
		{
			name:  "bare selector",
			query: selector("up"),
			want:  `up`,
		},
		{
			name:  "selector with one matcher",
			query: selector("http_requests_total", matcher("status", ir.MatchEQ, "500")),
			want:  `http_requests_total{status="500"}`,
		},
		{
			name: "all four matcher operators",
			query: selector("m",
				matcher("a", ir.MatchEQ, "1"),
				matcher("b", ir.MatchNEQ, "2"),
				matcher("c", ir.MatchRegex, "3"),
				matcher("d", ir.MatchNotRegex, "4")),
			want: `m{a="1",b!="2",c=~"3",d!~"4"}`,
		},
		{
			name: "matcher with no metric name",
			query: func() *ir.Query {
				return selector("", matcher("job", ir.MatchEQ, "api"))
			}(),
			want: `{job="api"}`,
		},
		{
			name: "temporal aggregation attaches the window",
			query: stage(withWindow(selector("http_requests_total",
				matcher("status", ir.MatchEQ, "500")), 5*time.Minute),
				agg(ir.AggRate, ir.AggScopeTemporal)),
			want: `rate(http_requests_total{status="500"}[5m])`,
		},
		{
			name: "group aggregation writes its clause first",
			query: stage(selector("up"),
				&ir.AggregationStage{Op: ir.AggSum, Scope: ir.AggScopeGroup, GroupBy: []string{"job"}}),
			want: `sum by (job) (up)`,
		},
		{
			name: "without clause",
			query: stage(selector("up"),
				&ir.AggregationStage{Op: ir.AggAvg, Scope: ir.AggScopeGroup, Without: []string{"pod"}}),
			want: `avg without (pod) (up)`,
		},
		{
			name: "nested aggregations unwind outward",
			query: stage(withWindow(selector("http_requests_total"), 5*time.Minute),
				agg(ir.AggRate, ir.AggScopeTemporal),
				&ir.AggregationStage{Op: ir.AggSum, Scope: ir.AggScopeGroup, GroupBy: []string{"job"}}),
			want: `sum by (job) (rate(http_requests_total[5m]))`,
		},
		{
			name: "parameterised aggregation",
			query: stage(selector("http_requests_total"),
				&ir.AggregationStage{
					Op: ir.AggTopK, Scope: ir.AggScopeGroup, Parameter: ir.NewNumberLiteral(5),
				}),
			want: `topk(5, http_requests_total)`,
		},
		{
			name: "three levels deep",
			query: stage(withWindow(selector("bucket"), 5*time.Minute),
				agg(ir.AggRate, ir.AggScopeTemporal),
				&ir.AggregationStage{Op: ir.AggSum, Scope: ir.AggScopeGroup, GroupBy: []string{"le"}},
				&ir.AggregationStage{
					Op: ir.AggHistogramQuantile, Scope: ir.AggScopeGroup,
					Parameter: ir.NewNumberLiteral(0.99),
				}),
			want: `histogram_quantile(0.99, sum by (le) (rate(bucket[5m])))`,
		},
		{
			name: "set membership becomes a regex alternation",
			query: selector("m", &ir.LabelMatcher{
				Key: "status", Op: ir.MatchIn, Values: []string{"500", "503"},
			}),
			want: `m{status=~"500|503"}`,
		},
		{
			name: "alternation values are regex-escaped",
			query: selector("m", &ir.LabelMatcher{
				Key: "path", Op: ir.MatchIn, Values: []string{"/a.b", "/c+d"},
			}),
			want: `m{path=~"/a\\.b|/c\\+d"}`,
		},
		{
			name: "offset attaches inside the range",
			query: func() *ir.Query {
				q := withWindow(selector("x"), 5*time.Minute)
				q.Output.Window.Offset = ir.NewInterval(time.Hour)
				return stage(q, agg(ir.AggRate, ir.AggScopeTemporal))
			}(),
			want: `rate(x[5m] offset 1h)`,
		},
		{
			name: "function stage renders as a call",
			query: stage(selector("x"),
				&ir.FunctionStage{Name: "abs", ReturnType: ir.DataTypeDouble}),
			want: `abs(x)`,
		},
		{
			name: "function arguments follow the operand",
			query: stage(selector("up"), &ir.FunctionStage{
				Name: "label_replace",
				Args: []ir.IRExpr{
					ir.NewStringLiteral("dst"), ir.NewStringLiteral("$1"),
					ir.NewStringLiteral("src"), ir.NewStringLiteral("(.*)"),
				},
				ReturnType: ir.DataTypeDouble,
			}),
			want: `label_replace(up, "dst", "$1", "src", "(.*)")`,
		},
		{
			name: "value comparison becomes a binary expression",
			query: stage(selector("up"),
				&ir.FilterStage{Predicate: &ir.MatchPredicate{
					Matcher: matcher(ir.FieldValue, ir.MatchGT, "5"),
				}}),
			want: `up > 5`,
		},
		{
			name: "leading label filters fold into the selector",
			query: stage(selector("up", matcher("job", ir.MatchEQ, "api")),
				&ir.FilterStage{Predicate: &ir.MatchPredicate{
					Matcher: matcher("env", ir.MatchEQ, "prod"),
				}}),
			want: `up{job="api",env="prod"}`,
		},
		{
			name: "a conjunction folds into comma-separated matchers",
			query: stage(selector("up"),
				&ir.FilterStage{Predicate: &ir.LogicalPredicate{
					Op: ir.LogicalAnd,
					Operands: []ir.Predicate{
						&ir.MatchPredicate{Matcher: matcher("a", ir.MatchEQ, "1")},
						&ir.MatchPredicate{Matcher: matcher("b", ir.MatchEQ, "2")},
					},
				}}),
			want: `up{a="1",b="2"}`,
		},
		{
			name: "a disjunction on one label collapses to an alternation",
			query: stage(selector("up"),
				&ir.FilterStage{Predicate: &ir.LogicalPredicate{
					Op: ir.LogicalOr,
					Operands: []ir.Predicate{
						&ir.MatchPredicate{Matcher: matcher("level", ir.MatchEQ, "error")},
						&ir.MatchPredicate{Matcher: matcher("level", ir.MatchEQ, "warn")},
					},
				}}),
			want: `up{level=~"error|warn"}`,
		},
		{
			name: "binary operands are not over-parenthesised",
			query: func() *ir.Query {
				left := stage(withWindow(selector("a"), 5*time.Minute), agg(ir.AggRate, ir.AggScopeTemporal))
				right := stage(withWindow(selector("b"), 5*time.Minute), agg(ir.AggRate, ir.AggScopeTemporal))
				return binary(ir.ArithDiv, left, right)
			}(),
			want: `rate(a[5m]) / rate(b[5m])`,
		},
		{
			name: "a looser operand is grouped",
			query: binary(ir.ArithMul,
				binary(ir.ArithAdd, selector("a"), selector("b")),
				literalQuery(2)),
			want: `(a + b) * 2`,
		},
		{
			name: "a tighter operand needs no grouping",
			query: binary(ir.ArithAdd,
				selector("a"),
				binary(ir.ArithMul, selector("b"), selector("c"))),
			want: `a + b * c`,
		},
		{
			name: "a left-associative operator groups only on the right",
			query: binary(ir.ArithSub,
				binary(ir.ArithSub, selector("a"), selector("b")),
				selector("c")),
			want: `a - b - c`,
		},
		{
			name: "the same operator on the right keeps its grouping",
			query: binary(ir.ArithSub,
				selector("a"),
				binary(ir.ArithSub, selector("b"), selector("c"))),
			want: `a - (b - c)`,
		},
		{
			name: "exponentiation groups to the right",
			query: binary(ir.ArithPow,
				selector("a"),
				binary(ir.ArithPow, selector("b"), selector("c"))),
			want: `a ^ b ^ c`,
		},
		{
			name:  "a set operator renders as a keyword",
			query: binary(ir.ArithUnless, selector("a"), selector("b")),
			want:  `a unless b`,
		},
		{
			name: "a filter with the bool modifier keeps it",
			query: func() *ir.Query {
				f := &ir.FilterStage{
					Predicate:   &ir.MatchPredicate{Matcher: matcher(ir.FieldValue, ir.MatchGT, "5")},
					ReturnsBool: true,
				}
				return stage(selector("up"), f)
			}(),
			want: `up > bool 5`,
		},
		{
			name: "a duration keeps the units it was written with",
			query: func() *ir.Query {
				q := selector("x")
				q.Output.Window = &ir.Window{Step: ir.NewIntervalFromSource(90*time.Minute, "90m")}
				return stage(q, agg(ir.AggRate, ir.AggScopeTemporal))
			}(),
			want: `rate(x[90m])`,
		},
		{
			name: "a duration with no recorded spelling is decomposed",
			query: func() *ir.Query {
				q := selector("x")
				q.Output.Window = &ir.Window{Step: ir.NewInterval(90 * time.Minute)}
				return stage(q, agg(ir.AggRate, ir.AggScopeTemporal))
			}(),
			want: `rate(x[1h30m])`,
		},
		{
			name: "a fractional spelling PromQL cannot write is decomposed",
			query: func() *ir.Query {
				q := selector("x")
				// LogQL writes 1.5h; PromQL has no such form.
				q.Output.Window = &ir.Window{Step: ir.NewIntervalFromSource(90*time.Minute, "1.5h")}
				return stage(q, agg(ir.AggRate, ir.AggScopeTemporal))
			}(),
			want: `rate(x[1h30m])`,
		},
		{
			name:  "empty pipeline renders just the selector",
			query: selector("up", matcher("job", ir.MatchEQ, "api")),
			want:  `up{job="api"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := emit(t, c.query); got != c.want {
				t.Errorf("Emit():\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestEmitMarksUnsupportedNodes covers the validator's verdict reaching the
// output. Neither target language has a block comment, so a note cannot sit
// inline; it is written as a full comment line above the query, which keeps the
// rendered text parseable.
func TestEmitMarksUnsupportedNodes(t *testing.T) {
	query := stage(withWindow(selector("bucket"), 5*time.Minute),
		agg(ir.AggRate, ir.AggScopeTemporal),
		agg(ir.AggSum, ir.AggScopeGroup))
	query.Pipeline[1].Base().SetTranslatability(
		ir.TranslatabilityUnsupported, "sum is not available in the target")

	got := emit(t, query)

	if !strings.Contains(got, "# UNSUPPORTED: sum is not available in the target") {
		t.Errorf("the reason should appear as a comment:\n%s", got)
	}
	// The stage was dropped, but what it wrapped is still valid on its own.
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; last != `rate(bucket[5m])` {
		t.Errorf("query line = %q, want the inner expression", last)
	}
}

func TestEmitErrors(t *testing.T) {
	t.Run("nil query", func(t *testing.T) {
		if _, err := (Emitter{}).Emit(nil, testRegistry(t)); err == nil {
			t.Error("expected an error for a nil query")
		}
	})
	t.Run("nil registry", func(t *testing.T) {
		if _, err := (Emitter{}).Emit(selector("up"), nil); err == nil {
			t.Error("expected an error for a nil registry")
		}
	})
}

// TestEmitterIsRegistered covers init-time registration.
func TestEmitterIsRegistered(t *testing.T) {
	e, err := emitter.Get("promql")
	if err != nil {
		t.Fatalf("emitter.Get: %v", err)
	}
	if e.DSL() != DSL {
		t.Errorf("DSL() = %q, want %q", e.DSL(), DSL)
	}
	if _, err := emitter.Get("  PromQL "); err != nil {
		t.Errorf("lookup should be normalised: %v", err)
	}
}

// TestEmitJoin covers vector matching, where PromQL writes the operator between
// the left operand and the matching clause. The IR records the join and the
// operator as two separate stages, so the emitter has to interleave them.
func TestEmitJoin(t *testing.T) {
	buildJoin := func(joinType ir.JoinType, on, ignoring, include []string) *ir.Query {
		right := stage(withWindow(selector("b"), 5*time.Minute), agg(ir.AggRate, ir.AggScopeTemporal))
		left := stage(withWindow(selector("a"), 5*time.Minute), agg(ir.AggRate, ir.AggScopeTemporal))
		return stage(left,
			&ir.JoinStage{
				JoinType: joinType, OnLabels: on, IgnoreLabels: ignoring,
				IncludeLabels: include, RightSide: right,
			},
			&ir.BinaryOpStage{Op: ir.ArithDiv})
	}

	cases := []struct {
		name     string
		joinType ir.JoinType
		on       []string
		ignoring []string
		include  []string
		want     string
	}{
		{
			name: "inner join on labels", joinType: ir.JoinInner, on: []string{"job"},
			want: `rate(a[5m]) / on (job) rate(b[5m])`,
		},
		{
			name: "left outer becomes group_left", joinType: ir.JoinLeftOuter, on: []string{"job"},
			want: `rate(a[5m]) / on (job) group_left rate(b[5m])`,
		},
		{
			// The labels copied from the one side must survive the round trip.
			name: "group_left keeps its label list", joinType: ir.JoinLeftOuter,
			on: []string{"job"}, include: []string{"env"},
			want: `rate(a[5m]) / on (job) group_left (env) rate(b[5m])`,
		},
		{
			name: "group_right keeps its label list", joinType: ir.JoinRightOuter,
			on: []string{"job"}, include: []string{"env", "zone"},
			want: `rate(a[5m]) / on (job) group_right (env, zone) rate(b[5m])`,
		},
		{
			name: "right outer becomes group_right", joinType: ir.JoinRightOuter, on: []string{"job"},
			want: `rate(a[5m]) / on (job) group_right rate(b[5m])`,
		},
		{
			name: "ignoring instead of on", joinType: ir.JoinInner, ignoring: []string{"env"},
			want: `rate(a[5m]) / ignoring (env) rate(b[5m])`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := emit(t, buildJoin(c.joinType, c.on, c.ignoring, c.include))
			if got != c.want {
				t.Errorf("Emit():\n got %s\nwant %s", got, c.want)
			}
		})
	}

	t.Run("a join with no operator is reported", func(t *testing.T) {
		left := selector("a")
		query := stage(left, &ir.JoinStage{
			JoinType: ir.JoinInner, OnLabels: []string{"job"}, RightSide: selector("b"),
		})
		got := emit(t, query)
		if !strings.Contains(got, "UNSUPPORTED") {
			t.Errorf("an unapplied vector match should be reported:\n%s", got)
		}
		// What remains is still valid PromQL.
		lines := strings.Split(got, "\n")
		if last := lines[len(lines)-1]; last != "a" {
			t.Errorf("query line = %q, want the left-hand side", last)
		}
	})
}

func TestEmitUnaryAndSubquery(t *testing.T) {
	t.Run("unary minus", func(t *testing.T) {
		base := selector("up")
		operand := selector("up")
		query := stage(base, &ir.UnaryOpStage{Op: ir.ArithNeg, Operand: operand})
		if got, want := emit(t, query), `-up`; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("unary over a binary expression is grouped", func(t *testing.T) {
		operand := binary(ir.ArithAdd, selector("a"), selector("b"))
		query := stage(&ir.Query{Signal: ir.SignalMetric, Output: &ir.Output{}}, &ir.UnaryOpStage{Op: ir.ArithNeg, Operand: operand})
		if got, want := emit(t, query), `-(a + b)`; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("subquery range and resolution", func(t *testing.T) {
		// All three durations are structural now: the inner window belongs to
		// the aggregation, the outer range and resolution to the subquery.
		query := stage(withWindow(selector("x"), 5*time.Minute), agg(ir.AggRate, ir.AggScopeTemporal))
		outer := ir.NewIntervalFromSource(30*time.Minute, "30m")
		step := ir.NewIntervalFromSource(time.Minute, "1m")
		query.Output.SubqueryRange = &outer
		query.Output.SubqueryStep = &step

		if got, want := emit(t, query), `rate(x[5m])[30m:1m]`; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("a range over an aggregated expression becomes a subquery", func(t *testing.T) {
		// PromQL can only append a range to a selector, so a second temporal
		// aggregation needs subquery syntax.
		query := stage(withWindow(selector("x"), 5*time.Minute),
			agg(ir.AggRate, ir.AggScopeTemporal),
			agg(ir.AggAvg, ir.AggScopeTemporal))
		got := emit(t, query)
		if !strings.Contains(got, "[5m:]") {
			t.Errorf("expected subquery syntax for the outer range:\n%s", got)
		}
	})

	t.Run("a pinned instant becomes an at modifier", func(t *testing.T) {
		query := withWindow(selector("x"), 5*time.Minute)
		pinned := ir.NewTimestamp(time.Unix(1609746000, 0))
		query.Output.Range = &ir.TimeRange{Start: pinned, End: pinned}
		query = stage(query, agg(ir.AggRate, ir.AggScopeTemporal))
		if got, want := emit(t, query), `rate(x[5m] @ 1.609746e+09)`; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
}

// TestNotesAPI covers the note collector the fidelity reporter will read.
func TestNotesAPI(t *testing.T) {
	var notes emitter.Notes
	if notes.Len() != 0 || notes.Lines() != nil {
		t.Error("a fresh collector should be empty")
	}
	notes.AddUnsupported("first")
	notes.AddUnsupported("first")
	notes.Addf("second %d", 2)
	if notes.Len() != 2 {
		t.Errorf("Len() = %d, want 2; an exact duplicate should not repeat", notes.Len())
	}
	if got := notes.Lines()[0]; got != "UNSUPPORTED: first" {
		t.Errorf("Lines()[0] = %q", got)
	}
	if got := notes.Prepend("up"); got != "# UNSUPPORTED: first\n# second 2\nup" {
		t.Errorf("Prepend() = %q", got)
	}
	var empty emitter.Notes
	if got := empty.Prepend("up"); got != "up" {
		t.Errorf("Prepend() with no notes = %q, want the query alone", got)
	}
}

// TestContainmentBecomesAnEscapedPattern covers Fix 3 from the PromQL side.
//
// PromQL has no containment operator, so the test becomes an unanchored
// pattern — and the escaping is the whole point. Without it the dot in
// "error.log" would mean "any character", and the query would also match
// "errorXlog".
func TestContainmentBecomesAnEscapedPattern(t *testing.T) {
	got := emit(t, selector("m", matcher("path", ir.MatchContains, "error.log")))

	wantQuery := `m{path=~".*error\\.log.*"}`
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; last != wantQuery {
		t.Errorf("query line:\n got %s\nwant %s", last, wantQuery)
	}
	if !strings.Contains(got, "PARTIAL") || !strings.Contains(got, "containment") {
		t.Errorf("the approximation should be reported:\n%s", got)
	}

	t.Run("negated containment", func(t *testing.T) {
		got := emit(t, selector("m", matcher("path", ir.MatchNotContains, "debug")))
		if !strings.Contains(got, `m{path!~".*debug.*"}`) {
			t.Errorf("got:\n%s", got)
		}
	})
}
