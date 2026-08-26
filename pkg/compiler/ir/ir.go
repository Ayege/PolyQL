package ir

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Node is implemented by every node in the TelemetryIR tree. Because each node
// embeds IRNode, every node carries translatability state that the validator can
// set and the emitter and fidelity reporter can read.
type Node interface {
	// Base returns the embedded IRNode holding this node's translatability state.
	Base() *IRNode
	// String renders a compact, readable form of the node for debugging.
	String() string
}

// IRNode is the state embedded in every IR node. The validator annotates nodes
// with a flag and a human-readable reason describing what a given target DSL
// loses; the fidelity reporter walks the tree and rolls those annotations up.
//
// The zero value is {FULL, ""}: nodes are translatable until proven otherwise.
type IRNode struct {
	Flag   TranslatabilityFlag `json:"flag"`
	Reason string              `json:"reason,omitempty"`
}

// Base satisfies Node for every type embedding IRNode.
func (n *IRNode) Base() *IRNode { return n }

// SetTranslatability records how well this node survives translation into the
// target DSL under consideration.
func (n *IRNode) SetTranslatability(flag TranslatabilityFlag, reason string) {
	n.Flag = flag
	n.Reason = reason
}

// Annotate is SetTranslatability with printf-style formatting of the reason.
func (n *IRNode) Annotate(flag TranslatabilityFlag, format string, args ...any) {
	n.SetTranslatability(flag, fmt.Sprintf(format, args...))
}

// Translatability returns the node's flag and reason.
func (n *IRNode) Translatability() (TranslatabilityFlag, string) { return n.Flag, n.Reason }

// ResetTranslatability returns the node to the FULL default. Flags are relative
// to a particular target DSL, so a pipeline that emits into several targets
// clears them between passes.
func (n *IRNode) ResetTranslatability() { n.Flag, n.Reason = TranslatabilityFull, "" }

// SignalType is the class of telemetry a query reads. QLS §Selection > Data
// Source requires that a single data source hold exactly one of these.
type SignalType int

const (
	SignalMetric SignalType = iota
	SignalLog
	SignalSpan
	SignalProfile
)

var signalTypeEnum = newEnumDef[SignalType]("SignalType", "METRIC", "LOG", "SPAN", "PROFILE")

func (s SignalType) String() string { return signalTypeEnum.String(s) }

// ParseSignalType resolves a signal type symbol, case-insensitively.
func ParseSignalType(s string) (SignalType, error) { return signalTypeEnum.Parse(s) }

func (s SignalType) MarshalJSON() ([]byte, error)  { return signalTypeEnum.marshalJSON(s) }
func (s *SignalType) UnmarshalJSON(b []byte) error { return signalTypeEnum.unmarshalJSON(b, s) }

// Query is the root of the IR tree: one translatable query.
type Query struct {
	IRNode
	// Signal is the telemetry class being read.
	Signal SignalType `json:"signal"`
	// Source names the data being selected from. QLS §Selection > Data Source
	// allows it to be omitted when a store implies it (Prometheus, for
	// instance, exposes a single implicit metric view).
	Source *DataSource `json:"source,omitempty"`
	// Pipeline holds the ordered stages applied to the source.
	Pipeline Pipeline `json:"pipeline,omitempty"`
	// Output describes the shape of the result set.
	Output *Output `json:"output,omitempty"`
	// Hints carries DSL-specific detail that has no home in the QLS model but
	// which an emitter may want back, such as a Prometheus metric's original
	// type. Hints never change IR semantics; anything load-bearing belongs in a
	// typed field.
	Hints map[string]string `json:"hints,omitempty"`
}

func (q *Query) String() string {
	var b strings.Builder
	b.WriteString(q.Signal.String())
	if q.Source != nil {
		b.WriteString(" ")
		b.WriteString(q.Source.String())
	}
	for _, stage := range q.Pipeline {
		b.WriteString(" | ")
		b.WriteString(stage.String())
	}
	if q.Output != nil {
		if out := q.Output.String(); out != "" {
			b.WriteString(" ")
			b.WriteString(out)
		}
	}
	return b.String()
}

// Scope qualifies which part of a record an attribute lookup addresses.
// TraceQL's resource/span/intrinsic scoping is the clearest expression of this;
// PromQL and LogQL are effectively unscoped.
type Scope int

const (
	// ScopeUnscoped leaves resolution to the backend, which is the behavior of
	// DSLs with a single flat label namespace. It is deliberately the zero
	// value: a node built without an explicit scope must not silently claim to
	// be resource-scoped, since that would change which attributes a TraceQL
	// emitter addresses.
	ScopeUnscoped Scope = iota
	// ScopeResource addresses resource-level attributes shared by every record
	// a resource emits.
	ScopeResource
	// ScopeSpan addresses attributes on the individual span.
	ScopeSpan
	// ScopeIntrinsic addresses fields of the telemetry model itself (duration,
	// name, status) rather than user attributes.
	ScopeIntrinsic
)

var scopeEnum = newEnumDef[Scope]("Scope", "UNSCOPED", "RESOURCE", "SPAN", "INTRINSIC")

func (s Scope) String() string { return scopeEnum.String(s) }

// ParseScope resolves a scope symbol, case-insensitively.
func ParseScope(s string) (Scope, error) { return scopeEnum.Parse(s) }

func (s Scope) MarshalJSON() ([]byte, error)  { return scopeEnum.marshalJSON(s) }
func (s *Scope) UnmarshalJSON(b []byte) error { return scopeEnum.unmarshalJSON(b, s) }

// DataSource is the QLS §Selection > Data Source FROM-clause equivalent: the
// metric name, log stream or span set being read, plus the selectors narrowing
// it. Names MAY be fully qualified, which is what makes federation across
// backends expressible.
type DataSource struct {
	IRNode
	Name      string      `json:"name"`
	Scope     Scope       `json:"scope"`
	Selectors []*Selector `json:"selectors,omitempty"`
	// Spanset holds a boolean filter tree over span attributes, for a source
	// selected the way TraceQL selects one. See SpansetSelector for why a
	// Selector cannot stand in for it.
	Spanset *SpansetSelector `json:"spanset,omitempty"`
}

func (d *DataSource) String() string {
	var b strings.Builder
	if d.Scope != ScopeUnscoped {
		b.WriteString(strings.ToLower(d.Scope.String()))
		b.WriteString(":")
	}
	b.WriteString(d.Name)
	for _, s := range d.Selectors {
		b.WriteString(s.String())
	}
	if d.Spanset != nil {
		b.WriteString(d.Spanset.String())
	}
	return b.String()
}

// SpansetSelector identifies spans by a boolean expression over their
// intrinsic, resource and span-scope attributes — QLS §.5 Spans read through
// §Selection > Predicates.
//
// It sits beside DataSource.Selectors rather than replacing them because the two
// express genuinely different things. A Selector's matchers are conjunctive:
// every one must hold, which is all PromQL and LogQL can write between their
// braces. TraceQL's braces hold a full boolean expression, so
// {span.http.status_code = 500 || span.error = true} has no faithful Selector
// form — flattening it to a conjunction would silently change which spans match,
// and lowering it to a regex is not available either, since the operands address
// different attributes.
//
// Scope is not tracked per matcher. An attribute key carries its own prefix —
// "span.http.status_code", "resource.service.name", or a bare intrinsic such as
// "duration" — which keeps the IR flat and keeps a matcher comparable across
// DSLs that have no scoping at all.
type SpansetSelector struct {
	IRNode
	// Filters is the predicate every selected span must satisfy. A nil Filters
	// is the empty spanset "{}", which selects every span.
	Filters Predicate `json:"filters,omitempty"`
}

