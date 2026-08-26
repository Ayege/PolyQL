// Package traceql renders TelemetryIR into TraceQL text.
//
// TraceQL neither nests like PromQL nor threads a pipeline like LogQL, so
// emission works differently from both. A query starts as a set of filter texts
// destined for one pair of braces, and stays open to absorbing more of them
// until something closes it: an aggregation, which wraps the span set in
// count() over (...), or a structural operator, which relates it to another span
// set. Filters arriving before that point are conjoined inside the braces rather
// than appended as stages, because TraceQL has nowhere else to put them.
//
// # What TraceQL cannot take
//
// Three whole classes of IR node have no TraceQL form, and the registry says so
// rather than this file deciding: arithmetic, because a span set is not a
// number; joins, because spans are related by the trace tree instead; and
// windows, because a Tempo request carries its time range outside the query
// text. Each is recorded as a note rather than silently dropped.
package traceql

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser/traceql"
	"github.com/polyql/polyql/pkg/registry"
)

// DSL is the name this emitter registers under.
const DSL = "traceql"

// Emitter renders IR as TraceQL. It holds no state, so the registered instance
// is safe to share across concurrent translations.
type Emitter struct{}

// DSL returns "traceql".
func (Emitter) DSL() string { return DSL }

// Emit renders a query as TraceQL.
func (Emitter) Emit(query *ir.Query, reg *registry.Registry) (string, error) {
	if query == nil {
		return "", fmt.Errorf("traceql: cannot emit a nil query")
	}
	if reg == nil {
		return "", fmt.Errorf("traceql: a registry is required")
	}
	def, err := reg.Get(DSL)
	if err != nil {
		return "", fmt.Errorf("traceql: %w", err)
	}

	w := &writer{def: def, reg: reg}
	text, err := w.emitQuery(query)
	if err != nil {
		return "", err
	}
	return w.notes.Prepend(text), nil
}

func init() { emitter.Register(Emitter{}) }

// writer carries the emission state for one query.
type writer struct {
	def   *registry.DSLDefinition
	reg   *registry.Registry
	notes emitter.Notes
}

// rendered is a span set part-way through emission.
//
// While the span set is open, filters holds the predicate texts bound for its
// braces and text is empty; a later FilterStage conjoins into that list. Once
// something wraps the span set, text becomes authoritative and closed is set,
// after which nothing more can go inside the braces.
type rendered struct {
	filters []string
	text    string
	closed  bool
	// atomic reports whether the text needs no parentheses as an operand.
	atomic bool
}

// String renders the span set as it currently stands.
func (r rendered) String() string {
	if r.closed {
		return r.text
	}
	if len(r.filters) == 0 {
		return "{}"
	}
	return "{ " + strings.Join(r.filters, " && ") + " }"
}

// operand renders the span set for a position where a bare relationship would
// be ambiguous, parenthesising it when it is not already atomic.
func (r rendered) operand() string {
	text := r.String()
	if r.atomic || !r.closed {
		return text
	}
	return "(" + text + ")"
}

func (w *writer) emitQuery(query *ir.Query) (string, error) {
	if reason, ok := emitter.Unsupported(query); ok {
		w.notes.AddUnsupported(reason)
	}

	// A node carries one reason, so a query the validator downgraded for
	// several causes reports only the first. The emitter therefore also asks
	// directly about what it knows it cannot write.
	w.noteTimeHandling(query)

	expr, err := w.emitSource(query)
	if err != nil {
		return "", err
	}

	for _, stage := range query.Pipeline {
		expr, err = w.applyStage(expr, stage)
		if err != nil {
			return "", err
		}
	}

	return expr.String(), nil
}

// noteTimeHandling says what happened to the query's time range.
//
// TraceQL has no way to write one: Tempo takes start and end as request
// parameters. A translated query that silently lost its window would run over
// whatever range the caller happened to ask for, which is exactly the kind of
// quiet difference the fidelity report exists to prevent.
func (w *writer) noteTimeHandling(query *ir.Query) {
	if query.Output == nil {
		return
	}
	if query.Output.IsSubquery() {
		w.notes.Addf("UNSUPPORTED: TraceQL has no subquery form; the inner expression was " +
			"written without its outer range and resolution")
	}
	if window := query.Output.Window; window != nil && !window.Step.IsZero() {
		w.notes.Addf("UNSUPPORTED: TraceQL has no range selector; the %s window was dropped, and "+
			"the query will read whatever time range the request asks for",
			window.Step)
	}
	if window := query.Output.Window; window != nil && !window.Offset.IsZero() {
		w.notes.Addf("UNSUPPORTED: TraceQL cannot offset a query in time; the %s offset was dropped",
			window.Offset)
	}
	if bounds := query.Output.Range; bounds != nil && !bounds.Start.IsZero() {
		w.notes.Addf("TraceQL carries its time range as request parameters rather than in the " +
			"query text; pass start and end to Tempo alongside this query")
	}
}

