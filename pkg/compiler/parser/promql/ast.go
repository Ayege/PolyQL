// Package promql implements the PromQL front end: a hand-written lexer and
// Pratt parser producing a PromQL-specific AST.
//
// The AST mirrors PromQL's own structure rather than the QLS-aligned IR. That
// is deliberate — the resolver is what normalises PromQL's shape onto the IR,
// and it can only do so honestly if the tree it reads still says what the user
// actually wrote. Every node renders back to valid PromQL through String, which
// is what makes round-trip translation testable.
package promql

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ast"
)

// DSL is the name this parser registers under.
const DSL = "promql"

// promqlNode marks a type as belonging to the PromQL AST, supplying the DSL
// half of the shared ast.Node contract.
type promqlNode struct{}

func (promqlNode) DSL() string { return DSL }

// ValueType is the type of value a PromQL expression evaluates to. PromQL's
// grammar is type-directed — rate requires a range vector, histogram_quantile
// requires a scalar and an instant vector — so the parser carries these in
// order to reject ill-typed queries at parse time.
type ValueType int

const (
	// ValueTypeNone is the type of an expression that yields nothing.
	ValueTypeNone ValueType = iota
	ValueTypeScalar
	ValueTypeString
	// ValueTypeVector is an instant vector: one sample per series.
	ValueTypeVector
	// ValueTypeMatrix is a range vector: a range of samples per series.
	ValueTypeMatrix
)

var valueTypeNames = map[ValueType]string{
	ValueTypeNone:   "none",
	ValueTypeScalar: "scalar",
	ValueTypeString: "string",
	ValueTypeVector: "instant vector",
	ValueTypeMatrix: "range vector",
}

func (v ValueType) String() string {
	if s, ok := valueTypeNames[v]; ok {
		return s
	}
	return fmt.Sprintf("ValueType(%d)", int(v))
}

// Expr is a PromQL expression node.
type Expr interface {
	ast.Node
	// Type is the value type the expression evaluates to.
	Type() ValueType
	exprNode()
}

// MatchType is a label matching operator inside a selector's braces.
type MatchType int

const (
	MatchEqual MatchType = iota
	MatchNotEqual
	MatchRegexp
	MatchNotRegexp
)

var matchTypeText = map[MatchType]string{
	MatchEqual:     "=",
	MatchNotEqual:  "!=",
	MatchRegexp:    "=~",
	MatchNotRegexp: "!~",
}

func (m MatchType) String() string {
	if s, ok := matchTypeText[m]; ok {
		return s
	}
	return fmt.Sprintf("MatchType(%d)", int(m))
}

// matchTypeForToken maps a lexer token to its matching operator.
func matchTypeForToken(t TokenType) (MatchType, bool) {
	switch t {
	case EQL:
		return MatchEqual, true
	case NEQ:
		return MatchNotEqual, true
	case EQL_REGEX:
		return MatchRegexp, true
	case NEQ_REGEX:
		return MatchNotRegexp, true
	}
	return 0, false
}

// LabelMatcher is one label predicate within a vector selector.
//
// This is PromQL's own matcher, not the IR's: it carries PromQL regex semantics
// (RE2, fully anchored) that the resolver translates into the QLS similar
// predicate.
type LabelMatcher struct {
	Name  string
	Type  MatchType
	Value string
}

func (m *LabelMatcher) String() string {
	return fmt.Sprintf("%s%s%s", m.Name, m.Type, strconv.Quote(m.Value))
}

// MetricNameLabel is the label under which a series' name is matched when it is
// written inside braces rather than as a bare name.
const MetricNameLabel = "__name__"

// VectorSelector selects an instant vector: a metric name, a set of label
// matchers, or both.
//
// A range makes a selector a MatrixSelector rather than a field here, because
// in PromQL a selector carrying a range is a different type of expression: it
// yields a range vector, and only functions like rate accept it.
//
// Name holds a bare metric name. A name written as a matcher — {__name__=~"..."}
// — stays in LabelMatchers with Name empty, so the tree records which of the two
// forms the user wrote and String reproduces it.
type VectorSelector struct {
	promqlNode
	Name          string
	LabelMatchers []*LabelMatcher
	// OriginalOffset is the offset modifier's duration, negative for a forward
	// shift. Zero means no offset.
	OriginalOffset time.Duration
	// At is the @ modifier, nil when absent.
	At *AtModifier
}

func (*VectorSelector) exprNode()       {}
func (*VectorSelector) Type() ValueType { return ValueTypeVector }

