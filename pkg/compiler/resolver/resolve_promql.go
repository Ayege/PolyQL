package resolver

import (
	"fmt"
	"math"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser/promql"
	"github.com/polyql/polyql/pkg/registry"
)

// promqlResolver maps a PromQL AST onto the IR.
type promqlResolver struct {
	def *registry.DSLDefinition
}

// resolve dispatches on the PromQL node type. Every case either produces a
// query or fails: an unhandled node type is an error rather than an empty
// result, matching ir.Walk's refusal to silently skip a node it does not know.
func (r *promqlResolver) resolve(expr promql.Expr) (*ir.Query, error) {
	switch node := expr.(type) {
	case *promql.VectorSelector:
		return r.resolveVectorSelector(node)
	case *promql.MatrixSelector:
		return r.resolveMatrixSelector(node)
	case *promql.Call:
		return r.resolveCall(node)
	case *promql.AggregateExpr:
		return r.resolveAggregateExpr(node)
	case *promql.BinaryExpr:
		return r.resolveBinaryExpr(node)
	case *promql.UnaryExpr:
		return r.resolveUnaryExpr(node)
	case *promql.ParenExpr:
		return r.resolveParenExpr(node)
	case *promql.SubqueryExpr:
		return r.resolveSubqueryExpr(node)
	case *promql.NumberLiteral:
		return r.resolveLiteral(numberLiteral(node.Val))
	case *promql.StringLiteral:
		return r.resolveLiteral(stringLiteral(node.Val))
	default:
		return nil, fmt.Errorf("resolver: promql: no rule for AST node type %T", expr)
	}
}

// resolveVectorSelector turns a series selector into the query's data source.
//
// PromQL has no attribute scoping — every label lives in one flat namespace — so
// the scope is UNSCOPED. That is a fact about the language, not a default: a
// TraceQL selector would resolve to RESOURCE or SPAN here.
func (r *promqlResolver) resolveVectorSelector(node *promql.VectorSelector) (*ir.Query, error) {
	source := &ir.DataSource{Name: node.Name, Scope: ir.ScopeUnscoped}

	if len(node.LabelMatchers) > 0 {
		selector := &ir.Selector{}
		for _, matcher := range node.LabelMatchers {
			resolved, err := r.resolveLabelMatcher(matcher)
			if err != nil {
				return nil, err
			}
			selector.Matchers = append(selector.Matchers, resolved)
		}
		source.Selectors = []*ir.Selector{selector}
	}

	query := newQuery(ir.SignalMetric, source)
	if err := r.applyModifiers(query, node.OriginalOffset, node.At); err != nil {
		return nil, err
	}
	return query, nil
}

// resolveLabelMatcher maps one PromQL matcher onto a QLS selection predicate.
//
// A regex matcher stays a regex even when its pattern is a plain alternation
// like "5..|4..". Rewriting that into an IN list would be a guess about the
// author's intent that changes what the query matches when the alternation is
// not fully literal, and it would hide the rewrite from the fidelity report.
// LabelMatcher.Values is populated only where a source DSL states a set
// membership outright.
func (r *promqlResolver) resolveLabelMatcher(m *promql.LabelMatcher) (*ir.LabelMatcher, error) {
	op, err := lookupOperator(r.def, m.Type.String(), registry.OperatorContextSelector)
	if err != nil {
		return nil, err
	}
	return &ir.LabelMatcher{Key: m.Name, Op: op, Value: m.Value}, nil
}

// resolveMatrixSelector resolves the underlying selector and records the range.
//
// The range is not part of the data source: it says how much history the
// temporal aggregation consuming this selector reads, which QLS models as the
// window a value is reduced over. It therefore lands in Output.Window.Step,
// where the enclosing range function will find it.
func (r *promqlResolver) resolveMatrixSelector(node *promql.MatrixSelector) (*ir.Query, error) {
	query, err := r.resolveVectorSelector(node.VectorSelector)
	if err != nil {
		return nil, err
	}
	window(query).Step = ir.NewIntervalFromSource(node.Range, promql.FormatDuration(node.Range))
	return query, nil
}

