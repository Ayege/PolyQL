package traceql

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/parser"
)

// mustParse parses a query or fails the test.
func mustParse(t *testing.T, input string) Expr {
	t.Helper()
	expr, err := ParseExpr(input)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", input, err)
	}
	return expr
}

// spansetOf unwraps a query expected to be a bare span set.
func spansetOf(t *testing.T, input string) *Spanset {
	t.Helper()
	expr := mustParse(t, input)
	spanset, ok := expr.(*Spanset)
	if !ok {
		t.Fatalf("ParseExpr(%q) = %T, want *Spanset", input, expr)
	}
	return spanset
}

// filterOf unwraps a query expected to be a span set holding one comparison.
func filterOf(t *testing.T, input string) *SpansetFilter {
	t.Helper()
	spanset := spansetOf(t, input)
	filter, ok := spanset.Filter.(*SpansetFilter)
	if !ok {
		t.Fatalf("ParseExpr(%q) filter = %T, want *SpansetFilter", input, spanset.Filter)
	}
	return filter
}

// TestSimpleSpanset covers the smallest complete query.
func TestSimpleSpanset(t *testing.T) {
	filter := filterOf(t, `{span.http.status_code = 500}`)

	if got, want := filter.Attribute.Scope, ScopeSpan; got != want {
		t.Errorf("Scope = %v, want %v", got, want)
	}
	if got, want := filter.Attribute.Name, "http.status_code"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := filter.Attribute.Qualified(), "span.http.status_code"; got != want {
		t.Errorf("Qualified() = %q, want %q", got, want)
	}
	if got, want := filter.Op, OpEqual; got != want {
		t.Errorf("Op = %v, want %v", got, want)
	}
	if got, want := filter.Value.Kind, ValueNumber; got != want {
		t.Errorf("Value.Kind = %v, want %v", got, want)
	}
	if got, want := filter.Value.Number, 500.0; got != want {
		t.Errorf("Value.Number = %v, want %v", got, want)
	}
}

// TestMultipleFilters covers && joining two comparisons of different types.
func TestMultipleFilters(t *testing.T) {
	spanset := spansetOf(t, `{span.http.status_code = 500 && duration > 2s}`)

	binary, ok := spanset.Filter.(*FieldBinary)
	if !ok {
		t.Fatalf("filter = %T, want *FieldBinary", spanset.Filter)
	}
	if got, want := binary.Op, BoolAnd; got != want {
		t.Errorf("Op = %v, want %v", got, want)
	}

	right, ok := binary.RHS.(*SpansetFilter)
	if !ok {
		t.Fatalf("RHS = %T, want *SpansetFilter", binary.RHS)
	}
	// A bare "duration" is the span-model intrinsic, not an unscoped attribute.
	if got, want := right.Attribute.Scope, ScopeIntrinsic; got != want {
		t.Errorf("duration scope = %v, want %v", got, want)
	}
	if got, want := right.Value.Duration, 2*time.Second; got != want {
		t.Errorf("Duration = %v, want %v", got, want)
	}
	if got, want := right.Value.Text, "2s"; got != want {
		// The spelling survives so the emitter can write 2s rather than 2000ms.
		t.Errorf("Text = %q, want %q", got, want)
	}
}

// TestScopeVariety covers the three scopes a query can name.
func TestScopeVariety(t *testing.T) {
	spanset := spansetOf(t, `{intrinsic.trace_id = "abc" && resource.service.name = "web"}`)

	binary, ok := spanset.Filter.(*FieldBinary)
	if !ok {
		t.Fatalf("filter = %T, want *FieldBinary", spanset.Filter)
	}

	left := binary.LHS.(*SpansetFilter)
	if got, want := left.Attribute.Scope, ScopeIntrinsic; got != want {
		t.Errorf("left scope = %v, want %v", got, want)
	}
	// An explicitly written scope is remembered, so String reproduces the form
	// the author used rather than normalising it away.
	if !left.Attribute.Explicit {
		t.Error("an explicitly written intrinsic. prefix should be recorded")
	}
	if got, want := left.Value.Str, "abc"; got != want {
		t.Errorf("left value = %q, want %q", got, want)
	}

	right := binary.RHS.(*SpansetFilter)
	if got, want := right.Attribute.Scope, ScopeResource; got != want {
		t.Errorf("right scope = %v, want %v", got, want)
	}
	if got, want := right.Attribute.Name, "service.name"; got != want {
		t.Errorf("right name = %q, want %q", got, want)
	}
}

