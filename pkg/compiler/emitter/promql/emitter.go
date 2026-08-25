// Package promql renders TelemetryIR into PromQL text.
//
// PromQL nests where the IR is flat, so emission folds the pipeline outward: the
// data source renders first, and each stage wraps what came before. A temporal
// aggregation is a call taking a range vector, a group aggregation is an
// operator with a by/without clause, and everything else is a function call.
package promql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/registry"
)

// DSL is the name this emitter registers under.
const DSL = "promql"

// Emitter renders IR as PromQL. It holds no state, so the registered instance is
// safe to share across concurrent translations.
type Emitter struct{}

// DSL returns "promql".
func (Emitter) DSL() string { return DSL }

// Emit renders a query as PromQL.
func (Emitter) Emit(query *ir.Query, reg *registry.Registry) (string, error) {
	if query == nil {
		return "", fmt.Errorf("promql: cannot emit a nil query")
	}
	if reg == nil {
		return "", fmt.Errorf("promql: a registry is required")
	}
	def, err := reg.Get(DSL)
	if err != nil {
		return "", fmt.Errorf("promql: %w", err)
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

// rendered is an expression together with what may still attach to it.
type rendered struct {
	text string
	// selector reports whether the text is still a bare series selector, which
	// is the only thing a range may be appended to directly. Once anything has
	// wrapped it, taking a range needs subquery syntax instead.
	selector bool
	// atomic reports whether the text needs no parentheses when it becomes an
	// operand. A call or a selector is atomic; a binary expression is not.
	atomic bool
	// prec is the binding strength of the operator the text applies, so an
	// enclosing operator can decide whether it needs grouping.
	prec int
	// join holds a vector-matching clause waiting for its operator. PromQL
	// writes the operator between the left operand and the clause — "a / on (job)
	// b" — but the IR records the join and the operator as two separate stages,
	// so the join parks its parts here for the binary_op stage that follows.
	join *joinParts
}

// joinParts are the three pieces PromQL interleaves with an operator.
type joinParts struct {
	left, clause, right string
}

func (w *writer) emitQuery(query *ir.Query) (string, error) {
	if reason, ok := emitter.Unsupported(query); ok {
		w.notes.AddUnsupported(reason)
	}

	// Leading label filters fold into the selector's braces, because that is
	// the only place PromQL can filter on labels.
	folded, remaining := foldableLeadingFilters(query.Pipeline)
	extra, dropped := w.foldedMatchers(folded)
	for _, note := range dropped {
		w.notes.Addf("%s", note)
	}

	expr, err := w.emitSource(query, extra)
	if err != nil {
		return "", err
	}

	for _, stage := range remaining {
		expr, err = w.applyStage(expr, stage, query)
		if err != nil {
			return "", err
		}
	}

	if expr.join != nil {
		w.notes.Addf("UNSUPPORTED: a vector match was recorded with no operator to apply it; " +
			"only the left-hand side was written")
	}
	return w.applyOutput(expr, query), nil
}

// emitSource renders the data source and its label matchers.
func (w *writer) emitSource(query *ir.Query, extra []*ir.LabelMatcher) (rendered, error) {
	if query.Source == nil {
		// A query with no source is an expression over literals or over other
		// queries; it has nothing to render here.
		return rendered{selector: false, atomic: true}, nil
	}

	matchers := make([]*ir.LabelMatcher, 0, len(extra))
	for _, selector := range query.Source.Selectors {
		matchers = append(matchers, selector.Matchers...)
	}
	matchers = append(matchers, extra...)

	parts := make([]string, 0, len(matchers))
	for _, matcher := range matchers {
		if reason, ok := emitter.Unsupported(matcher); ok {
			w.notes.AddUnsupported(reason)
			continue
		}
		text, err := w.matcherText(matcher)
		if err != nil {
			return rendered{}, err
		}
		parts = append(parts, text)
	}

	var b strings.Builder
	b.WriteString(query.Source.Name)
	if len(parts) > 0 {
		// PromQL's canonical rendering separates matchers with a bare comma.
		b.WriteString("{" + strings.Join(parts, ",") + "}")
	}
	if b.Len() == 0 {
		// Neither a name nor a matcher: PromQL has no way to select anything.
		w.notes.Addf("the query selects no series that PromQL can name")
		return rendered{selector: true, atomic: true}, nil
	}
	return rendered{text: b.String(), selector: true, atomic: true}, nil
}

// matcherText renders one label matcher.
func (w *writer) matcherText(m *ir.LabelMatcher) (string, error) {
	// A set-membership predicate becomes a regex alternation: PromQL has no IN
	// operator, and an anchored alternation is the idiom for the same thing.
	if len(m.Values) > 0 {
		op := ir.MatchRegex
		if m.Op == ir.MatchNotIn {
			op = ir.MatchNotRegex
		}
		symbol, err := w.operatorSymbol(op, registry.OperatorContextSelector)
		if err != nil {
			return "", err
		}
		quoted := make([]string, 0, len(m.Values))
		for _, value := range m.Values {
			quoted = append(quoted, regexp.QuoteMeta(value))
		}
		return m.Key + symbol + emitter.QuoteString(strings.Join(quoted, "|"),
			w.def.Normalizations.StringQuoting), nil
	}

	// PromQL has no containment operator. An unanchored pattern over the escaped
	// text says exactly the same thing, and escaping is what keeps a literal dot
	// from turning into "any character".
	if m.Op.IsContainment() {
		op := ir.MatchRegex
		if m.Op == ir.MatchNotContains {
			op = ir.MatchNotRegex
		}
		symbol, err := w.operatorSymbol(op, registry.OperatorContextSelector)
		if err != nil {
			return "", err
		}
		w.notes.Addf("PARTIAL: PromQL has no line containment filter; %q was written as a "+
			"regular expression over the escaped text", m.Value)
		pattern := ".*" + regexp.QuoteMeta(m.Value) + ".*"
		return m.Key + symbol + emitter.QuoteString(pattern, w.def.Normalizations.StringQuoting), nil
	}

	symbol, err := w.operatorSymbol(m.Op, registry.OperatorContextSelector)
	if err != nil {
		return "", err
	}
	return m.Key + symbol + emitter.QuoteString(m.Value, w.def.Normalizations.StringQuoting), nil
}

// operatorSymbol finds the target's spelling of an IR predicate, preferring one
// valid in the given context.
func (w *writer) operatorSymbol(op ir.MatchOp, ctx registry.OperatorContext) (string, error) {
	candidates := w.def.OperatorsForIROp(op)
	if len(candidates) == 0 {
		return "", fmt.Errorf("promql: no operator spells %s", op)
	}
	for _, candidate := range candidates {
		if candidate.Context == ctx || candidate.Context == registry.OperatorContextAny {
			return candidate.Symbol, nil
		}
	}
	// The operator exists but not in this position. Using another spelling
	// still says what was meant, and the validator has already reported the
	// mismatch if it matters.
	return candidates[0].Symbol, nil
}

// applyStage wraps the rendered expression in one pipeline stage.
func (w *writer) applyStage(expr rendered, stage ir.PipelineStage, query *ir.Query) (rendered, error) {
	if reason, ok := emitter.Unsupported(stage); ok {
		// The stage cannot be written. What it wrapped is still valid on its
		// own, so the inner expression is kept and the omission is noted rather
		// than producing text the target cannot parse.
		w.notes.AddUnsupported(reason)
		return expr, nil
	}

	switch node := stage.(type) {
	case *ir.AggregationStage:
		return w.applyAggregation(expr, node, query)
	case *ir.FunctionStage:
		return w.applyFunction(expr, node)
	case *ir.FilterStage:
		return w.applyFilter(expr, node)
	case *ir.JoinStage:
		return w.applyJoin(expr, node)
	case *ir.BinaryOpStage:
		return w.applyBinaryOp(expr, node)
	case *ir.UnaryOpStage:
		return w.applyUnaryOp(node)
	default:
		return expr, fmt.Errorf("promql: no rule for pipeline stage type %T", stage)
	}
}

func (w *writer) applyAggregation(expr rendered, stage *ir.AggregationStage, query *ir.Query) (rendered, error) {
	fn, ok := w.def.FunctionForAggregation(stage.Op, stage.Scope)
	if !ok {
		w.notes.Addf("UNSUPPORTED: PromQL has no %s aggregation on the %s axis",
			strings.ToLower(stage.Op.String()), strings.ToLower(stage.Scope.String()))
		return expr, nil
	}

	if stage.Scope == ir.AggScopeTemporal {
		return w.applyTemporal(expr, fn, stage, query)
	}
	return w.applyGroup(expr, fn, stage)
}

// applyTemporal renders a range function, attaching the window to the operand.
func (w *writer) applyTemporal(expr rendered, fn *registry.FunctionDef,
	stage *ir.AggregationStage, query *ir.Query) (rendered, error) {

	step := w.windowStep(query)
	operand := expr.text
	if expr.selector {
		operand += "[" + step + "]" + w.offsetSuffix(query) + w.atSuffix(query)
	} else {
		// A range can only follow a selector. Anything already wrapped needs
		// subquery syntax, which is PromQL's way of taking a range over an
		// arbitrary expression.
		operand = w.parenthesize(expr) + "[" + step + ":]" + w.offsetSuffix(query)
	}

	text := fn.Name + "("
	if stage.Parameter != nil {
		text += w.exprText(stage.Parameter) + ", "
	}
	text += operand + ")"
	return rendered{text: text, atomic: true}, nil
}

// applyGroup renders an aggregation operator across series.
func (w *writer) applyGroup(expr rendered, fn *registry.FunctionDef, stage *ir.AggregationStage) (rendered, error) {
	var b strings.Builder
	b.WriteString(fn.Name)

	if clause := groupingClause(stage); clause != "" {
		// PromQL canonically writes the clause before the operand.
		if w.def.Normalizations.AggregationClausePosition == registry.ClauseAfterOperand {
			b.WriteString("(")
			if stage.Parameter != nil {
				b.WriteString(w.exprText(stage.Parameter) + ", ")
			}
			b.WriteString(expr.text + ") " + clause)
			return rendered{text: b.String(), atomic: true}, nil
		}
		b.WriteString(" " + clause + " ")
	}

	b.WriteString("(")
	if stage.Parameter != nil {
		b.WriteString(w.exprText(stage.Parameter) + ", ")
	}
	b.WriteString(expr.text + ")")
	return rendered{text: b.String(), atomic: true}, nil
}

// groupingClause renders a by or without clause, or nothing.
func groupingClause(stage *ir.AggregationStage) string {
	switch {
	case len(stage.Without) > 0:
		return "without (" + strings.Join(stage.Without, ", ") + ")"
	case len(stage.GroupBy) > 0:
		return "by (" + strings.Join(stage.GroupBy, ", ") + ")"
	}
	return ""
}

// applyFunction renders a call, putting the operand in the argument position the
// target's signature expects.
func (w *writer) applyFunction(expr rendered, stage *ir.FunctionStage) (rendered, error) {
	switch stage.Name {
	case ir.FuncLiteral:
		if len(stage.Args) > 0 {
			return rendered{text: w.exprText(stage.Args[0]), atomic: true}, nil
		}
		return expr, nil
	}

	fn, ok := w.def.FunctionByIRName(stage.Name)
	if !ok {
		w.notes.Addf("UNSUPPORTED: function %q has no PromQL equivalent", stage.Name)
		return expr, nil
	}

	args := make([]string, 0, len(stage.Args)+1)
	for _, arg := range stage.Args {
		args = append(args, w.exprText(arg))
	}
	// The resolver lifted the operand out of the argument list, so it goes back
	// where the signature says the series belongs — which is not always first:
	// histogram_fraction takes two scalars before its vector.
	if expr.text != "" {
		at := subjectIndex(fn)
		if at < 0 || at > len(args) {
			at = 0
		}
		args = append(args[:at], append([]string{expr.text}, args[at:]...)...)
	}
	return rendered{text: fn.Name + "(" + strings.Join(args, ", ") + ")", atomic: true}, nil
}

// subjectIndex finds where a signature expects the series it operates on.
func subjectIndex(fn *registry.FunctionDef) int {
	for i, arg := range fn.ArgTypes {
		switch arg.Name {
		case "instant_vector", "range_vector", "unwrapped_range", "log_stream":
			return i
		}
	}
	return -1
}

// applyBinaryOp renders an operator over two operands, or completes a pending
// vector-matching clause.
func (w *writer) applyBinaryOp(expr rendered, stage *ir.BinaryOpStage) (rendered, error) {
	symbol, ok := arithSymbol(stage.Op)
	if !ok {
		w.notes.Addf("UNSUPPORTED: operator %s has no PromQL spelling", stage.Op)
		return expr, nil
	}

	// A join left its pieces waiting; the operator goes between them.
	if expr.join != nil {
		text := expr.join.left + " " + symbol + " " + expr.join.clause + " " + expr.join.right
		return rendered{text: text, atomic: false, prec: stage.Op.Precedence()}, nil
	}
	if stage.Left == nil || stage.Right == nil {
		return rendered{}, fmt.Errorf("promql: a %s stage outside a join needs both operands", stage.Op)
	}

	left := w.operandText(stage.Left, stage.Op, false)
	right := w.operandText(stage.Right, stage.Op, true)
	return rendered{text: left + " " + symbol + " " + right,
		atomic: false, prec: stage.Op.Precedence()}, nil
}

// applyUnaryOp renders a leading sign.
//
// The operand is grouped only when it binds less tightly than the sign, so
// "-rate(x[5m])" needs no parentheses while "-(a + b)" does.
func (w *writer) applyUnaryOp(stage *ir.UnaryOpStage) (rendered, error) {
	symbol, ok := arithSymbol(stage.Op)
	if !ok {
		w.notes.Addf("UNSUPPORTED: sign %s has no PromQL spelling", stage.Op)
		return rendered{}, nil
	}
	if stage.Operand == nil {
		return rendered{}, fmt.Errorf("promql: a %s stage has no operand", stage.Op)
	}

	text := w.operandText(stage.Operand, stage.Op, true)
	return rendered{text: symbol + text, atomic: false, prec: stage.Op.Precedence()}, nil
}

// applyFilter renders a filter that did not fold into the selector, which in
// PromQL means a comparison against the series value.
func (w *writer) applyFilter(expr rendered, stage *ir.FilterStage) (rendered, error) {
	match, ok := stage.Predicate.(*ir.MatchPredicate)
	if !ok || match.Matcher == nil {
		w.notes.Addf("UNSUPPORTED: PromQL cannot express this filter outside a series selector")
		return expr, nil
	}
	if match.Matcher.Key != ir.FieldValue {
		w.notes.Addf("UNSUPPORTED: PromQL cannot filter on %q at this point in the pipeline; "+
			"only a series selector may filter on labels", match.Matcher.Key)
		return expr, nil
	}

	symbol, err := w.operatorSymbol(match.Matcher.Op, registry.OperatorContextComparison)
	if err != nil {
		return expr, err
	}
	if stage.ReturnsBool {
		symbol += " bool"
	}
	text := expr.text + " " + symbol + " " + match.Matcher.Value
	return rendered{text: text, atomic: false, prec: ir.ArithGT.Precedence()}, nil
}

// applyJoin renders PromQL's vector matching.
// includeClause renders the labels copied from the one side onto the result.
func includeClause(stage *ir.JoinStage) string {
	if len(stage.IncludeLabels) == 0 {
		return ""
	}
	return " (" + strings.Join(stage.IncludeLabels, ", ") + ")"
}

func (w *writer) applyJoin(expr rendered, stage *ir.JoinStage) (rendered, error) {
	if stage.RightSide == nil {
		w.notes.Addf("UNSUPPORTED: a join with no right-hand side cannot be written")
		return expr, nil
	}
	right, err := w.emitQuery(stage.RightSide)
	if err != nil {
		return expr, err
	}

	var b strings.Builder
	switch {
	case len(stage.OnLabels) > 0:
		b.WriteString("on (" + strings.Join(stage.OnLabels, ", ") + ")")
	case len(stage.IgnoreLabels) > 0:
		b.WriteString("ignoring (" + strings.Join(stage.IgnoreLabels, ", ") + ")")
	}
	switch stage.JoinType {
	case ir.JoinLeftOuter:
		b.WriteString(" group_left" + includeClause(stage))
	case ir.JoinRightOuter:
		b.WriteString(" group_right" + includeClause(stage))
	case ir.JoinCross:
		w.notes.Addf("UNSUPPORTED: PromQL has no cross join; vector matching is written instead")
	}
	// The operator arrives as the binary_op stage that follows, so the pieces
	// wait rather than being concatenated now. The fallback text is the left
	// operand alone, which stays valid PromQL if no operator ever comes.
	return rendered{
		text:   expr.text,
		atomic: expr.atomic,
		join:   &joinParts{left: expr.text, clause: b.String(), right: right},
	}, nil
}

// applyOutput appends the modifiers recorded on the query's output.
func (w *writer) applyOutput(expr rendered, query *ir.Query) string {
	text := expr.text

	// A subquery's outer range and resolution go after the whole expression.
	if query.Output.IsSubquery() {
		if reason, ok := emitter.Unsupported(query.Output); ok {
			w.notes.AddUnsupported(reason)
			return text
		}
		step := ""
		if query.Output.SubqueryStep != nil {
			step = emitter.FormatDuration(*query.Output.SubqueryStep, w.def.Normalizations.DurationFormat)
		}
		rangeText := emitter.FormatDuration(*query.Output.SubqueryRange,
			w.def.Normalizations.DurationFormat)
		text += "[" + rangeText + ":" + step + "]"
	}
	return text
}

// windowStep renders the query's window as a duration.
func (w *writer) windowStep(query *ir.Query) string {
	if query.Output == nil || query.Output.Window == nil || query.Output.Window.Step.IsZero() {
		// A range function needs a window. Nothing in the IR gave one, so a
		// default is written and the substitution is noted.
		w.notes.Addf("no window was recorded for a range aggregation; [5m] was assumed")
		return "5m"
	}
	return emitter.FormatDuration(query.Output.Window.Step, w.def.Normalizations.DurationFormat)
}

// offsetSuffix renders the window offset, which attaches to the selector.
func (w *writer) offsetSuffix(query *ir.Query) string {
	if query.Output == nil || query.Output.Window == nil || query.Output.Window.Offset.IsZero() {
		return ""
	}
	return " offset " + emitter.FormatDuration(query.Output.Window.Offset, w.def.Normalizations.DurationFormat)
}

// atSuffix renders a pinned evaluation instant as an @ modifier.
func (w *writer) atSuffix(query *ir.Query) string {
	if query.Output == nil || query.Output.Range == nil {
		return ""
	}
	bounds := query.Output.Range
	// Only an instant is expressible: @ pins one timestamp, so a range with
	// distinct ends is a query time range rather than a modifier.
	if bounds.Start.IsZero() || bounds.Start.UnixNano != bounds.End.UnixNano {
		return ""
	}
	seconds := float64(bounds.Start.UnixNano) / float64(1e9)
	return " @ " + emitter.FormatNumber(seconds)
}

// parenthesize wraps an expression when it would otherwise bind wrongly as an
// operand.
func (w *writer) parenthesize(expr rendered) string {
	if expr.atomic {
		return expr.text
	}
	return "(" + expr.text + ")"
}

// exprText renders an IR expression.
func (w *writer) exprText(expr ir.IRExpr) string {
	switch node := expr.(type) {
	case *ir.LiteralExpr:
		return emitter.LiteralText(node, w.def.Normalizations.StringQuoting)
	case *ir.RefExpr:
		return node.Name
	case *ir.QueryExpr:
		if node.Query == nil {
			return ""
		}
		text, err := w.emitQuery(node.Query)
		if err != nil {
			w.notes.Addf("a nested query could not be rendered: %s", err)
			return ""
		}
		return text
	default:
		return ""
	}
}

// operandText renders one side of a binary operator, parenthesising it only when
// the reconstructed grouping would otherwise differ from the tree's.
//
// The rule is the parsers' own precedence table: an operand binding less tightly
// than the operator above it needs grouping, and so does an equally-binding
// operand on the side associativity does not favour — "a - (b - c)" must keep
// its parentheses, while "a - b - c" needs none.
func (w *writer) operandText(operand *ir.Query, parent ir.ArithOp, isRight bool) string {
	text, err := w.emitQuery(operand)
	if err != nil {
		w.notes.Addf("a nested query could not be rendered: %s", err)
		return ""
	}

	child, ok := binaryOpOf(operand)
	if !ok {
		return text
	}
	switch {
	case child.Precedence() < parent.Precedence():
		return "(" + text + ")"
	case child.Precedence() > parent.Precedence():
		return text
	case isRight != parent.IsRightAssociative():
		// Equal precedence on the side associativity does not favour.
		return "(" + text + ")"
	default:
		return text
	}
}

// binaryOpOf reports the operator a query's last stage applies, when it is a
// binary one.
func binaryOpOf(query *ir.Query) (ir.ArithOp, bool) {
	if query == nil || len(query.Pipeline) == 0 {
		return 0, false
	}
	switch stage := query.Pipeline[len(query.Pipeline)-1].(type) {
	case *ir.BinaryOpStage:
		return stage.Op, true
	case *ir.UnaryOpStage:
		return stage.Op, true
	}
	return 0, false
}

// arithSymbols maps the IR's binary operators to PromQL spellings.
var arithSymbols = map[ir.ArithOp]string{
	ir.ArithAdd: "+", ir.ArithSub: "-", ir.ArithMul: "*",
	ir.ArithDiv: "/", ir.ArithMod: "%", ir.ArithPow: "^",
	ir.ArithAnd: "and", ir.ArithOr: "or", ir.ArithUnless: "unless",
	ir.ArithEQ: "==", ir.ArithNEQ: "!=", ir.ArithGT: ">",
	ir.ArithGTE: ">=", ir.ArithLT: "<", ir.ArithLTE: "<=",
	ir.ArithNeg: "-", ir.ArithPos: "+",
}

func arithSymbol(op ir.ArithOp) (string, bool) {
	symbol, ok := arithSymbols[op]
	return symbol, ok
}

// literalString reads a string literal's value, for the sign carried by a unary
// stage.
func literalString(expr ir.IRExpr) string {
	literal, ok := expr.(*ir.LiteralExpr)
	if !ok {
		return ""
	}
	if s, ok := literal.Value.(string); ok {
		return s
	}
	return ""
}

// foldableLeadingFilters splits the pipeline into the leading filters that can
// fold into the series selector and everything after them.
//
// PromQL can only filter on labels inside a selector's braces, so a filter that
// reaches the pipeline has to move there — and it can only move if nothing
// before it has already changed the series.
func foldableLeadingFilters(pipeline ir.Pipeline) (folded []*ir.FilterStage, remaining ir.Pipeline) {
	i := 0
	for ; i < len(pipeline); i++ {
		filter, ok := pipeline[i].(*ir.FilterStage)
		if !ok {
			break
		}
		if !predicateIsLabelOnly(filter.Predicate) {
			break
		}
		folded = append(folded, filter)
	}
	return folded, pipeline[i:]
}

// predicateIsLabelOnly reports whether every leaf addresses a label rather than
// the series value or the log body.
func predicateIsLabelOnly(predicate ir.Predicate) bool {
	switch node := predicate.(type) {
	case *ir.MatchPredicate:
		if node.Matcher == nil {
			return false
		}
		return node.Matcher.Key != ir.FieldValue && node.Matcher.Key != ir.FieldBody
	case *ir.LogicalPredicate:
		if len(node.Operands) == 0 {
			return false
		}
		for _, operand := range node.Operands {
			if !predicateIsLabelOnly(operand) {
				return false
			}
		}
		return true
	}
	return false
}

// foldedMatchers turns the foldable filters into label matchers, reporting what
// could not be folded.
func (w *writer) foldedMatchers(filters []*ir.FilterStage) ([]*ir.LabelMatcher, []string) {
	var matchers []*ir.LabelMatcher
	var notes []string
	for _, filter := range filters {
		got, note := flattenPredicate(filter.Predicate)
		matchers = append(matchers, got...)
		if note != "" {
			notes = append(notes, note)
		}
	}
	return matchers, notes
}

// flattenPredicate turns a predicate tree into label matchers.
//
// A conjunction flattens directly: PromQL's comma-separated matchers are an
// implicit AND. A disjunction has no such form, and only collapses when every
// branch tests the same key for equality, which an anchored alternation says
// exactly. Anything else is reported rather than approximated.
func flattenPredicate(predicate ir.Predicate) ([]*ir.LabelMatcher, string) {
	switch node := predicate.(type) {
	case *ir.MatchPredicate:
		if node.Matcher == nil {
			return nil, ""
		}
		return []*ir.LabelMatcher{node.Matcher}, ""

	case *ir.LogicalPredicate:
		switch node.Op {
		case ir.LogicalAnd:
			var all []*ir.LabelMatcher
			var notes []string
			for _, operand := range node.Operands {
				got, note := flattenPredicate(operand)
				all = append(all, got...)
				if note != "" {
					notes = append(notes, note)
				}
			}
			return all, strings.Join(notes, "; ")

		case ir.LogicalOr:
			if matcher, ok := alternationMatcher(node); ok {
				return []*ir.LabelMatcher{matcher}, ""
			}
			return nil, "UNSUPPORTED: an OR of filters on different labels has no PromQL " +
				"series-selector form; the filter was omitted"

		default:
			return nil, "UNSUPPORTED: a negated filter has no PromQL series-selector form; " +
				"the filter was omitted"
		}
	}
	return nil, ""
}

// alternationMatcher collapses an OR of equality tests on one key into a single
// regex alternation.
func alternationMatcher(node *ir.LogicalPredicate) (*ir.LabelMatcher, bool) {
	if len(node.Operands) < 2 {
		return nil, false
	}
	key := ""
	values := make([]string, 0, len(node.Operands))
	for _, operand := range node.Operands {
		match, ok := operand.(*ir.MatchPredicate)
		if !ok || match.Matcher == nil || match.Matcher.Op != ir.MatchEQ {
			return nil, false
		}
		if key == "" {
			key = match.Matcher.Key
		} else if key != match.Matcher.Key {
			return nil, false
		}
		values = append(values, regexp.QuoteMeta(match.Matcher.Value))
	}
	sort.Strings(values)
	return &ir.LabelMatcher{Key: key, Op: ir.MatchRegex, Value: strings.Join(values, "|")}, true
}