// applyModifiers records a selector's offset and @ modifier on the query.
func (r *promqlResolver) applyModifiers(query *ir.Query, offset time.Duration, at *promql.AtModifier) error {
	if offset != 0 {
		window(query).Offset = ir.NewIntervalFromSource(offset, promql.FormatDuration(offset))
	}
	if at == nil {
		return nil
	}
	switch at.Preset {
	case promql.AtTimestamp:
		// The @ modifier pins evaluation to an instant, which is a query time
		// range of zero length starting there.
		seconds, fraction := math.Modf(at.Timestamp)
		pinned := ir.NewTimestamp(time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC())
		bounds := timeRange(query)
		bounds.Start = pinned
		bounds.End = pinned
	case promql.AtStart, promql.AtEnd:
		// start() and end() resolve against the enclosing range query, which is
		// out-of-band information the IR's absolute TimeRange cannot hold.
		setHint(query, HintAtModifier, at.String())
	}
	return nil
}

// resolveCall maps a function call onto either an aggregation or a function
// stage, according to whether the registry gives the function an IR aggregation
// operator.
func (r *promqlResolver) resolveCall(node *promql.Call) (*ir.Query, error) {
	fn, err := lookupFunction(r.def, node.Func.Name)
	if err != nil {
		return nil, err
	}
	if err := checkArity(fn, len(node.Args), node.Func.Name); err != nil {
		return nil, err
	}

	// The subject is the argument carrying the series the function operates on;
	// the rest are parameters. Folding the function into the subject's query is
	// what flattens PromQL's nesting into an IR pipeline.
	subjectIndex := promqlSubjectIndex(node.Args)
	if subjectIndex < 0 {
		// A function over no series at all, such as time() or pi().
		query := newQuery(ir.SignalMetric, nil)
		args, err := r.resolveArgs(node.Args, -1)
		if err != nil {
			return nil, err
		}
		appendStage(query, &ir.FunctionStage{Name: fn.IRName, Args: args, ReturnType: fn.ReturnType})
		return query, nil
	}

	query, err := r.resolve(node.Args[subjectIndex])
	if err != nil {
		return nil, err
	}
	args, err := r.resolveArgs(node.Args, subjectIndex)
	if err != nil {
		return nil, err
	}

	if !fn.IsAggregation {
		appendStage(query, &ir.FunctionStage{Name: fn.IRName, Args: args, ReturnType: fn.ReturnType})
		return query, nil
	}

	stage := aggregationStage(fn)
	// A range function's leading scalar — the phi of quantile_over_time, say —
	// is the aggregation's parameter rather than a function argument.
	if len(args) > 0 {
		stage.Parameter = args[0]
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("resolver: promql: %q maps to the IR aggregation %s, "+
			"which takes at most one parameter, but the call has %d arguments besides its operand",
			node.Func.Name, fn.AggOp, len(args))
	}
	appendStage(query, stage)
	if stage.Scope == ir.AggScopeGroup {
		recordOutputGrouping(query, stage)
	}
	return query, nil
}

// promqlSubjectIndex finds the argument that carries the series. It is the
// first argument that is not a bare literal; PromQL never passes two series to
// one function, so the first such argument is unambiguous.
func promqlSubjectIndex(args []promql.Expr) int {
	for i, arg := range args {
		switch arg.(type) {
		case *promql.NumberLiteral, *promql.StringLiteral:
			continue
		default:
			return i
		}
	}
	return -1
}

// resolveArgs turns every argument except the subject into an IR expression.
func (r *promqlResolver) resolveArgs(args []promql.Expr, subjectIndex int) ([]ir.IRExpr, error) {
	var resolved []ir.IRExpr
	for i, arg := range args {
		if i == subjectIndex {
			continue
		}
		expr, err := r.resolveArg(arg)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, expr)
	}
	return resolved, nil
}

