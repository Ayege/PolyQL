package logql

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ast"
	"github.com/polyql/polyql/pkg/compiler/parser"
)

// parenthesize renders an expression with every operator grouping made
// explicit, so precedence and associativity can be stated directly.
func parenthesize(e Expr) string {
	switch n := e.(type) {
	case *BinaryExpr:
		return "(" + parenthesize(n.LHS) + " " + n.Op.String() + " " + parenthesize(n.RHS) + ")"
	case *UnaryExpr:
		return "(" + n.Op.String() + parenthesize(n.Expr) + ")"
	case *ParenExpr:
		return parenthesize(n.Expr)
	default:
		return e.String()
	}
}

func mustParse(t *testing.T, query string) Expr {
	t.Helper()
	expr, err := Parse(query)
	if err != nil {
		t.Fatalf("Parse(%s): unexpected error: %v", query, err)
	}
	return expr
}

// assertRoundTrips checks that rendering an AST produces LogQL that parses back
// to a tree rendering identically.
func assertRoundTrips(t *testing.T, query string) {
	t.Helper()

	first := mustParse(t, query)
	rendered := first.String()

	second, err := Parse(rendered)
	if err != nil {
		t.Fatalf("re-parsing %s (rendered from %s) failed: %v", rendered, query, err)
	}
	if again := second.String(); again != rendered {
		t.Errorf("round trip is not stable:\n  input:  %s\n  pass 1: %s\n  pass 2: %s", query, rendered, again)
	}
	if got, want := parenthesize(second), parenthesize(first); got != want {
		t.Errorf("round trip changed the tree shape:\n  pass 1: %s\n  pass 2: %s", want, got)
	}
}

// stageKinds lists a log expression's pipeline stages in order.
func stageKinds(t *testing.T, e Expr) []StageKind {
	t.Helper()
	pipeline, ok := e.(*PipelineExpr)
	if !ok {
		t.Fatalf("got %T, want *PipelineExpr", e)
	}
	kinds := make([]StageKind, 0, len(pipeline.Stages))
	for _, s := range pipeline.Stages {
		kinds = append(kinds, s.StageKind())
	}
	return kinds
}