// emitSource renders the span set selector.
//
// Two shapes arrive here. A query resolved from TraceQL carries a
// SpansetSelector holding a boolean tree, which renders directly. A query from
// PromQL or LogQL carries flat conjunctive Selectors instead, whose matchers
// conjoin — and whose bare label keys have to be given a scope, since TraceQL
// has no unscoped namespace of the kind those languages assume.
func (w *writer) emitSource(query *ir.Query) (rendered, error) {
	var expr rendered
	if query.Source == nil {
		return expr, nil
	}

	// A metric name has no TraceQL equivalent: spans are not named series.
	// Mapping it onto the span name would claim an equivalence that does not
	// hold, so it is reported rather than invented.
	if query.Source.Name != "" {
		w.notes.Addf("UNSUPPORTED: %q is a metric name, and TraceQL selects spans rather than "+
			"named series; the name was dropped", query.Source.Name)
	}

	if spanset := query.Source.Spanset; spanset != nil {
		if spanset.Filters != nil {
			text, err := w.predicateText(spanset.Filters, false)
			if err != nil {
				return expr, err
			}
			if text != "" {
				expr.filters = append(expr.filters, text)
			}
		}
	}

	for _, selector := range query.Source.Selectors {
		for _, matcher := range selector.Matchers {
			if reason, ok := emitter.Unsupported(matcher); ok {
				w.notes.AddUnsupported(reason)
				continue
			}
			text, err := w.matcherText(matcher)
			if err != nil {
				return expr, err
			}
			expr.filters = append(expr.filters, text)
		}
	}

	return expr, nil
}

func (w *writer) applyStage(expr rendered, stage ir.PipelineStage) (rendered, error) {
	if reason, ok := emitter.Unsupported(stage); ok {
		w.notes.AddUnsupported(reason)
		return expr, nil
	}

	switch node := stage.(type) {
	case *ir.FilterStage:
		return w.applyFilter(expr, node)
	case *ir.AggregationStage:
		return w.applyAggregation(expr, node)
	case *ir.StructuralStage:
		return w.applyStructural(expr, node)
	case *ir.CoercionStage:
		return w.applyCoercion(expr, node)
	case *ir.FunctionStage:
		return w.applyFunction(expr, node)
	case *ir.JoinStage:
		w.notes.Addf("UNSUPPORTED: TraceQL has no join; spans are related by the trace tree " +
			"rather than by matching their attributes, so the second operand was dropped")
		return expr, nil
	case *ir.BinaryOpStage:
		w.notes.Addf("UNSUPPORTED: TraceQL has no arithmetic between span sets; the %q operation "+
			"was dropped", strings.ToLower(node.Op.String()))
		return expr, nil
	case *ir.UnaryOpStage:
		w.notes.Addf("UNSUPPORTED: TraceQL has no unary sign; a span set is not a number, so the "+
			"%q was dropped", strings.ToLower(node.Op.String()))
		return expr, nil
	default:
		return expr, fmt.Errorf("traceql: no rule for pipeline stage type %T", stage)
	}
}

// applyFilter conjoins a predicate into the span set's braces.
//
// TraceQL has no pipeline, so a filter has only one place to go. Once something
// has closed the span set there is no longer a pair of braces to put it in, and
// the filter is reported rather than written somewhere it would mean something
// different.
func (w *writer) applyFilter(expr rendered, stage *ir.FilterStage) (rendered, error) {
	if stage.Predicate == nil {
		return expr, nil
	}
	text, err := w.predicateText(stage.Predicate, false)
	if err != nil {
		return expr, err
	}
	if text == "" {
		// Nothing survived the lowering, and the reason was already noted.
		return expr, nil
	}

	if expr.closed {
		w.notes.Addf("UNSUPPORTED: TraceQL cannot filter a span set once it has been aggregated "+
			"or related to another; the filter %s was dropped", text)
		return expr, nil
	}
	expr.filters = append(expr.filters, text)
	return expr, nil
}

