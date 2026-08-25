package promql

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ast"
	"github.com/polyql/polyql/pkg/compiler/parser"
)

// parenthesize renders an expression with every operator grouping made
// explicit. Comparing against it states precedence and associativity
// expectations directly, which String cannot do because it reproduces the
// user's own (unparenthesised) spelling.
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

// TestParseRequiredQueries covers the grammar constructs the PromQL front end
// must handle, checking both the tree that comes out and the text it renders
// back to.
func TestParseRequiredQueries(t *testing.T) {
	cases := []struct {
		name  string
		query string
		// want is the canonical rendering. Where it differs from the input, the
		// parser has normalised an alternative spelling.
		want  string
		check func(t *testing.T, e Expr)
	}{
		{
			name:  "simple metric",
			query: `up`,
			want:  `up`,
			check: func(t *testing.T, e Expr) {
				vs, ok := e.(*VectorSelector)
				if !ok {
					t.Fatalf("got %T, want *VectorSelector", e)
				}
				if vs.Name != "up" {
					t.Errorf("Name = %q, want %q", vs.Name, "up")
				}
				if len(vs.LabelMatchers) != 0 {
					t.Errorf("got %d matchers, want none", len(vs.LabelMatchers))
				}
				if vs.Type() != ValueTypeVector {
					t.Errorf("Type() = %s, want instant vector", vs.Type())
				}
			},
		},
		{
			name:  "label selector",
			query: `http_requests_total{status="500"}`,
			want:  `http_requests_total{status="500"}`,
			check: func(t *testing.T, e Expr) {
				vs := e.(*VectorSelector)
				if vs.Name != "http_requests_total" {
					t.Errorf("Name = %q", vs.Name)
				}
				if len(vs.LabelMatchers) != 1 {
					t.Fatalf("got %d matchers, want 1", len(vs.LabelMatchers))
				}
				m := vs.LabelMatchers[0]
				if m.Name != "status" || m.Type != MatchEqual || m.Value != "500" {
					t.Errorf("matcher = %+v, want status = \"500\"", m)
				}
			},
		},
		{
			name:  "range vector",
			query: `http_requests_total{status="500"}[5m]`,
			want:  `http_requests_total{status="500"}[5m]`,
			check: func(t *testing.T, e Expr) {
				ms, ok := e.(*MatrixSelector)
				if !ok {
					t.Fatalf("got %T, want *MatrixSelector", e)
				}
				if ms.Range != 5*time.Minute {
					t.Errorf("Range = %s, want 5m", ms.Range)
				}
				if ms.Type() != ValueTypeMatrix {
					t.Errorf("Type() = %s, want range vector", ms.Type())
				}
				if ms.VectorSelector.Name != "http_requests_total" {
					t.Errorf("inner selector name = %q", ms.VectorSelector.Name)
				}
			},
		},
		{
			name:  "rate function",
			query: `rate(http_requests_total[5m])`,
			want:  `rate(http_requests_total[5m])`,
			check: func(t *testing.T, e Expr) {
				call, ok := e.(*Call)
				if !ok {
					t.Fatalf("got %T, want *Call", e)
				}
				if call.Func.Name != "rate" {
					t.Errorf("function = %q, want rate", call.Func.Name)
				}
				if len(call.Args) != 1 {
					t.Fatalf("got %d args, want 1", len(call.Args))
				}
				if _, ok := call.Args[0].(*MatrixSelector); !ok {
					t.Errorf("arg is %T, want *MatrixSelector", call.Args[0])
				}
				if call.Type() != ValueTypeVector {
					t.Errorf("Type() = %s, want instant vector", call.Type())
				}
			},
		},
		{
			name:  "aggregation with by",
			query: `sum by (job) (rate(http_requests_total[5m]))`,
			want:  `sum by (job) (rate(http_requests_total[5m]))`,
			check: func(t *testing.T, e Expr) {
				agg, ok := e.(*AggregateExpr)
				if !ok {
					t.Fatalf("got %T, want *AggregateExpr", e)
				}
				if agg.Op != SUM {
					t.Errorf("Op = %s, want sum", agg.Op)
				}
				if agg.Without {
					t.Error("Without should be false for a by clause")
				}
				if len(agg.Grouping) != 1 || agg.Grouping[0] != "job" {
					t.Errorf("Grouping = %v, want [job]", agg.Grouping)
				}
				if agg.Param != nil {
					t.Errorf("Param = %v, want nil for sum", agg.Param)
				}
				if _, ok := agg.Expr.(*Call); !ok {
					t.Errorf("aggregated expression is %T, want *Call", agg.Expr)
				}
			},
		},
		{
			name:  "binary expression",
			query: `http_requests_total / http_requests_failed`,
			want:  `http_requests_total / http_requests_failed`,
			check: func(t *testing.T, e Expr) {
				bin, ok := e.(*BinaryExpr)
				if !ok {
					t.Fatalf("got %T, want *BinaryExpr", e)
				}
				if bin.Op != DIV {
					t.Errorf("Op = %s, want /", bin.Op)
				}
				if bin.ReturnBool {
					t.Error("ReturnBool should be false without a bool modifier")
				}
				if bin.VectorMatching != nil {
					t.Errorf("VectorMatching = %+v, want nil when no on/ignoring is written", bin.VectorMatching)
				}
				if bin.LHS.(*VectorSelector).Name != "http_requests_total" {
					t.Error("left operand is wrong")
				}
				if bin.RHS.(*VectorSelector).Name != "http_requests_failed" {
					t.Error("right operand is wrong")
				}
			},
		},
		{
			name:  "nested aggregation inside a call",
			query: `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
			want:  `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
			check: func(t *testing.T, e Expr) {
				call := e.(*Call)
				if call.Func.Name != "histogram_quantile" {
					t.Fatalf("function = %q", call.Func.Name)
				}
				if len(call.Args) != 2 {
					t.Fatalf("got %d args, want 2", len(call.Args))
				}
				num, ok := call.Args[0].(*NumberLiteral)
				if !ok || num.Val != 0.99 {
					t.Errorf("first arg = %v, want the scalar 0.99", call.Args[0])
				}
				agg, ok := call.Args[1].(*AggregateExpr)
				if !ok {
					t.Fatalf("second arg is %T, want *AggregateExpr", call.Args[1])
				}
				if len(agg.Grouping) != 1 || agg.Grouping[0] != "le" {
					t.Errorf("Grouping = %v, want [le]", agg.Grouping)
				}
				inner, ok := agg.Expr.(*Call)
				if !ok || inner.Func.Name != "rate" {
					t.Errorf("aggregated expression = %v, want a rate call", agg.Expr)
				}
			},
		},
		{
			name:  "offset inside a range vector",
			query: `rate(http_requests_total[5m] offset 1h)`,
			want:  `rate(http_requests_total[5m] offset 1h)`,
			check: func(t *testing.T, e Expr) {
				call := e.(*Call)
				ms, ok := call.Args[0].(*MatrixSelector)
				if !ok {
					t.Fatalf("arg is %T, want *MatrixSelector", call.Args[0])
				}
				if ms.Range != 5*time.Minute {
					t.Errorf("Range = %s, want 5m", ms.Range)
				}
				// The offset belongs to the selector even though it is written
				// after the range.
				if got := ms.VectorSelector.OriginalOffset; got != time.Hour {
					t.Errorf("offset = %s, want 1h", got)
				}
			},
		},
		{
			name:  "subquery over a call",
			query: `rate(http_requests_total[5m])[30m:1m]`,
			want:  `rate(http_requests_total[5m])[30m:1m]`,
			check: func(t *testing.T, e Expr) {
				sq, ok := e.(*SubqueryExpr)
				if !ok {
					t.Fatalf("got %T, want *SubqueryExpr", e)
				}
				if sq.Range != 30*time.Minute {
					t.Errorf("Range = %s, want 30m", sq.Range)
				}
				if sq.Step != time.Minute {
					t.Errorf("Step = %s, want 1m", sq.Step)
				}
				if sq.Type() != ValueTypeMatrix {
					t.Errorf("Type() = %s, want range vector", sq.Type())
				}
				if _, ok := sq.Expr.(*Call); !ok {
					t.Errorf("subquery expression is %T, want *Call", sq.Expr)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expr := mustParse(t, c.query)
			c.check(t, expr)

			got := expr.String()
			if got != c.want {
				t.Errorf("String():\n got %s\nwant %s", got, c.want)
			}
			assertRoundTrips(t, c.query)
		})
	}
}

// assertRoundTrips checks that rendering an AST produces PromQL that parses
// back to a tree rendering identically. That property is what lets the
// translator trust its own output.
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

// TestRoundTrip covers a wider corpus than the required cases, including the
// spellings the parser normalises.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{`up`, `up`},
		{`up{}`, `up`},
		{`{__name__=~"job:.*"}`, `{__name__=~"job:.*"}`},
		{`job:requests:rate5m`, `job:requests:rate5m`},
		{`foo{a="1",b!="2",c=~"3",d!~"4"}`, `foo{a="1",b!="2",c=~"3",d!~"4"}`},
		{`foo offset 5m`, `foo offset 5m`},
		{`foo offset -1w`, `foo offset -1w`},
		{`foo @ 1609746000`, `foo @ 1.609746e+09`},
		{`foo @ start()`, `foo @ start()`},
		{`foo @ end() offset 1h`, `foo @ end() offset 1h`},
		{`foo[1h30m]`, `foo[1h30m]`},
		{`foo[54s321ms]`, `foo[54s321ms]`},
		{`foo[30m:]`, `foo[30m:]`},
		{`sum(foo)`, `sum(foo)`},
		{`sum without (pod) (foo)`, `sum without (pod) (foo)`},
		{`topk(3, foo)`, `topk(3, foo)`},
		{`quantile(0.95, foo)`, `quantile(0.95, foo)`},
		{`count_values("version", foo)`, `count_values("version", foo)`},
		{`foo * on (job) group_left (env) bar`, `foo * on (job) group_left (env) bar`},
		{`foo * ignoring (env) group_right bar`, `foo * ignoring (env) group_right () bar`},
		{`foo and bar`, `foo and bar`},
		{`foo unless on (job) bar`, `foo unless on (job) bar`},
		{`foo == bool 1`, `foo == bool 1`},
		{`1 == bool 2`, `1 == bool 2`},
		{`-foo`, `-foo`},
		{`(foo + bar) * 2`, `(foo + bar) * 2`},
		{`label_replace(foo, "dst", "$1", "src", "(.*)")`, `label_replace(foo, "dst", "$1", "src", "(.*)")`},
		{`time()`, `time()`},
		{`round(foo)`, `round(foo)`},
		{`round(foo, 5)`, `round(foo, 5)`},
		{`clamp(foo, 0, 100)`, `clamp(foo, 0, 100)`},
		{`0x8f`, `143`},
		{`1_000_000`, `1e+06`},
		{`.5`, `0.5`},
		{`3.4e-9`, `3.4e-09`},
		{`NaN`, `NaN`},
		{`Inf`, `Inf`},
		{`-Inf`, `-Inf`},
		// Alternative spellings the parser normalises to a canonical form.
		{`sum(foo) by (job)`, `sum by (job) (foo)`},
		{`sum(foo) without (job)`, `sum without (job) (foo)`},
		{`foo{a='single quoted'}`, `foo{a="single quoted"}`},
		{"foo{a=`raw \\n`}", `foo{a="raw \\n"}`},
		{`foo # trailing comment`, `foo`},
		{`foo[7d]`, `foo[1w]`},
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

// TestOperatorPrecedence pins PromQL's precedence table and the
// right-associativity of the power operator.
func TestOperatorPrecedence(t *testing.T) {
	cases := []struct{ query, want string }{
		{`1 + 2 * 3`, `(1 + (2 * 3))`},
		{`1 * 2 + 3`, `((1 * 2) + 3)`},
		{`1 - 2 - 3`, `((1 - 2) - 3)`},
		{`1 / 2 / 3`, `((1 / 2) / 3)`},
		{`2 ^ 3 ^ 2`, `(2 ^ (3 ^ 2))`},
		{`2 ^ 3 * 4`, `((2 ^ 3) * 4)`},
		{`-2 ^ 2`, `(-(2 ^ 2))`},
		{`-2 * 3`, `((-2) * 3)`},
		{`foo > 1 and bar`, `((foo > 1) and bar)`},
		{`foo and bar or baz`, `((foo and bar) or baz)`},
		{`foo or bar and baz`, `(foo or (bar and baz))`},
		{`foo unless bar or baz`, `((foo unless bar) or baz)`},
		{`1 + 2 == bool 3`, `((1 + 2) == 3)`},
		{`foo atan2 bar * 2`, `((foo atan2 bar) * 2)`},
		{`(1 + 2) * 3`, `((1 + 2) * 3)`},
		{`1 + (2 * 3)`, `(1 + (2 * 3))`},
	}

	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			if got := parenthesize(mustParse(t, c.query)); got != c.want {
				t.Errorf("grouping:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestAggregationClauseOrderIsEquivalent(t *testing.T) {
	before := mustParse(t, `sum by (job, instance) (foo)`)
	after := mustParse(t, `sum(foo) by (job, instance)`)

	if before.String() != after.String() {
		t.Errorf("the two spellings should produce the same tree:\n  by-first: %s\n  by-last:  %s",
			before, after)
	}
	agg := after.(*AggregateExpr)
	if len(agg.Grouping) != 2 || agg.Grouping[0] != "job" || agg.Grouping[1] != "instance" {
		t.Errorf("Grouping = %v, want [job instance]", agg.Grouping)
	}
}

func TestVectorMatching(t *testing.T) {
	expr := mustParse(t, `foo / on (job, env) group_left (region, zone) bar`)
	bin := expr.(*BinaryExpr)

	vm := bin.VectorMatching
	if vm == nil {
		t.Fatal("VectorMatching is nil")
	}
	if !vm.On {
		t.Error("On should be true for an on(...) clause")
	}
	if len(vm.MatchingLabels) != 2 || vm.MatchingLabels[0] != "job" {
		t.Errorf("MatchingLabels = %v", vm.MatchingLabels)
	}
	if vm.Card != CardManyToOne {
		t.Errorf("Card = %v, want CardManyToOne for group_left", vm.Card)
	}
	if len(vm.Include) != 2 || vm.Include[1] != "zone" {
		t.Errorf("Include = %v", vm.Include)
	}

	ignoring := mustParse(t, `foo / ignoring (env) bar`).(*BinaryExpr)
	if ignoring.VectorMatching.On {
		t.Error("On should be false for an ignoring(...) clause")
	}
	if ignoring.VectorMatching.Card != CardOneToOne {
		t.Error("Card should stay one-to-one without a group modifier")
	}

	// Set operators match many-to-many regardless of the labels named.
	setOp := mustParse(t, `foo and on (job) bar`).(*BinaryExpr)
	if setOp.VectorMatching.Card != CardManyToMany {
		t.Errorf("Card = %v, want CardManyToMany for a set operator", setOp.VectorMatching.Card)
	}
}

func TestModifiers(t *testing.T) {
	t.Run("offset on a plain selector", func(t *testing.T) {
		vs := mustParse(t, `foo offset 1h`).(*VectorSelector)
		if vs.OriginalOffset != time.Hour {
			t.Errorf("offset = %s, want 1h", vs.OriginalOffset)
		}
	})
	t.Run("negative offset shifts forward", func(t *testing.T) {
		vs := mustParse(t, `foo offset -30m`).(*VectorSelector)
		if vs.OriginalOffset != -30*time.Minute {
			t.Errorf("offset = %s, want -30m", vs.OriginalOffset)
		}
	})
	t.Run("at and offset may be written in either order", func(t *testing.T) {
		a := mustParse(t, `foo @ 100 offset 5m`)
		b := mustParse(t, `foo offset 5m @ 100`)
		if a.String() != b.String() {
			t.Errorf("order should not matter:\n  %s\n  %s", a, b)
		}
	})
	t.Run("at start and end presets", func(t *testing.T) {
		vs := mustParse(t, `foo @ start()`).(*VectorSelector)
		if vs.At == nil || vs.At.Preset != AtStart {
			t.Errorf("At = %+v, want start()", vs.At)
		}
	})
	t.Run("offset on a subquery", func(t *testing.T) {
		sq := mustParse(t, `rate(foo[5m])[1h:1m] offset 1d`).(*SubqueryExpr)
		if sq.OriginalOffset != 24*time.Hour {
			t.Errorf("offset = %s, want 1d", sq.OriginalOffset)
		}
	})
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantMsg string
	}{
		{"empty query", ``, "empty query"},
		{"empty selector", `{}`, "at least one non-empty matcher"},
		{"selector matching only absent labels", `{foo=""}`, "at least one non-empty matcher"},
		{"unclosed brace", `foo{a="1"`, `expected "}"`},
		{"unclosed paren", `sum(foo`, `expected ")"`},
		{"missing matcher operator", `foo{a "1"}`, "label matching operator"},
		{"unquoted matcher value", `foo{a=1}`, "quoted string"},
		{"invalid regex", `foo{a=~"("}`, "invalid regular expression"},
		{"unknown function", `no_such_function(foo)`, "unknown function"},
		{"wrong arity", `rate(foo[5m], 1)`, "expected 1 argument(s)"},
		{"wrong argument type", `rate(foo)`, "expects type range vector"},
		{"range on a non-selector", `sum(foo)[5m]`, "must follow a series selector"},
		{"subquery of a range vector", `foo[5m][1h:1m]`, "only allowed on an instant vector"},
		{"aggregation needs a vector", `sum(foo[5m])`, "expects an instant vector"},
		{"missing aggregation parameter", `topk(foo)`, "requires a parameter"},
		{"unexpected aggregation parameter", `sum(1, foo)`, "does not take a parameter"},
		{"scalar comparison without bool", `1 == 2`, "must use the bool modifier"},
		{"bool on a non-comparison", `foo + bool bar`, "bool modifier can only follow a comparison"},
		{"set operator on scalars", `1 and 2`, "requires instant vectors"},
		{"range vector in arithmetic", `foo[5m] + 1`, "must be scalar or instant vector"},
		{"grouping on a set operator", `foo and group_left bar`, "no grouping allowed"},
		{"offset before range", `foo offset 1h[5m]`, "must be written after the range"},
		{"repeated offset", `foo offset 1h offset 2h`, "only be set once"},
		{"malformed duration order", `foo[1s2h]`, "longest to shortest"},
		{"repeated duration unit", `foo[1m2m]`, "longest to shortest"},
		{"unterminated string", `foo{a="unterminated}`, "unterminated string"},
		{"trailing tokens", `foo bar`, "after the end of the expression"},
		{"bare aggregation keyword", `sum`, `expected "("`},
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
	_, err := Parse(`sum(foo) + no_such_function(bar)`)
	if err == nil {
		t.Fatal("expected an error")
	}
	parseErr, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("error is %T, want *ParseError", err)
	}
	if parseErr.Pos != 11 {
		t.Errorf("Pos = %d, want 11 (the start of the unknown function)", parseErr.Pos)
	}
	if got, want := parseErr.Error(), "1:12: parse error:"; !strings.HasPrefix(got, want) {
		t.Errorf("Error() = %q, want it to start with %q", got, want)
	}
}

func TestSelectorNameAndMatcherForms(t *testing.T) {
	// A bare name is kept in Name, leaving LabelMatchers to the braces.
	bare := mustParse(t, `http_requests_total{code="200"}`).(*VectorSelector)
	if bare.Name != "http_requests_total" {
		t.Errorf("Name = %q", bare.Name)
	}
	if len(bare.LabelMatchers) != 1 {
		t.Errorf("got %d matchers, want only the one written in braces", len(bare.LabelMatchers))
	}

	// A name written as a matcher stays a matcher, so String reproduces the
	// form the user chose.
	viaMatcher := mustParse(t, `{__name__=~"job:.*"}`).(*VectorSelector)
	if viaMatcher.Name != "" {
		t.Errorf("Name = %q, want empty when the name is written as a matcher", viaMatcher.Name)
	}
	if len(viaMatcher.LabelMatchers) != 1 || viaMatcher.LabelMatchers[0].Name != MetricNameLabel {
		t.Errorf("matchers = %v", viaMatcher.LabelMatchers)
	}
}

func TestFunctionNameIsNotReserved(t *testing.T) {
	// PromQL treats a function name as a function only when it is followed by
	// "(", so a series may be called "rate".
	vs, ok := mustParse(t, `rate`).(*VectorSelector)
	if !ok {
		t.Fatalf("`rate` alone should parse as a selector, got %T", mustParse(t, `rate`))
	}
	if vs.Name != "rate" {
		t.Errorf("Name = %q, want rate", vs.Name)
	}
}

func TestVariadicFunctions(t *testing.T) {
	// label_join takes at least three arguments and any number of source labels.
	for _, q := range []string{
		`label_join(foo, "dst", ",", "a")`,
		`label_join(foo, "dst", ",", "a", "b", "c")`,
	} {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%s): %v", q, err)
		}
	}
	if _, err := Parse(`label_join(foo, "dst")`); err == nil {
		t.Error("label_join with too few arguments should fail")
	}
	// round takes one optional argument.
	if _, err := Parse(`round(foo, 5, 6)`); err == nil {
		t.Error("round with three arguments should fail")
	}
}

