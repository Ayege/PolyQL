package logql

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

// stream builds a log query over a stream selector.
func stream(matchers ...*ir.LabelMatcher) *ir.Query {
	q := &ir.Query{
		Signal: ir.SignalLog,
		Source: &ir.DataSource{Name: "", Scope: ir.ScopeUnscoped},
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

func agg(op ir.AggOp, scope ir.AggScope) *ir.AggregationStage {
	return &ir.AggregationStage{Op: op, Scope: scope}
}

func fn(name string, args ...ir.IRExpr) *ir.FunctionStage {
	return &ir.FunctionStage{Name: name, Args: args, ReturnType: ir.DataTypeString}
}

func filter(key string, op ir.MatchOp, value string) *ir.FilterStage {
	return &ir.FilterStage{Predicate: &ir.MatchPredicate{Matcher: matcher(key, op, value)}}
}

func TestEmitStructures(t *testing.T) {
	cases := []struct {
		name  string
		query *ir.Query
		want  string
	}{
		{
			name:  "stream selector has no name before the braces",
			query: stream(matcher("app", ir.MatchEQ, "frontend")),
			want:  `{app="frontend"}`,
		},
		{
			name: "several matchers are comma-space separated",
			query: stream(
				matcher("app", ir.MatchEQ, "frontend"),
				matcher("env", ir.MatchNEQ, "dev")),
			want: `{app="frontend", env!="dev"}`,
		},
		{
			name: "a containment line filter",
			query: stage(stream(matcher("app", ir.MatchEQ, "frontend")),
				filter(ir.FieldBody, ir.MatchContains, "error")),
			want: `{app="frontend"} |= "error"`,
		},
		{
			// The IR names containment outright, so a metacharacter in the text
			// changes nothing: this is still a literal substring test.
			name: "containment keeps a metacharacter literal",
			query: stage(stream(matcher("app", ir.MatchEQ, "x")),
				filter(ir.FieldBody, ir.MatchContains, "error.log")),
			want: `{app="x"} |= "error.log"`,
		},
		{
			name: "a regex line filter",
			query: stage(stream(matcher("app", ir.MatchEQ, "x")),
				filter(ir.FieldBody, ir.MatchRegex, "err.*")),
			want: `{app="x"} |~ "err.*"`,
		},
		{
			name: "negated line filters",
			query: stage(stream(matcher("a", ir.MatchEQ, "b")),
				filter(ir.FieldBody, ir.MatchNotContains, "debug"),
				filter(ir.FieldBody, ir.MatchNotRegex, "trace.*")),
			want: `{a="b"} != "debug" !~ "trace.*"`,
		},
		{
			name: "a duration keeps the units it was written with",
			query: func() *ir.Query {
				q := stream(matcher("a", ir.MatchEQ, "b"))
				q.Output.Window = &ir.Window{Step: ir.NewIntervalFromSource(90*time.Minute, "90m")}
				return stage(q, agg(ir.AggCount, ir.AggScopeTemporal))
			}(),
			want: `count_over_time({a="b"}[90m])`,
		},
		{
			name: "a duration with no recorded spelling is decomposed",
			query: func() *ir.Query {
				q := stream(matcher("a", ir.MatchEQ, "b"))
				q.Output.Window = &ir.Window{Step: ir.NewInterval(90 * time.Minute)}
				return stage(q, agg(ir.AggCount, ir.AggScopeTemporal))
			}(),
			want: `count_over_time({a="b"}[1h30m])`,
		},
		{
			// LogQL admits fractional durations, so its own spelling survives.
			name: "a fractional spelling survives",
			query: func() *ir.Query {
				q := stream(matcher("a", ir.MatchEQ, "b"))
				q.Output.Window = &ir.Window{Step: ir.NewIntervalFromSource(90*time.Minute, "1.5h")}
				return stage(q, agg(ir.AggCount, ir.AggScopeTemporal))
			}(),
			want: `count_over_time({a="b"}[1.5h])`,
		},
		{
			name: "parser stage then a numeric label filter",
			query: stage(stream(matcher("app", ir.MatchEQ, "frontend")),
				fn("parse_json"),
				filter("status", ir.MatchGTE, "400")),
			// A numeric comparison is spaced and unquoted; quoting it would
			// make the comparison lexical.
			want: `{app="frontend"} | json | status >= 400`,
		},
		{
			name: "a string label filter is written tight",
			query: stage(stream(matcher("app", ir.MatchEQ, "frontend")),
				fn("parse_logfmt"),
				filter("level", ir.MatchEQ, "error")),
			want: `{app="frontend"} | logfmt | level="error"`,
		},
		{
			name: "duration and byte operands keep their source units",
			query: stage(stream(matcher("a", ir.MatchEQ, "b")),
				&ir.FilterStage{Predicate: &ir.LogicalPredicate{
					Op: ir.LogicalAnd,
					Operands: []ir.Predicate{
						&ir.MatchPredicate{Matcher: matcher("duration", ir.MatchGT, "1m")},
						&ir.MatchPredicate{Matcher: matcher("size", ir.MatchGT, "20MB")},
					},
				}}),
			want: `{a="b"} | duration > 1m and size > 20MB`,
		},
		{
			name: "regexp and pattern parsers take an operand",
			query: stage(stream(matcher("a", ir.MatchEQ, "b")),
				fn("parse_regexp", ir.NewStringLiteral(`(?P<m>\w+)`))),
			want: `{a="b"} | regexp "(?P<m>\\w+)"`,
		},
		{
			name: "line_format takes a template",
			query: stage(stream(matcher("a", ir.MatchEQ, "b")),
				fn("line_format", ir.NewStringLiteral("{{.message}}"))),
			want: `{a="b"} | line_format "{{.message}}"`,
		},
		{
			name: "label_format renames from a reference",
			query: stage(stream(matcher("a", ir.MatchEQ, "b")),
				fn("label_format", ir.NewStringLiteral("lvl"),
					&ir.RefExpr{Name: "level", Type: ir.DataTypeString})),
			want: `{a="b"} | label_format lvl=level`,
		},
		{
			name:  "decolorize takes nothing",
			query: stage(stream(matcher("a", ir.MatchEQ, "b")), fn("decolorize")),
			want:  `{a="b"} | decolorize`,
		},
		{
			name: "a bare selector abuts its range",
			query: stage(withWindow(stream(matcher("app", ir.MatchEQ, "frontend")), time.Hour),
				agg(ir.AggCount, ir.AggScopeTemporal)),
			want: `count_over_time({app="frontend"}[1h])`,
		},
		{
			name: "a pipeline gets a space before the range",
			query: stage(withWindow(stream(matcher("app", ir.MatchEQ, "frontend")), 5*time.Minute),
				filter(ir.FieldBody, ir.MatchContains, "error"),
				agg(ir.AggRate, ir.AggScopeTemporal)),
			want: `rate({app="frontend"} |= "error" [5m])`,
		},
		{
			name: "the log pipeline stays inside the range aggregation",
			query: stage(withWindow(stream(matcher("app", ir.MatchEQ, "frontend")), 5*time.Minute),
				fn("parse_json"),
				fn("unwrap", &ir.RefExpr{Name: "duration", Type: ir.DataTypeString}),
				agg(ir.AggAvg, ir.AggScopeTemporal)),
			want: `avg_over_time({app="frontend"} | json | unwrap duration [5m])`,
		},
		{
			name: "an unwrap conversion wraps the label",
			query: stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
				fn("unwrap", &ir.RefExpr{Name: "size", Type: ir.DataTypeString},
					ir.NewStringLiteral("bytes")),
				agg(ir.AggSum, ir.AggScopeTemporal)),
			want: `sum_over_time({a="b"} | unwrap bytes(size) [5m])`,
		},
		{
			name: "vector aggregation wraps the range aggregation",
			query: stage(withWindow(stream(matcher("app", ir.MatchEQ, "frontend")), time.Hour),
				agg(ir.AggCount, ir.AggScopeTemporal),
				&ir.AggregationStage{Op: ir.AggSum, Scope: ir.AggScopeGroup, GroupBy: []string{"level"}}),
			want: `sum by (level) (count_over_time({app="frontend"}[1h]))`,
		},
		{
			name: "a range aggregation's grouping goes after the call",
			query: stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
				fn("unwrap", &ir.RefExpr{Name: "y", Type: ir.DataTypeString}),
				&ir.AggregationStage{
					Op: ir.AggMax, Scope: ir.AggScopeTemporal, GroupBy: []string{"pod"},
				}),
			want: `max_over_time({a="b"} | unwrap y [5m]) by (pod)`,
		},
		{
			name: "parameterised vector aggregation",
			query: stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
				agg(ir.AggRate, ir.AggScopeTemporal),
				&ir.AggregationStage{
					Op: ir.AggTopK, Scope: ir.AggScopeGroup, Parameter: ir.NewNumberLiteral(3),
				}),
			want: `topk(3, rate({a="b"}[5m]))`,
		},
		{
			name: "set membership becomes a regex alternation",
			query: stream(&ir.LabelMatcher{
				Key: "app", Op: ir.MatchIn, Values: []string{"frontend", "backend"},
			}),
			want: `{app=~"frontend|backend"}`,
		},
		{
			name: "a value comparison after aggregation",
			query: stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
				agg(ir.AggRate, ir.AggScopeTemporal),
				filter(ir.FieldValue, ir.MatchGT, "5")),
			want: `rate({a="b"}[5m]) > 5`,
		},
		{
			name:  "empty pipeline renders just the selector",
			query: stream(matcher("app", ir.MatchEQ, "frontend")),
			want:  `{app="frontend"}`,
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

// TestMetricNameBecomesALabel covers the one thing LogQL has no place for: a
// query naming a metric.
func TestMetricNameBecomesALabel(t *testing.T) {
	query := &ir.Query{
		Signal: ir.SignalLog,
		Source: &ir.DataSource{Name: "http_requests_total", Scope: ir.ScopeUnscoped,
			Selectors: []*ir.Selector{{Matchers: []*ir.LabelMatcher{
				matcher("status", ir.MatchEQ, "500"),
			}}}},
		Output: &ir.Output{},
	}

	got := emit(t, query)

	if !strings.Contains(got, `{__name__="http_requests_total", status="500"}`) {
		t.Errorf("the name should become a label matcher:\n%s", got)
	}
	if !strings.Contains(got, "# the metric name") {
		t.Errorf("the substitution should be noted:\n%s", got)
	}
}

func TestEmitMarksUnsupportedNodes(t *testing.T) {
	query := stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
		agg(ir.AggRate, ir.AggScopeTemporal),
		agg(ir.AggSum, ir.AggScopeGroup))
	query.Pipeline[1].Base().SetTranslatability(
		ir.TranslatabilityUnsupported, "sum is not available in the target")

	got := emit(t, query)

	if !strings.Contains(got, "# UNSUPPORTED: sum is not available in the target") {
		t.Errorf("the reason should appear as a comment:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; last != `rate({a="b"}[5m])` {
		t.Errorf("query line = %q, want the inner expression", last)
	}
}

// TestJoinIsRefused covers the one construct LogQL simply has no form for.
func TestJoinIsRefused(t *testing.T) {
	query := stage(stream(matcher("a", ir.MatchEQ, "b")),
		&ir.JoinStage{JoinType: ir.JoinInner, OnLabels: []string{"job"}})

	got := emit(t, query)

	if !strings.Contains(got, "UNSUPPORTED") || !strings.Contains(got, "no join") {
		t.Errorf("a join should be refused with a reason:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; last != `{a="b"}` {
		t.Errorf("query line = %q, want the left-hand side alone", last)
	}
}

func TestEmitErrors(t *testing.T) {
	t.Run("nil query", func(t *testing.T) {
		if _, err := (Emitter{}).Emit(nil, testRegistry(t)); err == nil {
			t.Error("expected an error for a nil query")
		}
	})
	t.Run("nil registry", func(t *testing.T) {
		if _, err := (Emitter{}).Emit(stream(matcher("a", ir.MatchEQ, "b")), nil); err == nil {
			t.Error("expected an error for a nil registry")
		}
	})
}

func TestEmitterIsRegistered(t *testing.T) {
	e, err := emitter.Get("logql")
	if err != nil {
		t.Fatalf("emitter.Get: %v", err)
	}
	if e.DSL() != DSL {
		t.Errorf("DSL() = %q, want %q", e.DSL(), DSL)
	}
}

// TestEmitBinaryAndUnary covers the metric-phase operators LogQL borrows from
// PromQL.
func TestEmitBinaryAndUnary(t *testing.T) {
	rateOf := func(app string) *ir.Query {
		return stage(withWindow(stream(matcher("app", ir.MatchEQ, app)), 5*time.Minute),
			agg(ir.AggRate, ir.AggScopeTemporal))
	}

	binary := func(op ir.ArithOp, left, right *ir.Query) *ir.Query {
		return stage(&ir.Query{Signal: ir.SignalLog, Output: &ir.Output{}},
			&ir.BinaryOpStage{Op: op, Left: left, Right: right})
	}
	literalQuery := func(v float64) *ir.Query {
		return stage(&ir.Query{Signal: ir.SignalLog, Output: &ir.Output{}},
			&ir.FunctionStage{Name: ir.FuncLiteral,
				Args: []ir.IRExpr{ir.NewNumberLiteral(v)}, ReturnType: ir.DataTypeDouble})
	}

	t.Run("binary operator between two aggregations", func(t *testing.T) {
		query := binary(ir.ArithDiv, rateOf("frontend"), rateOf("backend"))
		want := `rate({app="frontend"}[5m]) / rate({app="backend"}[5m])`
		if got := emit(t, query); got != want {
			t.Errorf("Emit():\n got %s\nwant %s", got, want)
		}
	})

	t.Run("a scalar operand renders as a literal", func(t *testing.T) {
		query := binary(ir.ArithMul, literalQuery(2), rateOf("frontend"))
		want := `2 * rate({app="frontend"}[5m])`
		if got := emit(t, query); got != want {
			t.Errorf("Emit():\n got %s\nwant %s", got, want)
		}
	})

	t.Run("precedence decides the grouping", func(t *testing.T) {
		cases := []struct {
			name  string
			query *ir.Query
			want  string
		}{
			{
				name: "a looser operand is grouped",
				query: binary(ir.ArithMul,
					binary(ir.ArithAdd, rateOf("a"), rateOf("b")), literalQuery(2)),
				want: `(rate({app="a"}[5m]) + rate({app="b"}[5m])) * 2`,
			},
			{
				name: "a tighter operand is not",
				query: binary(ir.ArithAdd, rateOf("a"),
					binary(ir.ArithMul, rateOf("b"), literalQuery(2))),
				want: `rate({app="a"}[5m]) + rate({app="b"}[5m]) * 2`,
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if got := emit(t, c.query); got != c.want {
					t.Errorf("Emit():\n got %s\nwant %s", got, c.want)
				}
			})
		}
	})

	t.Run("unary minus", func(t *testing.T) {
		base := rateOf("frontend")
		operand := rateOf("frontend")
		query := stage(base, &ir.UnaryOpStage{Op: ir.ArithNeg, Operand: operand})
		want := `-rate({app="frontend"}[5m])`
		if got := emit(t, query); got != want {
			t.Errorf("Emit():\n got %s\nwant %s", got, want)
		}
	})

	t.Run("a set operator renders as a keyword", func(t *testing.T) {
		query := binary(ir.ArithUnless, rateOf("a"), rateOf("b"))
		want := `rate({app="a"}[5m]) unless rate({app="b"}[5m])`
		if got := emit(t, query); got != want {
			t.Errorf("Emit():\n got %s\nwant %s", got, want)
		}
	})
}

// TestEmitRefusesImpossibleShapes covers the two orderings LogQL's grammar
// cannot express, which reach the emitter only when a validator has not run.
func TestEmitRefusesImpossibleShapes(t *testing.T) {
	t.Run("a range over an aggregated expression", func(t *testing.T) {
		// LogQL has no subquery, so it cannot take a second range.
		query := stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
			agg(ir.AggRate, ir.AggScopeTemporal),
			agg(ir.AggAvg, ir.AggScopeTemporal))
		got := emit(t, query)
		if !strings.Contains(got, "no subquery form") {
			t.Errorf("expected a refusal:\n%s", got)
		}
	})

	t.Run("a pipeline stage after aggregation", func(t *testing.T) {
		query := stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
			agg(ir.AggRate, ir.AggScopeTemporal),
			fn("parse_json"))
		got := emit(t, query)
		if !strings.Contains(got, "already") {
			t.Errorf("expected a refusal:\n%s", got)
		}
	})

	t.Run("a vector aggregation over raw log lines", func(t *testing.T) {
		query := stage(stream(matcher("a", ir.MatchEQ, "b")),
			&ir.AggregationStage{Op: ir.AggSum, Scope: ir.AggScopeGroup})
		got := emit(t, query)
		if !strings.Contains(got, "range aggregation first") {
			t.Errorf("expected a refusal:\n%s", got)
		}
	})

	t.Run("a selector with no matchers", func(t *testing.T) {
		query := &ir.Query{
			Signal: ir.SignalLog,
			Source: &ir.DataSource{Scope: ir.ScopeUnscoped},
			Output: &ir.Output{},
		}
		got := emit(t, query)
		if !strings.Contains(got, "at least one matcher") {
			t.Errorf("expected a refusal:\n%s", got)
		}
	})
}

