// Package resolver is stage 3 of the compiler pipeline: it walks a DSL-specific
// AST and builds the shared, QLS-aligned TelemetryIR from it.
//
// The resolver knows only the source DSL. It never asks what the query is being
// translated into, and it never refuses to represent something because a target
// could not express it — that judgement belongs to the validator, which annotates
// the finished tree per target. The resolver's contract is narrower and stricter:
// build a faithful IR tree, or fail loudly.
//
// # Shape
//
// The IR flattens what the source DSLs nest. PromQL writes
// sum by (job) (rate(x[5m])) as nested calls; LogQL writes its pipeline left to
// right. Both resolve to the same shape — a Query naming one DataSource, an
// ordered Pipeline of stages, and an Output describing the result — with the
// innermost selector becoming the DataSource and each enclosing operation
// appending a stage. That is what makes the two languages comparable, and it is
// why the resolver folds an enclosing node into the Query its operand produced
// rather than building a tree of nested Queries.
//
// # Fidelity
//
// Every node is created with the zero TranslatabilityFlag, which is FULL. The
// resolver never downgrades a flag: a construct it cannot represent is an error,
// not a silently degraded translation. Detail with no home in the QLS model is
// recorded in Query.Hints rather than dropped.
package resolver

import (
	"fmt"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/ast"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser/logql"
	"github.com/polyql/polyql/pkg/compiler/parser/promql"
	"github.com/polyql/polyql/pkg/registry"
)

// The IR owns this vocabulary, since these are keys into IR structures rather
// than resolver state. They are re-exported here so that reading resolver code
// does not require jumping packages.
const (
	FieldBody  = ir.FieldBody
	FieldValue = ir.FieldValue

	FuncLiteral = ir.FuncLiteral

	HintSourceDSL  = ir.HintSourceDSL
	HintAtModifier = ir.HintAtModifier
	HintParen      = ir.HintParen
)

// FuncUnwrap is LogQL's coercion of a label's text into a number, which
// QLS §Attributes > Coercion/Casting into Metrics describes. Unlike the
// structural names above it is DSL-specific, so a language registry names it.
const FuncUnwrap = "unwrap"

// Resolve builds a TelemetryIR query from a DSL-specific AST.
//
// sourceDSL names the language the node was parsed from; it selects the registry
// definition that supplies function and operator mappings. The target DSL is
// deliberately not a parameter — the IR this produces is DSL-neutral, and
// target compatibility is the validator's concern.
func Resolve(node ast.Node, sourceDSL string, reg *registry.Registry) (*ir.Query, error) {
	if node == nil {
		return nil, fmt.Errorf("resolver: cannot resolve a nil AST node")
	}
	if reg == nil {
		return nil, fmt.Errorf("resolver: a registry is required")
	}

	dsl := strings.ToLower(strings.TrimSpace(sourceDSL))
	if dsl == "" {
		return nil, fmt.Errorf("resolver: a source DSL name is required")
	}
	// A node carries the DSL it was parsed from, so a mismatch means the caller
	// paired a tree with the wrong registry definition — which would otherwise
	// surface as a stream of confusing "unknown function" errors.
	if nodeDSL := strings.ToLower(node.DSL()); nodeDSL != dsl {
		return nil, fmt.Errorf("resolver: asked to resolve a %s AST as %s", node.DSL(), sourceDSL)
	}

	def, err := reg.Get(dsl)
	if err != nil {
		return nil, fmt.Errorf("resolver: %w", err)
	}

	var query *ir.Query
	// Dispatch on the tree's own type rather than on the DSL name, so a node
	// type that no resolver handles cannot slip through as a silently empty
	// query.
	switch typed := node.(type) {
	case promql.Expr:
		query, err = (&promqlResolver{def: def}).resolve(typed)
	case logql.Expr:
		query, err = (&logqlResolver{def: def}).resolve(typed)
	default:
		return nil, fmt.Errorf("resolver: no resolver handles AST node type %T (from DSL %q)", node, node.DSL())
	}
	if err != nil {
		return nil, err
	}

	setHint(query, HintSourceDSL, def.DSL)
	return query, nil
}