// applyAggregation wraps the span set in a metric extraction.
func (w *writer) applyAggregation(expr rendered, stage *ir.AggregationStage) (rendered, error) {
	fn, ok := w.def.FunctionForAggregation(stage.Op, stage.Scope)
	if !ok {
		// The operator may still exist on the other axis. TraceQL only has the
		// group axis, so a temporal aggregation lands here.
		if alternatives := w.def.FunctionsForAggOp(stage.Op); len(alternatives) > 0 {
			w.notes.Addf("PARTIAL: TraceQL aggregates across spans rather than over time; "+
				"%q was written as %s(), which collapses a different axis",
				strings.ToLower(stage.Op.String()), alternatives[0].Name)
			fn = alternatives[0]
		} else {
			w.notes.Addf("UNSUPPORTED: TraceQL has no %s aggregation",
				strings.ToLower(stage.Op.String()))
			return expr, nil
		}
	}

	// sum, avg, min and max need to know which attribute they are combining;
	// only count() stands alone. A query arriving from PromQL aggregates a
	// metric's own value, which has no TraceQL spelling.
	argument := ""
	if fn.Name != "count" {
		attribute, ok := w.aggregatedAttribute(stage)
		if !ok {
			w.notes.Addf("UNSUPPORTED: TraceQL's %s() aggregates one named span attribute, and "+
				"the query names none; the aggregation was dropped", fn.Name)
			return expr, nil
		}
		argument = attribute
	}

	var b strings.Builder
	b.WriteString(fn.Name + "(" + argument + ") over (" + expr.String() + ")")
	if clause := w.groupingClause(stage); clause != "" {
		b.WriteString(" " + clause)
	}
	return rendered{text: b.String(), closed: true, atomic: true}, nil
}

// aggregatedAttribute finds the attribute an aggregation combines, which the
// resolver parks on the stage's parameter.
func (w *writer) aggregatedAttribute(stage *ir.AggregationStage) (string, bool) {
	ref, ok := stage.Parameter.(*ir.RefExpr)
	if !ok || ref.Name == "" {
		return "", false
	}
	return w.attributeText(ref.Name), true
}

// groupingClause renders a by clause. TraceQL has no "without": a query can only
// name the attributes to keep.
func (w *writer) groupingClause(stage *ir.AggregationStage) string {
	if len(stage.Without) > 0 {
		w.notes.Addf("UNSUPPORTED: TraceQL can only name the attributes to group by, not the ones "+
			"to drop; \"without (%s)\" was left out and the result is grouped over everything",
			strings.Join(stage.Without, ", "))
	}
	if len(stage.GroupBy) == 0 {
		return ""
	}
	keys := make([]string, 0, len(stage.GroupBy))
	for _, key := range stage.GroupBy {
		keys = append(keys, w.attributeText(key))
	}
	return "by (" + strings.Join(keys, ", ") + ")"
}

// applyStructural relates the span set so far to another.
func (w *writer) applyStructural(expr rendered, stage *ir.StructuralStage) (rendered, error) {
	op, ok := w.def.StructuralOperatorForIROp(stage.Op)
	if !ok {
		w.notes.Addf("UNSUPPORTED: TraceQL has no %s relationship",
			strings.ToLower(stage.Op.String()))
		return expr, nil
	}
	if stage.Right == nil {
		w.notes.Addf("UNSUPPORTED: a %s relationship was recorded with no span set on its right; "+
			"only the left-hand side was written", strings.ToLower(stage.Op.String()))
		return expr, nil
	}

	right, err := w.emitOperand(stage.Right)
	if err != nil {
		return expr, err
	}
	return rendered{
		text:   expr.operand() + " " + op.Symbol + " " + right,
		closed: true,
	}, nil
}

// emitOperand renders a nested query — the right-hand side of a structural
// operator — reusing this writer so its notes join the same list.
func (w *writer) emitOperand(query *ir.Query) (string, error) {
	if reason, ok := emitter.Unsupported(query); ok {
		w.notes.AddUnsupported(reason)
	}
	expr, err := w.emitSource(query)
	if err != nil {
		return "", err
	}
	for _, stage := range query.Pipeline {
		expr, err = w.applyStage(expr, stage)
		if err != nil {
			return "", err
		}
	}
	return expr.operand(), nil
}