func (s *SpansetSelector) String() string {
	if s.Filters == nil {
		return "{}"
	}
	return "{" + s.Filters.String() + "}"
}

func (s *SpansetSelector) UnmarshalJSON(data []byte) error {
	type alias SpansetSelector
	aux := struct {
		Filters json.RawMessage `json:"filters"`
		*alias
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	p, err := unmarshalPredicate(aux.Filters)
	if err != nil {
		return err
	}
	s.Filters = p
	return nil
}

// Selector is one set of label matchers applied to a data source. Multiple
// selectors on a source are independent alternatives; matchers within one
// selector all apply.
type Selector struct {
	IRNode
	Matchers []*LabelMatcher `json:"matchers,omitempty"`
}

func (s *Selector) String() string {
	parts := make([]string, 0, len(s.Matchers))
	for _, m := range s.Matchers {
		parts = append(parts, m.String())
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// MatchOp is a QLS §Selection > Predicates comparison operator.
//
// QLS §.2 requires that predicands match case-sensitively by default, so EQ is a
// case-sensitive comparison; a DSL whose default differs must be normalised by
// its resolver rather than by changing the meaning of EQ here.
type MatchOp int

const (
	// MatchEQ and MatchNEQ are the comparison predicates of QLS §.2.1.
	MatchEQ MatchOp = iota
	MatchNEQ
	// MatchRegex and MatchNotRegex are the similar predicate of QLS §.2.4,
	// which mandates a subset of POSIX regular expression syntax.
	MatchRegex
	MatchNotRegex
	// MatchGT through MatchLTE complete the ordered comparisons of QLS §.2.1.
	MatchGT
	MatchGTE
	MatchLT
	MatchLTE
	// MatchIn and MatchNotIn are the in predicate of QLS §.2.2 and read
	// LabelMatcher.Values rather than Value.
	MatchIn
	MatchNotIn
	// MatchIsNull and MatchIsNotNull are the NULL predicate of QLS §.2.3 and
	// take no operand. QLS §Aggregation makes NULL, not NaN, the sentinel for
	// absent data, so these are the correct way to test for missing values.
	MatchIsNull
	MatchIsNotNull
	// MatchContains and MatchNotContains test for a substring: QLS §.2.4's
	// similar predicate in its simplest form, where the operand is literal text
	// rather than a pattern.
	//
	// They fill the gap between EQ, which demands the whole value match, and
	// REGEX, which reads its operand as a pattern. Lowering containment to a
	// regex is not free — every metacharacter in the operand then means
	// something else, so "error.log" would also match "errorXlog" — which is
	// why the IR names containment outright instead of approximating it.
	MatchContains
	MatchNotContains
)

var matchOpEnum = newEnumDef[MatchOp]("MatchOp",
	"EQ", "NEQ", "REGEX", "NOT_REGEX",
	"GT", "GTE", "LT", "LTE",
	"IN", "NOT_IN",
	"IS_NULL", "IS_NOT_NULL",
	"CONTAINS", "NOT_CONTAINS",
)

func (o MatchOp) String() string { return matchOpEnum.String(o) }

// ParseMatchOp resolves a match operator symbol, case-insensitively.
func ParseMatchOp(s string) (MatchOp, error) { return matchOpEnum.Parse(s) }

func (o MatchOp) MarshalJSON() ([]byte, error)  { return matchOpEnum.marshalJSON(o) }
func (o *MatchOp) UnmarshalJSON(b []byte) error { return matchOpEnum.unmarshalJSON(b, o) }

var matchOpSymbols = map[MatchOp]string{
	MatchEQ: "=", MatchNEQ: "!=", MatchRegex: "=~", MatchNotRegex: "!~",
	MatchGT: ">", MatchGTE: ">=", MatchLT: "<", MatchLTE: "<=",
	MatchIn: "IN", MatchNotIn: "NOT IN",
	MatchIsNull: "IS NULL", MatchIsNotNull: "IS NOT NULL",
	MatchContains: "CONTAINS", MatchNotContains: "NOT CONTAINS",
}

// Symbol returns the operator's conventional written form, for debug rendering.
// Emitters render their own syntax from the registry rather than using this.
func (o MatchOp) Symbol() string {
	if s, ok := matchOpSymbols[o]; ok {
		return s
	}
	return o.String()
}

// IsUnary reports whether the operator takes no right-hand operand.
func (o MatchOp) IsUnary() bool { return o == MatchIsNull || o == MatchIsNotNull }

// IsSetOp reports whether the operator reads LabelMatcher.Values instead of
// LabelMatcher.Value.
func (o MatchOp) IsSetOp() bool { return o == MatchIn || o == MatchNotIn }

// IsContainment reports whether the operator tests for a literal substring
// rather than a whole-value match or a pattern.
func (o MatchOp) IsContainment() bool { return o == MatchContains || o == MatchNotContains }

// LabelMatcher is a single predicate over one attribute key.
//
// Value carries the operand for the scalar operators. The set operators IN and
// NOT_IN read Values instead, and the NULL operators read neither.
type LabelMatcher struct {
	IRNode
	Key    string   `json:"key"`
	Op     MatchOp  `json:"op"`
	Value  string   `json:"value,omitempty"`
	Values []string `json:"values,omitempty"`
}

func (m *LabelMatcher) String() string {
	switch {
	case m.Op.IsUnary():
		return fmt.Sprintf("%s %s", m.Key, m.Op.Symbol())
	case m.Op.IsSetOp():
		quoted := make([]string, 0, len(m.Values))
		for _, v := range m.Values {
			quoted = append(quoted, strconv.Quote(v))
		}
		return fmt.Sprintf("%s %s (%s)", m.Key, m.Op.Symbol(), strings.Join(quoted, ", "))
	default:
		return fmt.Sprintf("%s%s%s", m.Key, m.Op.Symbol(), strconv.Quote(m.Value))
	}
}

// Predicate is a filter expression over telemetry records, following QLS
// §Selection > Predicates. It is a tree so that DSLs supporting boolean
// composition survive translation intact.
type Predicate interface {
	Node
	// PredicateKind identifies the variant, and is the discriminator a
	// serialized predicate carries.
	PredicateKind() PredicateKind
	predicateNode()
}

// PredicateKind discriminates the Predicate implementations.
type PredicateKind int

const (
	PredicateKindMatch PredicateKind = iota
	PredicateKindLogical
)

var predicateKindEnum = newEnumDef[PredicateKind]("PredicateKind", "MATCH", "LOGICAL")

func (k PredicateKind) String() string { return predicateKindEnum.String(k) }

// ParsePredicateKind resolves a predicate kind symbol, case-insensitively.
func ParsePredicateKind(s string) (PredicateKind, error) { return predicateKindEnum.Parse(s) }

func (k PredicateKind) MarshalJSON() ([]byte, error) { return predicateKindEnum.marshalJSON(k) }
func (k *PredicateKind) UnmarshalJSON(b []byte) error {
	return predicateKindEnum.unmarshalJSON(b, k)
}

// MatchPredicate is a leaf predicate wrapping a single label matcher.
type MatchPredicate struct {
	IRNode
	Matcher *LabelMatcher `json:"matcher"`
}

func (p *MatchPredicate) PredicateKind() PredicateKind { return PredicateKindMatch }
func (p *MatchPredicate) predicateNode()               {}
func (p *MatchPredicate) String() string {
	if p.Matcher == nil {
		return "<empty match>"
	}
	return p.Matcher.String()
}

func (p *MatchPredicate) MarshalJSON() ([]byte, error) {
	type alias MatchPredicate
	return json.Marshal(struct {
		Kind PredicateKind `json:"kind"`
		*alias
	}{Kind: p.PredicateKind(), alias: (*alias)(p)})
}

// LogicalOp composes predicates. NOT applies to a single operand; AND and OR
// take two or more.
type LogicalOp int

const (
	LogicalAnd LogicalOp = iota
	LogicalOr
	LogicalNot
)

var logicalOpEnum = newEnumDef[LogicalOp]("LogicalOp", "AND", "OR", "NOT")

func (o LogicalOp) String() string { return logicalOpEnum.String(o) }

// ParseLogicalOp resolves a logical operator symbol, case-insensitively.
func ParseLogicalOp(s string) (LogicalOp, error) { return logicalOpEnum.Parse(s) }

func (o LogicalOp) MarshalJSON() ([]byte, error)  { return logicalOpEnum.marshalJSON(o) }
func (o *LogicalOp) UnmarshalJSON(b []byte) error { return logicalOpEnum.unmarshalJSON(b, o) }

// LogicalPredicate composes operand predicates with a boolean operator.
type LogicalPredicate struct {
	IRNode
	Op       LogicalOp   `json:"op"`
	Operands []Predicate `json:"operands,omitempty"`
}

func (p *LogicalPredicate) PredicateKind() PredicateKind { return PredicateKindLogical }
func (p *LogicalPredicate) predicateNode()               {}
func (p *LogicalPredicate) String() string {
	parts := make([]string, 0, len(p.Operands))
	for _, o := range p.Operands {
		parts = append(parts, o.String())
	}
	if p.Op == LogicalNot {
		return "NOT (" + strings.Join(parts, ", ") + ")"
	}
	return "(" + strings.Join(parts, " "+p.Op.String()+" ") + ")"
}

func (p *LogicalPredicate) MarshalJSON() ([]byte, error) {
	type alias LogicalPredicate
	return json.Marshal(struct {
		Kind PredicateKind `json:"kind"`
		*alias
	}{Kind: p.PredicateKind(), alias: (*alias)(p)})
}

func (p *LogicalPredicate) UnmarshalJSON(data []byte) error {
	type alias LogicalPredicate
	aux := struct {
		Operands []json.RawMessage `json:"operands"`
		*alias
	}{alias: (*alias)(p)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	p.Operands = nil
	for _, raw := range aux.Operands {
		operand, err := unmarshalPredicate(raw)
		if err != nil {
			return err
		}
		p.Operands = append(p.Operands, operand)
	}
	return nil
}

func unmarshalPredicate(raw []byte) (Predicate, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var disc struct {
		Kind *PredicateKind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &disc); err != nil {
		return nil, fmt.Errorf("ir: predicate has an invalid \"kind\": %w", err)
	}
	if disc.Kind == nil {
		return nil, fmt.Errorf("ir: predicate is missing its \"kind\" discriminator")
	}
	var p Predicate
	switch *disc.Kind {
	case PredicateKindMatch:
		p = &MatchPredicate{}
	case PredicateKindLogical:
		p = &LogicalPredicate{}
	default:
		return nil, fmt.Errorf("ir: unknown predicate kind %s", *disc.Kind)
	}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	return p, nil
}

// IRExpr is a value expression usable as a function or aggregation argument.
type IRExpr interface {
	Node
	// ExprKind identifies the variant, and is the discriminator a serialized
	// expression carries.
	ExprKind() ExprKind
	exprNode()
}

// ExprKind discriminates the IRExpr implementations.
type ExprKind int

const (
	ExprKindLiteral ExprKind = iota
	ExprKindRef
	ExprKindQuery
)

var exprKindEnum = newEnumDef[ExprKind]("ExprKind", "LITERAL", "REF", "QUERY")

func (k ExprKind) String() string { return exprKindEnum.String(k) }

// ParseExprKind resolves an expression kind symbol, case-insensitively.
func ParseExprKind(s string) (ExprKind, error) { return exprKindEnum.Parse(s) }

func (k ExprKind) MarshalJSON() ([]byte, error)  { return exprKindEnum.marshalJSON(k) }
func (k *ExprKind) UnmarshalJSON(b []byte) error { return exprKindEnum.unmarshalJSON(b, k) }

// LiteralExpr is a constant value. Type is authoritative for interpreting Value;
// a nil Value is the QLS NULL, which §Aggregation makes the sentinel for absent
// data. Translating PromQL's NaN-as-absent convention into a NULL literal here,
// and back on emission, is the resolver's job.
type LiteralExpr struct {
	IRNode
	Type  QlsDataType `json:"type"`
	Value any         `json:"value"`
}

func (e *LiteralExpr) ExprKind() ExprKind { return ExprKindLiteral }
func (e *LiteralExpr) exprNode()          {}
func (e *LiteralExpr) String() string {
	if e.Value == nil {
		return "NULL"
	}
	if e.Type == DataTypeString || e.Type == DataTypeBinaryString {
		return fmt.Sprintf("%q", e.Value)
	}
	return fmt.Sprintf("%v", e.Value)
}

func (e *LiteralExpr) MarshalJSON() ([]byte, error) {
	type alias LiteralExpr
	return json.Marshal(struct {
		Kind ExprKind `json:"kind"`
		*alias
	}{Kind: e.ExprKind(), alias: (*alias)(e)})
}

// NewNumberLiteral builds a DOUBLE literal.
func NewNumberLiteral(v float64) *LiteralExpr {
	return &LiteralExpr{Type: DataTypeDouble, Value: v}
}

// NewStringLiteral builds a STRING literal.
func NewStringLiteral(v string) *LiteralExpr {
	return &LiteralExpr{Type: DataTypeString, Value: v}
}

// NewBoolLiteral builds a BOOLEAN literal.
func NewBoolLiteral(v bool) *LiteralExpr {
	return &LiteralExpr{Type: DataTypeBoolean, Value: v}
}

// NewIntervalLiteral builds an INTERVAL literal, the form a range selector such
// as PromQL's [5m] resolves to.
func NewIntervalLiteral(v Interval) *LiteralExpr {
	return &LiteralExpr{Type: DataTypeInterval, Value: v}
}

// NewNullLiteral builds a typed NULL, the QLS sentinel for absent data.
func NewNullLiteral(t QlsDataType) *LiteralExpr {
	return &LiteralExpr{Type: t, Value: nil}
}

// RefExpr references an attribute key or an intrinsic model field by name.
type RefExpr struct {
	IRNode
	Name  string      `json:"name"`
	Scope Scope       `json:"scope"`
	Type  QlsDataType `json:"type"`
}

func (e *RefExpr) ExprKind() ExprKind { return ExprKindRef }
func (e *RefExpr) exprNode()          {}
func (e *RefExpr) String() string {
	if e.Scope != ScopeUnscoped {
		return strings.ToLower(e.Scope.String()) + "." + e.Name
	}
	return e.Name
}

func (e *RefExpr) MarshalJSON() ([]byte, error) {
	type alias RefExpr
	return json.Marshal(struct {
		Kind ExprKind `json:"kind"`
		*alias
	}{Kind: e.ExprKind(), alias: (*alias)(e)})
}

// QueryExpr embeds a subquery as an argument, which is how nested calls such as
// histogram_quantile(0.99, sum(rate(...))) are represented.
type QueryExpr struct {
	IRNode
	Query *Query `json:"query"`
}

func (e *QueryExpr) ExprKind() ExprKind { return ExprKindQuery }
func (e *QueryExpr) exprNode()          {}
func (e *QueryExpr) String() string {
	if e.Query == nil {
		return "<empty query>"
	}
	return "(" + e.Query.String() + ")"
}

func (e *QueryExpr) MarshalJSON() ([]byte, error) {
	type alias QueryExpr
	return json.Marshal(struct {
		Kind ExprKind `json:"kind"`
		*alias
	}{Kind: e.ExprKind(), alias: (*alias)(e)})
}

func unmarshalExpr(raw []byte) (IRExpr, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var disc struct {
		Kind *ExprKind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &disc); err != nil {
		return nil, fmt.Errorf("ir: expression has an invalid \"kind\": %w", err)
	}
	if disc.Kind == nil {
		return nil, fmt.Errorf("ir: expression is missing its \"kind\" discriminator")
	}
	var e IRExpr
	switch *disc.Kind {
	case ExprKindLiteral:
		e = &LiteralExpr{}
	case ExprKindRef:
		e = &RefExpr{}
	case ExprKindQuery:
		e = &QueryExpr{}
	default:
		return nil, fmt.Errorf("ir: unknown expression kind %s", *disc.Kind)
	}
	if err := json.Unmarshal(raw, e); err != nil {
		return nil, err
	}
	return e, nil
}

// PipelineStage is one ordered transformation applied to a data source.
type PipelineStage interface {
	Node
	// StageKind identifies the variant, and is the discriminator a serialized
	// stage carries.
	StageKind() StageKind
	stageNode()
}

// StageKind discriminates the PipelineStage implementations.
type StageKind int

const (
	StageKindFilter StageKind = iota
	StageKindAggregation
	StageKindFunction
	StageKindJoin
	StageKindBinaryOp
	StageKindUnaryOp
	// StageKindStructural relates one span set to another by their position in
	// the trace tree.
	StageKindStructural
	// StageKindCoercion reinterprets an attribute's type.
	StageKindCoercion
)

var stageKindEnum = newEnumDef[StageKind]("StageKind",
	"FILTER", "AGGREGATION", "FUNCTION", "JOIN", "BINARY_OP", "UNARY_OP",
	"STRUCTURAL", "COERCION")

func (k StageKind) String() string { return stageKindEnum.String(k) }

// ParseStageKind resolves a stage kind symbol, case-insensitively.
func ParseStageKind(s string) (StageKind, error) { return stageKindEnum.Parse(s) }

func (k StageKind) MarshalJSON() ([]byte, error)  { return stageKindEnum.marshalJSON(k) }
func (k *StageKind) UnmarshalJSON(b []byte) error { return stageKindEnum.unmarshalJSON(b, k) }

// Pipeline is the ordered list of stages applied to a query's data source.
type Pipeline []PipelineStage

func (p Pipeline) String() string {
	parts := make([]string, 0, len(p))
	for _, s := range p {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, " | ")
}

// UnmarshalJSON reconstructs the concrete stage types from the "kind"
// discriminator each stage marshals.
func (p *Pipeline) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return fmt.Errorf("ir: pipeline must be a JSON array: %w", err)
	}
	stages := make(Pipeline, 0, len(raws))
	for _, raw := range raws {
		var disc struct {
			Kind *StageKind `json:"kind"`
		}
		if err := json.Unmarshal(raw, &disc); err != nil {
			return fmt.Errorf("ir: pipeline stage has an invalid \"kind\": %w", err)
		}
		if disc.Kind == nil {
			return fmt.Errorf("ir: pipeline stage is missing its \"kind\" discriminator")
		}
		var stage PipelineStage
		switch *disc.Kind {
		case StageKindFilter:
			stage = &FilterStage{}
		case StageKindAggregation:
			stage = &AggregationStage{}
		case StageKindFunction:
			stage = &FunctionStage{}
		case StageKindJoin:
			stage = &JoinStage{}
		case StageKindBinaryOp:
			stage = &BinaryOpStage{}
		case StageKindUnaryOp:
			stage = &UnaryOpStage{}
		case StageKindStructural:
			stage = &StructuralStage{}
		case StageKindCoercion:
			stage = &CoercionStage{}
		default:
			return fmt.Errorf("ir: unknown pipeline stage kind %s", *disc.Kind)
		}
		if err := json.Unmarshal(raw, stage); err != nil {
			return err
		}
		stages = append(stages, stage)
	}
	*p = stages
	return nil
}

// FilterStage narrows the record set with a QLS §Selection predicate.
type FilterStage struct {
	IRNode
	Predicate Predicate `json:"predicate"`
	// ReturnsBool marks a comparison that yields 0 or 1 for every record rather
	// than dropping the ones that fail it — PromQL's bool modifier.
	//
	// The two are genuinely different operations, and the IR has only the
	// filtering form, so this records which was written. A target without the
	// modifier can still emit the filter, and the validator says what changed.
	ReturnsBool bool `json:"returns_bool,omitempty"`
}

func (s *FilterStage) StageKind() StageKind { return StageKindFilter }
func (s *FilterStage) stageNode()           {}
func (s *FilterStage) String() string {
	body := "<empty>"
	if s.Predicate != nil {
		body = s.Predicate.String()
	}
	if s.ReturnsBool {
		return "filter(" + body + ") [bool]"
	}
	return "filter(" + body + ")"
}

func (s *FilterStage) MarshalJSON() ([]byte, error) {
	type alias FilterStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

func (s *FilterStage) UnmarshalJSON(data []byte) error {
	type alias FilterStage
	aux := struct {
		Predicate json.RawMessage `json:"predicate"`
		*alias
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	p, err := unmarshalPredicate(aux.Predicate)
	if err != nil {
		return err
	}
	s.Predicate = p
	return nil
}

// AggOp is an aggregation or range function the IR can express. QLS §Aggregation
// governs their NULL handling: absent data is NULL, never NaN, and addition and
// subtraction substitute 0 for NULL, multiplication 1, and division 0 in the
// divisor.
type AggOp int

const (
	AggSum AggOp = iota
	AggAvg
	AggMin
	AggMax
	AggCount
	AggCountDistinct
	AggStddev
	AggQuantile
	AggRate
	AggIncrease
	AggDelta
	AggIrate
	AggHistogramQuantile
	AggTopK
	AggBottomK
)

var aggOpEnum = newEnumDef[AggOp]("AggOp",
	"SUM", "AVG", "MIN", "MAX", "COUNT", "COUNT_DISTINCT", "STDDEV", "QUANTILE",
	"RATE", "INCREASE", "DELTA", "IRATE", "HISTOGRAM_QUANTILE", "TOPK", "BOTTOMK",
)

func (o AggOp) String() string { return aggOpEnum.String(o) }

// ParseAggOp resolves an aggregation operator symbol, case-insensitively.
func ParseAggOp(s string) (AggOp, error) { return aggOpEnum.Parse(s) }

func (o AggOp) MarshalJSON() ([]byte, error)  { return aggOpEnum.marshalJSON(o) }
func (o *AggOp) UnmarshalJSON(b []byte) error { return aggOpEnum.unmarshalJSON(b, o) }

// IsTemporalOnly reports whether the operator is meaningful only over a time
// window, which fixes its AggScope to TEMPORAL.
func (o AggOp) IsTemporalOnly() bool {
	switch o {
	case AggRate, AggIncrease, AggDelta, AggIrate:
		return true
	default:
		return false
	}
}

// RequiresParameter reports whether the operator needs an argument beyond the
// series it aggregates, such as the phi of a quantile or the k of a topk.
func (o AggOp) RequiresParameter() bool {
	switch o {
	case AggQuantile, AggHistogramQuantile, AggTopK, AggBottomK:
		return true
	default:
		return false
	}
}

// AggScope distinguishes the two axes an aggregation can collapse. The
// distinction is load-bearing for translation: PromQL writes temporal
// aggregation as a range function and group aggregation as an operator with
// by/without, and conflating them silently changes results.
type AggScope int

const (
	// AggScopeTemporal collapses points within one series over time, and feeds
	// the QLS metric model's temporal_aggregation field.
	AggScopeTemporal AggScope = iota
	// AggScopeGroup collapses across series that share attributes, and feeds the
	// QLS metric model's group_aggregation field.
	AggScopeGroup
)

var aggScopeEnum = newEnumDef[AggScope]("AggScope", "TEMPORAL", "GROUP")

func (s AggScope) String() string { return aggScopeEnum.String(s) }

// ParseAggScope resolves an aggregation scope symbol, case-insensitively.
func ParseAggScope(s string) (AggScope, error) { return aggScopeEnum.Parse(s) }

func (s AggScope) MarshalJSON() ([]byte, error)  { return aggScopeEnum.marshalJSON(s) }
func (s *AggScope) UnmarshalJSON(b []byte) error { return aggScopeEnum.unmarshalJSON(b, s) }

// AggregationStage collapses records along one of the two QLS aggregation axes.
//
// GroupBy and Without are mutually exclusive: GroupBy names the attributes to
// keep, Without names those to drop. QLS §Aggregation > Aggregating Attributes
// carries only grouped keys into the result set.
type AggregationStage struct {
	IRNode
	Op      AggOp    `json:"op"`
	GroupBy []string `json:"group_by,omitempty"`
	Without []string `json:"without,omitempty"`
	Scope   AggScope `json:"scope"`
	// Parameter holds the extra argument the operators reporting
	// RequiresParameter need, such as the 0.99 in quantile(0.99, ...).
	Parameter IRExpr `json:"parameter,omitempty"`
}

func (s *AggregationStage) StageKind() StageKind { return StageKindAggregation }
func (s *AggregationStage) stageNode()           {}
func (s *AggregationStage) String() string {
	var b strings.Builder
	b.WriteString(strings.ToLower(s.Op.String()))
	if s.Parameter != nil {
		b.WriteString("(" + s.Parameter.String() + ")")
	}
	if len(s.GroupBy) > 0 {
		b.WriteString(" by (" + strings.Join(s.GroupBy, ", ") + ")")
	}
	if len(s.Without) > 0 {
		b.WriteString(" without (" + strings.Join(s.Without, ", ") + ")")
	}
	b.WriteString(" [" + s.Scope.String() + "]")
	return b.String()
}

func (s *AggregationStage) MarshalJSON() ([]byte, error) {
	type alias AggregationStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

func (s *AggregationStage) UnmarshalJSON(data []byte) error {
	type alias AggregationStage
	aux := struct {
		Parameter json.RawMessage `json:"parameter"`
		*alias
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	e, err := unmarshalExpr(aux.Parameter)
	if err != nil {
		return err
	}
	s.Parameter = e
	return nil
}

// FunctionStage applies a named function. The name is the QLS-neutral name from
// the language registry, not the source DSL's spelling; the registry maps
// between them. ReturnType lets the validator type-check the pipeline without
// knowing the function's semantics.
type FunctionStage struct {
	IRNode
	Name       string      `json:"name"`
	Args       []IRExpr    `json:"args,omitempty"`
	ReturnType QlsDataType `json:"return_type"`
}

func (s *FunctionStage) StageKind() StageKind { return StageKindFunction }
func (s *FunctionStage) stageNode()           {}
func (s *FunctionStage) String() string {
	args := make([]string, 0, len(s.Args))
	for _, a := range s.Args {
		args = append(args, a.String())
	}
	return fmt.Sprintf("%s(%s) -> %s", s.Name, strings.Join(args, ", "), s.ReturnType)
}

func (s *FunctionStage) MarshalJSON() ([]byte, error) {
	type alias FunctionStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

func (s *FunctionStage) UnmarshalJSON(data []byte) error {
	type alias FunctionStage
	aux := struct {
		Args []json.RawMessage `json:"args"`
		*alias
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	s.Args = nil
	for _, raw := range aux.Args {
		e, err := unmarshalExpr(raw)
		if err != nil {
			return err
		}
		s.Args = append(s.Args, e)
	}
	return nil
}

// JoinType is a QLS §Joins join. Version 1 of the spec allows only equi-joins,
// and permits CROSS solely for scalar-to-set operations.
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeftOuter
	JoinRightOuter
	JoinFullOuter
	JoinCross
)

var joinTypeEnum = newEnumDef[JoinType]("JoinType",
	"INNER", "LEFT_OUTER", "RIGHT_OUTER", "FULL_OUTER", "CROSS")

func (t JoinType) String() string { return joinTypeEnum.String(t) }

// ParseJoinType resolves a join type symbol, case-insensitively.
func ParseJoinType(s string) (JoinType, error) { return joinTypeEnum.Parse(s) }

func (t JoinType) MarshalJSON() ([]byte, error)  { return joinTypeEnum.marshalJSON(t) }
func (t *JoinType) UnmarshalJSON(b []byte) error { return joinTypeEnum.unmarshalJSON(b, t) }

// JoinStage correlates the pipeline so far with a second query.
//
// QLS §Joins > Default Joins makes the default an inner equi-join on attributes
// for set-to-set operations, and a cross join for set-to-scalar. OnLabels names
// the join keys explicitly; IgnoreLabels excludes keys from an otherwise
// implicit join on all shared attributes.
type JoinStage struct {
	IRNode
	JoinType     JoinType `json:"join_type"`
	OnLabels     []string `json:"on_labels,omitempty"`
	IgnoreLabels []string `json:"ignore_labels,omitempty"`
	// IncludeLabels are the labels copied from the one side onto the result,
	// which PromQL writes as the argument of group_left or group_right. They
	// are meaningful only for the outer join types.
	IncludeLabels []string `json:"include_labels,omitempty"`
	RightSide     *Query   `json:"right_side,omitempty"`
}

func (s *JoinStage) StageKind() StageKind { return StageKindJoin }
func (s *JoinStage) stageNode()           {}
func (s *JoinStage) String() string {
	var b strings.Builder
	b.WriteString("join " + s.JoinType.String())
	if len(s.OnLabels) > 0 {
		b.WriteString(" on (" + strings.Join(s.OnLabels, ", ") + ")")
	}
	if len(s.IgnoreLabels) > 0 {
		b.WriteString(" ignoring (" + strings.Join(s.IgnoreLabels, ", ") + ")")
	}
	if len(s.IncludeLabels) > 0 {
		b.WriteString(" include (" + strings.Join(s.IncludeLabels, ", ") + ")")
	}
	if s.RightSide != nil {
		b.WriteString(" with (" + s.RightSide.String() + ")")
	}
	return b.String()
}

func (s *JoinStage) MarshalJSON() ([]byte, error) {
	type alias JoinStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

// BinaryOpStage applies an operator to two result sets.
//
// Left and Right hold the operands. They are nil when the operator completes a
// preceding JoinStage instead: PromQL writes vector matching as
// "a / on (job) b", so the join stage supplies the right operand and the
// matching clause, and this stage supplies only the operator that goes between
// them. Anywhere else both are set.
type BinaryOpStage struct {
	IRNode
	Op    ArithOp `json:"op"`
	Left  *Query  `json:"left,omitempty"`
	Right *Query  `json:"right,omitempty"`
}

func (s *BinaryOpStage) StageKind() StageKind { return StageKindBinaryOp }
func (s *BinaryOpStage) stageNode()           {}

func (s *BinaryOpStage) String() string {
	if s.Left == nil || s.Right == nil {
		// The operands live in the surrounding pipeline and its join stage.
		return "binary " + s.Op.String()
	}
	return "(" + s.Left.String() + " " + s.Op.String() + " " + s.Right.String() + ")"
}

func (s *BinaryOpStage) MarshalJSON() ([]byte, error) {
	type alias BinaryOpStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

// UnaryOpStage applies a leading sign to a result set.
//
// It is a node rather than a function stage carrying its sign as text, for the
// same reason BinaryOpStage is: a misspelling in a string compiles and
// mistranslates silently, while a wrong ArithOp does not compile at all.
type UnaryOpStage struct {
	IRNode
	Op      ArithOp `json:"op"`
	Operand *Query  `json:"operand,omitempty"`
}

func (s *UnaryOpStage) StageKind() StageKind { return StageKindUnaryOp }
func (s *UnaryOpStage) stageNode()           {}

func (s *UnaryOpStage) String() string {
	if s.Operand == nil {
		// The operand is the pipeline this stage sits in.
		return "unary " + s.Op.String()
	}
	return "(" + s.Op.String() + " " + s.Operand.String() + ")"
}

func (s *UnaryOpStage) MarshalJSON() ([]byte, error) {
	type alias UnaryOpStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

// StructuralOp relates two span sets by their position in the trace tree.
//
// These are not joins. A JoinStage correlates two result sets on attribute
// values the query names; a structural operator correlates them on the trace
// structure itself, which no attribute records. QLS §Joins cannot express that,
// which is why this is an operator of its own rather than a JoinType.
type StructuralOp int

const (
	// StructuralChild selects spans whose direct parent is in the left set.
	StructuralChild StructuralOp = iota
	// StructuralDescendant selects spans anywhere below the left set, at any
	// depth. It is the transitive closure of StructuralChild, and the two are
	// kept apart because a target that can express only one of them must say so.
	StructuralDescendant
	// StructuralSibling selects spans sharing a parent with the left set.
	StructuralSibling
)

var structuralOpEnum = newEnumDef[StructuralOp]("StructuralOp",
	"CHILD", "DESCENDANT", "SIBLING")

func (o StructuralOp) String() string { return structuralOpEnum.String(o) }

// ParseStructuralOp resolves a structural operator symbol, case-insensitively.
func ParseStructuralOp(s string) (StructuralOp, error) { return structuralOpEnum.Parse(s) }

func (o StructuralOp) MarshalJSON() ([]byte, error)  { return structuralOpEnum.marshalJSON(o) }
func (o *StructuralOp) UnmarshalJSON(b []byte) error { return structuralOpEnum.unmarshalJSON(b, o) }

// StructuralStage narrows the span set so far to those standing in a given
// relationship to a second span set.
//
// Right is the span set on the other side of the operator. It is a whole Query
// rather than a bare selector because TraceQL admits a filtered, aggregated span
// set there just as it does on the left.
type StructuralStage struct {
	IRNode
	Op    StructuralOp `json:"op"`
	Right *Query       `json:"right,omitempty"`
}

func (s *StructuralStage) StageKind() StageKind { return StageKindStructural }
func (s *StructuralStage) stageNode()           {}

func (s *StructuralStage) String() string {
	if s.Right == nil {
		return "structural " + s.Op.String()
	}
	return "structural " + s.Op.String() + " (" + s.Right.String() + ")"
}

func (s *StructuralStage) MarshalJSON() ([]byte, error) {
	type alias StructuralStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

// CoercionStage reinterprets one attribute's value as another type, which
// QLS §Attributes > Coercion/Casting into Metrics describes.
//
// Span attributes arrive as text, so comparing one numerically means saying so.
// The cast is a stage rather than a property of the matcher because it applies
// to every later reference to that attribute, not only to the comparison that
// prompted it — and because a target without the cast loses the whole stage
// rather than a corner of a predicate.
type CoercionStage struct {
	IRNode
	// Attribute is the key being reinterpreted, carrying its scope prefix as
	// SpansetSelector describes.
	Attribute string `json:"attribute"`
	// TargetType is what the attribute is read as from here on.
	TargetType QlsDataType `json:"target_type"`
}

func (s *CoercionStage) StageKind() StageKind { return StageKindCoercion }
func (s *CoercionStage) stageNode()           {}

func (s *CoercionStage) String() string {
	return fmt.Sprintf("coerce(%s -> %s)", s.Attribute, s.TargetType)
}

func (s *CoercionStage) MarshalJSON() ([]byte, error) {
	type alias CoercionStage
	return json.Marshal(struct {
		Kind StageKind `json:"kind"`
		*alias
	}{Kind: s.StageKind(), alias: (*alias)(s)})
}

// SortOrder is the ordering applied to the result set. QLS §.3.0 Metrics orders
// by end_timestamp ascending by default, and allows that to be overridden, so
// SortNone means "leave the spec default in place" rather than "unordered".
type SortOrder int

const (
	SortNone SortOrder = iota
	SortAsc
	SortDesc
)

var sortOrderEnum = newEnumDef[SortOrder]("SortOrder", "NONE", "ASC", "DESC")

func (s SortOrder) String() string { return sortOrderEnum.String(s) }

// ParseSortOrder resolves a sort order symbol, case-insensitively.
func ParseSortOrder(s string) (SortOrder, error) { return sortOrderEnum.Parse(s) }

func (s SortOrder) MarshalJSON() ([]byte, error)  { return sortOrderEnum.marshalJSON(s) }
func (s *SortOrder) UnmarshalJSON(b []byte) error { return sortOrderEnum.unmarshalJSON(b, s) }

// TimeRange is the overall span a query reads, per QLS §Selection > Query Time
// Range Selection. Its bounds are inclusive, and only results within them may
// appear in the output. It applies to at-rest queries; streaming queries do not
// need one. Windows and windowing functions may still widen the data actually
// fetched.
type TimeRange struct {
	IRNode
	Start Timestamp `json:"start"`
	End   Timestamp `json:"end"`
}

func (r *TimeRange) String() string { return fmt.Sprintf("[%s, %s]", r.Start, r.End) }

// Duration returns the length of the range.
func (r *TimeRange) Duration() Interval { return Interval{Nanos: r.End.UnixNano - r.Start.UnixNano} }

// WindowAlignment is how window boundaries are normalised, per QLS §Time Based
// Windowing.
type WindowAlignment int

const (
	// WindowUTCNormalized is the spec default: boundaries normalise against UTC
	// using the modulo method, which the spec requires for durations of an hour
	// or less.
	WindowUTCNormalized WindowAlignment = iota
	// WindowCalendarAligned normalises on Gregorian calendar boundaries, which
	// the spec requires for intervals over an hour carrying a timezone: a day
	// aligns to 00:00:00, a week to the start of Sunday.
	WindowCalendarAligned
	// WindowQueryStartAligned anchors the first interval on the query start
	// time and increments by step from there.
	WindowQueryStartAligned
)

var windowAlignmentEnum = newEnumDef[WindowAlignment]("WindowAlignment",
	"UTC_NORMALIZED", "CALENDAR_ALIGNED", "QUERY_START_ALIGNED")

func (a WindowAlignment) String() string { return windowAlignmentEnum.String(a) }

// ParseWindowAlignment resolves a window alignment symbol, case-insensitively.
func ParseWindowAlignment(s string) (WindowAlignment, error) { return windowAlignmentEnum.Parse(s) }

func (a WindowAlignment) MarshalJSON() ([]byte, error) {
	return windowAlignmentEnum.marshalJSON(a)
}
func (a *WindowAlignment) UnmarshalJSON(b []byte) error {
	return windowAlignmentEnum.unmarshalJSON(b, a)
}

// Window slices the query time range into buckets. QLS §Time Based Windowing
// makes windows [start, end) — inclusive of the start, exclusive of the end —
// and UTC-normalised by default.
type Window struct {
	IRNode
	Step      Interval        `json:"step"`
	Offset    Interval        `json:"offset"`
	Alignment WindowAlignment `json:"alignment"`
}

func (w *Window) String() string {
	var b strings.Builder
	b.WriteString("step=" + w.Step.String())
	if !w.Offset.IsZero() {
		b.WriteString(" offset=" + w.Offset.String())
	}
	b.WriteString(" align=" + w.Alignment.String())
	return b.String()
}

// Output describes the shape of a query's result set.
type Output struct {
	IRNode
	Range   *TimeRange `json:"range,omitempty"`
	Window  *Window    `json:"window,omitempty"`
	GroupBy []string   `json:"group_by,omitempty"`
	Sort    SortOrder  `json:"sort"`
	Limit   int        `json:"limit,omitempty"`
	// SubqueryRange and SubqueryStep describe a subquery: an expression
	// evaluated repeatedly over an outer range at its own resolution, which
	// PromQL writes as expr[30m:1m]. Both are nil when the query is not one,
	// and SubqueryRange being non-nil is what marks it as a subquery.
	//
	// They sit beside Window rather than in it because a subquery nests two
	// windows and a Window holds one: Window.Step stays the window the innermost
	// temporal aggregation reduces over — the 5m of rate(x[5m]) — while these
	// carry the outer range and the resolution.
	SubqueryRange *Interval `json:"subquery_range,omitempty"`
	SubqueryStep  *Interval `json:"subquery_step,omitempty"`
}

// IsSubquery reports whether the output describes a subquery.
func (o *Output) IsSubquery() bool { return o != nil && o.SubqueryRange != nil }

func (o *Output) String() string {
	var parts []string
	if o.Range != nil {
		parts = append(parts, "@"+o.Range.String())
	}
	if o.Window != nil {
		parts = append(parts, o.Window.String())
	}
	if len(o.GroupBy) > 0 {
		parts = append(parts, "by ("+strings.Join(o.GroupBy, ", ")+")")
	}
	if o.Sort != SortNone {
		parts = append(parts, "sort="+o.Sort.String())
	}
	if o.Limit > 0 {
		parts = append(parts, fmt.Sprintf("limit=%d", o.Limit))
	}
	if o.SubqueryRange != nil {
		step := ""
		if o.SubqueryStep != nil {
			step = o.SubqueryStep.String()
		}
		parts = append(parts, "subquery=["+o.SubqueryRange.String()+":"+step+"]")
	}
	return strings.Join(parts, " ")
}

// Visitor is implemented by anything that traverses the IR tree. The validator,
// the emitters and the fidelity reporter all walk the tree this way rather than
// each writing their own type switch.
//
// Visit is called with each node in depth-first, pre-order sequence. Returning
// nil stops the walk from descending into that node's children; returning a
// visitor (usually the receiver) continues the descent with it. After a node's
// children have been walked, Visit is called once more on that same visitor with
// a nil node, which lets stateful visitors track depth.
type Visitor interface {
	Visit(node Node) Visitor
}

// VisitorFunc adapts a plain function into a Visitor that descends into every
// node it is given. The function is called with a nil node when a subtree
// finishes, matching the Visitor contract.
type VisitorFunc func(node Node)

// Visit satisfies Visitor.
func (f VisitorFunc) Visit(node Node) Visitor {
	f(node)
	return f
}

// Walk traverses the tree rooted at node in depth-first, pre-order sequence.
// Nil children are skipped, so a partially built tree walks safely.
func Walk(v Visitor, node Node) {
	if v == nil || node == nil {
		return
	}
	w := v.Visit(node)
	if w == nil {
		return
	}

	switch n := node.(type) {
	case *Query:
		if n.Source != nil {
			Walk(w, n.Source)
		}
		for _, stage := range n.Pipeline {
			if stage != nil {
				Walk(w, stage)
			}
		}
		if n.Output != nil {
			Walk(w, n.Output)
		}

	case *DataSource:
		for _, s := range n.Selectors {
			if s != nil {
				Walk(w, s)
			}
		}
		if n.Spanset != nil {
			Walk(w, n.Spanset)
		}

	case *SpansetSelector:
		if n.Filters != nil {
			Walk(w, n.Filters)
		}

	case *Selector:
		for _, m := range n.Matchers {
			if m != nil {
				Walk(w, m)
			}
		}

	case *LabelMatcher:
		// Leaf.

	case *FilterStage:
		if n.Predicate != nil {
			Walk(w, n.Predicate)
		}

	case *AggregationStage:
		if n.Parameter != nil {
			Walk(w, n.Parameter)
		}

	case *FunctionStage:
		for _, a := range n.Args {
			if a != nil {
				Walk(w, a)
			}
		}

	case *JoinStage:
		if n.RightSide != nil {
			Walk(w, n.RightSide)
		}

	case *BinaryOpStage:
		if n.Left != nil {
			Walk(w, n.Left)
		}
		if n.Right != nil {
			Walk(w, n.Right)
		}

	case *UnaryOpStage:
		if n.Operand != nil {
			Walk(w, n.Operand)
		}

	case *StructuralStage:
		if n.Right != nil {
			Walk(w, n.Right)
		}

	case *CoercionStage:
		// Leaf: it names an attribute and a type, and holds no child node.

	case *MatchPredicate:
		if n.Matcher != nil {
			Walk(w, n.Matcher)
		}

	case *LogicalPredicate:
		for _, o := range n.Operands {
			if o != nil {
				Walk(w, o)
			}
		}

	case *LiteralExpr, *RefExpr:
		// Leaves.

	case *QueryExpr:
		if n.Query != nil {
			Walk(w, n.Query)
		}

	case *Output:
		if n.Range != nil {
			Walk(w, n.Range)
		}
		if n.Window != nil {
			Walk(w, n.Window)
		}

	case *TimeRange, *Window:
		// Leaves.

	default:
		panic(fmt.Sprintf("ir: Walk does not handle node type %T", node))
	}

	w.Visit(nil)
}

// Inspect walks the tree calling f for each node, descending into a node's
// children only when f returns true. It is the convenience form of Walk for
// visitors that need no state.
func Inspect(node Node, f func(Node) bool) {
	Walk(inspector(f), node)
}

type inspector func(Node) bool

func (f inspector) Visit(node Node) Visitor {
	if node == nil {
		return nil
	}
	if f(node) {
		return f
	}
	return nil
}

// Dump renders the tree as an indented outline, annotating any node the
// validator has flagged as less than fully translatable. It exists for
// debugging and for golden-file tests.
func Dump(node Node) string {
	d := &dumper{}
	Walk(d, node)
	return d.out.String()
}

type dumper struct {
	out   strings.Builder
	depth int
}

func (d *dumper) Visit(node Node) Visitor {
	if node == nil {
		d.depth--
		return nil
	}
	d.out.WriteString(strings.Repeat("  ", d.depth))
	fmt.Fprintf(&d.out, "%T %s", node, node.String())
	if base := node.Base(); base.Flag != TranslatabilityFull {
		fmt.Fprintf(&d.out, "  !! %s: %s", base.Flag, base.Reason)
	}
	d.out.WriteString("\n")
	d.depth++
	return d
}

// WorstTranslatability folds the whole subtree's flags into the single worst
// one, along with that node's reason. It is the summary line of a fidelity
// report.
func WorstTranslatability(node Node) (TranslatabilityFlag, string) {
	worst, reason := TranslatabilityFull, ""
	Inspect(node, func(n Node) bool {
		if flag, r := n.Base().Translatability(); flag > worst {
			worst, reason = flag, r
		}
		return true
	})
	return worst, reason
}

// Field names from the QLS data models that a predicate can address. An IR
// filter needs a key, and a record has parts that are not user attributes; these
// name them.
const (
	// FieldBody is the log body (QLS §Logs > Body). A filter over whole log
	// lines, such as a LogQL line filter, addresses this rather than a label.
	FieldBody = "body"
	// FieldValue is the metric value (QLS §Metrics > Value). A comparison that
	// keeps or drops series by their magnitude addresses this.
	FieldValue = "value"
)

// flatNameUnsafe matches the characters a single-namespace label name may not
// contain. PromQL and LogQL both accept [a-zA-Z_][a-zA-Z0-9_]* and nothing else.
var flatNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// FlatLabelName folds a scoped attribute key onto one flat label name, reporting
// whether it had to change.
//
// A span attribute is dotted — "span.http.status_code" — and a DSL with a single
// flat label namespace admits no dot in a name, so a query carrying one would
// not parse. Replacing each with an underscore is the rule OpenTelemetry's own
// Prometheus exporter follows, which makes the rewritten name the one the data
// is most likely already stored under.
//
// It lives here rather than in an emitter because both the validator and the
// emitters need the same answer: one has to report the rewrite, the other has to
// perform it, and the two disagreeing would mean a note describing something
// other than what was written.
func FlatLabelName(key string) (string, bool) {
	if key == "" {
		return key, false
	}
	safe := flatNameUnsafe.ReplaceAllString(key, "_")
	// A leading digit is legal in neither language.
	if safe[0] >= '0' && safe[0] <= '9' {
		safe = "_" + safe
	}
	return safe, safe != key
}

// Names for IR operations that no DSL function corresponds to. They are
// structural: every DSL can write arithmetic and literals, so they are not
// looked up in a language registry.
const (
	// FuncLiteral wraps a bare scalar or string query.
	FuncLiteral = "literal"
	// FuncUnaryOp wraps a leading unary sign applied to one operand.
	FuncUnaryOp = "unary_op"
)

// StructuralFunctions are the function names above, as a set. A validator
// checking whether a target DSL can express a FunctionStage skips these: they
// describe IR structure rather than a DSL's vocabulary, so no registry entry
// names them.
var StructuralFunctions = map[string]bool{
	FuncLiteral: true,
	FuncUnaryOp: true,
}

// Keys for Query.Hints. Hints carry source detail with no home in the QLS model;
// they never change IR semantics, and anything load-bearing belongs in a typed
// field instead.
const (
	// HintSourceDSL names the language the query was written in, which later
	// stages need in order to compare source and target semantics.
	HintSourceDSL = "source.dsl"
	// HintAtModifier records an evaluation instant named relative to the query's
	// own range, which the absolute Output.TimeRange cannot hold.
	HintAtModifier = "at_modifier"
	// HintParen records that the source wrapped the query in parentheses.
	HintParen = "paren"
)

// Hint returns a hint's value, and whether it was set.
func (q *Query) Hint(key string) (string, bool) {
	if q.Hints == nil {
		return "", false
	}
	value, ok := q.Hints[key]
	return value, ok
}

// NodeTypeName returns a node's type without its package qualifier, so that
// *ir.AggregationStage reads as "AggregationStage".
func NodeTypeName(node Node) string {
	name := fmt.Sprintf("%T", node)
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// PathFunc is called for each node of a traversal together with the path that
// reached it. Returning false stops the descent into that node's children.
type PathFunc func(path string, node Node) bool

// InspectPath walks the tree depth-first in pre-order, giving each node a path
// that says how it was reached — "Query.Pipeline[1].FilterStage.Predicate".
//
// It exists because Walk's Visitor contract cannot carry one: Visit receives a
// node with no indication of the field or index it came through, and both the
// validator and the fidelity reporter need to name the part of a query they are
// talking about. The two visit the same node set; TestInspectPathMatchesWalk
// pins them together, so a node type added to one and not the other fails
// immediately rather than going quietly unvisited.
//
// The callback runs before the descent, so a caller that rewrites a node's
// children — reordering a pipeline, say — sees paths reflecting the new order.
//
// Like Walk, this panics on a node type it does not handle.
func InspectPath(root Node, path string, f PathFunc) {
	if root == nil || f == nil {
		return
	}
	if !f(path, root) {
		return
	}

	switch n := root.(type) {
	case *Query:
		if n.Source != nil {
			InspectPath(n.Source, path+".Source", f)
		}
		// Read the pipeline after the callback, so a reordering caller's new
		// order is what gets walked and numbered.
		for i, stage := range n.Pipeline {
			if stage != nil {
				InspectPath(stage, fmt.Sprintf("%s.Pipeline[%d].%s", path, i, NodeTypeName(stage)), f)
			}
		}
		if n.Output != nil {
			InspectPath(n.Output, path+".Output", f)
		}

	case *DataSource:
		for i, selector := range n.Selectors {
			if selector != nil {
				InspectPath(selector, fmt.Sprintf("%s.Selectors[%d]", path, i), f)
			}
		}
		if n.Spanset != nil {
			InspectPath(n.Spanset, path+".Spanset", f)
		}

	case *SpansetSelector:
		if n.Filters != nil {
			InspectPath(n.Filters, path+".Filters", f)
		}

	case *Selector:
		for i, matcher := range n.Matchers {
			if matcher != nil {
				InspectPath(matcher, fmt.Sprintf("%s.Matchers[%d]", path, i), f)
			}
		}

	case *LabelMatcher:
		// Leaf.

	case *FilterStage:
		if n.Predicate != nil {
			InspectPath(n.Predicate, path+".Predicate", f)
		}

	case *AggregationStage:
		if n.Parameter != nil {
			InspectPath(n.Parameter, path+".Parameter", f)
		}

	case *FunctionStage:
		for i, arg := range n.Args {
			if arg != nil {
				InspectPath(arg, fmt.Sprintf("%s.Args[%d]", path, i), f)
			}
		}

	case *JoinStage:
		if n.RightSide != nil {
			InspectPath(n.RightSide, path+".RightSide", f)
		}

	case *BinaryOpStage:
		if n.Left != nil {
			InspectPath(n.Left, path+".Left", f)
		}
		if n.Right != nil {
			InspectPath(n.Right, path+".Right", f)
		}

	case *UnaryOpStage:
		if n.Operand != nil {
			InspectPath(n.Operand, path+".Operand", f)
		}

	case *StructuralStage:
		if n.Right != nil {
			InspectPath(n.Right, path+".Right", f)
		}

	case *CoercionStage:
		// Leaf, as in Walk.

	case *MatchPredicate:
		if n.Matcher != nil {
			InspectPath(n.Matcher, path+".Matcher", f)
		}

	case *LogicalPredicate:
		for i, operand := range n.Operands {
			if operand != nil {
				InspectPath(operand, fmt.Sprintf("%s.Operands[%d]", path, i), f)
			}
		}

	case *LiteralExpr, *RefExpr:
		// Leaves.

	case *QueryExpr:
		if n.Query != nil {
			InspectPath(n.Query, path+".Query", f)
		}

	case *Output:
		if n.Range != nil {
			InspectPath(n.Range, path+".Range", f)
		}
		if n.Window != nil {
			InspectPath(n.Window, path+".Window", f)
		}

	case *TimeRange, *Window:
		// Leaves.

	default:
		panic(fmt.Sprintf("ir: InspectPath does not handle node type %T", root))
	}
}