// newQuery starts an IR query for a data source. Output is always present so
// that enclosing stages have somewhere to record a window without each having to
// check for nil.
func newQuery(signal ir.SignalType, source *ir.DataSource) *ir.Query {
	return &ir.Query{
		Signal: signal,
		Source: source,
		Output: &ir.Output{},
	}
}

// appendStage adds a stage to the end of a query's pipeline, which is how an
// enclosing AST node folds itself into the query its operand produced.
func appendStage(q *ir.Query, stage ir.PipelineStage) {
	q.Pipeline = append(q.Pipeline, stage)
}

// window returns the query's window, creating it on first use. A window only
// appears in the IR once something in the source actually specifies one.
func window(q *ir.Query) *ir.Window {
	if q.Output == nil {
		q.Output = &ir.Output{}
	}
	if q.Output.Window == nil {
		q.Output.Window = &ir.Window{}
	}
	return q.Output.Window
}

// timeRange returns the query's time range, creating it on first use.
func timeRange(q *ir.Query) *ir.TimeRange {
	if q.Output == nil {
		q.Output = &ir.Output{}
	}
	if q.Output.Range == nil {
		q.Output.Range = &ir.TimeRange{}
	}
	return q.Output.Range
}

func setHint(q *ir.Query, key, value string) {
	if q.Hints == nil {
		q.Hints = make(map[string]string)
	}
	q.Hints[key] = value
}

// matchPredicate builds a single-comparison predicate.
func matchPredicate(key string, op ir.MatchOp, value string) *ir.MatchPredicate {
	return &ir.MatchPredicate{Matcher: &ir.LabelMatcher{Key: key, Op: op, Value: value}}
}

// filterStage wraps a predicate in a pipeline stage.
func filterStage(p ir.Predicate) *ir.FilterStage {
	return &ir.FilterStage{Predicate: p}
}

// lookupOperator resolves a DSL operator spelling to its QLS selection
// predicate through the registry.
//
// The wanted context is carried for the error message only. The registry keys
// operators by spelling, and a spelling maps to exactly one IR operator whatever
// context it appears in — PromQL's selector "=" and comparison "==" are distinct
// spellings, and LogQL's "!=" means NEQ both as a stream matcher and as a line
// filter. Context exists for the reverse direction, where an emitter choosing
// between "=" and "==" does need to know which it is writing.
func lookupOperator(def *registry.DSLDefinition, symbol string, want registry.OperatorContext) (ir.MatchOp, error) {
	op, err := def.Operator(symbol)
	if err != nil {
		return 0, fmt.Errorf("resolver: %s has no %s operator %q: %w", def.DSL, want, symbol, err)
	}
	return op.IROp, nil
}

// lookupFunction resolves a DSL function name through the registry.
func lookupFunction(def *registry.DSLDefinition, name string) (*registry.FunctionDef, error) {
	fn, err := def.Function(name)
	if err != nil {
		return nil, fmt.Errorf("resolver: %w; the registry definition at %s needs an entry for it",
			err, def.SourcePath)
	}
	return fn, nil
}

// aggregationStage builds an aggregation from a registry function definition.
// The scope comes from the registry rather than from the call site, because it
// is the only thing distinguishing operators that share an IR operator — sum and
// sum_over_time are both IR SUM, and differ only in the axis they collapse.
func aggregationStage(fn *registry.FunctionDef) *ir.AggregationStage {
	return &ir.AggregationStage{Op: fn.AggOp, Scope: fn.AggScope}
}

// numberLiteral builds a DOUBLE literal, the form a scalar argument takes.
func numberLiteral(v float64) *ir.LiteralExpr { return ir.NewNumberLiteral(v) }

// stringLiteral builds a STRING literal.
func stringLiteral(v string) *ir.LiteralExpr { return ir.NewStringLiteral(v) }

// queryExpr wraps a resolved query so it can be an argument.
func queryExpr(q *ir.Query) *ir.QueryExpr { return &ir.QueryExpr{Query: q} }

// formatFloat renders a float for a hint value.
func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}
