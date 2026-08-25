package resolver

import (
	"fmt"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser/logql"
	"github.com/polyql/polyql/pkg/registry"
)

// Names the resolver gives LogQL pipeline stages. These are grammar rather than
// functions — LogQL writes "| json", not "json(...)" — so they are not registry
// entries, in the same way PromQL's "sum" operator is not a parser function.
const (
	FuncParseJSON    = "parse_json"
	FuncParseLogfmt  = "parse_logfmt"
	FuncParseRegexp  = "parse_regexp"
	FuncParsePattern = "parse_pattern"
	FuncParseUnpack  = "parse_unpack"
	FuncLineFormat   = "line_format"
	FuncLabelFormat  = "label_format"
	FuncDropLabels   = "drop_labels"
	FuncKeepLabels   = "keep_labels"
	FuncDecolorize   = "decolorize"
)

// logqlResolver maps a LogQL AST onto the IR.
type logqlResolver struct {
	def *registry.DSLDefinition
}

func (r *logqlResolver) resolve(expr logql.Expr) (*ir.Query, error) {
	switch node := expr.(type) {
	case *logql.LogStreamSelector:
		return r.resolveStreamSelector(node)
	case *logql.PipelineExpr:
		return r.resolvePipelineExpr(node)
	case *logql.RangeAggregation:
		return r.resolveRangeAggregation(node)
	case *logql.VectorAggregation:
		return r.resolveVectorAggregation(node)
	case *logql.BinaryExpr:
		return r.resolveBinaryExpr(node)
	case *logql.UnaryExpr:
		return r.resolveUnaryExpr(node)
	case *logql.ParenExpr:
		return r.resolveParenExpr(node)
	case *logql.LabelReplace:
		return r.resolveLabelReplace(node)
	case *logql.NumberLiteral:
		return r.resolveLiteral(numberLiteral(node.Val))
	default:
		return nil, fmt.Errorf("resolver: logql: no rule for AST node type %T", expr)
	}
}

// resolveStreamSelector turns a stream selector into the query's data source.
//
// A LogQL stream has no name of its own — it is identified entirely by its
// labels — so DataSource.Name stays empty and the matchers carry the whole
// selection. QLS §Selection allows an omitted data source for exactly this
// reason.
func (r *logqlResolver) resolveStreamSelector(node *logql.LogStreamSelector) (*ir.Query, error) {
	source := &ir.DataSource{Name: "", Scope: ir.ScopeUnscoped}

	if len(node.Matchers) > 0 {
		selector := &ir.Selector{}
		for _, matcher := range node.Matchers {
			op, err := lookupOperator(r.def, matcher.Type.String(), registry.OperatorContextSelector)
			if err != nil {
				return nil, err
			}
			selector.Matchers = append(selector.Matchers,
				&ir.LabelMatcher{Key: matcher.Name, Op: op, Value: matcher.Value})
		}
		source.Selectors = []*ir.Selector{selector}
	}

	return newQuery(ir.SignalLog, source), nil
}

// resolvePipelineExpr resolves the selector and then each stage in written
// order. This is the closest correspondence in the whole compiler: LogQL's
// pipeline and the IR's are both ordered stage lists, so resolution is a
// translation of each stage rather than a restructuring.
func (r *logqlResolver) resolvePipelineExpr(node *logql.PipelineExpr) (*ir.Query, error) {
	query, err := r.resolveStreamSelector(node.Selector)
	if err != nil {
		return nil, err
	}
	for _, stage := range node.Stages {
		resolved, err := r.resolveStage(stage)
		if err != nil {
			return nil, err
		}
		appendStage(query, resolved)
	}
	return query, nil
}

func (r *logqlResolver) resolveStage(stage logql.PipelineStage) (ir.PipelineStage, error) {
	switch node := stage.(type) {
	case *logql.LineFilter:
		return r.resolveLineFilter(node)
	case *logql.LabelFilter:
		return r.resolveLabelFilter(node)
	case *logql.ParserStage:
		return r.resolveParserStage(node)
	case *logql.FormatterStage:
		return r.resolveFormatterStage(node)
	case *logql.DropStage:
		return &ir.FunctionStage{
			Name:       FuncDropLabels,
			Args:       labelRefArgs(node.Labels),
			ReturnType: ir.DataTypeString,
		}, nil
	case *logql.KeepStage:
		return &ir.FunctionStage{
			Name:       FuncKeepLabels,
			Args:       labelRefArgs(node.Labels),
			ReturnType: ir.DataTypeString,
		}, nil
	case *logql.DecolorizeStage:
		return &ir.FunctionStage{Name: FuncDecolorize, ReturnType: ir.DataTypeString}, nil
	default:
		return nil, fmt.Errorf("resolver: logql: no rule for pipeline stage type %T", stage)
	}
}

