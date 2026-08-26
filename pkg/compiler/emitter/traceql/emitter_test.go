package traceql

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser/traceql"
	"github.com/polyql/polyql/pkg/registry"

	// The parser is imported so an emitted query can be fed straight back
	// through it, which is the property most of these tests turn on.
	_ "github.com/polyql/polyql/pkg/compiler/parser/traceql"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	return reg
}

// emit renders a query and splits the emitter's notes from the text.
func emit(t *testing.T, query *ir.Query) (text string, notes []string) {
	t.Helper()
	out, err := Emitter{}.Emit(query, testRegistry(t))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			notes = append(notes, strings.TrimSpace(strings.TrimPrefix(line, "#")))
			continue
		}
		text = line
	}
	return text, notes
}

// assertNote fails unless some note contains substr, listing what was recorded.
func assertNote(t *testing.T, notes []string, substr string) {
	t.Helper()
	for _, note := range notes {
		if strings.Contains(note, substr) {
			return
		}
	}
	t.Errorf("no note contains %q; got:\n  %s", substr, strings.Join(notes, "\n  "))
}

// assertParses is the property every emitted query must have: TraceQL's own
// parser has to accept it. A translation that does not parse is not one.
func assertParses(t *testing.T, text string) {
	t.Helper()
	if _, err := traceql.ParseExpr(text); err != nil {
		t.Errorf("emitted %q, which does not parse as TraceQL: %v", text, err)
	}
}

// spanQuery starts a span query whose source carries the given filter.
func spanQuery(filter ir.Predicate) *ir.Query {
	return &ir.Query{
		Signal: ir.SignalSpan,
		Source: &ir.DataSource{Scope: ir.ScopeSpan, Spanset: &ir.SpansetSelector{Filters: filter}},
		Output: &ir.Output{},
	}
}

func match(key string, op ir.MatchOp, value string) ir.Predicate {
	return &ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: key, Op: op, Value: value}}
}

func logical(op ir.LogicalOp, operands ...ir.Predicate) ir.Predicate {
	return &ir.LogicalPredicate{Op: op, Operands: operands}
}