// TestBoolModifierIsReported covers Fix 5 from the LogQL side: the modifier has
// no LogQL spelling, and the difference is not cosmetic — a filtering
// comparison drops the series that fail it rather than returning 0 for them.
func TestBoolModifierIsReported(t *testing.T) {
	f := &ir.FilterStage{
		Predicate:   &ir.MatchPredicate{Matcher: matcher(ir.FieldValue, ir.MatchGT, "5")},
		ReturnsBool: true,
	}
	query := stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
		agg(ir.AggRate, ir.AggScopeTemporal), f)

	got := emit(t, query)

	if !strings.Contains(got, "PARTIAL") || !strings.Contains(got, "bool modifier") {
		t.Errorf("the difference should be reported:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; last != `rate({a="b"}[5m]) > 5` {
		t.Errorf("query line = %q, want the filtering form", last)
	}
}

// TestSubqueryIsReported covers Fix 6 from the LogQL side.
func TestSubqueryIsReported(t *testing.T) {
	query := stage(withWindow(stream(matcher("a", ir.MatchEQ, "b")), 5*time.Minute),
		agg(ir.AggRate, ir.AggScopeTemporal))
	outer := ir.NewIntervalFromSource(30*time.Minute, "30m")
	step := ir.NewIntervalFromSource(time.Minute, "1m")
	query.Output.SubqueryRange = &outer
	query.Output.SubqueryStep = &step

	got := emit(t, query)

	if !strings.Contains(got, "no subquery form") {
		t.Errorf("the missing construct should be reported:\n%s", got)
	}
	// What remains is the inner expression, which is valid on its own.
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; last != `rate({a="b"}[5m])` {
		t.Errorf("query line = %q", last)
	}
}