// resolveLineFilter turns a line filter into a predicate over the log body.
//
// A line filter tests the whole line rather than a label, so the predicate's key
// is the QLS log body field. The operator comes from the registry, which is what
// decides how a substring test lowers: QLS §Selection has no containment
// predicate, so "|=" maps to the similar (regex) predicate rather than to
// equality, and the registry file is where that choice lives.
func (r *logqlResolver) resolveLineFilter(node *logql.LineFilter) (ir.PipelineStage, error) {
	// The operator comes from the AST rather than from the registry, because
	// the registry is keyed by spelling and LogQL spells a negated line filter
	// "!=" — the same as a label matcher meaning something else. The parser
	// already told the two apart by position, so that distinction is used here
	// rather than being thrown away and guessed at again.
	var op ir.MatchOp
	switch node.Op {
	case logql.LineContains:
		op = ir.MatchContains
	case logql.LineNotContains:
		op = ir.MatchNotContains
	case logql.LineMatchesRegex:
		op = ir.MatchRegex
	case logql.LineNotMatchesRegex:
		op = ir.MatchNotRegex
	default:
		return nil, fmt.Errorf("resolver: logql: no IR operator for the line filter %q", node.Op)
	}
	return filterStage(matchPredicate(FieldBody, op, node.Match)), nil
}

// resolveLabelFilter turns a label filter expression into a predicate tree.
func (r *logqlResolver) resolveLabelFilter(node *logql.LabelFilter) (ir.PipelineStage, error) {
	predicate, err := r.resolveLabelFilterExpr(node.Predicate)
	if err != nil {
		return nil, err
	}
	return filterStage(predicate), nil
}

// resolveLabelFilterExpr walks LogQL's boolean tree of label predicates into the
// IR's. The shapes correspond directly, so the recursion is structural.
func (r *logqlResolver) resolveLabelFilterExpr(expr logql.LabelFilterExpr) (ir.Predicate, error) {
	switch node := expr.(type) {
	case *logql.LabelPredicate:
		return r.resolveLabelPredicate(node)

	case *logql.LabelFilterParen:
		// Parentheses are already reflected in the tree's shape, so the group
		// collapses into the predicate it wraps.
		return r.resolveLabelFilterExpr(node.Inner)

	case *logql.LabelFilterBinary:
		left, err := r.resolveLabelFilterExpr(node.LHS)
		if err != nil {
			return nil, err
		}
		right, err := r.resolveLabelFilterExpr(node.RHS)
		if err != nil {
			return nil, err
		}
		op := ir.LogicalAnd
		if node.Op == logql.FilterOr {
			op = ir.LogicalOr
		}
		// LogQL's comma is a synonym for "and", so both fold to the same IR
		// operator; the spelling is the parser's to reproduce, not the IR's.
		return &ir.LogicalPredicate{Op: op, Operands: []ir.Predicate{left, right}}, nil

	default:
		return nil, fmt.Errorf("resolver: logql: no rule for label filter type %T", expr)
	}
}

func (r *logqlResolver) resolveLabelPredicate(node *logql.LabelPredicate) (ir.Predicate, error) {
	op, err := lookupOperator(r.def, node.Op.String(), registry.OperatorContextComparison)
	if err != nil {
		return nil, err
	}
	// The source spelling is kept rather than the parsed number, so "20MB" and
	// "1.5h" survive translation as written. The registry's type_coercion
	// records what each kind means in QLS terms.
	return matchPredicate(node.Name, op, labelFilterValueText(node.Value)), nil
}

// labelFilterValueText renders a filter operand for the IR matcher's string
// value.
func labelFilterValueText(value *logql.FilterValue) string {
	if value == nil {
		return ""
	}
	if value.Kind == logql.FilterValueString {
		return value.Str
	}
	return value.Text
}