// TestSpansetSelector covers the smallest complete rendering.
func TestSpansetSelector(t *testing.T) {
	query := spanQuery(match("span.http.status_code", ir.MatchEQ, "500"))

	text, _ := emit(t, query)
	if want := "{ span.http.status_code = 500 }"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestFilterConjunction covers && joining what arrives as separate stages.
//
// TraceQL has no pipeline, so a FilterStage has only one place to go: inside the
// span set's braces, conjoined with what is already there.
func TestFilterConjunction(t *testing.T) {
	query := spanQuery(match("span.http.status_code", ir.MatchEQ, "500"))
	query.Pipeline = ir.Pipeline{
		&ir.FilterStage{Predicate: match("duration", ir.MatchGT, "100ms")},
	}

	text, _ := emit(t, query)
	if want := "{ span.http.status_code = 500 && duration > 100ms }"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestDisjunction covers ||, which is the reason a span set's filter is a tree
// rather than a matcher list.
func TestDisjunction(t *testing.T) {
	query := spanQuery(logical(ir.LogicalOr,
		match("span.error", ir.MatchEQ, "true"),
		match("span.status_code", ir.MatchGTE, "500"),
	))

	text, _ := emit(t, query)
	if want := "{ span.error = true || span.status_code >= 500 }"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestNestedBooleanLogic covers grouping: an OR beneath an AND has to be
// parenthesised or the query means something else.
func TestNestedBooleanLogic(t *testing.T) {
	query := spanQuery(logical(ir.LogicalAnd,
		logical(ir.LogicalOr,
			match("span.a", ir.MatchEQ, "1"),
			match("span.b", ir.MatchEQ, "2"),
		),
		match("span.c", ir.MatchEQ, "3"),
	))

	text, _ := emit(t, query)
	if want := "{ (span.a = 1 || span.b = 2) && span.c = 3 }"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)

	t.Run("deeply nested", func(t *testing.T) {
		query := spanQuery(logical(ir.LogicalOr,
			logical(ir.LogicalAnd,
				match("span.a", ir.MatchEQ, "1"),
				logical(ir.LogicalOr,
					match("span.b", ir.MatchEQ, "2"),
					match("span.c", ir.MatchEQ, "3"),
				),
			),
			match("span.d", ir.MatchEQ, "4"),
		))
		text, _ := emit(t, query)
		assertParses(t, text)
		// Whatever the spacing, the grouping has to survive the round trip.
		reparsed, err := traceql.ParseExpr(text)
		if err != nil {
			t.Fatal(err)
		}
		if got := reparsed.String(); got != text {
			t.Errorf("re-rendering changed the query:\n  %s\n  %s", text, got)
		}
	})
}

// TestNegation covers the NOT connective, whose operand is always parenthesised
// rather than relying on the reader to know the precedence.
func TestNegation(t *testing.T) {
	query := spanQuery(logical(ir.LogicalNot, match("span.error", ir.MatchEQ, "true")))

	text, _ := emit(t, query)
	if want := "{ !(span.error = true) }"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestEmptySpanset covers a source with nothing to select on.
func TestEmptySpanset(t *testing.T) {
	query := spanQuery(nil)

	text, _ := emit(t, query)
	if want := "{}"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestStructuralOperators covers the three relationships, and that child and
// descendant stay distinct in the output.
func TestStructuralOperators(t *testing.T) {
	cases := []struct {
		op   ir.StructuralOp
		want string
	}{
		// "status" is a span-model intrinsic, so it stays bare rather than
		// gaining the leading dot an unscoped attribute would.
		{ir.StructuralChild, "{ status = 500 } > { status = 200 }"},
		{ir.StructuralDescendant, "{ status = 500 } >> { status = 200 }"},
		{ir.StructuralSibling, "{ status = 500 } ~ { status = 200 }"},
	}

	for _, c := range cases {
		t.Run(c.op.String(), func(t *testing.T) {
			query := spanQuery(match("status", ir.MatchEQ, "500"))
			query.Pipeline = ir.Pipeline{&ir.StructuralStage{
				Op:    c.op,
				Right: spanQuery(match("status", ir.MatchEQ, "200")),
			}}

			text, _ := emit(t, query)
			if text != c.want {
				t.Errorf("text = %q, want %q", text, c.want)
			}
			assertParses(t, text)
		})
	}
}

// TestCountAggregation covers metric extraction, which TraceQL writes prefix.
func TestCountAggregation(t *testing.T) {
	query := spanQuery(match("span.error", ir.MatchEQ, "true"))
	query.Pipeline = ir.Pipeline{
		&ir.AggregationStage{Op: ir.AggCount, Scope: ir.AggScopeGroup},
	}

	text, _ := emit(t, query)
	if want := "count() over ({ span.error = true })"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestCountAggregationWithGrouping covers the by clause.
func TestCountAggregationWithGrouping(t *testing.T) {
	query := spanQuery(match("span.error", ir.MatchEQ, "true"))
	query.Pipeline = ir.Pipeline{
		&ir.AggregationStage{
			Op:      ir.AggCount,
			Scope:   ir.AggScopeGroup,
			GroupBy: []string{"resource.service.name"},
		},
	}

	text, _ := emit(t, query)
	if want := "count() over ({ span.error = true }) by (resource.service.name)"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestSumAggregation covers an aggregate that names the attribute it combines.
func TestSumAggregation(t *testing.T) {
	query := spanQuery(nil)
	query.Pipeline = ir.Pipeline{
		&ir.AggregationStage{
			Op:        ir.AggSum,
			Scope:     ir.AggScopeGroup,
			Parameter: &ir.RefExpr{Name: "span.duration", Scope: ir.ScopeSpan, Type: ir.DataTypeDouble},
		},
	}

	text, _ := emit(t, query)
	if want := "sum(span.duration) over ({})"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestSumWithoutAnAttributeIsReported covers the shape a PromQL sum arrives in:
// it aggregates a metric's own value, and TraceQL's sum() needs a named span
// attribute instead.
func TestSumWithoutAnAttributeIsReported(t *testing.T) {
	query := spanQuery(nil)
	query.Pipeline = ir.Pipeline{
		&ir.AggregationStage{Op: ir.AggSum, Scope: ir.AggScopeGroup},
	}

	text, notes := emit(t, query)
	assertNote(t, notes, "aggregates one named span attribute")
	// Dropping the aggregation must still leave something that parses.
	assertParses(t, text)
}

// TestTemporalAggregationIsPartial covers the axis mismatch. TraceQL has no
// window, so every aggregation collapses across spans; a temporal one from
// PromQL or LogQL is therefore an approximation, not a clean translation.
func TestTemporalAggregationIsPartial(t *testing.T) {
	query := spanQuery(nil)
	query.Pipeline = ir.Pipeline{
		&ir.AggregationStage{Op: ir.AggCount, Scope: ir.AggScopeTemporal},
	}

	text, notes := emit(t, query)
	assertNote(t, notes, "across spans rather than over time")
	assertParses(t, text)
}

// TestUnsupportedAggregation covers an operator TraceQL has on no axis.
func TestUnsupportedAggregation(t *testing.T) {
	query := spanQuery(nil)
	query.Pipeline = ir.Pipeline{
		&ir.AggregationStage{Op: ir.AggRate, Scope: ir.AggScopeTemporal},
	}

	_, notes := emit(t, query)
	assertNote(t, notes, "no rate aggregation")
}

// TestCoercion covers the "as" cast.
func TestCoercion(t *testing.T) {
	query := spanQuery(match("span.http.status_code", ir.MatchEQ, "500"))
	query.Pipeline = ir.Pipeline{
		&ir.CoercionStage{Attribute: "span.http.status_code", TargetType: ir.DataTypeSignedInt},
	}

	text, _ := emit(t, query)
	if want := "{ span.http.status_code = 500 } as (span.http.status_code: int)"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestArithmeticIsUnsupported covers the class of node TraceQL cannot take at
// all: a span set is not a number.
func TestArithmeticIsUnsupported(t *testing.T) {
	t.Run("binary", func(t *testing.T) {
		query := spanQuery(nil)
		query.Pipeline = ir.Pipeline{&ir.BinaryOpStage{Op: ir.ArithDiv}}

		text, notes := emit(t, query)
		assertNote(t, notes, "no arithmetic between span sets")
		assertParses(t, text)
	})

	t.Run("unary", func(t *testing.T) {
		query := spanQuery(nil)
		query.Pipeline = ir.Pipeline{&ir.UnaryOpStage{Op: ir.ArithNeg}}

		text, notes := emit(t, query)
		assertNote(t, notes, "no unary sign")
		assertParses(t, text)
	})
}

// TestJoinIsUnsupported covers vector matching, which has no TraceQL form: spans
// carry their relationships in the trace tree instead.
func TestJoinIsUnsupported(t *testing.T) {
	query := spanQuery(nil)
	query.Pipeline = ir.Pipeline{&ir.JoinStage{
		JoinType: ir.JoinInner,
		OnLabels: []string{"job"},
		RightSide: &ir.Query{
			Signal: ir.SignalMetric,
			Source: &ir.DataSource{Name: "up"},
		},
	}}

	text, notes := emit(t, query)
	assertNote(t, notes, "no join")
	assertParses(t, text)
}

// TestWindowIsReported covers the time range, which Tempo takes as request
// parameters rather than in the query text. Losing it silently would change
// which spans the query reads.
func TestWindowIsReported(t *testing.T) {
	query := spanQuery(nil)
	query.Output.Window = &ir.Window{Step: ir.NewIntervalFromSource(5*time.Minute, "5m")}

	text, notes := emit(t, query)
	assertNote(t, notes, "no range selector")
	assertNote(t, notes, "5m")
	assertParses(t, text)

	t.Run("offset", func(t *testing.T) {
		query := spanQuery(nil)
		query.Output.Window = &ir.Window{Offset: ir.NewIntervalFromSource(time.Hour, "1h")}
		_, notes := emit(t, query)
		assertNote(t, notes, "cannot offset a query in time")
	})

	t.Run("subquery", func(t *testing.T) {
		query := spanQuery(nil)
		outer := ir.NewIntervalFromSource(30*time.Minute, "30m")
		query.Output.SubqueryRange = &outer
		_, notes := emit(t, query)
		assertNote(t, notes, "no subquery form")
	})
}

// TestMetricNameIsReported covers a PromQL source arriving here. TraceQL selects
// spans rather than named series, so mapping the name onto the span name would
// claim an equivalence that does not hold.
func TestMetricNameIsReported(t *testing.T) {
	query := &ir.Query{
		Signal: ir.SignalMetric,
		Source: &ir.DataSource{Name: "http_requests_total"},
		Output: &ir.Output{},
	}

	text, notes := emit(t, query)
	assertNote(t, notes, "is a metric name")
	assertParses(t, text)
}

// TestFlatLabelsGainAScope covers a query from PromQL or LogQL, whose bare label
// keys have no scope. TraceQL has no unscoped namespace, and the leading dot is
// its "resolve this against every scope".
func TestFlatLabelsGainAScope(t *testing.T) {
	query := &ir.Query{
		Signal: ir.SignalLog,
		Source: &ir.DataSource{Selectors: []*ir.Selector{{
			Matchers: []*ir.LabelMatcher{{Key: "app", Op: ir.MatchEQ, Value: "web"}},
		}}},
		Output: &ir.Output{},
	}

	text, _ := emit(t, query)
	if want := `{ .app = "web" }`; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestValueTyping covers the operand types the IR stores as text and this
// emitter has to recover, since writing a duration quoted would change its
// meaning.
func TestValueTyping(t *testing.T) {
	cases := []struct {
		key, value string
		want       string
	}{
		{"span.http.status_code", "500", "{ span.http.status_code = 500 }"},
		{"duration", "100ms", "{ duration = 100ms }"},
		{"span.error", "true", "{ span.error = true }"},
		{"status", "error", "{ status = error }"},
		{"kind", "server", "{ kind = server }"},
		{"resource.service.name", "web", `{ resource.service.name = "web" }`},
		{"span.name", "GET /api", `{ span.name = "GET /api" }`},
	}

	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			text, _ := emit(t, spanQuery(match(c.key, ir.MatchEQ, c.value)))
			if text != c.want {
				t.Errorf("text = %q, want %q", text, c.want)
			}
			assertParses(t, text)
		})
	}
}

// TestSetMembershipBecomesRegex covers IN, which TraceQL has no operator for.
func TestSetMembershipBecomesRegex(t *testing.T) {
	query := spanQuery(&ir.MatchPredicate{Matcher: &ir.LabelMatcher{
		Key: "span.method", Op: ir.MatchIn, Values: []string{"GET", "POST"},
	}})

	text, _ := emit(t, query)
	if want := `{ span.method =~ "GET|POST" }`; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	assertParses(t, text)
}

// TestContainmentBecomesRegex covers a LogQL line filter reaching here.
func TestContainmentBecomesRegex(t *testing.T) {
	query := spanQuery(match("span.msg", ir.MatchContains, "error.log"))

	text, notes := emit(t, query)
	assertNote(t, notes, "no containment filter")
	// The dot must be escaped, or "error.log" would also match "errorXlog".
	// The text carries the escape doubled because the pattern is inside a
	// quoted string literal, which is what TraceQL's own unquoting expects.
	if !strings.Contains(text, `error\\.log`) {
		t.Errorf("text = %q, want the literal dot escaped", text)
	}
	assertParses(t, text)
}

// TestNullPredicateIsAnError covers the one predicate this emitter refuses
// rather than approximates: TraceQL has no IS NULL, and there is no pattern that
// says the same thing.
func TestNullPredicateIsAnError(t *testing.T) {
	query := spanQuery(match("span.region", ir.MatchIsNull, ""))

	if _, err := (Emitter{}).Emit(query, testRegistry(t)); err == nil {
		t.Error("expected an error for a NULL predicate")
	}
}

// TestFilterAfterCloseIsReported covers the one-way door: once an aggregation or
// a structural operator has closed the span set, there are no braces left to put
// a filter in.
func TestFilterAfterCloseIsReported(t *testing.T) {
	query := spanQuery(nil)
	query.Pipeline = ir.Pipeline{
		&ir.AggregationStage{Op: ir.AggCount, Scope: ir.AggScopeGroup},
		&ir.FilterStage{Predicate: match("span.error", ir.MatchEQ, "true")},
	}

	text, notes := emit(t, query)
	assertNote(t, notes, "once it has been aggregated")
	assertParses(t, text)
}

// TestWithoutClauseIsReported covers the grouping form TraceQL lacks: it can
// name the attributes to keep, never the ones to drop.
func TestWithoutClauseIsReported(t *testing.T) {
	query := spanQuery(nil)
	query.Pipeline = ir.Pipeline{&ir.AggregationStage{
		Op:      ir.AggCount,
		Scope:   ir.AggScopeGroup,
		Without: []string{"span.id"},
	}}

	text, notes := emit(t, query)
	assertNote(t, notes, "only name the attributes to group by")
	assertParses(t, text)
}

// TestEmitterIsRegistered covers the init-time registration the CLI depends on.
func TestEmitterIsRegistered(t *testing.T) {
	e, err := emitter.Get(DSL)
	if err != nil {
		t.Fatalf("emitter.Get(%q): %v", DSL, err)
	}
	if got := e.DSL(); got != DSL {
		t.Errorf("DSL() = %q, want %q", got, DSL)
	}
}

// TestEmitRejectsBadInput covers the two arguments Emit cannot work without.
func TestEmitRejectsBadInput(t *testing.T) {
	if _, err := (Emitter{}).Emit(nil, testRegistry(t)); err == nil {
		t.Error("expected an error for a nil query")
	}
	var noRegistry *registry.Registry
	if _, err := (Emitter{}).Emit(spanQuery(nil), noRegistry); err == nil {
		t.Error("expected an error for a nil registry")
	}
}

// TestLogBodyFilterIsDroppedNotInvented covers a predicate that has nothing to
// address in a span.
//
// A log line filter arriving from LogQL used to be written as ".body =~ ..." —
// valid TraceQL against an attribute spans do not have, so it could only ever
// match nothing while reading like a working filter. Dropping it and saying so
// is what every other target does with a construct it cannot express.
func TestLogBodyFilterIsDroppedNotInvented(t *testing.T) {
	for _, op := range []ir.MatchOp{ir.MatchContains, ir.MatchRegex, ir.MatchNotContains} {
		t.Run(op.String(), func(t *testing.T) {
			query := &ir.Query{
				Signal: ir.SignalLog,
				Source: &ir.DataSource{Selectors: []*ir.Selector{{
					Matchers: []*ir.LabelMatcher{{Key: "app", Op: ir.MatchEQ, Value: "web"}},
				}}},
				Pipeline: ir.Pipeline{
					&ir.FilterStage{Predicate: match(ir.FieldBody, op, "err")},
				},
				Output: &ir.Output{},
			}

			text, notes := emit(t, query)

			if strings.Contains(text, "body") {
				t.Errorf("text = %q, want no filter on an attribute spans do not have", text)
			}
			// What did survive must still be there: dropping the body test is
			// not a reason to lose the stream label beside it.
			if !strings.Contains(text, `.app = "web"`) {
				t.Errorf("text = %q, want the other matcher kept", text)
			}
			assertNote(t, notes, "no log line to filter on")
			assertParses(t, text)
		})
	}

	t.Run("a body test under a conjunction leaves the rest", func(t *testing.T) {
		query := &ir.Query{
			Signal: ir.SignalLog,
			Source: &ir.DataSource{Scope: ir.ScopeSpan, Spanset: &ir.SpansetSelector{
				Filters: logical(ir.LogicalAnd,
					match("span.a", ir.MatchEQ, "1"),
					match(ir.FieldBody, ir.MatchContains, "err"),
				),
			}},
			Output: &ir.Output{},
		}

		text, notes := emit(t, query)

		if strings.Contains(text, "body") {
			t.Errorf("text = %q, want the body test gone", text)
		}
		if !strings.Contains(text, "span.a = 1") {
			t.Errorf("text = %q, want the other conjunct kept", text)
		}
		assertNote(t, notes, "no log line to filter on")
		assertParses(t, text)
	})

	t.Run("a query that was only a body test writes nothing but says why", func(t *testing.T) {
		query := &ir.Query{
			Signal: ir.SignalLog,
			Source: &ir.DataSource{Scope: ir.ScopeSpan, Spanset: &ir.SpansetSelector{
				Filters: match(ir.FieldBody, ir.MatchContains, "err"),
			}},
			Output: &ir.Output{},
		}

		text, notes := emit(t, query)

		// An empty span set selects everything, which is wider than was asked —
		// so the note is the whole of the honesty here.
		if got := text; got != "{}" {
			t.Errorf("text = %q, want the empty selector", got)
		}
		assertNote(t, notes, "no log line to filter on")
		assertParses(t, text)
	})
}