// resolveArg turns one argument into an IR expression: a literal stays a
// literal, and anything else becomes a nested query.
func (r *promqlResolver) resolveArg(arg promql.Expr) (ir.IRExpr, error) {
	switch typed := arg.(type) {
	case *promql.NumberLiteral:
		return numberLiteral(typed.Val), nil
	case *promql.StringLiteral:
		return stringLiteral(typed.Val), nil
	default:
		nested, err := r.resolve(arg)
		if err != nil {
			return nil, err
		}
		return queryExpr(nested), nil
	}
}

// checkArity verifies an argument count against the registry signature.
// Variadic functions are skipped: their signature states a minimum rather than
// an exact count, and the parser has already enforced it against the source text
// where it could report a position.
func checkArity(fn *registry.FunctionDef, got int, name string) error {
	if fn.Variadic != 0 {
		return nil
	}
	if got != fn.Arity {
		return fmt.Errorf("resolver: %q takes %d argument(s) per the registry, got %d",
			name, fn.Arity, got)
	}
	return nil
}

// resolveAggregateExpr maps a PromQL aggregation operator onto the IR.
//
// These always collapse across series rather than over time, so the scope is
// GROUP. The registry says so too, and a disagreement is reported rather than
// quietly overridden, since it would mean the definition file is wrong.
func (r *promqlResolver) resolveAggregateExpr(node *promql.AggregateExpr) (*ir.Query, error) {
	name := node.Op.String()
	fn, err := lookupFunction(r.def, name)
	if err != nil {
		return nil, err
	}
	if !fn.IsAggregation {
		return nil, fmt.Errorf("resolver: promql: %q is an aggregation operator but the registry "+
			"definition at %s gives it no ir_kind", name, r.def.SourcePath)
	}
	if fn.AggScope != ir.AggScopeGroup {
		return nil, fmt.Errorf("resolver: promql: %q aggregates across series, but the registry "+
			"definition at %s gives it scope %s", name, r.def.SourcePath, fn.AggScope)
	}

	query, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}

	stage := aggregationStage(fn)
	if node.Without {
		stage.Without = node.Grouping
	} else {
		stage.GroupBy = node.Grouping
	}
	if node.Param != nil {
		param, err := r.resolveArg(node.Param)
		if err != nil {
			return nil, err
		}
		stage.Parameter = param
	}

	appendStage(query, stage)
	recordOutputGrouping(query, stage)
	return query, nil
}

// recordOutputGrouping mirrors the outermost group aggregation's labels onto the
// output, which is what determines the shape of the result set. QLS §Aggregation
// carries only grouped keys into the result, so the last group aggregation wins.
func recordOutputGrouping(query *ir.Query, stage *ir.AggregationStage) {
	if query.Output == nil {
		query.Output = &ir.Output{}
	}
	if len(stage.GroupBy) > 0 {
		query.Output.GroupBy = stage.GroupBy
	}
}

// resolveBinaryExpr maps a binary operator onto the IR.
//
// Three shapes come out of this, because PromQL spells three different
// operations the same way:
//
//   - a vector-matching clause makes it a join, which is what on/ignoring and
//     group_left/group_right describe;
//   - a comparison against a scalar filters series, so it becomes a filter stage
//     over the QLS metric value field;
//   - anything else is arithmetic, which the IR has no operator enum for and so
//     becomes a function stage naming the operator.
func (r *promqlResolver) resolveBinaryExpr(node *promql.BinaryExpr) (*ir.Query, error) {
	if node.VectorMatching != nil {
		return r.resolveJoin(node)
	}
	if node.Op.IsComparison() {
		if query, handled, err := r.resolveComparisonFilter(node); err != nil {
			return nil, err
		} else if handled {
			return query, nil
		}
	}
	return r.resolveArithmetic(node)
}