// TestScopeOnlyCountsBeforeADot covers the ambiguity the lexer deliberately
// leaves to the parser: "span" is a scope prefix only when a "." follows it, and
// an attribute genuinely named "span" must stay filterable.
func TestScopeOnlyCountsBeforeADot(t *testing.T) {
	filter := filterOf(t, `{.span = "x"}`)
	if got, want := filter.Attribute.Scope, ScopeNone; got != want {
		t.Errorf("Scope = %v, want %v", got, want)
	}
	if got, want := filter.Attribute.Name, "span"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
}

// TestStructuralOperators covers the three trace-tree relationships, and that
// child and descendant stay distinct.
func TestStructuralOperators(t *testing.T) {
	cases := []struct {
		input string
		want  StructuralOp
	}{
		{`{status = error} > {status = ok}`, StructChild},
		{`{status = error} >> {status = ok}`, StructDescendant},
		{`{.error = true} ~ {.error = true}`, StructSibling},
	}

	for _, c := range cases {
		t.Run(c.want.String(), func(t *testing.T) {
			expr := mustParse(t, c.input)
			structural, ok := expr.(*StructuralExpr)
			if !ok {
				t.Fatalf("ParseExpr(%q) = %T, want *StructuralExpr", c.input, expr)
			}
			if got := structural.Op; got != c.want {
				t.Errorf("Op = %v, want %v", got, c.want)
			}
			if _, ok := structural.LHS.(*Spanset); !ok {
				t.Errorf("LHS = %T, want *Spanset", structural.LHS)
			}
			if _, ok := structural.RHS.(*Spanset); !ok {
				t.Errorf("RHS = %T, want *Spanset", structural.RHS)
			}
		})
	}
}

// TestStructuralChainIsLeftAssociative pins the grouping of a three-way chain.
// "a > b > c" is the grandchildren of a, not a related to a nested pair.
func TestStructuralChainIsLeftAssociative(t *testing.T) {
	expr := mustParse(t, `{.a = 1} > {.b = 2} > {.c = 3}`)

	outer, ok := expr.(*StructuralExpr)
	if !ok {
		t.Fatalf("expr = %T, want *StructuralExpr", expr)
	}
	if _, ok := outer.LHS.(*StructuralExpr); !ok {
		t.Errorf("LHS = %T, want the nesting on the left", outer.LHS)
	}
	if _, ok := outer.RHS.(*Spanset); !ok {
		t.Errorf("RHS = %T, want a bare span set", outer.RHS)
	}
}

// TestMetricExtraction covers count() over (...), which TraceQL writes prefix
// rather than as a call wrapping its operand.
func TestMetricExtraction(t *testing.T) {
	expr := mustParse(t, `count() over ({.error = true})`)

	aggregate, ok := expr.(*AggregateExpr)
	if !ok {
		t.Fatalf("expr = %T, want *AggregateExpr", expr)
	}
	if got, want := aggregate.Op, AggCount; got != want {
		t.Errorf("Op = %v, want %v", got, want)
	}
	if aggregate.Attribute != nil {
		t.Errorf("count() counts spans and should carry no attribute, got %v", aggregate.Attribute)
	}
	if _, ok := aggregate.Operand.(*Spanset); !ok {
		t.Errorf("Operand = %T, want *Spanset", aggregate.Operand)
	}
	if aggregate.Grouping != nil {
		t.Error("no by clause was written")
	}
}