func (e *VectorSelector) String() string {
	var b strings.Builder
	b.WriteString(e.Name)
	if len(e.LabelMatchers) > 0 {
		parts := make([]string, 0, len(e.LabelMatchers))
		for _, m := range e.LabelMatchers {
			parts = append(parts, m.String())
		}
		b.WriteString("{" + strings.Join(parts, ",") + "}")
	}
	b.WriteString(modifiersString(e.At, e.OriginalOffset))
	return b.String()
}

// MatrixSelector is a vector selector with a range, yielding a range vector.
type MatrixSelector struct {
	promqlNode
	VectorSelector *VectorSelector
	Range          time.Duration
}

func (*MatrixSelector) exprNode()       {}
func (*MatrixSelector) Type() ValueType { return ValueTypeMatrix }

func (e *MatrixSelector) String() string {
	// The modifiers belong to the selector but are written after the range, so
	// render the selector without them and re-append.
	inner := *e.VectorSelector
	inner.OriginalOffset = 0
	inner.At = nil
	return fmt.Sprintf("%s[%s]%s", inner.String(), FormatDuration(e.Range),
		modifiersString(e.VectorSelector.At, e.VectorSelector.OriginalOffset))
}

// SubqueryExpr evaluates an instant-vector expression over a range at a fixed
// resolution, yielding a range vector.
type SubqueryExpr struct {
	promqlNode
	Expr  Expr
	Range time.Duration
	// Step is the resolution. Zero means the query engine's default evaluation
	// interval, and is written as an empty resolution: [30m:].
	Step           time.Duration
	OriginalOffset time.Duration
	At             *AtModifier
}

func (*SubqueryExpr) exprNode()       {}
func (*SubqueryExpr) Type() ValueType { return ValueTypeMatrix }

func (e *SubqueryExpr) String() string {
	step := ""
	if e.Step > 0 {
		step = FormatDuration(e.Step)
	}
	return fmt.Sprintf("%s[%s:%s]%s", e.Expr, FormatDuration(e.Range), step,
		modifiersString(e.At, e.OriginalOffset))
}

// AtPreset distinguishes an absolute @ timestamp from the start() and end()
// forms, which resolve against the query's own range.
type AtPreset int

const (
	AtTimestamp AtPreset = iota
	AtStart
	AtEnd
)

// AtModifier pins evaluation to a fixed instant.
type AtModifier struct {
	Preset AtPreset
	// Timestamp is the Unix time in seconds, used when Preset is AtTimestamp.
	Timestamp float64
}

func (a *AtModifier) String() string {
	switch a.Preset {
	case AtStart:
		return "start()"
	case AtEnd:
		return "end()"
	default:
		return formatFloat(a.Timestamp)
	}
}

// modifiersString renders the @ and offset suffixes shared by the selector and
// subquery nodes.
func modifiersString(at *AtModifier, offset time.Duration) string {
	var b strings.Builder
	if at != nil {
		b.WriteString(" @ " + at.String())
	}
	if offset > 0 {
		b.WriteString(" offset " + FormatDuration(offset))
	} else if offset < 0 {
		b.WriteString(" offset -" + FormatDuration(-offset))
	}
	return b.String()
}

// AggregateExpr collapses an instant vector across series.
//
// Param carries the leading argument of the operators that take one, such as
// the 5 of topk(5, x); it is nil otherwise.
type AggregateExpr struct {
	promqlNode
	Op       TokenType
	Expr     Expr
	Param    Expr
	Grouping []string
	// Without inverts Grouping: the listed labels are dropped rather than kept.
	Without bool
}

func (*AggregateExpr) exprNode()       {}
func (*AggregateExpr) Type() ValueType { return ValueTypeVector }

func (e *AggregateExpr) String() string {
	var b strings.Builder
	b.WriteString(e.Op.String())
	switch {
	case e.Without:
		b.WriteString(" without (" + strings.Join(e.Grouping, ", ") + ") ")
	case len(e.Grouping) > 0:
		b.WriteString(" by (" + strings.Join(e.Grouping, ", ") + ") ")
	}
	b.WriteString("(")
	if e.Param != nil {
		b.WriteString(e.Param.String() + ", ")
	}
	b.WriteString(e.Expr.String() + ")")
	return b.String()
}

// VectorMatchCardinality describes how many series on each side of a binary
// operator may match.
type VectorMatchCardinality int