func (r *logqlResolver) resolveParserStage(node *logql.ParserStage) (ir.PipelineStage, error) {
	var name string
	switch node.Kind {
	case logql.ParserJSON:
		name = FuncParseJSON
	case logql.ParserLogfmt:
		name = FuncParseLogfmt
	case logql.ParserRegexp:
		name = FuncParseRegexp
	case logql.ParserPattern:
		name = FuncParsePattern
	case logql.ParserUnpack:
		name = FuncParseUnpack
	default:
		return nil, fmt.Errorf("resolver: logql: no rule for parser stage kind %s", node.Kind)
	}

	var args []ir.IRExpr
	if node.Pattern != "" {
		args = append(args, stringLiteral(node.Pattern))
	}
	for _, flag := range node.Flags {
		args = append(args, stringLiteral(flag))
	}
	for _, param := range node.Params {
		if param.Expression == "" {
			// A bare name extracts the label of that name.
			args = append(args, &ir.RefExpr{Name: param.Name, Scope: ir.ScopeUnscoped, Type: ir.DataTypeString})
			continue
		}
		args = append(args, stringLiteral(param.Name), stringLiteral(param.Expression))
	}

	// A parser stage turns a log line into attributes, so what flows on is still
	// the log record.
	return &ir.FunctionStage{Name: name, Args: args, ReturnType: ir.DataTypeString}, nil
}

func (r *logqlResolver) resolveFormatterStage(node *logql.FormatterStage) (ir.PipelineStage, error) {
	if node.Kind == logql.FormatLine {
		return &ir.FunctionStage{
			Name:       FuncLineFormat,
			Args:       []ir.IRExpr{stringLiteral(node.Template)},
			ReturnType: ir.DataTypeString,
		}, nil
	}

	var args []ir.IRExpr
	for _, param := range node.Params {
		if param.IsTemplate {
			args = append(args, stringLiteral(param.Dst), stringLiteral(param.Template))
			continue
		}
		// A rename names a source label, which is a reference rather than a
		// literal.
		args = append(args,
			stringLiteral(param.Dst),
			&ir.RefExpr{Name: param.Src, Scope: ir.ScopeUnscoped, Type: ir.DataTypeString})
	}
	return &ir.FunctionStage{Name: FuncLabelFormat, Args: args, ReturnType: ir.DataTypeString}, nil
}

// labelRefArgs turns a drop or keep operand list into arguments. A bare name is
// a label reference; a matcher keeps its comparison as a literal pair.
func labelRefArgs(refs []*logql.LabelRef) []ir.IRExpr {
	var args []ir.IRExpr
	for _, ref := range refs {
		if ref.Matcher == nil {
			args = append(args, &ir.RefExpr{Name: ref.Name, Scope: ir.ScopeUnscoped, Type: ir.DataTypeString})
			continue
		}
		args = append(args,
			&ir.RefExpr{Name: ref.Matcher.Name, Scope: ir.ScopeUnscoped, Type: ir.DataTypeString},
			stringLiteral(ref.Matcher.Type.String()),
			stringLiteral(ref.Matcher.Value))
	}
	return args
}

// resolveLogRange resolves the log expression a range aggregation reads, along
// with the window and the unwrap that belong to the range rather than to the
// pipeline.
//
// The unwrap becomes a pipeline stage placed after the log pipeline and before
// the aggregation, because that is what it does: QLS §Attributes describes
// coercing an attribute value into a metric as its own operation, distinct from
// the temporal aggregation that then reduces those values.
func (r *logqlResolver) resolveLogRange(node *logql.LogRange) (*ir.Query, error) {
	query, err := r.resolve(node.Selector)
	if err != nil {
		return nil, err
	}

	if node.Unwrap != nil {
		appendStage(query, unwrapStage(node.Unwrap))
		// Filters written after the unwrap discard conversion failures, and run
		// after the coercion.
		for _, filter := range node.Unwrap.PostFilters {
			stage, err := r.resolveLabelFilter(filter)
			if err != nil {
				return nil, err
			}
			appendStage(query, stage)
		}
	}

	bounds := window(query)
	// The LogQL parser keeps each duration's source text, so "[90m]" survives
	// the round trip as "90m" rather than being re-derived as "1h30m".
	bounds.Step = ir.NewIntervalFromSource(node.Interval.Value, node.Interval.Text)
	if node.Offset != nil {
		bounds.Offset = ir.NewIntervalFromSource(node.Offset.Value, node.Offset.Text)
	}
	return query, nil
}

// unwrapStage builds the metric coercion stage for an unwrap.
func unwrapStage(unwrap *logql.UnwrapExpr) *ir.FunctionStage {
	args := []ir.IRExpr{&ir.RefExpr{
		Name:  unwrap.Identifier,
		Scope: ir.ScopeUnscoped,
		Type:  ir.DataTypeString,
	}}
	returnType := ir.DataTypeDouble
	if unwrap.Conversion != logql.ConvNone {
		// The conversion names how the label's text becomes a number.
		args = append(args, stringLiteral(unwrap.Conversion.String()))
		switch unwrap.Conversion {
		case logql.ConvDuration:
			returnType = ir.DataTypeInterval
		case logql.ConvBytes:
			returnType = ir.DataTypeUnsignedInt
		}
	}
	return &ir.FunctionStage{Name: FuncUnwrap, Args: args, ReturnType: returnType}
}

