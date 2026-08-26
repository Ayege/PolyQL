package resolver

import (
	"fmt"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser/traceql"
	"github.com/polyql/polyql/pkg/registry"
)

// traceqlResolver maps a TraceQL AST onto the IR.
//
// TraceQL is the language the IR's span vocabulary exists for, so most of this
// is a direct correspondence rather than a restructuring: a span set becomes the
// query's data source, a structural operator becomes a stage, and an aggregate
// becomes an AggregationStage. The two places it is not direct are noted where
// they arise — the boolean filter tree, and the absence of any window.
type traceqlResolver struct {
	def *registry.DSLDefinition
}

func (r *traceqlResolver) resolve(expr traceql.Expr) (*ir.Query, error) {
	switch node := expr.(type) {
	case *traceql.Spanset:
		return r.resolveSpanset(node)
	case *traceql.StructuralExpr:
		return r.resolveStructural(node)
	case *traceql.AggregateExpr:
		return r.resolveAggregate(node)
	case *traceql.AttributeCoercion:
		return r.resolveCoercion(node)
	case *traceql.ParenExpr:
		return r.resolveParen(node)
	default:
		return nil, fmt.Errorf("resolver: traceql: no rule for AST node type %T", expr)
	}
}

// resolveSpanset turns a span set selector into the query's data source.
//
// A TraceQL span set has no name of its own — it is identified entirely by the
// predicate between its braces — so DataSource.Name stays empty, as it does for
// a LogQL stream. QLS §Selection allows an omitted data source for exactly this
// reason.
//
// The predicate goes into DataSource.Spanset rather than into a Selector because
// a Selector's matchers are conjunctive and a TraceQL filter is a full boolean
// expression; see ir.SpansetSelector.
func (r *traceqlResolver) resolveSpanset(node *traceql.Spanset) (*ir.Query, error) {
	source := &ir.DataSource{Name: "", Scope: ir.ScopeSpan}

	if node.Filter != nil {
		filters, err := r.resolveFieldExpr(node.Filter)
		if err != nil {
			return nil, err
		}
		source.Spanset = &ir.SpansetSelector{Filters: filters}
	} else {
		// An empty selector still selects: "{}" matches every span, which is a
		// spanset with no predicate rather than the absence of one.
		source.Spanset = &ir.SpansetSelector{}
	}

	return newQuery(ir.SignalSpan, source), nil
}

// resolveFieldExpr walks TraceQL's boolean tree into the IR's. The shapes
// correspond directly, so the recursion is structural.
func (r *traceqlResolver) resolveFieldExpr(expr traceql.FieldExpr) (ir.Predicate, error) {
	switch node := expr.(type) {
	case *traceql.SpansetFilter:
		return r.resolveFilter(node)

	case *traceql.FieldParen:
		// Parentheses are already reflected in the tree's shape, so the group
		// collapses into the predicate it wraps.
		return r.resolveFieldExpr(node.Inner)

	case *traceql.FieldNot:
		inner, err := r.resolveFieldExpr(node.Inner)
		if err != nil {
			return nil, err
		}
		return &ir.LogicalPredicate{Op: ir.LogicalNot, Operands: []ir.Predicate{inner}}, nil

	case *traceql.FieldBinary:
		left, err := r.resolveFieldExpr(node.LHS)
		if err != nil {
			return nil, err
		}
		right, err := r.resolveFieldExpr(node.RHS)
		if err != nil {
			return nil, err
		}
		op := ir.LogicalAnd
		if node.Op == traceql.BoolOr {
			op = ir.LogicalOr
		}
		return &ir.LogicalPredicate{Op: op, Operands: []ir.Predicate{left, right}}, nil

	default:
		return nil, fmt.Errorf("resolver: traceql: no rule for field expression type %T", expr)
	}
}

// resolveFilter turns one comparison into an IR match predicate.
//
// The matcher's key is the attribute's qualified form — "span.http.status_code",
// "duration" — so the scope travels with the key rather than beside it. See
// ir.SpansetSelector for why.
func (r *traceqlResolver) resolveFilter(node *traceql.SpansetFilter) (ir.Predicate, error) {
	op, err := lookupOperator(r.def, node.Op.String(), registry.OperatorContextSelector)
	if err != nil {
		return nil, err
	}
	return matchPredicate(node.Attribute.Qualified(), op, spansetValueText(node.Value)), nil
}

// spansetValueText renders a comparison operand for the IR matcher's string
// value.
//
// The source spelling is kept rather than the parsed number, so "100ms" survives
// translation as written instead of being re-derived as "0.1s". The registry's
// type_coercion records what each kind means in QLS terms.
func spansetValueText(value *traceql.Value) string {
	if value == nil {
		return ""
	}
	switch value.Kind {
	case traceql.ValueString:
		return value.Str
	case traceql.ValueStatus, traceql.ValueSpanKind:
		// The bare enum words are unquoted in TraceQL, and their text is what a
		// target with no such enum has to write as a plain string.
		return value.Str
	default:
		return value.Text
	}
}