// TestParserIsRegistered covers the init-time registration and the shared
// ast.Node contract the registry hands back.
func TestParserIsRegistered(t *testing.T) {
	p, err := parser.Get("promql")
	if err != nil {
		t.Fatalf("Get(\"promql\"): %v", err)
	}
	if p.DSL() != DSL {
		t.Errorf("DSL() = %q, want %q", p.DSL(), DSL)
	}

	// Lookup is case-insensitive, since a CLI flag may arrive spelled anyhow.
	if _, err := parser.Get("PromQL"); err != nil {
		t.Errorf("Get(\"PromQL\"): %v", err)
	}

	node, err := p.Parse(`rate(http_requests_total[5m])`)
	if err != nil {
		t.Fatalf("Parse via the registry: %v", err)
	}
	if node.DSL() != DSL {
		t.Errorf("node.DSL() = %q, want %q", node.DSL(), DSL)
	}
	if got, want := node.String(), `rate(http_requests_total[5m])`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	// On failure the interface must be a true nil, not a nil pointer in a
	// non-nil interface.
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
	_ ast.Node      = (*VectorSelector)(nil)
	_ []Expr        = []Expr{
		(*VectorSelector)(nil), (*MatrixSelector)(nil), (*SubqueryExpr)(nil),
		(*AggregateExpr)(nil), (*BinaryExpr)(nil), (*UnaryExpr)(nil),
		(*ParenExpr)(nil), (*Call)(nil), (*NumberLiteral)(nil), (*StringLiteral)(nil),
	}
)