// coercionNames maps a QLS type onto the TraceQL cast that produces it.
var coercionNames = map[ir.QlsDataType]string{
	ir.DataTypeSignedInt:   "int",
	ir.DataTypeUnsignedInt: "int",
	ir.DataTypeDouble:      "float",
	ir.DataTypeString:      "string",
	ir.DataTypeInterval:    "duration",
	ir.DataTypeBoolean:     "bool",
}

// applyCoercion appends an "as" cast.
func (w *writer) applyCoercion(expr rendered, stage *ir.CoercionStage) (rendered, error) {
	target, ok := coercionNames[stage.TargetType]
	if !ok {
		w.notes.Addf("UNSUPPORTED: TraceQL cannot cast an attribute to %s; the cast was dropped",
			stage.TargetType)
		return expr, nil
	}
	return rendered{
		text: expr.operand() + " as (" +
			w.attributeText(stage.Attribute) + ": " + target + ")",
		closed: true,
		atomic: false,
	}, nil
}

// applyFunction covers the function stages that reach a span query.
//
// Almost none of the other languages' functions apply to spans: a log parser has
// no line to read, and a metric function has no series. The registry decides
// which are expressible, and the validator has already flagged the rest.
func (w *writer) applyFunction(expr rendered, stage *ir.FunctionStage) (rendered, error) {
	if stage.Name == ir.FuncLiteral {
		// A bare literal is not a span set at all, and TraceQL has no scalar
		// expression to put one in.
		w.notes.Addf("UNSUPPORTED: TraceQL has no scalar expression; a bare literal cannot be " +
			"written as a query")
		return expr, nil
	}
	if _, ok := w.def.FunctionByIRName(stage.Name); !ok {
		w.notes.Addf("UNSUPPORTED: function %q has no TraceQL equivalent", stage.Name)
		return expr, nil
	}
	// A registry entry exists but TraceQL spells every one of them as an
	// aggregate, which applyAggregation already handles. Reaching here means the
	// resolver produced a FunctionStage for something the registry calls an
	// aggregation, which is a bug rather than a translation loss.
	return expr, fmt.Errorf("traceql: function %q reached the emitter as a plain function stage",
		stage.Name)
}

// predicateText renders a predicate tree.
//
// grouped says whether the caller has already placed the text somewhere that
// binds it, which decides whether a composite needs parentheses of its own.
func (w *writer) predicateText(predicate ir.Predicate, grouped bool) (string, error) {
	switch node := predicate.(type) {
	case *ir.MatchPredicate:
		if node.Matcher == nil {
			return "", fmt.Errorf("traceql: a match predicate carries no matcher")
		}
		// A predicate over the log body has nothing to address here: a span has
		// no line. Writing it against an invented attribute would produce valid
		// TraceQL that can only ever match nothing, which reads as a working
		// filter — so it is dropped and reported, the same way every other
		// target drops what it cannot express.
		if node.Matcher.Key == ir.FieldBody {
			w.notes.Addf("UNSUPPORTED: a span has no log line to filter on; the %s test on the "+
				"body was dropped rather than written against an attribute that does not exist",
				strings.ToLower(node.Matcher.Op.String()))
			return "", nil
		}
		return w.matcherText(node.Matcher)

	case *ir.LogicalPredicate:
		return w.logicalText(node, grouped)

	default:
		return "", fmt.Errorf("traceql: no rule for predicate type %T", predicate)
	}
}

func (w *writer) logicalText(node *ir.LogicalPredicate, grouped bool) (string, error) {
	if len(node.Operands) == 0 {
		return "", fmt.Errorf("traceql: a %s predicate has no operands", node.Op)
	}

	op, ok := w.def.LogicalOperatorForIROp(node.Op)
	if !ok {
		return "", fmt.Errorf("traceql: no operator spells %s", node.Op)
	}

	if node.Op == ir.LogicalNot {
		inner, err := w.predicateText(node.Operands[0], true)
		if err != nil {
			return "", err
		}
		if inner == "" {
			return "", nil
		}
		// A negation binds tightly, so its operand is always parenthesised
		// rather than relying on the reader to know the precedence.
		return op.Symbol + "(" + inner + ")", nil
	}

	parts := make([]string, 0, len(node.Operands))
	for _, operand := range node.Operands {
		text, err := w.predicateText(operand, true)
		if err != nil {
			return "", err
		}
		// A dropped operand leaves nothing to join. Under an AND the rest still
		// narrows correctly; under an OR it would widen, which the validator
		// has already reported on the predicate itself.
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return "", nil
	}
	joined := strings.Join(parts, " "+op.Symbol+" ")
	if grouped && len(parts) > 1 {
		return "(" + joined + ")", nil
	}
	return joined, nil
}