const (
	CardOneToOne VectorMatchCardinality = iota
	CardManyToOne
	CardOneToMany
	CardManyToMany
)

// VectorMatching configures how the two sides of a binary operator are paired.
type VectorMatching struct {
	Card VectorMatchCardinality
	// MatchingLabels are the labels named by on or ignoring.
	MatchingLabels []string
	// On distinguishes on(...) from ignoring(...).
	On bool
	// Include are the labels named by group_left or group_right, copied from
	// the "one" side onto the result.
	Include []string
}

// BinaryExpr applies an operator to two expressions.
type BinaryExpr struct {
	promqlNode
	Op             TokenType
	LHS, RHS       Expr
	VectorMatching *VectorMatching
	// ReturnBool is the bool modifier, which turns a comparison's filtering
	// behaviour into 0/1 values.
	ReturnBool bool
}

func (*BinaryExpr) exprNode() {}

// Type is scalar only when both operands are scalars; any vector operand makes
// the result an instant vector.
func (e *BinaryExpr) Type() ValueType {
	if e.LHS.Type() == ValueTypeScalar && e.RHS.Type() == ValueTypeScalar {
		return ValueTypeScalar
	}
	return ValueTypeVector
}

func (e *BinaryExpr) String() string {
	var b strings.Builder
	b.WriteString(e.LHS.String() + " " + e.Op.String())
	if e.ReturnBool {
		b.WriteString(" bool")
	}
	if vm := e.VectorMatching; vm != nil && (vm.On || len(vm.MatchingLabels) > 0) {
		keyword := "ignoring"
		if vm.On {
			keyword = "on"
		}
		b.WriteString(" " + keyword + " (" + strings.Join(vm.MatchingLabels, ", ") + ")")
	}
	if vm := e.VectorMatching; vm != nil {
		switch vm.Card {
		case CardManyToOne:
			b.WriteString(" group_left (" + strings.Join(vm.Include, ", ") + ")")
		case CardOneToMany:
			b.WriteString(" group_right (" + strings.Join(vm.Include, ", ") + ")")
		}
	}
	b.WriteString(" " + e.RHS.String())
	return b.String()
}

// UnaryExpr applies a leading + or - to an expression.
//
// The negation is kept as its own node rather than folded into a number
// literal, because the AST's job is to record what the user wrote; folding
// would erase the distinction before the resolver ever sees it.
type UnaryExpr struct {
	promqlNode
	Op   TokenType
	Expr Expr
}

func (*UnaryExpr) exprNode()         {}
func (e *UnaryExpr) Type() ValueType { return e.Expr.Type() }
func (e *UnaryExpr) String() string  { return e.Op.String() + e.Expr.String() }

// ParenExpr is an explicitly parenthesised expression. It is preserved rather
// than dropped so that String reproduces the user's grouping instead of
// re-deriving it from precedence.
type ParenExpr struct {
	promqlNode
	Expr Expr
}

func (*ParenExpr) exprNode()         {}
func (e *ParenExpr) Type() ValueType { return e.Expr.Type() }
func (e *ParenExpr) String() string  { return "(" + e.Expr.String() + ")" }

// Call is a function invocation.
type Call struct {
	promqlNode
	Func *Function
	Args []Expr
}

func (*Call) exprNode()         {}
func (e *Call) Type() ValueType { return e.Func.ReturnType }

func (e *Call) String() string {
	args := make([]string, 0, len(e.Args))
	for _, a := range e.Args {
		args = append(args, a.String())
	}
	return e.Func.Name + "(" + strings.Join(args, ", ") + ")"
}

// NumberLiteral is a scalar literal.
type NumberLiteral struct {
	promqlNode
	Val float64
}

func (*NumberLiteral) exprNode()        {}
func (*NumberLiteral) Type() ValueType  { return ValueTypeScalar }
func (e *NumberLiteral) String() string { return formatFloat(e.Val) }

// StringLiteral is a string literal.
type StringLiteral struct {
	promqlNode
	Val string
}

func (*StringLiteral) exprNode()        {}
func (*StringLiteral) Type() ValueType  { return ValueTypeString }
func (e *StringLiteral) String() string { return strconv.Quote(e.Val) }