// resolveRangeAggregation maps a LogQL range function onto the IR.
//
// These always reduce values within a window, so the scope is TEMPORAL. The
// registry is asked anyway, and a disagreement is an error rather than an
// override: it would mean the definition file is wrong.
func (r *logqlResolver) resolveRangeAggregation(node *logql.RangeAggregation) (*ir.Query, error) {
	name := node.Op.String()
	fn, err := lookupFunction(r.def, name)
	if err != nil {
		return nil, err
	}

	query, err := r.resolveLogRange(node.Range)
	if err != nil {
		return nil, err
	}

	if !fn.IsAggregation {
		// bytes_rate and its kind have no IR aggregation operator, because they
		// measure something the operator does not name. They stay functions.
		var args []ir.IRExpr
		if node.Param != nil {
			args = append(args, numberLiteral(node.Param.Val))
		}
		appendStage(query, &ir.FunctionStage{Name: fn.IRName, Args: args, ReturnType: fn.ReturnType})
		recordGroupingOnOutput(query, node.Grouping)
		return query, nil
	}

	if fn.AggScope != ir.AggScopeTemporal {
		return nil, fmt.Errorf("resolver: logql: %q reduces values over a window, but the registry "+
			"definition at %s gives it scope %s", name, r.def.SourcePath, fn.AggScope)
	}

	stage := aggregationStage(fn)
	if node.Param != nil {
		stage.Parameter = numberLiteral(node.Param.Val)
	}
	applyGroupingToStage(stage, node.Grouping)
	appendStage(query, stage)
	recordOutputGrouping(query, stage)
	return query, nil
}

// resolveVectorAggregation maps a LogQL aggregation across streams onto the IR.
func (r *logqlResolver) resolveVectorAggregation(node *logql.VectorAggregation) (*ir.Query, error) {
	name := node.Op.String()
	fn, err := lookupFunction(r.def, name)
	if err != nil {
		return nil, err
	}

	query, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}

	if !fn.IsAggregation {
		// sort, sort_desc and approx_topk have no IR aggregation operator.
		var args []ir.IRExpr
		if node.Param != nil {
			args = append(args, numberLiteral(node.Param.Val))
		}
		appendStage(query, &ir.FunctionStage{Name: fn.IRName, Args: args, ReturnType: fn.ReturnType})
		recordGroupingOnOutput(query, node.Grouping)
		return query, nil
	}

	if fn.AggScope != ir.AggScopeGroup {
		return nil, fmt.Errorf("resolver: logql: %q aggregates across streams, but the registry "+
			"definition at %s gives it scope %s", name, r.def.SourcePath, fn.AggScope)
	}

	stage := aggregationStage(fn)
	applyGroupingToStage(stage, node.Grouping)
	if node.Param != nil {
		stage.Parameter = numberLiteral(node.Param.Val)
	}

	appendStage(query, stage)
	recordOutputGrouping(query, stage)
	return query, nil
}

// applyGroupingToStage records a by/without clause on an aggregation.
//
// LogQL allows the clause on a range aggregation as well as on a vector one. On
// a range aggregation it partitions the streams before reducing each partition
// over time, so it belongs on that stage rather than becoming a second,
// invented aggregation.
func applyGroupingToStage(stage *ir.AggregationStage, grouping *logql.Grouping) {
	if grouping == nil {
		return
	}
	if grouping.Without {
		stage.Without = grouping.Labels
	} else {
		stage.GroupBy = grouping.Labels
	}
}

// recordGroupingOnOutput records a grouping for a stage that cannot carry one,
// which is the case when the function has no IR aggregation operator.
func recordGroupingOnOutput(query *ir.Query, grouping *logql.Grouping) {
	if grouping == nil || grouping.Without || len(grouping.Labels) == 0 {
		return
	}
	if query.Output == nil {
		query.Output = &ir.Output{}
	}
	query.Output.GroupBy = grouping.Labels
}