// resolveJoin builds a join from a vector-matching binary operator.
func (r *promqlResolver) resolveJoin(node *promql.BinaryExpr) (*ir.Query, error) {
	left, err := r.resolve(node.LHS)
	if err != nil {
		return nil, err
	}
	right, err := r.resolve(node.RHS)
	if err != nil {
		return nil, err
	}

	matching := node.VectorMatching
	stage := &ir.JoinStage{JoinType: ir.JoinInner, RightSide: right}
	// group_left and group_right make the join many-to-one and one-to-many: the
	// "many" side keeps every series whether or not it matched, which is what
	// QLS §Joins calls a left or right outer equi-join.
	switch matching.Card {
	case promql.CardManyToOne:
		stage.JoinType = ir.JoinLeftOuter
	case promql.CardOneToMany:
		stage.JoinType = ir.JoinRightOuter
	}
	if matching.On {
		stage.OnLabels = matching.MatchingLabels
	} else {
		stage.IgnoreLabels = matching.MatchingLabels
	}
	// group_left(env) copies env from the one side onto the result; dropping
	// the list would change which labels the joined series carry.
	stage.IncludeLabels = matching.Include

	op, err := promqlArithOp(node.Op)
	if err != nil {
		return nil, err
	}
	appendStage(left, stage)
	// The operator applies to the joined series. Its operands are the join's
	// two sides, already in place, so the stage carries only the operator.
	appendStage(left, &ir.BinaryOpStage{Op: op})
	return left, nil
}

// resolveComparisonFilter turns a comparison against a scalar into a filter over
// the metric value. It reports whether it handled the node: a comparison between
// two series is not a predicate over a constant and falls through to arithmetic.
func (r *promqlResolver) resolveComparisonFilter(node *promql.BinaryExpr) (*ir.Query, bool, error) {
	var seriesSide promql.Expr
	var literal *promql.NumberLiteral

	leftNum, leftIsNum := node.LHS.(*promql.NumberLiteral)
	rightNum, rightIsNum := node.RHS.(*promql.NumberLiteral)
	switch {
	case rightIsNum && !leftIsNum:
		seriesSide, literal = node.LHS, rightNum
	case leftIsNum && !rightIsNum:
		seriesSide, literal = node.RHS, leftNum
	default:
		return nil, false, nil
	}

	op, err := lookupOperator(r.def, node.Op.String(), registry.OperatorContextComparison)
	if err != nil {
		return nil, false, err
	}
	query, err := r.resolve(seriesSide)
	if err != nil {
		return nil, false, err
	}

	filter := filterStage(matchPredicate(FieldValue, op, formatFloat(literal.Val)))
	// With bool the comparison stops filtering and yields 0 or 1 for every
	// series. The IR has only the filter form, so which was written is recorded
	// on the stage rather than lost.
	filter.ReturnsBool = node.ReturnBool
	appendStage(query, filter)
	return query, true, nil
}

// resolveArithmetic builds a binary operator stage.
//
// Both operands become sub-queries of a fresh query rather than one of them
// being folded into. Folding would make the enclosing query its own operand — a
// cycle ir.Walk would never terminate on — and it would misdescribe the result:
// an operator over two sources has no single data source, and saying so is more
// honest than picking the left one.
func (r *promqlResolver) resolveArithmetic(node *promql.BinaryExpr) (*ir.Query, error) {
	op, err := promqlArithOp(node.Op)
	if err != nil {
		return nil, err
	}
	left, err := r.resolveOperand(node.LHS)
	if err != nil {
		return nil, err
	}
	right, err := r.resolveOperand(node.RHS)
	if err != nil {
		return nil, err
	}

	query := newQuery(ir.SignalMetric, nil)
	appendStage(query, &ir.BinaryOpStage{Op: op, Left: left, Right: right})
	return query, nil
}

// resolveOperand resolves one side of a binary operator. A bare scalar has no
// data source of its own, so it becomes a query holding just that literal.
func (r *promqlResolver) resolveOperand(expr promql.Expr) (*ir.Query, error) {
	switch typed := expr.(type) {
	case *promql.NumberLiteral:
		return r.resolveLiteral(numberLiteral(typed.Val))
	case *promql.StringLiteral:
		return r.resolveLiteral(stringLiteral(typed.Val))
	}
	return r.resolve(expr)
}