// formatFloat renders a float in a form PromQL re-parses to the same value.
//
// Positive infinity is written "Inf" rather than "+Inf": PromQL has no signed
// float literal, so a leading "+" lexes as a unary operator and every render
// and re-parse cycle would stack another one.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Function describes a PromQL function's signature, which the parser uses to
// check calls.
type Function struct {
	Name     string
	ArgTypes []ValueType
	// Variadic is 0 for a fixed arity, -1 when the final argument type may
	// repeat without limit, and n > 0 when up to n optional arguments of the
	// final type may follow.
	Variadic   int
	ReturnType ValueType
}

// functions is the function table. A name is only a function when followed by
// "(", so this table is consulted at parse time rather than by the lexer.
var functions = map[string]*Function{}

func addFunction(name string, ret ValueType, variadic int, args ...ValueType) {
	functions[name] = &Function{Name: name, ArgTypes: args, Variadic: variadic, ReturnType: ret}
}

func init() {
	v, s, m, str := ValueTypeVector, ValueTypeScalar, ValueTypeMatrix, ValueTypeString

	// Rate and counter functions, which consume range vectors.
	for _, name := range []string{"rate", "irate", "increase", "delta", "idelta", "deriv", "resets", "changes"} {
		addFunction(name, v, 0, m)
	}
	addFunction("predict_linear", v, 0, m, s)
	addFunction("double_exponential_smoothing", v, 0, m, s, s)

	// Aggregations over time, which also consume range vectors.
	for _, name := range []string{
		"avg_over_time", "min_over_time", "max_over_time", "sum_over_time", "count_over_time",
		"stddev_over_time", "stdvar_over_time", "last_over_time", "present_over_time",
		"mad_over_time", "absent_over_time",
	} {
		addFunction(name, v, 0, m)
	}
	addFunction("quantile_over_time", v, 0, s, m)

	// Histograms.
	addFunction("histogram_quantile", v, 0, s, v)
	for _, name := range []string{"histogram_count", "histogram_sum", "histogram_avg", "histogram_stddev", "histogram_stdvar"} {
		addFunction(name, v, 0, v)
	}
	addFunction("histogram_fraction", v, 0, s, s, v)

	// Element-wise math over instant vectors.
	for _, name := range []string{
		"abs", "ceil", "floor", "exp", "ln", "log2", "log10", "sqrt", "sgn",
		"sin", "cos", "tan", "asin", "acos", "atan", "sinh", "cosh", "tanh",
		"asinh", "acosh", "atanh", "deg", "rad", "absent",
	} {
		addFunction(name, v, 0, v)
	}
	addFunction("round", v, 1, v, s)
	addFunction("clamp", v, 0, v, s, s)
	addFunction("clamp_max", v, 0, v, s)
	addFunction("clamp_min", v, 0, v, s)

	// Label manipulation, sorting and time.
	addFunction("label_replace", v, 0, v, str, str, str, str)
	addFunction("label_join", v, -1, v, str, str, str)
	addFunction("sort", v, 0, v)
	addFunction("sort_desc", v, 0, v)
	addFunction("sort_by_label", v, -1, v, str)
	addFunction("sort_by_label_desc", v, -1, v, str)
	addFunction("scalar", s, 0, v)
	addFunction("vector", v, 0, s)
	addFunction("timestamp", v, 0, v)
	addFunction("time", s, 0)
	addFunction("pi", s, 0)
	for _, name := range []string{
		"day_of_month", "day_of_week", "day_of_year", "days_in_month",
		"hour", "minute", "month", "year",
	} {
		addFunction(name, v, 1, v)
	}
}

// AggregatorNames returns the aggregation operators PromQL spells as keywords
// rather than as function calls — sum, topk and the rest.
//
// They are not in the function table, because "sum" is not a call in PromQL's
// grammar. The language registry still carries their IR mapping, so a
// consistency check comparing the two needs this list to tell that intended
// asymmetry apart from a genuine gap.
func AggregatorNames() []string {
	var names []string
	for word, tok := range keywords {
		if tok.IsAggregator() {
			names = append(names, word)
		}
	}
	sort.Strings(names)
	return names
}

// IsAggregatorName reports whether a name is one of PromQL's aggregation
// operator keywords.
func IsAggregatorName(name string) bool {
	tok, ok := keywords[strings.ToLower(name)]
	return ok && tok.IsAggregator()
}

// LookupFunction returns the function of the given name.
func LookupFunction(name string) (*Function, bool) {
	f, ok := functions[name]
	return f, ok
}

// FunctionNames returns every known function name, sorted. It gives the
// language registry a way to check its own function mappings against what the
// parser accepts.
func FunctionNames() []string {
	names := make([]string, 0, len(functions))
	for name := range functions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