// TestParseRequiredQueries covers the LogQL constructs the front end must
// handle, checking both the tree and the text it renders back to.
func TestParseRequiredQueries(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
		check func(t *testing.T, e Expr)
	}{
		{
			name:  "stream selector",
			query: `{app="frontend"}`,
			want:  `{app="frontend"}`,
			check: func(t *testing.T, e Expr) {
				sel, ok := e.(*LogStreamSelector)
				if !ok {
					t.Fatalf("got %T, want *LogStreamSelector", e)
				}
				if len(sel.Matchers) != 1 {
					t.Fatalf("got %d matchers, want 1", len(sel.Matchers))
				}
				m := sel.Matchers[0]
				if m.Name != "app" || m.Type != MatchEqual || m.Value != "frontend" {
					t.Errorf("matcher = %+v", m)
				}
				if sel.Type() != ExprTypeLog {
					t.Errorf("Type() = %s, want a log expression", sel.Type())
				}
			},
		},
		{
			name:  "line filter",
			query: `{app="frontend"} |= "error"`,
			want:  `{app="frontend"} |= "error"`,
			check: func(t *testing.T, e Expr) {
				pipeline, ok := e.(*PipelineExpr)
				if !ok {
					t.Fatalf("got %T, want *PipelineExpr", e)
				}
				if len(pipeline.Stages) != 1 {
					t.Fatalf("got %d stages, want 1", len(pipeline.Stages))
				}
				filter, ok := pipeline.Stages[0].(*LineFilter)
				if !ok {
					t.Fatalf("stage is %T, want *LineFilter", pipeline.Stages[0])
				}
				if filter.Op != LineContains || filter.Match != "error" {
					t.Errorf("filter = %+v, want |= \"error\"", filter)
				}
				if filter.Op.IsRegex() {
					t.Error("|= is a substring filter, not a regex one")
				}
			},
		},
		{
			name:  "parser stage then numeric label filter",
			query: `{app="frontend"} | json | status >= 400`,
			want:  `{app="frontend"} | json | status >= 400`,
			check: func(t *testing.T, e Expr) {
				kinds := stageKinds(t, e)
				want := []StageKind{StageParser, StageLabelFilter}
				if len(kinds) != len(want) || kinds[0] != want[0] || kinds[1] != want[1] {
					t.Fatalf("stages = %v, want %v", kinds, want)
				}
				pipeline := e.(*PipelineExpr)
				if p := pipeline.Stages[0].(*ParserStage); p.Kind != ParserJSON {
					t.Errorf("parser kind = %s, want json", p.Kind)
				}
				filter := pipeline.Stages[1].(*LabelFilter)
				pred, ok := filter.Predicate.(*LabelPredicate)
				if !ok {
					t.Fatalf("predicate is %T, want *LabelPredicate", filter.Predicate)
				}
				if pred.Name != "status" || pred.Op != FilterGTE {
					t.Errorf("predicate = %+v", pred)
				}
				if pred.Value.Kind != FilterValueNumber || pred.Value.Number != 400 {
					t.Errorf("value = %+v, want the number 400", pred.Value)
				}
			},
		},
		{
			name:  "logfmt then string label filter",
			query: `{app="frontend"} | logfmt | level="error"`,
			want:  `{app="frontend"} | logfmt | level="error"`,
			check: func(t *testing.T, e Expr) {
				pipeline := e.(*PipelineExpr)
				if p := pipeline.Stages[0].(*ParserStage); p.Kind != ParserLogfmt {
					t.Errorf("parser kind = %s, want logfmt", p.Kind)
				}
				pred := pipeline.Stages[1].(*LabelFilter).Predicate.(*LabelPredicate)
				if pred.Name != "level" || pred.Op != FilterEq {
					t.Errorf("predicate = %+v", pred)
				}
				if pred.Value.Kind != FilterValueString || pred.Value.Str != "error" {
					t.Errorf("value = %+v, want the string \"error\"", pred.Value)
				}
			},
		},
		{
			name:  "metric query over a filtered pipeline",
			query: `rate({app="frontend"} |= "error" [5m])`,
			want:  `rate({app="frontend"} |= "error" [5m])`,
			check: func(t *testing.T, e Expr) {
				agg, ok := e.(*RangeAggregation)
				if !ok {
					t.Fatalf("got %T, want *RangeAggregation", e)
				}
				if agg.Op != OpRate {
					t.Errorf("Op = %s, want rate", agg.Op)
				}
				if agg.Type() != ExprTypeMetric {
					t.Errorf("Type() = %s, want a metric expression", agg.Type())
				}
				if agg.Range.Interval.Value != 5*time.Minute {
					t.Errorf("interval = %s, want 5m", agg.Range.Interval.Value)
				}
				if agg.Range.Unwrap != nil {
					t.Error("rate over log lines should have no unwrap")
				}
				if _, ok := agg.Range.Selector.(*PipelineExpr); !ok {
					t.Errorf("range selector is %T, want *PipelineExpr", agg.Range.Selector)
				}
			},
		},
		{
			name:  "vector aggregation with by grouping",
			query: `sum by (level) (count_over_time({app="frontend"}[1h]))`,
			want:  `sum by (level) (count_over_time({app="frontend"}[1h]))`,
			check: func(t *testing.T, e Expr) {
				agg, ok := e.(*VectorAggregation)
				if !ok {
					t.Fatalf("got %T, want *VectorAggregation", e)
				}
				if agg.Op != OpSum {
					t.Errorf("Op = %s, want sum", agg.Op)
				}
				if agg.Grouping == nil || agg.Grouping.Without {
					t.Fatalf("Grouping = %+v, want a by clause", agg.Grouping)
				}
				if len(agg.Grouping.Labels) != 1 || agg.Grouping.Labels[0] != "level" {
					t.Errorf("Grouping.Labels = %v, want [level]", agg.Grouping.Labels)
				}
				if agg.Param != nil {
					t.Errorf("Param = %v, want nil for sum", agg.Param)
				}
				inner, ok := agg.Expr.(*RangeAggregation)
				if !ok || inner.Op != OpCountOverTime {
					t.Errorf("inner expression = %v, want a count_over_time", agg.Expr)
				}
			},
		},
		{
			name:  "unwrapped range aggregation",
			query: `avg_over_time({app="frontend"} | json | unwrap duration [5m])`,
			want:  `avg_over_time({app="frontend"} | json | unwrap duration [5m])`,
			check: func(t *testing.T, e Expr) {
				agg := e.(*RangeAggregation)
				if agg.Op != OpAvgOverTime {
					t.Errorf("Op = %s, want avg_over_time", agg.Op)
				}
				unwrap := agg.Range.Unwrap
				if unwrap == nil {
					t.Fatal("Unwrap is nil")
				}
				// "duration" here is the label being unwrapped, not the
				// duration() conversion, because no parentheses follow it.
				if unwrap.Identifier != "duration" {
					t.Errorf("Identifier = %q, want duration", unwrap.Identifier)
				}
				if unwrap.Conversion != ConvNone {
					t.Errorf("Conversion = %v, want none", unwrap.Conversion)
				}
				// The unwrap belongs to the range, so the pipeline holds only
				// the json parser.
				kinds := stageKinds(t, agg.Range.Selector)
				if len(kinds) != 1 || kinds[0] != StageParser {
					t.Errorf("pipeline stages = %v, want just the parser", kinds)
				}
			},
		},
		{
			name: "multi-stage pipeline",
			query: `{app="frontend"} |= "error" | json | line_format "{{.message}}" ` +
				`| label_format level="critical"`,
			want: `{app="frontend"} |= "error" | json | line_format "{{.message}}" ` +
				`| label_format level="critical"`,
			check: func(t *testing.T, e Expr) {
				// The defining property of a pipeline language: stages are held
				// in written order, not nested.
				kinds := stageKinds(t, e)
				want := []StageKind{StageLineFilter, StageParser, StageFormatter, StageFormatter}
				if len(kinds) != len(want) {
					t.Fatalf("stages = %v, want %v", kinds, want)
				}
				for i := range want {
					if kinds[i] != want[i] {
						t.Fatalf("stages = %v, want %v", kinds, want)
					}
				}
				pipeline := e.(*PipelineExpr)
				lineFmt := pipeline.Stages[2].(*FormatterStage)
				if lineFmt.Kind != FormatLine || lineFmt.Template != "{{.message}}" {
					t.Errorf("line_format = %+v", lineFmt)
				}
				labelFmt := pipeline.Stages[3].(*FormatterStage)
				if labelFmt.Kind != FormatLabel {
					t.Fatalf("expected a label_format stage, got %v", labelFmt.Kind)
				}
				if len(labelFmt.Params) != 1 {
					t.Fatalf("got %d params, want 1", len(labelFmt.Params))
				}
				param := labelFmt.Params[0]
				if param.Dst != "level" || !param.IsTemplate || param.Template != "critical" {
					t.Errorf("label_format param = %+v", param)
				}
			},
		},
		{
			name:  "binary expression between two aggregations",
			query: `sum(rate({app="frontend"}[5m])) / sum(rate({app="backend"}[5m]))`,
			want:  `sum(rate({app="frontend"}[5m])) / sum(rate({app="backend"}[5m]))`,
			check: func(t *testing.T, e Expr) {
				bin, ok := e.(*BinaryExpr)
				if !ok {
					t.Fatalf("got %T, want *BinaryExpr", e)
				}
				if bin.Op != DIV {
					t.Errorf("Op = %s, want /", bin.Op)
				}
				if bin.VectorMatching != nil {
					t.Errorf("VectorMatching = %+v, want nil when no on/ignoring is written", bin.VectorMatching)
				}
				for _, side := range []Expr{bin.LHS, bin.RHS} {
					agg, ok := side.(*VectorAggregation)
					if !ok || agg.Op != OpSum {
						t.Errorf("operand = %T, want a sum aggregation", side)
					}
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expr := mustParse(t, c.query)
			c.check(t, expr)
			if got := expr.String(); got != c.want {
				t.Errorf("String():\n got %s\nwant %s", got, c.want)
			}
			assertRoundTrips(t, c.query)
		})
	}
}