// TestMetricExtractionWithGrouping covers the by clause and an aggregate that
// takes an attribute.
func TestMetricExtractionWithGrouping(t *testing.T) {
	expr := mustParse(t, `sum(span.duration) over ({name = "GET"}) by (resource.service.name)`)

	aggregate, ok := expr.(*AggregateExpr)
	if !ok {
		t.Fatalf("expr = %T, want *AggregateExpr", expr)
	}
	if got, want := aggregate.Op, AggSum; got != want {
		t.Errorf("Op = %v, want %v", got, want)
	}
	if aggregate.Attribute == nil {
		t.Fatal("sum() should carry the attribute it aggregates")
	}
	if got, want := aggregate.Attribute.Qualified(), "span.duration"; got != want {
		t.Errorf("Attribute = %q, want %q", got, want)
	}
	if aggregate.Grouping == nil || len(aggregate.Grouping.Attributes) != 1 {
		t.Fatalf("Grouping = %v, want one attribute", aggregate.Grouping)
	}
	if got, want := aggregate.Grouping.Attributes[0].Qualified(), "resource.service.name"; got != want {
		t.Errorf("group key = %q, want %q", got, want)
	}
}

// TestAggregateArityIsChecked covers the two mismatches worth naming precisely,
// since both would otherwise surface much later as a type error.
func TestAggregateArityIsChecked(t *testing.T) {
	if _, err := ParseExpr(`sum() over ({})`); err == nil {
		t.Error("sum() with no attribute should fail")
	} else if !strings.Contains(err.Error(), "needs the attribute") {
		t.Errorf("error %q should say what is missing", err)
	}

	if _, err := ParseExpr(`count(span.duration) over ({})`); err == nil {
		t.Error("count() with an attribute should fail")
	} else if !strings.Contains(err.Error(), "takes no attribute") {
		t.Errorf("error %q should say what is extra", err)
	}
}

// TestNegation covers both a negated comparison and a negated group.
func TestNegation(t *testing.T) {
	t.Run("not equal", func(t *testing.T) {
		filter := filterOf(t, `{.error != true}`)
		if got, want := filter.Op, OpNotEqual; got != want {
			t.Errorf("Op = %v, want %v", got, want)
		}
		if got, want := filter.Value.Kind, ValueBool; got != want {
			t.Errorf("Value.Kind = %v, want %v", got, want)
		}
		if !filter.Value.Bool {
			t.Error("the operand is true")
		}
	})

	t.Run("logical not", func(t *testing.T) {
		spanset := spansetOf(t, `{!(.error = true)}`)
		not, ok := spanset.Filter.(*FieldNot)
		if !ok {
			t.Fatalf("filter = %T, want *FieldNot", spanset.Filter)
		}
		if _, ok := not.Inner.(*FieldParen); !ok {
			t.Errorf("Inner = %T, want *FieldParen", not.Inner)
		}
	})
}

// TestOrLogic covers || and the precedence that makes it bind looser than &&.
func TestOrLogic(t *testing.T) {
	spanset := spansetOf(t, `{.error = true || .status >= 500}`)

	binary, ok := spanset.Filter.(*FieldBinary)
	if !ok {
		t.Fatalf("filter = %T, want *FieldBinary", spanset.Filter)
	}
	if got, want := binary.Op, BoolOr; got != want {
		t.Errorf("Op = %v, want %v", got, want)
	}

	t.Run("or binds looser than and", func(t *testing.T) {
		// "a || b && c" must group as "a || (b && c)".
		spanset := spansetOf(t, `{.a = 1 || .b = 2 && .c = 3}`)
		top, ok := spanset.Filter.(*FieldBinary)
		if !ok {
			t.Fatalf("filter = %T, want *FieldBinary", spanset.Filter)
		}
		if top.Op != BoolOr {
			t.Fatalf("the top of the tree is %v, want OR", top.Op)
		}
		right, ok := top.RHS.(*FieldBinary)
		if !ok || right.Op != BoolAnd {
			t.Errorf("RHS = %v, want the && grouped beneath the ||", top.RHS)
		}
	})
}