// promqlArithOp maps a PromQL binary operator token onto the IR operator.
func promqlArithOp(op promql.TokenType) (ir.ArithOp, error) {
	switch op {
	case promql.ADD:
		return ir.ArithAdd, nil
	case promql.SUB:
		return ir.ArithSub, nil
	case promql.MUL:
		return ir.ArithMul, nil
	case promql.DIV:
		return ir.ArithDiv, nil
	case promql.MOD:
		return ir.ArithMod, nil
	case promql.POW:
		return ir.ArithPow, nil
	case promql.LAND:
		return ir.ArithAnd, nil
	case promql.LOR:
		return ir.ArithOr, nil
	case promql.LUNLESS:
		return ir.ArithUnless, nil
	case promql.EQLC:
		return ir.ArithEQ, nil
	case promql.NEQ:
		return ir.ArithNEQ, nil
	case promql.GTR:
		return ir.ArithGT, nil
	case promql.GTE:
		return ir.ArithGTE, nil
	case promql.LSS:
		return ir.ArithLT, nil
	case promql.LTE:
		return ir.ArithLTE, nil
	}
	// atan2 is a PromQL function spelled as an operator, with no IR operator of
	// its own.
	return 0, fmt.Errorf("resolver: promql: no IR operator for %q", op)
}

// resolveUnaryExpr builds a unary sign stage.
//
// The operand becomes a sub-query rather than the stage folding into it, so the
// sign is a node in its own right and the tree stays acyclic.
func (r *promqlResolver) resolveUnaryExpr(node *promql.UnaryExpr) (*ir.Query, error) {
	op := ir.ArithNeg
	if node.Op == promql.ADD {
		op = ir.ArithPos
	}
	operand, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}

	query := newQuery(ir.SignalMetric, nil)
	appendStage(query, &ir.UnaryOpStage{Op: op, Operand: operand})
	return query, nil
}

func (r *promqlResolver) resolveParenExpr(node *promql.ParenExpr) (*ir.Query, error) {
	query, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}
	setHint(query, HintParen, "true")
	return query, nil
}

// resolveSubqueryExpr resolves a subquery: an inner expression evaluated at its
// own resolution over an outer range.
//
// A subquery nests two windows, and the IR carries one Window per Query. The
// resolution goes into Window.Step, since QLS defines step as the duration
// between values, and the outer range and the inner aggregation's window are
// recorded as hints rather than dropped. Collapsing them into a single typed
// window would silently discard one of the two.
func (r *promqlResolver) resolveSubqueryExpr(node *promql.SubqueryExpr) (*ir.Query, error) {
	query, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}

	// The outer range and the resolution are the subquery's own; the window
	// already on the query belongs to the inner aggregation and stays put, so
	// rate(x[5m])[30m:1m] keeps all three durations.
	rangeInterval := ir.NewIntervalFromSource(node.Range, promql.FormatDuration(node.Range))
	if query.Output == nil {
		query.Output = &ir.Output{}
	}
	query.Output.SubqueryRange = &rangeInterval
	if node.Step > 0 {
		step := ir.NewIntervalFromSource(node.Step, promql.FormatDuration(node.Step))
		query.Output.SubqueryStep = &step
	}
	if node.OriginalOffset != 0 {
		window(query).Offset = ir.NewIntervalFromSource(node.OriginalOffset,
			promql.FormatDuration(node.OriginalOffset))
	}

	if err := r.applyModifiers(query, 0, node.At); err != nil {
		return nil, err
	}
	return query, nil
}

// resolveLiteral wraps a bare scalar or string query, which PromQL allows as a
// whole expression.
func (r *promqlResolver) resolveLiteral(value *ir.LiteralExpr) (*ir.Query, error) {
	query := newQuery(ir.SignalMetric, nil)
	appendStage(query, &ir.FunctionStage{
		Name:       FuncLiteral,
		Args:       []ir.IRExpr{value},
		ReturnType: value.Type,
	})
	return query, nil
}