// TestPipelineStagesAreOrdered covers the structural difference from PromQL:
// stages chain left to right instead of nesting.
func TestPipelineStagesAreOrdered(t *testing.T) {
	query := `{a="b"} |= "x" | json | status >= 400 | line_format "{{.msg}}" | ` +
		`label_format lvl=level | drop path | keep lvl | decolorize`

	kinds := stageKinds(t, mustParse(t, query))
	want := []StageKind{
		StageLineFilter, StageParser, StageLabelFilter, StageFormatter,
		StageFormatter, StageDrop, StageKeep, StageDecolorize,
	}
	if len(kinds) != len(want) {
		t.Fatalf("stages = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("stage %d = %s, want %s", i, kinds[i], want[i])
		}
	}
	assertRoundTrips(t, query)
}

func TestChainedLineFilters(t *testing.T) {
	query := `{a="b"} |= "x" != "y" |~ "z.*" !~ "w"`
	kinds := stageKinds(t, mustParse(t, query))
	if len(kinds) != 4 {
		t.Fatalf("got %d stages, want 4 line filters", len(kinds))
	}

	pipeline := mustParse(t, query).(*PipelineExpr)
	wantOps := []LineFilterOp{LineContains, LineNotContains, LineMatchesRegex, LineNotMatchesRegex}
	for i, want := range wantOps {
		got := pipeline.Stages[i].(*LineFilter)
		if got.Op != want {
			t.Errorf("filter %d op = %s, want %s", i, got.Op, want)
		}
	}
	assertRoundTrips(t, query)
}