// TestParentheses covers explicit grouping overriding precedence, and that the
// grouping is preserved rather than re-derived.
func TestParentheses(t *testing.T) {
	input := `{(.a = 1 || .b = 2) && .c = 3}`
	spanset := spansetOf(t, input)

	top, ok := spanset.Filter.(*FieldBinary)
	if !ok {
		t.Fatalf("filter = %T, want *FieldBinary", spanset.Filter)
	}
	if got, want := top.Op, BoolAnd; got != want {
		t.Errorf("Op = %v, want %v", got, want)
	}
	if _, ok := top.LHS.(*FieldParen); !ok {
		t.Errorf("LHS = %T, want the parentheses preserved", top.LHS)
	}
	if !strings.Contains(spanset.String(), "(.a = 1 || .b = 2)") {
		t.Errorf("String() = %q, want the author's grouping reproduced", spanset.String())
	}
}

// TestDurationComparisons covers the sub-second units span queries rely on.
func TestDurationComparisons(t *testing.T) {
	cases := []struct {
		input string
		op    CompareOp
		want  time.Duration
		text  string
	}{
		{`{duration > 100ms}`, OpGreater, 100 * time.Millisecond, "100ms"},
		{`{duration < 1s}`, OpLess, time.Second, "1s"},
		{`{duration >= 1h30m}`, OpGreaterEqual, 90 * time.Minute, "1h30m"},
		{`{duration <= 500us}`, OpLessEqual, 500 * time.Microsecond, "500us"},
		{`{duration > 1.5s}`, OpGreater, 1500 * time.Millisecond, "1.5s"},
	}

	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			filter := filterOf(t, c.input)
			if got := filter.Op; got != c.op {
				t.Errorf("Op = %v, want %v", got, c.op)
			}
			if got := filter.Value.Duration; got != c.want {
				t.Errorf("Duration = %v, want %v", got, c.want)
			}
			// The spelling is kept so a translation stays recognizable.
			if got := filter.Value.Text; got != c.text {
				t.Errorf("Text = %q, want %q", got, c.text)
			}
		})
	}
}

// TestEmptySpanset covers "{}", which selects every span.
func TestEmptySpanset(t *testing.T) {
	spanset := spansetOf(t, `{}`)
	if spanset.Filter != nil {
		t.Errorf("Filter = %v, want nil for an empty selector", spanset.Filter)
	}
	if got, want := spanset.String(), "{}"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestAttributeCoercion covers the "as" cast.
func TestAttributeCoercion(t *testing.T) {
	expr := mustParse(t, `{span.http.status_code = 500} as (span.http.status_code: int)`)

	coercion, ok := expr.(*AttributeCoercion)
	if !ok {
		t.Fatalf("expr = %T, want *AttributeCoercion", expr)
	}
	if got, want := coercion.AsType, CoerceInt; got != want {
		t.Errorf("AsType = %v, want %v", got, want)
	}
	if got, want := coercion.Attribute.Qualified(), "span.http.status_code"; got != want {
		t.Errorf("Attribute = %q, want %q", got, want)
	}
	if _, ok := coercion.Expr.(*Spanset); !ok {
		t.Errorf("Expr = %T, want *Spanset", coercion.Expr)
	}

	t.Run("an unknown target is rejected", func(t *testing.T) {
		if _, err := ParseExpr(`{} as (.x: uint128)`); err == nil {
			t.Error("expected an error for an unknown cast target")
		}
	})
}

// TestStatusAndKindKeywords covers TraceQL's unquoted enum operands, which would
// otherwise scan as attribute names.
func TestStatusAndKindKeywords(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		filter := filterOf(t, `{status = error}`)
		if got, want := filter.Value.Kind, ValueStatus; got != want {
			t.Errorf("Value.Kind = %v, want %v", got, want)
		}
		if got, want := filter.Value.Str, "error"; got != want {
			t.Errorf("Value.Str = %q, want %q", got, want)
		}
	})

	t.Run("kind", func(t *testing.T) {
		filter := filterOf(t, `{kind = server}`)
		if got, want := filter.Value.Kind, ValueSpanKind; got != want {
			t.Errorf("Value.Kind = %v, want %v", got, want)
		}
	})

	t.Run("a bare word elsewhere is an error", func(t *testing.T) {
		// The same word against an ordinary attribute is a missing quote, and
		// saying so beats accepting it as an identifier.
		_, err := ParseExpr(`{span.thing = error}`)
		if err == nil {
			t.Fatal("expected an error for an unquoted operand")
		}
		if !strings.Contains(err.Error(), "quoted") {
			t.Errorf("error %q should mention quoting", err)
		}
	})
}

