// Package traceql implements the TraceQL front end: a hand-written lexer and
// recursive-descent parser producing a TraceQL-specific AST.
//
// TraceQL is neither a nesting language like PromQL nor a pipeline one like
// LogQL. It is closer to a relational algebra over span sets: a query names a
// set of spans with a boolean expression between braces, and then relates that
// set to another by their position in the trace tree. Aggregation is written
// prefix — count() over ({...}) — rather than as a call wrapping its operand or
// a stage appended to a pipeline.
//
// Two consequences shape this tree. First, the contents of a span set's braces
// are a full boolean expression, so the filter is a tree rather than the flat
// conjunctive matcher list both other DSLs get away with; {a = 1 || b = 2} has
// no conjunctive form. Second, the structural operators join two whole span
// sets, so the spine of a query is a left-associative chain of those rather than
// a stage list. Keeping both shapes here is the point: the resolver is what maps
// them onto the shared IR, and it can only do that honestly if the tree still
// records what was written.
package traceql

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ast"
)

// DSL is the name this parser registers under.
const DSL = "traceql"

// traceqlNode marks a type as belonging to the TraceQL AST, supplying the DSL
// half of the shared ast.Node contract.
type traceqlNode struct{}

func (traceqlNode) DSL() string { return DSL }

// Expr is a TraceQL expression node. Every TraceQL expression yields a span set,
// which is why there is no expression-type distinction of the kind LogQL needs
// between log and metric expressions.
type Expr interface {
	ast.Node
	exprNode()
}

// Scope qualifies which part of a span an attribute lookup addresses.
//
// TraceQL is the reason the IR has a scope concept at all: the same name can
// exist on the resource and on the span, and "service.name" alone does not say
// which is meant.
type Scope int

const (
	// ScopeNone is an unscoped attribute, written bare or with a leading dot.
	// The backend resolves it against every scope.
	ScopeNone Scope = iota
	// ScopeSpan addresses an attribute on the individual span.
	ScopeSpan
	// ScopeResource addresses an attribute shared by everything a resource
	// emits.
	ScopeResource
	// ScopeIntrinsic addresses a field of the span model itself — duration,
	// name, status, kind — rather than a user attribute.
	ScopeIntrinsic
)

var scopeText = map[Scope]string{
	ScopeSpan:      "span",
	ScopeResource:  "resource",
	ScopeIntrinsic: "intrinsic",
}

func (s Scope) String() string {
	if text, ok := scopeText[s]; ok {
		return text
	}
	return ""
}

var scopesByName = func() map[string]Scope {
	m := make(map[string]Scope, len(scopeText))
	for scope, name := range scopeText {
		m[name] = scope
	}
	return m
}()

// Intrinsics are the span-model fields TraceQL exposes as bare words, without a
// scope prefix. A query writes "duration > 100ms", not "span.duration > 100ms".
//
// They are listed rather than inferred because the same word is a perfectly good
// user attribute name: "span.status" is an attribute, while a bare "status" is
// the span's own status field, and only this list tells them apart.
var Intrinsics = map[string]bool{
	"duration":        true,
	"name":            true,
	"status":          true,
	"kind":            true,
	"statusMessage":   true,
	"rootName":        true,
	"rootServiceName": true,
	"traceDuration":   true,
}