func TestRoundTrip(t *testing.T) {
	cases := []struct{ query, want string }{
		{`{a="b", c!="d", e=~"f", g!~"h"}`, `{a="b", c!="d", e=~"f", g!~"h"}`},
		{`{a="b"} | json first="servers[0]", second`, `{a="b"} | json first="servers[0]", second`},
		{`{a="b"} | logfmt --strict --keep-empty host`, `{a="b"} | logfmt --strict --keep-empty host`},
		{`{a="b"} | pattern "<ip> - - <_>"`, `{a="b"} | pattern "<ip> - - <_>"`},
		{`{a="b"} | regexp "(?P<method>\\w+)"`, `{a="b"} | regexp "(?P<method>\\w+)"`},
		{`{a="b"} | unpack`, `{a="b"} | unpack`},
		{`{a="b"} | decolorize`, `{a="b"} | decolorize`},
		{`{a="b"} | drop level, method="GET"`, `{a="b"} | drop level, method="GET"`},
		{`{a="b"} | keep level, app=~"some-api.*"`, `{a="b"} | keep level, app=~"some-api.*"`},
		{`{a="b"} | label_format dst=src, other="{{.x}}"`, `{a="b"} | label_format dst=src, other="{{.x}}"`},
		{`{a="b"} | duration > 1m and bytes_consumed > 20MB`, `{a="b"} | duration > 1m and bytes_consumed > 20MB`},
		{`{a="b"} | duration >= 20ms or (method="GET" and size <= 20KB)`,
			`{a="b"} | duration >= 20ms or (method="GET" and size <= 20KB)`},
		{`{a="b"} | status == 200, method!~"2.."`, `{a="b"} | status == 200, method!~"2.."`},
		{`{a="b"} | size > 1.5GiB`, `{a="b"} | size > 1.5GiB`},
		{`{a="b"} | latency > 1.5h`, `{a="b"} | latency > 1.5h`},
		{`count_over_time({a="b"}[5m] offset 1h)`, `count_over_time({a="b"}[5m] offset 1h)`},
		{`bytes_rate({a="b"}[5m])`, `bytes_rate({a="b"}[5m])`},
		{`bytes_over_time({a="b"}[5m])`, `bytes_over_time({a="b"}[5m])`},
		{`absent_over_time({a="b"}[5m])`, `absent_over_time({a="b"}[5m])`},
		{`quantile_over_time(0.99, {a="b"} | unwrap duration(latency) [5m])`,
			`quantile_over_time(0.99, {a="b"} | unwrap duration(latency) [5m])`},
		{`max_over_time({a="b"} | json | unwrap bytes(size) [5m]) without (pod)`,
			`max_over_time({a="b"} | json | unwrap bytes(size) [5m]) without (pod)`},
		{`sum_over_time({a="b"} | unwrap duration_seconds(t) [5m])`,
			`sum_over_time({a="b"} | unwrap duration_seconds(t) [5m])`},
		{`avg_over_time({a="b"} | unwrap x | __error__="" [5m])`,
			`avg_over_time({a="b"} | unwrap x | __error__="" [5m])`},
		{`topk(10, sum by (x) (rate({a="b"}[5m])))`, `topk(10, sum by (x) (rate({a="b"}[5m])))`},
		{`bottomk(3, rate({a="b"}[5m]))`, `bottomk(3, rate({a="b"}[5m]))`},
		{`sort_desc(rate({a="b"}[5m]))`, `sort_desc(rate({a="b"}[5m]))`},
		{`count without (pod) (rate({a="b"}[5m]))`, `count without (pod) (rate({a="b"}[5m]))`},
		{`label_replace(rate({a="b"}[5m]), "dst", "$1", "src", "(.*)")`,
			`label_replace(rate({a="b"}[5m]), "dst", "$1", "src", "(.*)")`},
		{`rate({a="b"}[5m]) / on (x) group_left (y) rate({c="d"}[5m])`,
			`rate({a="b"}[5m]) / on (x) group_left (y) rate({c="d"}[5m])`},
		{`rate({a="b"}[5m]) > bool 5`, `rate({a="b"}[5m]) > bool 5`},
		{`2 * sum(rate({a="b"}[5m]))`, `2 * sum(rate({a="b"}[5m]))`},
		{`-sum(rate({a="b"}[5m]))`, `-sum(rate({a="b"}[5m]))`},
		{`(rate({a="b"}[5m]) + 1) * 2`, `(rate({a="b"}[5m]) + 1) * 2`},
		// Alternative spellings the parser normalises.
		{`sum(rate({a="b"}[5m])) by (level)`, `sum by (level) (rate({a="b"}[5m]))`},
		{`{a="b"} | level="x" status=500`, `{a="b"} | level="x" and status = 500`},
		{`{a='single'}`, `{a="single"}`},
		{"{a=`raw`}", `{a="raw"}`},
		{`{a="b"} # comment`, `{a="b"}`},
	}

	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			expr := mustParse(t, c.query)
			if got := expr.String(); got != c.want {
				t.Errorf("String():\n got %s\nwant %s", got, c.want)
			}
			assertRoundTrips(t, c.query)
		})
	}
}