// TestRegexOperators covers =~ and !~.
func TestRegexOperators(t *testing.T) {
	for _, c := range []struct {
		input string
		want  CompareOp
	}{
		{`{span.name =~ "GET /api/.*"}`, OpRegex},
		{`{span.name !~ "health.*"}`, OpNotRegex},
	} {
		filter := filterOf(t, c.input)
		if got := filter.Op; got != c.want {
			t.Errorf("%s: Op = %v, want %v", c.input, got, c.want)
		}
	}
}

// TestRoundTripsAreStable is the property the whole front end rests on: whatever
// String writes, the parser must accept and render identically. A tree that
// renders to something it cannot re-read would break translation the moment an
// emitter's output was fed back.
func TestRoundTripsAreStable(t *testing.T) {
	inputs := []string{
		`{span.http.status_code = 500}`,
		`{span.http.status_code = 500 && duration > 2s}`,
		`{intrinsic.trace_id = "abc" && resource.service.name = "web"}`,
		`{status = error} > {status = ok}`,
		`{status = error} >> {status = ok}`,
		`{.error = true} ~ {.error = true}`,
		`count() over ({.error = true})`,
		`count() over ({}) by (resource.service.name)`,
		`sum(span.duration) over ({name = "GET"}) by (resource.service.name, span.kind)`,
		`max(span.duration) over ({duration > 100ms})`,
		`{.error != true}`,
		`{.error = true || .status >= 500}`,
		`{(.a = 1 || .b = 2) && .c = 3}`,
		`{!(.a = 1)}`,
		`{duration > 100ms}`,
		`{}`,
		`{span.http.status_code = 500} as (span.http.status_code: int)`,
		`{kind = server}`,
		`({.a = 1} > {.b = 2}) ~ {.c = 3}`,
		`{span.name =~ "GET /.*"}`,
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			first := mustParse(t, input)
			rendered := first.String()

			second, err := ParseExpr(rendered)
			if err != nil {
				t.Fatalf("rendered %q does not re-parse: %v", rendered, err)
			}
			if got := second.String(); got != rendered {
				t.Errorf("rendering is not stable:\n  first:  %s\n  second: %s", rendered, got)
			}
		})
	}
}

// TestParseErrors covers the failures worth a precise message.
func TestParseErrors(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{``, "expected a span set"},
		{`{`, "expected an attribute name"},
		{`{.a = 1`, `expected "}"`},
		{`{.a}`, "expected a comparison operator"},
		{`{.a = }`, "expected a value"},
		{`span.foo = 1`, "written between braces"},
		{`{.a = 1} > `, "expected a span set"},
		{`{.a = 1} {.b = 2}`, "unexpected"},
		{`count() ({})`, `expected "over"`},
		{`nonsuch() over ({})`, "unknown aggregate"},
		{`{.a = 1 & .b = 2}`, "&&"},
		{`{.a = "unterminated}`, "unterminated"},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			_, err := ParseExpr(c.input)
			if err == nil {
				t.Fatalf("ParseExpr(%q) should have failed", c.input)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should contain %q", err, c.want)
			}
		})
	}
}

// TestParserIsRegistered covers the init-time registration the CLI depends on.
func TestParserIsRegistered(t *testing.T) {
	p, err := parser.Get(DSL)
	if err != nil {
		t.Fatalf("parser.Get(%q): %v", DSL, err)
	}
	if got := p.DSL(); got != DSL {
		t.Errorf("DSL() = %q, want %q", got, DSL)
	}

	node, err := p.Parse(`{span.http.status_code = 500}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := node.DSL(); got != DSL {
		t.Errorf("the tree reports DSL %q, want %q", got, DSL)
	}
}