func (r *logqlResolver) resolveBinaryExpr(node *logql.BinaryExpr) (*ir.Query, error) {
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

func (r *logqlResolver) resolveJoin(node *logql.BinaryExpr) (*ir.Query, error) {
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
	switch matching.Card {
	case logql.CardManyToOne:
		stage.JoinType = ir.JoinLeftOuter
	case logql.CardOneToMany:
		stage.JoinType = ir.JoinRightOuter
	}
	if matching.On {
		stage.OnLabels = matching.MatchingLabels
	} else {
		stage.IgnoreLabels = matching.MatchingLabels
	}
	stage.IncludeLabels = matching.Include

	op, err := logqlArithOp(node.Op)
	if err != nil {
		return nil, err
	}
	appendStage(left, stage)
	appendStage(left, &ir.BinaryOpStage{Op: op})
	return left, nil
}

func (r *logqlResolver) resolveComparisonFilter(node *logql.BinaryExpr) (*ir.Query, bool, error) {
	var seriesSide logql.Expr
	var literal *logql.NumberLiteral

	leftNum, leftIsNum := node.LHS.(*logql.NumberLiteral)
	rightNum, rightIsNum := node.RHS.(*logql.NumberLiteral)
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
	filter.ReturnsBool = node.ReturnBool
	appendStage(query, filter)
	return query, true, nil
}

// resolveArithmetic builds a function stage for an operator the IR has no enum
// for. As in PromQL, both operands become arguments of a fresh query rather than
// one being folded into, so the tree stays acyclic.
func (r *logqlResolver) resolveArithmetic(node *logql.BinaryExpr) (*ir.Query, error) {
	op, err := logqlArithOp(node.Op)
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

	query := newQuery(ir.SignalLog, nil)
	appendStage(query, &ir.BinaryOpStage{Op: op, Left: left, Right: right})
	return query, nil
}

func (r *logqlResolver) resolveOperand(expr logql.Expr) (*ir.Query, error) {
	if literal, ok := expr.(*logql.NumberLiteral); ok {
		return r.resolveLiteral(numberLiteral(literal.Val))
	}
	return r.resolve(expr)
}

// logqlArithOp maps a LogQL binary operator token onto the IR operator. LogQL
// borrows PromQL's operator set, so the two mappings agree.
func logqlArithOp(op logql.TokenType) (ir.ArithOp, error) {
	switch op {
	case logql.ADD:
		return ir.ArithAdd, nil
	case logql.SUB:
		return ir.ArithSub, nil
	case logql.MUL:
		return ir.ArithMul, nil
	case logql.DIV:
		return ir.ArithDiv, nil
	case logql.MOD:
		return ir.ArithMod, nil
	case logql.POW:
		return ir.ArithPow, nil
	case logql.LAND:
		return ir.ArithAnd, nil
	case logql.LOR:
		return ir.ArithOr, nil
	case logql.LUNLESS:
		return ir.ArithUnless, nil
	case logql.EQLC:
		return ir.ArithEQ, nil
	case logql.NEQ:
		return ir.ArithNEQ, nil
	case logql.GTR:
		return ir.ArithGT, nil
	case logql.GTE:
		return ir.ArithGTE, nil
	case logql.LSS:
		return ir.ArithLT, nil
	case logql.LTE:
		return ir.ArithLTE, nil
	}
	return 0, fmt.Errorf("resolver: logql: no IR operator for %q", op)
}

// resolveUnaryExpr builds a unary sign stage.
//
// The operand becomes a sub-query rather than the stage folding into it, so the
// sign is a node in its own right and the tree stays acyclic.
func (r *logqlResolver) resolveUnaryExpr(node *logql.UnaryExpr) (*ir.Query, error) {
	op := ir.ArithNeg
	if node.Op == logql.ADD {
		op = ir.ArithPos
	}
	operand, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}

	query := newQuery(ir.SignalLog, nil)
	appendStage(query, &ir.UnaryOpStage{Op: op, Operand: operand})
	return query, nil
}

func (r *logqlResolver) resolveParenExpr(node *logql.ParenExpr) (*ir.Query, error) {
	query, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}
	setHint(query, HintParen, "true")
	return query, nil
}

func (r *logqlResolver) resolveLabelReplace(node *logql.LabelReplace) (*ir.Query, error) {
	fn, err := lookupFunction(r.def, "label_replace")
	if err != nil {
		return nil, err
	}
	query, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}
	appendStage(query, &ir.FunctionStage{
		Name: fn.IRName,
		Args: []ir.IRExpr{
			stringLiteral(node.Dst),
			stringLiteral(node.Replacement),
			stringLiteral(node.Src),
			stringLiteral(node.Regex),
		},
		ReturnType: fn.ReturnType,
	})
	return query, nil
}

func (r *logqlResolver) resolveLiteral(value *ir.LiteralExpr) (*ir.Query, error) {
	query := newQuery(ir.SignalLog, nil)
	appendStage(query, &ir.FunctionStage{
		Name:       FuncLiteral,
		Args:       []ir.IRExpr{value},
		ReturnType: value.Type,
	})
	return query, nil
}