// matcherText renders one comparison.
func (w *writer) matcherText(m *ir.LabelMatcher) (string, error) {
	// A set-membership test becomes a regex alternation: TraceQL has no IN
	// operator, and an alternation says the same thing.
	if len(m.Values) > 0 {
		op := ir.MatchRegex
		if m.Op == ir.MatchNotIn {
			op = ir.MatchNotRegex
		}
		symbol, err := w.operatorSymbol(op)
		if err != nil {
			return "", err
		}
		quoted := make([]string, 0, len(m.Values))
		for _, value := range m.Values {
			quoted = append(quoted, regexp.QuoteMeta(value))
		}
		return w.attributeText(m.Key) + " " + symbol + " " +
			emitter.QuoteString(strings.Join(quoted, "|"), w.def.Normalizations.StringQuoting), nil
	}

	// TraceQL has no NULL predicate. Testing that an attribute is absent is
	// exactly what a span query most wants, so this is worth naming precisely
	// rather than approximating.
	if m.Op.IsUnary() {
		return "", fmt.Errorf("traceql: %s has no TraceQL spelling", m.Op)
	}

	// Nor a containment operator; an unanchored pattern over the escaped text
	// says the same thing.
	if m.Op.IsContainment() {
		op := ir.MatchRegex
		if m.Op == ir.MatchNotContains {
			op = ir.MatchNotRegex
		}
		symbol, err := w.operatorSymbol(op)
		if err != nil {
			return "", err
		}
		w.notes.Addf("PARTIAL: TraceQL has no containment filter; %q was written as a regular "+
			"expression over the escaped text", m.Value)
		pattern := ".*" + regexp.QuoteMeta(m.Value) + ".*"
		return w.attributeText(m.Key) + " " + symbol + " " +
			emitter.QuoteString(pattern, w.def.Normalizations.StringQuoting), nil
	}

	symbol, err := w.operatorSymbol(m.Op)
	if err != nil {
		return "", err
	}
	return w.attributeText(m.Key) + " " + symbol + " " + w.valueText(m.Key, m.Value), nil
}

func (w *writer) operatorSymbol(op ir.MatchOp) (string, error) {
	candidates := w.def.OperatorsForIROp(op)
	if len(candidates) == 0 {
		return "", fmt.Errorf("traceql: no operator spells %s", op)
	}
	return candidates[0].Symbol, nil
}

// attributeText writes an IR attribute key as TraceQL addresses it.
//
// A key already carrying a scope — "span.http.status_code" — goes out as it came
// in, and a span-model intrinsic stays bare. Anything else arrived from a
// language with one flat label namespace, and TraceQL has no such namespace: the
// leading dot is its "resolve this against every scope", which is the honest
// rendering of a key whose scope nothing recorded.
func (w *writer) attributeText(key string) string {
	if key == "" {
		return key
	}
	if traceql.Intrinsics[key] {
		return key
	}
	for _, scope := range []string{"span.", "resource.", "intrinsic."} {
		if strings.HasPrefix(key, scope) {
			return key
		}
	}
	if strings.HasPrefix(key, ".") {
		return key
	}
	return "." + key
}

// enumValues are the intrinsics whose operands TraceQL writes as bare words.
var enumValues = map[string]map[string]bool{
	"status": {"ok": true, "error": true, "unset": true},
	"kind": {
		"server": true, "client": true, "producer": true,
		"consumer": true, "internal": true, "unspecified": true,
	},
}

// valueText renders a comparison's operand.
//
// The IR stores every operand as text, so the type has to be recovered here. A
// duration, a number and a boolean are written bare; anything else is quoted,
// which is what keeps a service name from being read as an identifier.
func (w *writer) valueText(key, value string) string {
	if value == "" {
		return `""`
	}
	if words, ok := enumValues[key]; ok && words[strings.ToLower(value)] {
		return strings.ToLower(value)
	}
	if value == "true" || value == "false" {
		return value
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return value
	}
	if _, err := traceql.ParseDuration(value); err == nil {
		return value
	}
	return emitter.QuoteString(value, w.def.Normalizations.StringQuoting)
}