func TestOperatorPrecedence(t *testing.T) {
	r := `rate({a="b"}[5m])`
	cases := []struct{ query, want string }{
		{`1 + 2 * 3`, `(1 + (2 * 3))`},
		{`1 * 2 + 3`, `((1 * 2) + 3)`},
		{`1 - 2 - 3`, `((1 - 2) - 3)`},
		{`2 ^ 3 ^ 2`, `(2 ^ (3 ^ 2))`},
		{`-2 ^ 2`, `(-(2 ^ 2))`},
		{`-2 * 3`, `((-2) * 3)`},
		{r + ` > 1 and ` + r, `((` + r + ` > 1) and ` + r + `)`},
		{`(1 + 2) * 3`, `((1 + 2) * 3)`},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			got := parenthesize(mustParse(t, c.query))
			if got != c.want {
				t.Errorf("grouping:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestLabelFilterPrecedence covers "and" binding more tightly than "or", and the
// comma being a synonym for "and".
func TestLabelFilterPrecedence(t *testing.T) {
	filter := mustParse(t, `{a="b"} | x="1" or y="2" and z="3"`).(*PipelineExpr).
		Stages[0].(*LabelFilter)

	top, ok := filter.Predicate.(*LabelFilterBinary)
	if !ok {
		t.Fatalf("predicate is %T, want *LabelFilterBinary", filter.Predicate)
	}
	if top.Op != FilterOr {
		t.Fatalf("top-level operator = %s, want or", top.Op)
	}
	right, ok := top.RHS.(*LabelFilterBinary)
	if !ok || right.Op != FilterAnd {
		t.Errorf("right operand = %v, want an and-expression", top.RHS)
	}

	// A comma means and, and is preserved as written.
	comma := mustParse(t, `{a="b"} | x="1", y="2"`).(*PipelineExpr).
		Stages[0].(*LabelFilter).Predicate.(*LabelFilterBinary)
	if comma.Op != FilterComma {
		t.Errorf("operator = %s, want the comma form", comma.Op)
	}
}

func TestUnwrapConversions(t *testing.T) {
	cases := []struct {
		query      string
		conversion ConversionOp
		identifier string
	}{
		{`sum_over_time({a="b"} | unwrap latency [5m])`, ConvNone, "latency"},
		{`sum_over_time({a="b"} | unwrap duration(latency) [5m])`, ConvDuration, "latency"},
		{`sum_over_time({a="b"} | unwrap duration_seconds(latency) [5m])`, ConvDurationSeconds, "latency"},
		{`sum_over_time({a="b"} | unwrap bytes(size) [5m])`, ConvBytes, "size"},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			unwrap := mustParse(t, c.query).(*RangeAggregation).Range.Unwrap
			if unwrap.Conversion != c.conversion {
				t.Errorf("Conversion = %v, want %v", unwrap.Conversion, c.conversion)
			}
			if unwrap.Identifier != c.identifier {
				t.Errorf("Identifier = %q, want %q", unwrap.Identifier, c.identifier)
			}
		})
	}

	t.Run("post-unwrap label filters are kept", func(t *testing.T) {
		unwrap := mustParse(t, `avg_over_time({a="b"} | unwrap x | __error__="" | y > 1 [5m])`).(*RangeAggregation).Range.Unwrap
		if len(unwrap.PostFilters) != 2 {
			t.Fatalf("got %d post filters, want 2", len(unwrap.PostFilters))
		}
	})
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ name, query, wantMsg string }{
		{"empty query", ``, "empty query"},
		{"empty stream selector", `{}`, "at least one label matcher"},
		{"unclosed brace", `{a="b"`, `expected "}"`},
		{"missing matcher operator", `{a "b"}`, "label matching operator"},
		{"unquoted matcher value", `{a=b}`, "quoted string"},
		{"invalid matcher regex", `{a=~"("}`, "invalid regular expression"},
		{"invalid line filter regex", `{a="b"} |~ "("`, "invalid regular expression"},
		{"line filter without a string", `{a="b"} |= 5`, "expected a quoted string"},
		{"unknown function", `no_such_function({a="b"}[5m])`, "unknown function"},
		{"bare identifier", `foo`, "starts with a stream selector"},
		{"unwrap outside a range", `{a="b"} | unwrap x`, "only allowed inside a range aggregation"},
		{"missing range", `rate({a="b"})`, `expected "["`},
		{"count_over_time rejects unwrap", `count_over_time({a="b"} | unwrap x [5m])`, "must not use | unwrap"},
		{"avg_over_time requires unwrap", `avg_over_time({a="b"}[5m])`, "must end with | unwrap"},
		{"bytes_over_time rejects unwrap", `bytes_over_time({a="b"} | unwrap x [5m])`, "must not use | unwrap"},
		{"quantile_over_time needs a parameter", `quantile_over_time({a="b"} | unwrap x [5m])`, "expects a scalar parameter"},
		{"topk needs a parameter", `topk(rate({a="b"}[5m]))`, "expects a numeric parameter"},
		{"aggregation over a log expression", `sum({a="b"})`, "expects a metric expression"},
		{"binary operator on a log expression", `{a="b"} / 2`, "requires metric expressions"},
		{"bool on a non-comparison", `rate({a="b"}[5m]) + bool 1`, "bool modifier can only follow a comparison"},
		{"ordered comparison against a string", `{a="b"} | level > "x"`, "cannot be applied to a string"},
		{"label filter without an operator", `{a="b"} | level`, "expected a comparison operator"},
		{"unknown unwrap conversion", `sum_over_time({a="b"} | unwrap nope(x) [5m])`, "unknown unwrap conversion"},
		{"double unwrap", `sum_over_time({a="b"} | unwrap x | unwrap y [5m])`, "only be unwrapped once"},
		{"unterminated string", `{a="b}`, "unterminated string"},
		{"trailing tokens", `{a="b"} {c="d"}`, "after the end of the expression"},
		{"label_replace on a log expression", `label_replace({a="b"}, "d", "r", "s", "x")`, "metric expression"},
		{"unknown byte unit", `{a="b"} | size > 5QB`, "unknown"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expr, err := Parse(c.query)
			if err == nil {
				t.Fatalf("expected an error, got the expression %s", expr)
			}
			if expr != nil {
				t.Errorf("expression should be nil on error, got %v", expr)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error %q should contain %q", err, c.wantMsg)
			}
		})
	}
}