// resolveStructural folds a trace-tree relationship into the query its left
// operand produced, which is the same fold every enclosing node does.
func (r *traceqlResolver) resolveStructural(node *traceql.StructuralExpr) (*ir.Query, error) {
	query, err := r.resolve(node.LHS)
	if err != nil {
		return nil, err
	}
	right, err := r.resolve(node.RHS)
	if err != nil {
		return nil, err
	}

	var op ir.StructuralOp
	switch node.Op {
	case traceql.StructChild:
		op = ir.StructuralChild
	case traceql.StructDescendant:
		op = ir.StructuralDescendant
	case traceql.StructSibling:
		op = ir.StructuralSibling
	default:
		return nil, fmt.Errorf("resolver: traceql: no IR operator for the structural operator %q", node.Op)
	}

	appendStage(query, &ir.StructuralStage{Op: op, Right: right})
	return query, nil
}

// resolveAggregate turns a metric extraction into an AggregationStage.
//
// Every TraceQL aggregation collapses across spans, never over time: there is no
// window in the query to aggregate over, since a Tempo request carries its range
// separately. The scope is therefore always GROUP, and it comes from the
// registry rather than being written here — the same rule the other resolvers
// follow, and what makes a PromQL sum_over_time translate into TraceQL as an
// honest scope mismatch instead of a silent re-axis.
func (r *traceqlResolver) resolveAggregate(node *traceql.AggregateExpr) (*ir.Query, error) {
	query, err := r.resolve(node.Operand)
	if err != nil {
		return nil, err
	}

	fn, err := lookupFunction(r.def, node.Op.String())
	if err != nil {
		return nil, err
	}
	if !fn.IsAggregation {
		return nil, fmt.Errorf("resolver: traceql: %q is not an aggregation in %s",
			node.Op, r.def.SourcePath)
	}

	stage := aggregationStage(fn)
	if node.Grouping != nil {
		for _, attribute := range node.Grouping.Attributes {
			stage.GroupBy = append(stage.GroupBy, attribute.Qualified())
		}
	}

	// sum, avg, min and max aggregate one attribute's values. The IR's
	// AggregationStage has no field naming the aggregated attribute — a metric
	// series is its own value — so it travels as the stage's parameter, which is
	// where a per-operator argument belongs.
	if node.Attribute != nil {
		stage.Parameter = &ir.RefExpr{
			Name:  node.Attribute.Qualified(),
			Scope: irScopeOf(node.Attribute.Scope),
			Type:  fn.ReturnType,
		}
	}

	appendStage(query, stage)
	return query, nil
}

// irScopeOf maps a TraceQL scope onto the IR's.
func irScopeOf(scope traceql.Scope) ir.Scope {
	switch scope {
	case traceql.ScopeSpan:
		return ir.ScopeSpan
	case traceql.ScopeResource:
		return ir.ScopeResource
	case traceql.ScopeIntrinsic:
		return ir.ScopeIntrinsic
	default:
		return ir.ScopeUnscoped
	}
}

// coercionTargets maps a TraceQL cast onto the QLS type it produces.
var coercionTargets = map[traceql.CoercionType]ir.QlsDataType{
	traceql.CoerceInt:      ir.DataTypeSignedInt,
	traceql.CoerceFloat:    ir.DataTypeDouble,
	traceql.CoerceString:   ir.DataTypeString,
	traceql.CoerceDuration: ir.DataTypeInterval,
	traceql.CoerceBool:     ir.DataTypeBoolean,
}

// resolveCoercion turns an "as" cast into a CoercionStage.
func (r *traceqlResolver) resolveCoercion(node *traceql.AttributeCoercion) (*ir.Query, error) {
	query, err := r.resolve(node.Expr)
	if err != nil {
		return nil, err
	}
	target, ok := coercionTargets[node.AsType]
	if !ok {
		return nil, fmt.Errorf("resolver: traceql: no QLS type for the cast target %q", node.AsType)
	}
	appendStage(query, &ir.CoercionStage{
		Attribute:  node.Attribute.Qualified(),
		TargetType: target,
	})
	return query, nil
}

// resolveParen resolves the grouped expression and records that it was written
// with parentheses, so an emitter can reproduce the author's grouping.
func (r *traceqlResolver) resolveParen(node *traceql.ParenExpr) (*ir.Query, error) {
	query, err := r.resolve(node.Inner)
	if err != nil {
		return nil, err
	}
	setHint(query, HintParen, "true")
	return query, nil
}