// IntrinsicNames returns the intrinsic field names, sorted.
func IntrinsicNames() []string {
	names := make([]string, 0, len(Intrinsics))
	for name := range Intrinsics {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Attribute names one field a predicate addresses, together with the scope that
// says where to look for it.
type Attribute struct {
	Scope Scope
	// Name is the attribute path with its scope prefix removed, dots included:
	// "http.status_code" for span.http.status_code.
	Name string
	// Explicit records that the scope was written out. It distinguishes a bare
	// intrinsic ("duration") from one spelled with its prefix
	// ("intrinsic.duration"), so String reproduces the form the query used.
	Explicit bool
}

func (a *Attribute) String() string {
	switch {
	case a.Explicit && a.Scope != ScopeNone:
		return a.Scope.String() + "." + a.Name
	case a.Scope == ScopeIntrinsic:
		// A bare intrinsic is written without any prefix.
		return a.Name
	case a.Scope == ScopeNone:
		// An unscoped attribute keeps TraceQL's leading dot, which is what
		// distinguishes it from an intrinsic.
		return "." + a.Name
	default:
		return a.Scope.String() + "." + a.Name
	}
}

// Qualified returns the attribute as one flat key carrying its own scope —
// "span.http.status_code", "duration" — which is the form the IR stores. See
// ir.SpansetSelector for why the scope travels in the key rather than beside it.
func (a *Attribute) Qualified() string {
	if a.Scope == ScopeIntrinsic || a.Scope == ScopeNone {
		return a.Name
	}
	return a.Scope.String() + "." + a.Name
}

// CompareOp is a comparison operator inside a span set's braces.
type CompareOp int

const (
	OpEqual CompareOp = iota
	OpNotEqual
	OpLess
	OpGreater
	OpLessEqual
	OpGreaterEqual
	OpRegex
	OpNotRegex
)

var compareOpText = map[CompareOp]string{
	OpEqual:        "=",
	OpNotEqual:     "!=",
	OpLess:         "<",
	OpGreater:      ">",
	OpLessEqual:    "<=",
	OpGreaterEqual: ">=",
	OpRegex:        "=~",
	OpNotRegex:     "!~",
}

func (o CompareOp) String() string {
	if s, ok := compareOpText[o]; ok {
		return s
	}
	return fmt.Sprintf("CompareOp(%d)", int(o))
}

// ValueKind is the type of a comparison's right-hand side.
type ValueKind int

const (
	ValueString ValueKind = iota
	ValueNumber
	ValueDuration
	ValueBool
	// ValueStatus is one of TraceQL's bare status words — ok, error, unset —
	// which are written without quotes.
	ValueStatus
	// ValueKind is one of the bare span-kind words: server, client, producer,
	// consumer, internal.
	ValueSpanKind
)

func (k ValueKind) String() string {
	switch k {
	case ValueNumber:
		return "number"
	case ValueDuration:
		return "duration"
	case ValueBool:
		return "boolean"
	case ValueStatus:
		return "status"
	case ValueSpanKind:
		return "kind"
	default:
		return "string"
	}
}

// Value is a comparison's operand. Kind selects which field carries it; Text is
// the source spelling, which String renders so that "100ms" survives translation
// as written rather than being re-derived as "0.1s".
type Value struct {
	Kind     ValueKind
	Text     string
	Str      string
	Number   float64
	Duration time.Duration
	Bool     bool
}

func (v *Value) String() string {
	if v.Kind == ValueString {
		return strconv.Quote(v.Str)
	}
	return v.Text
}

// FieldExpr is the boolean expression between a span set's braces.
//
// It is a tree rather than a list because TraceQL composes predicates with || as
// well as &&, and a flat list can only mean conjunction.
type FieldExpr interface {
	ast.Node
	fieldExprNode()
}

// SpansetFilter is a leaf comparison against one attribute.
type SpansetFilter struct {
	traceqlNode
	Attribute *Attribute
	Op        CompareOp
	Value     *Value
}

func (*SpansetFilter) fieldExprNode() {}

// String writes the comparison spaced, which is TraceQL's documented idiom
// throughout — "span.http.status_code = 500", not "span.http.status_code=500".
func (e *SpansetFilter) String() string {
	return e.Attribute.String() + " " + e.Op.String() + " " + e.Value.String()
}

// BoolOp joins two field expressions.
type BoolOp int

const (
	BoolAnd BoolOp = iota
	BoolOr
)

func (o BoolOp) String() string {
	if o == BoolOr {
		return "||"
	}
	return "&&"
}

// FieldBinary joins two field expressions with && or ||.
type FieldBinary struct {
	traceqlNode
	Op       BoolOp
	LHS, RHS FieldExpr
}

func (*FieldBinary) fieldExprNode() {}
func (e *FieldBinary) String() string {
	return e.LHS.String() + " " + e.Op.String() + " " + e.RHS.String()
}

// FieldNot negates a field expression.
type FieldNot struct {
	traceqlNode
	Inner FieldExpr
}

func (*FieldNot) fieldExprNode()   {}
func (e *FieldNot) String() string { return "!" + e.Inner.String() }

// FieldParen preserves explicit grouping, so String reproduces the user's
// parentheses rather than re-deriving them from precedence.
type FieldParen struct {
	traceqlNode
	Inner FieldExpr
}

func (*FieldParen) fieldExprNode()   {}
func (e *FieldParen) String() string { return "(" + e.Inner.String() + ")" }

// Spanset is a span set selector: the braces and the boolean expression inside
// them. A nil Filter is the empty selector "{}", which matches every span.
type Spanset struct {
	traceqlNode
	Filter FieldExpr
}

func (*Spanset) exprNode() {}
func (e *Spanset) String() string {
	if e.Filter == nil {
		return "{}"
	}
	return "{ " + e.Filter.String() + " }"
}

// StructuralOp relates two span sets by their position in the trace tree.
type StructuralOp int

const (
	// StructChild is ">": spans whose direct parent is in the left set.
	StructChild StructuralOp = iota
	// StructDescendant is ">>": spans anywhere below the left set.
	StructDescendant
	// StructSibling is "~": spans sharing a parent with the left set.
	StructSibling
)

var structuralOpText = map[StructuralOp]string{
	StructChild:      ">",
	StructDescendant: ">>",
	StructSibling:    "~",
}

func (o StructuralOp) String() string {
	if s, ok := structuralOpText[o]; ok {
		return s
	}
	return fmt.Sprintf("StructuralOp(%d)", int(o))
}

// StructuralExpr relates two span sets.
//
// The child and descendant operators are genuinely different, not one operator
// written two ways: ">" is the direct parent-child edge and ">>" is its
// transitive closure. Collapsing them would widen every child query into a
// descendant one.
type StructuralExpr struct {
	traceqlNode
	Op       StructuralOp
	LHS, RHS Expr
}

func (*StructuralExpr) exprNode() {}
func (e *StructuralExpr) String() string {
	return e.LHS.String() + " " + e.Op.String() + " " + e.RHS.String()
}

// AggregateOp is a metric extraction over a span set.
type AggregateOp int

const (
	AggCount AggregateOp = iota
	AggSum
	AggAvg
	AggMin
	AggMax
)

var aggregateOpText = map[AggregateOp]string{
	AggCount: "count",
	AggSum:   "sum",
	AggAvg:   "avg",
	AggMin:   "min",
	AggMax:   "max",
}

var aggregateOpsByName = func() map[string]AggregateOp {
	m := make(map[string]AggregateOp, len(aggregateOpText))
	for op, name := range aggregateOpText {
		m[name] = op
	}
	return m
}()

func (o AggregateOp) String() string {
	if s, ok := aggregateOpText[o]; ok {
		return s
	}
	return fmt.Sprintf("AggregateOp(%d)", int(o))
}

// TakesAttribute reports whether the operator aggregates an attribute's values
// rather than counting whole spans. count() takes nothing; sum(), avg(), min()
// and max() need to know what they are adding up.
func (o AggregateOp) TakesAttribute() bool { return o != AggCount }

// Grouping is a by clause on an aggregate.
type Grouping struct {
	Attributes []*Attribute
}

func (g *Grouping) String() string {
	parts := make([]string, 0, len(g.Attributes))
	for _, a := range g.Attributes {
		parts = append(parts, a.String())
	}
	return "by (" + strings.Join(parts, ", ") + ")"
}

// AggregateExpr extracts a number from a span set.
//
// TraceQL writes this prefix — count() over ({...}) — where PromQL and LogQL
// both wrap their operand in a call. There is no window: the range comes from
// the request rather than from the query, which is why nothing here carries one.
type AggregateExpr struct {
	traceqlNode
	Op AggregateOp
	// Attribute is what sum, avg, min and max aggregate. It is nil for count,
	// which counts spans rather than values.
	Attribute *Attribute
	Operand   Expr
	Grouping  *Grouping
}

func (*AggregateExpr) exprNode() {}
func (e *AggregateExpr) String() string {
	var b strings.Builder
	b.WriteString(e.Op.String() + "(")
	if e.Attribute != nil {
		b.WriteString(e.Attribute.String())
	}
	b.WriteString(") over (" + e.Operand.String() + ")")
	if e.Grouping != nil {
		b.WriteString(" " + e.Grouping.String())
	}
	return b.String()
}

// CoercionType is the type an "as" cast reinterprets an attribute into.
type CoercionType int

const (
	CoerceInt CoercionType = iota
	CoerceFloat
	CoerceString
	CoerceDuration
	CoerceBool
)

var coercionTypeText = map[CoercionType]string{
	CoerceInt:      "int",
	CoerceFloat:    "float",
	CoerceString:   "string",
	CoerceDuration: "duration",
	CoerceBool:     "bool",
}

var coercionTypesByName = func() map[string]CoercionType {
	m := make(map[string]CoercionType, len(coercionTypeText))
	for t, name := range coercionTypeText {
		m[name] = t
	}
	return m
}()

func (t CoercionType) String() string {
	if s, ok := coercionTypeText[t]; ok {
		return s
	}
	return fmt.Sprintf("CoercionType(%d)", int(t))
}

// CoercionTypeNames returns the cast target names, sorted.
func CoercionTypeNames() []string {
	names := make([]string, 0, len(coercionTypeText))
	for _, name := range coercionTypeText {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AttributeCoercion reinterprets an attribute's type for the expression it
// wraps.
//
// Span attributes arrive as text, so comparing one numerically means saying so.
// The cast applies to the whole span set rather than to a single comparison,
// which is why it wraps an expression instead of sitting on a filter.
type AttributeCoercion struct {
	traceqlNode
	Expr      Expr
	Attribute *Attribute
	AsType    CoercionType
}

func (*AttributeCoercion) exprNode() {}
func (e *AttributeCoercion) String() string {
	return e.Expr.String() + " as (" + e.Attribute.String() + ": " + e.AsType.String() + ")"
}

// ParenExpr is an explicitly parenthesised span set expression, preserved so
// that String reproduces the user's grouping.
type ParenExpr struct {
	traceqlNode
	Inner Expr
}

func (*ParenExpr) exprNode()        {}
func (e *ParenExpr) String() string { return "(" + e.Inner.String() + ")" }

// FunctionNames returns every name the parser accepts as a function, which is
// the aggregate operators. It gives the language registry a way to check its own
// coverage against what the parser will actually produce.
func FunctionNames() []string {
	names := make([]string, 0, len(aggregateOpText))
	for _, name := range aggregateOpText {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LookupAggregate resolves an aggregate operator by name.
func LookupAggregate(name string) (AggregateOp, bool) {
	op, ok := aggregateOpsByName[strings.ToLower(name)]
	return op, ok
}