func TestParseErrorReportsPosition(t *testing.T) {
	_, err := Parse(`sum(rate({a="b"}[5m])) / no_such_function({c="d"}[5m])`)
	if err == nil {
		t.Fatal("expected an error")
	}
	parseErr, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("error is %T, want *ParseError", err)
	}
	if parseErr.Pos != 25 {
		t.Errorf("Pos = %d, want 25 (the start of the unknown function)", parseErr.Pos)
	}
	if got, want := parseErr.Error(), "1:26: parse error:"; !strings.HasPrefix(got, want) {
		t.Errorf("Error() = %q, want it to start with %q", got, want)
	}
}

// TestStageKeywordsAreUsableAsLabels covers the parser accepting a stage keyword
// where a label name is expected.
func TestStageKeywordsAreUsableAsLabels(t *testing.T) {
	for _, query := range []string{
		`{a="b"} | drop json`,
		`{a="b"} | keep pattern`,
		`sum by (json) (rate({a="b"}[5m]))`,
	} {
		if _, err := Parse(query); err != nil {
			t.Errorf("Parse(%s): %v", query, err)
		}
	}
}

// TestParserIsRegistered covers the init-time registration alongside PromQL.
func TestParserIsRegistered(t *testing.T) {
	p, err := parser.Get("logql")
	if err != nil {
		t.Fatalf("Get(\"logql\"): %v", err)
	}
	if p.DSL() != DSL {
		t.Errorf("DSL() = %q, want %q", p.DSL(), DSL)
	}
	if _, err := parser.Get("LogQL"); err != nil {
		t.Errorf("Get(\"LogQL\"): %v", err)
	}

	node, err := p.Parse(`rate({app="frontend"}[5m])`)
	if err != nil {
		t.Fatalf("Parse via the registry: %v", err)
	}
	if node.DSL() != DSL {
		t.Errorf("node.DSL() = %q, want %q", node.DSL(), DSL)
	}
	if got, want := node.String(), `rate({app="frontend"}[5m])`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	bad, err := p.Parse(`{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if bad != nil {
		t.Errorf("node = %v, want nil on error", bad)
	}
}

// Compile-time proof that the front end satisfies the pipeline's contracts.
var (
	_ parser.Parser = Parser{}
	_ ast.Node      = (*LogStreamSelector)(nil)
	_ []Expr        = []Expr{
		(*LogStreamSelector)(nil), (*PipelineExpr)(nil), (*RangeAggregation)(nil),
		(*VectorAggregation)(nil), (*BinaryExpr)(nil), (*UnaryExpr)(nil),
		(*ParenExpr)(nil), (*NumberLiteral)(nil), (*LabelReplace)(nil),
	}
	_ []PipelineStage = []PipelineStage{
		(*LineFilter)(nil), (*LabelFilter)(nil), (*ParserStage)(nil),
		(*FormatterStage)(nil), (*DropStage)(nil), (*KeepStage)(nil),
		(*DecolorizeStage)(nil),
	}
	_ []LabelFilterExpr = []LabelFilterExpr{
		(*LabelPredicate)(nil), (*LabelFilterBinary)(nil), (*LabelFilterParen)(nil),
	}
)
