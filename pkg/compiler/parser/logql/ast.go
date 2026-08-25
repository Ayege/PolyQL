// Package logql implements the LogQL front end: a hand-written lexer and Pratt
// parser producing a LogQL-specific AST.
//
// LogQL is a pipeline language where PromQL is a nesting one. A LogQL query
// starts from a stream selector and threads log lines left to right through
// stages joined by "|", so the AST's spine is an ordered stage list rather than
// a tree of nested calls. Only the metric layer — range and vector aggregations
// over a log range — nests in the PromQL manner. Keeping that shape in the tree
// is the point: the resolver is what flattens both languages onto the shared
// QLS-aligned IR, and it can only do that honestly if the AST still records what
// the user wrote.
package logql

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
const DSL = "logql"

// logqlNode marks a type as belonging to the LogQL AST, supplying the DSL half
// of the shared ast.Node contract.
type logqlNode struct{}

func (logqlNode) DSL() string { return DSL }

// ExprType distinguishes LogQL's two kinds of query. A log expression yields log
// lines; a metric expression yields samples. The distinction is load-bearing:
// vector aggregations consume metric expressions, range aggregations consume log
// ranges, and mixing them is a query error.
type ExprType int

const (
	ExprTypeLog ExprType = iota
	ExprTypeMetric
)

func (t ExprType) String() string {
	if t == ExprTypeMetric {
		return "metric expression"
	}
	return "log expression"
}

// Expr is a LogQL expression node.
type Expr interface {
	ast.Node
	Type() ExprType
	exprNode()
}

// Duration is a duration literal together with the text it was written as.
// Keeping the source text means "[5m]" renders back as "5m" rather than being
// re-derived as "300s", so a translated query stays recognizable to whoever
// wrote it.
type Duration struct {
	Text  string
	Value time.Duration
}

func (d Duration) String() string { return d.Text }

// Bytes is a byte-size literal together with its source text, kept for the same
// reason as Duration: "20MB" should not come back as "20000000".
type Bytes struct {
	Text  string
	Value uint64
}

func (b Bytes) String() string { return b.Text }

// MatchType is a label matching operator inside a stream selector's braces.
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

// LabelMatcher is one label predicate in a stream selector.
type LabelMatcher struct {
	Name  string
	Type  MatchType
	Value string
}

func (m *LabelMatcher) String() string {
	return fmt.Sprintf("%s%s%s", m.Name, m.Type, strconv.Quote(m.Value))
}

// LogStreamSelector selects log streams by their labels. It is the head of
// every LogQL query.
//
// A query with no pipeline stages parses to this node directly rather than to an
// empty PipelineExpr, which keeps the tree honest about what was written.
type LogStreamSelector struct {
	logqlNode
	Matchers []*LabelMatcher
}

func (*LogStreamSelector) exprNode()      {}
func (*LogStreamSelector) Type() ExprType { return ExprTypeLog }

func (e *LogStreamSelector) String() string {
	parts := make([]string, 0, len(e.Matchers))
	for _, m := range e.Matchers {
		parts = append(parts, m.String())
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// PipelineExpr is a stream selector followed by one or more ordered stages.
type PipelineExpr struct {
	logqlNode
	Selector *LogStreamSelector
	Stages   []PipelineStage
}

func (*PipelineExpr) exprNode()      {}
func (*PipelineExpr) Type() ExprType { return ExprTypeLog }

func (e *PipelineExpr) String() string {
	var b strings.Builder
	b.WriteString(e.Selector.String())
	for _, s := range e.Stages {
		b.WriteString(" ")
		b.WriteString(s.String())
	}
	return b.String()
}

// StageKind identifies a pipeline stage variant.
type StageKind int

const (
	StageLineFilter StageKind = iota
	StageLabelFilter
	StageParser
	StageFormatter
	StageDrop
	StageKeep
	StageDecolorize
)

var stageKindNames = map[StageKind]string{
	StageLineFilter:  "line filter",
	StageLabelFilter: "label filter",
	StageParser:      "parser",
	StageFormatter:   "formatter",
	StageDrop:        "drop",
	StageKeep:        "keep",
	StageDecolorize:  "decolorize",
}

func (k StageKind) String() string {
	if s, ok := stageKindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("StageKind(%d)", int(k))
}

// PipelineStage is one step of a log pipeline.
type PipelineStage interface {
	ast.Node
	StageKind() StageKind
	stageNode()
}

// LineFilterOp is a line filter operator.
type LineFilterOp int

const (
	LineContains LineFilterOp = iota
	LineNotContains
	LineMatchesRegex
	LineNotMatchesRegex
)

var lineFilterOpText = map[LineFilterOp]string{
	LineContains:        "|=",
	LineNotContains:     "!=",
	LineMatchesRegex:    "|~",
	LineNotMatchesRegex: "!~",
}

func (o LineFilterOp) String() string {
	if s, ok := lineFilterOpText[o]; ok {
		return s
	}
	return fmt.Sprintf("LineFilterOp(%d)", int(o))
}

// IsRegex reports whether the operator treats its operand as a regular
// expression rather than a literal substring.
func (o LineFilterOp) IsRegex() bool {
	return o == LineMatchesRegex || o == LineNotMatchesRegex
}

// LineFilter keeps or drops whole log lines by their content. Unlike a label
// matcher's regex, a line filter's regex is not anchored.
type LineFilter struct {
	logqlNode
	Op    LineFilterOp
	Match string
}

func (*LineFilter) StageKind() StageKind { return StageLineFilter }
func (*LineFilter) stageNode()           {}
func (e *LineFilter) String() string {
	return e.Op.String() + " " + strconv.Quote(e.Match)
}

// LabelFilterOp is a comparison operator in a label filter expression.
type LabelFilterOp int

const (
	// FilterEq and FilterEqEq are both equality. LogQL accepts either
	// spelling, and they are kept apart so String reproduces the one written.
	FilterEq LabelFilterOp = iota
	FilterEqEq
	FilterNeq
	FilterRegex
	FilterNotRegex
	FilterGT
	FilterGTE
	FilterLT
	FilterLTE
)

var labelFilterOpText = map[LabelFilterOp]string{
	FilterEq:       "=",
	FilterEqEq:     "==",
	FilterNeq:      "!=",
	FilterRegex:    "=~",
	FilterNotRegex: "!~",
	FilterGT:       ">",
	FilterGTE:      ">=",
	FilterLT:       "<",
	FilterLTE:      "<=",
}

func (o LabelFilterOp) String() string {
	if s, ok := labelFilterOpText[o]; ok {
		return s
	}
	return fmt.Sprintf("LabelFilterOp(%d)", int(o))
}

// IsOrdered reports whether the operator compares magnitude, which only the
// numeric, duration and byte value kinds support.
func (o LabelFilterOp) IsOrdered() bool {
	switch o {
	case FilterGT, FilterGTE, FilterLT, FilterLTE:
		return true
	}
	return false
}

// FilterValueKind is the type of the right-hand side of a label predicate.
type FilterValueKind int

const (
	FilterValueString FilterValueKind = iota
	FilterValueNumber
	FilterValueDuration
	FilterValueBytes
)

func (k FilterValueKind) String() string {
	switch k {
	case FilterValueNumber:
		return "number"
	case FilterValueDuration:
		return "duration"
	case FilterValueBytes:
		return "bytes"
	default:
		return "string"
	}
}

// FilterValue is a label predicate's operand. Kind selects which field carries
// the value; Text is the source spelling, which String renders so that "20MB"
// and "1.5h" survive translation intact.
type FilterValue struct {
	Kind     FilterValueKind
	Text     string
	Str      string
	Number   float64
	Duration time.Duration
	Bytes    uint64
}

func (v *FilterValue) String() string {
	if v.Kind == FilterValueString {
		return strconv.Quote(v.Str)
	}
	return v.Text
}

// LabelFilterExpr is a boolean tree of label predicates.
type LabelFilterExpr interface {
	ast.Node
	labelFilterNode()
}

// LabelPredicate is a leaf comparison against one extracted label.
type LabelPredicate struct {
	logqlNode
	Name  string
	Op    LabelFilterOp
	Value *FilterValue
}

func (*LabelPredicate) labelFilterNode() {}

// String follows LogQL's documented idiom: comparisons against a number,
// duration or byte size are spaced ("status >= 400"), while string matchers are
// written tight ("level=\"error\"").
func (e *LabelPredicate) String() string {
	if e.Value.Kind == FilterValueString {
		return e.Name + e.Op.String() + e.Value.String()
	}
	return e.Name + " " + e.Op.String() + " " + e.Value.String()
}

// LabelFilterBoolOp joins two label filter expressions.
type LabelFilterBoolOp int

const (
	FilterAnd LabelFilterBoolOp = iota
	FilterOr
	// FilterComma is LogQL's comma, which means and. It is distinguished from
	// FilterAnd only so that String reproduces the separator written.
	FilterComma
)

func (o LabelFilterBoolOp) String() string {
	switch o {
	case FilterOr:
		return "or"
	case FilterComma:
		return ","
	default:
		return "and"
	}
}

// LabelFilterBinary joins two label filter expressions with and, or, or a comma.
type LabelFilterBinary struct {
	logqlNode
	Op       LabelFilterBoolOp
	LHS, RHS LabelFilterExpr
}

func (*LabelFilterBinary) labelFilterNode() {}
func (e *LabelFilterBinary) String() string {
	if e.Op == FilterComma {
		return e.LHS.String() + ", " + e.RHS.String()
	}
	return e.LHS.String() + " " + e.Op.String() + " " + e.RHS.String()
}

// LabelFilterParen preserves explicit grouping in a label filter.
type LabelFilterParen struct {
	logqlNode
	Inner LabelFilterExpr
}

func (*LabelFilterParen) labelFilterNode() {}
func (e *LabelFilterParen) String() string { return "(" + e.Inner.String() + ")" }

// LabelFilter is the pipeline stage wrapping a label filter expression.
type LabelFilter struct {
	logqlNode
	Predicate LabelFilterExpr
}

func (*LabelFilter) StageKind() StageKind { return StageLabelFilter }
func (*LabelFilter) stageNode()           {}
func (e *LabelFilter) String() string     { return "| " + e.Predicate.String() }

// ParserKind identifies a log parser stage.
type ParserKind int

const (
	ParserJSON ParserKind = iota
	ParserLogfmt
	ParserRegexp
	ParserPattern
	ParserUnpack
)

var parserKindText = map[ParserKind]string{
	ParserJSON:    "json",
	ParserLogfmt:  "logfmt",
	ParserRegexp:  "regexp",
	ParserPattern: "pattern",
	ParserUnpack:  "unpack",
}

func (k ParserKind) String() string {
	if s, ok := parserKindText[k]; ok {
		return s
	}
	return fmt.Sprintf("ParserKind(%d)", int(k))
}

// ParserParam is one label extraction in a json or logfmt stage. An empty
// Expression means the bare form, as in "| json servers".
type ParserParam struct {
	Name       string
	Expression string
}

func (p *ParserParam) String() string {
	if p.Expression == "" {
		return p.Name
	}
	return p.Name + "=" + strconv.Quote(p.Expression)
}

// ParserStage extracts labels out of a log line.
//
// Pattern carries the operand of regexp and pattern; Params carries the optional
// extraction list of json and logfmt; Flags carries logfmt's --strict and
// --keep-empty.
type ParserStage struct {
	logqlNode
	Kind    ParserKind
	Pattern string
	Params  []*ParserParam
	Flags   []string
}

func (*ParserStage) StageKind() StageKind { return StageParser }
func (*ParserStage) stageNode()           {}

func (e *ParserStage) String() string {
	var b strings.Builder
	b.WriteString("| " + e.Kind.String())
	switch e.Kind {
	case ParserRegexp, ParserPattern:
		b.WriteString(" " + strconv.Quote(e.Pattern))
	default:
		for _, f := range e.Flags {
			b.WriteString(" " + f)
		}
		if len(e.Params) > 0 {
			parts := make([]string, 0, len(e.Params))
			for _, p := range e.Params {
				parts = append(parts, p.String())
			}
			b.WriteString(" " + strings.Join(parts, ", "))
		}
	}
	return b.String()
}

// FormatterKind identifies a formatting stage.
type FormatterKind int

const (
	FormatLine FormatterKind = iota
	FormatLabel
)

func (k FormatterKind) String() string {
	if k == FormatLabel {
		return "label_format"
	}
	return "line_format"
}

// LabelFormatParam is one entry of a label_format stage: either a rename
// (dst=src) or a template (dst="{{.x}}").
type LabelFormatParam struct {
	Dst string
	// Src is the source label for a rename.
	Src string
	// Template is the Go text/template body, used when IsTemplate is set. The
	// parser treats it as an opaque string.
	Template   string
	IsTemplate bool
}

func (p *LabelFormatParam) String() string {
	if p.IsTemplate {
		return p.Dst + "=" + strconv.Quote(p.Template)
	}
	return p.Dst + "=" + p.Src
}

// FormatterStage rewrites the log line or its labels through Go templates.
type FormatterStage struct {
	logqlNode
	Kind FormatterKind
	// Template is the line_format body.
	Template string
	// Params are the label_format entries.
	Params []*LabelFormatParam
}

func (*FormatterStage) StageKind() StageKind { return StageFormatter }
func (*FormatterStage) stageNode()           {}

func (e *FormatterStage) String() string {
	if e.Kind == FormatLine {
		return "| line_format " + strconv.Quote(e.Template)
	}
	parts := make([]string, 0, len(e.Params))
	for _, p := range e.Params {
		parts = append(parts, p.String())
	}
	return "| label_format " + strings.Join(parts, ", ")
}

// LabelRef names a label in a drop or keep stage, either bare or as a matcher.
type LabelRef struct {
	Name    string
	Matcher *LabelMatcher
}

func (r *LabelRef) String() string {
	if r.Matcher != nil {
		return r.Matcher.String()
	}
	return r.Name
}

// DropStage removes labels from the pipeline.
type DropStage struct {
	logqlNode
	Labels []*LabelRef
}

func (*DropStage) StageKind() StageKind { return StageDrop }
func (*DropStage) stageNode()           {}
func (e *DropStage) String() string     { return "| drop " + joinLabelRefs(e.Labels) }

// KeepStage restricts the pipeline to the named labels.
type KeepStage struct {
	logqlNode
	Labels []*LabelRef
}

func (*KeepStage) StageKind() StageKind { return StageKeep }
func (*KeepStage) stageNode()           {}
func (e *KeepStage) String() string     { return "| keep " + joinLabelRefs(e.Labels) }

func joinLabelRefs(refs []*LabelRef) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, ", ")
}

// DecolorizeStage strips ANSI escape sequences from log lines.
type DecolorizeStage struct {
	logqlNode
}

func (*DecolorizeStage) StageKind() StageKind { return StageDecolorize }
func (*DecolorizeStage) stageNode()           {}
func (*DecolorizeStage) String() string       { return "| decolorize" }

// ConversionOp is the optional conversion applied by an unwrap.
type ConversionOp int

const (
	// ConvNone unwraps the label value as a bare number.
	ConvNone ConversionOp = iota
	ConvDuration
	ConvDurationSeconds
	ConvBytes
)

var conversionOpText = map[ConversionOp]string{
	ConvDuration:        "duration",
	ConvDurationSeconds: "duration_seconds",
	ConvBytes:           "bytes",
}

func (o ConversionOp) String() string {
	if s, ok := conversionOpText[o]; ok {
		return s
	}
	return ""
}

// UnwrapExpr turns an extracted label into the numeric sample stream that an
// unwrapped range aggregation consumes.
//
// PostFilters are the label filters written after the unwrap, which LogQL
// commonly uses to discard conversion failures with | __error__="".
type UnwrapExpr struct {
	logqlNode
	Identifier  string
	Conversion  ConversionOp
	PostFilters []*LabelFilter
}

func (e *UnwrapExpr) String() string {
	var b strings.Builder
	b.WriteString("| unwrap ")
	if e.Conversion == ConvNone {
		b.WriteString(e.Identifier)
	} else {
		b.WriteString(e.Conversion.String() + "(" + e.Identifier + ")")
	}
	for _, f := range e.PostFilters {
		b.WriteString(" " + f.String())
	}
	return b.String()
}

// LogRange is a log expression narrowed to a time window, optionally unwrapped.
// It is not an Expr: LogQL only admits it as the argument of a range
// aggregation, and giving it its own type is what lets the parser say so.
type LogRange struct {
	logqlNode
	Selector Expr
	Interval Duration
	Offset   *Duration
	Unwrap   *UnwrapExpr
}

func (e *LogRange) String() string {
	var b strings.Builder
	b.WriteString(e.Selector.String())
	if e.Unwrap != nil {
		b.WriteString(" " + e.Unwrap.String())
	}
	// A bare selector abuts its range, as in {app="x"}[5m]. Once a pipeline or
	// an unwrap comes first, a space keeps the bracket from reading as an index
	// on the preceding word.
	if _, bare := e.Selector.(*LogStreamSelector); !bare || e.Unwrap != nil {
		b.WriteString(" ")
	}
	b.WriteString("[" + e.Interval.String() + "]")
	if e.Offset != nil {
		b.WriteString(" offset " + e.Offset.String())
	}
	return b.String()
}

// Grouping is a by or without clause.
type Grouping struct {
	Labels  []string
	Without bool
}

func (g *Grouping) String() string {
	keyword := "by"
	if g.Without {
		keyword = "without"
	}
	return keyword + " (" + strings.Join(g.Labels, ", ") + ")"
}

// RangeOp is a range aggregation function.
type RangeOp int

const (
	OpRate RangeOp = iota
	OpRateCounter
	OpCountOverTime
	OpBytesRate
	OpBytesOverTime
	OpSumOverTime
	OpAvgOverTime
	OpMinOverTime
	OpMaxOverTime
	OpFirstOverTime
	OpLastOverTime
	OpAbsentOverTime
	OpStdvarOverTime
	OpStddevOverTime
	OpQuantileOverTime
)

var rangeOpText = map[RangeOp]string{
	OpRate:             "rate",
	OpRateCounter:      "rate_counter",
	OpCountOverTime:    "count_over_time",
	OpBytesRate:        "bytes_rate",
	OpBytesOverTime:    "bytes_over_time",
	OpSumOverTime:      "sum_over_time",
	OpAvgOverTime:      "avg_over_time",
	OpMinOverTime:      "min_over_time",
	OpMaxOverTime:      "max_over_time",
	OpFirstOverTime:    "first_over_time",
	OpLastOverTime:     "last_over_time",
	OpAbsentOverTime:   "absent_over_time",
	OpStdvarOverTime:   "stdvar_over_time",
	OpStddevOverTime:   "stddev_over_time",
	OpQuantileOverTime: "quantile_over_time",
}

var rangeOpsByName = func() map[string]RangeOp {
	m := make(map[string]RangeOp, len(rangeOpText))
	for op, name := range rangeOpText {
		m[name] = op
	}
	return m
}()

func (o RangeOp) String() string {
	if s, ok := rangeOpText[o]; ok {
		return s
	}
	return fmt.Sprintf("RangeOp(%d)", int(o))
}

// RequiresUnwrap reports whether the operator only accepts an unwrapped range,
// because it aggregates extracted numbers rather than log lines.
func (o RangeOp) RequiresUnwrap() bool {
	switch o {
	case OpRateCounter, OpSumOverTime, OpAvgOverTime, OpMinOverTime, OpMaxOverTime,
		OpFirstOverTime, OpLastOverTime, OpStdvarOverTime, OpStddevOverTime, OpQuantileOverTime:
		return true
	}
	return false
}

// RejectsUnwrap reports whether the operator only accepts a plain log range,
// because it counts lines or bytes rather than extracted values.
func (o RangeOp) RejectsUnwrap() bool {
	switch o {
	case OpCountOverTime, OpBytesRate, OpBytesOverTime:
		return true
	}
	return false
}

// TakesParameter reports whether the operator needs a leading scalar argument.
func (o RangeOp) TakesParameter() bool { return o == OpQuantileOverTime }

// RangeAggregation applies a range function to a log range.
type RangeAggregation struct {
	logqlNode
	Op    RangeOp
	Range *LogRange
	// Param is the leading scalar of quantile_over_time, nil otherwise.
	Param    *NumberLiteral
	Grouping *Grouping
}

func (*RangeAggregation) exprNode()      {}
func (*RangeAggregation) Type() ExprType { return ExprTypeMetric }

func (e *RangeAggregation) String() string {
	var b strings.Builder
	b.WriteString(e.Op.String() + "(")
	if e.Param != nil {
		b.WriteString(e.Param.String() + ", ")
	}
	b.WriteString(e.Range.String() + ")")
	if e.Grouping != nil {
		b.WriteString(" " + e.Grouping.String())
	}
	return b.String()
}

// VectorOp is a vector aggregation operator.
type VectorOp int

const (
	OpSum VectorOp = iota
	OpAvg
	OpMin
	OpMax
	OpCount
	OpStddev
	OpStdvar
	OpTopK
	OpBottomK
	OpApproxTopK
	OpSort
	OpSortDesc
)

var vectorOpText = map[VectorOp]string{
	OpSum:        "sum",
	OpAvg:        "avg",
	OpMin:        "min",
	OpMax:        "max",
	OpCount:      "count",
	OpStddev:     "stddev",
	OpStdvar:     "stdvar",
	OpTopK:       "topk",
	OpBottomK:    "bottomk",
	OpApproxTopK: "approx_topk",
	OpSort:       "sort",
	OpSortDesc:   "sort_desc",
}

var vectorOpsByName = func() map[string]VectorOp {
	m := make(map[string]VectorOp, len(vectorOpText))
	for op, name := range vectorOpText {
		m[name] = op
	}
	return m
}()

func (o VectorOp) String() string {
	if s, ok := vectorOpText[o]; ok {
		return s
	}
	return fmt.Sprintf("VectorOp(%d)", int(o))
}

// TakesParameter reports whether the operator needs a leading k argument.
func (o VectorOp) TakesParameter() bool {
	switch o {
	case OpTopK, OpBottomK, OpApproxTopK:
		return true
	}
	return false
}

// VectorAggregation aggregates a metric expression across streams.
type VectorAggregation struct {
	logqlNode
	Op       VectorOp
	Expr     Expr
	Param    *NumberLiteral
	Grouping *Grouping
}

func (*VectorAggregation) exprNode()      {}
func (*VectorAggregation) Type() ExprType { return ExprTypeMetric }

func (e *VectorAggregation) String() string {
	var b strings.Builder
	b.WriteString(e.Op.String())
	if e.Grouping != nil {
		b.WriteString(" " + e.Grouping.String() + " ")
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

// VectorMatching configures how the sides of a binary operator are paired.
type VectorMatching struct {
	Card           VectorMatchCardinality
	MatchingLabels []string
	On             bool
	Include        []string
}

// BinaryExpr applies an operator to two metric expressions.
type BinaryExpr struct {
	logqlNode
	Op             TokenType
	LHS, RHS       Expr
	VectorMatching *VectorMatching
	ReturnBool     bool
}

func (*BinaryExpr) exprNode()      {}
func (*BinaryExpr) Type() ExprType { return ExprTypeMetric }

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

// UnaryExpr applies a leading sign to a metric expression.
type UnaryExpr struct {
	logqlNode
	Op   TokenType
	Expr Expr
}

func (*UnaryExpr) exprNode()        {}
func (*UnaryExpr) Type() ExprType   { return ExprTypeMetric }
func (e *UnaryExpr) String() string { return e.Op.String() + e.Expr.String() }

// ParenExpr is an explicitly parenthesised expression, preserved so that String
// reproduces the user's grouping rather than re-deriving it from precedence.
type ParenExpr struct {
	logqlNode
	Expr Expr
}

func (*ParenExpr) exprNode()        {}
func (e *ParenExpr) Type() ExprType { return e.Expr.Type() }
func (e *ParenExpr) String() string { return "(" + e.Expr.String() + ")" }

// NumberLiteral is a scalar literal.
type NumberLiteral struct {
	logqlNode
	Val float64
}

func (*NumberLiteral) exprNode()      {}
func (*NumberLiteral) Type() ExprType { return ExprTypeMetric }
func (e *NumberLiteral) String() string {
	switch {
	case math.IsNaN(e.Val):
		return "NaN"
	case math.IsInf(e.Val, 1):
		return "Inf"
	case math.IsInf(e.Val, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(e.Val, 'g', -1, 64)
}

// LabelReplace rewrites a label on every series of a metric expression, using
// the same signature as its PromQL counterpart.
type LabelReplace struct {
	logqlNode
	Expr        Expr
	Dst         string
	Replacement string
	Src         string
	Regex       string
}

func (*LabelReplace) exprNode()      {}
func (*LabelReplace) Type() ExprType { return ExprTypeMetric }

// StageKeywordNames returns the pipeline stage names LogQL spells as keywords
// rather than as function calls — json, line_format, unwrap and the rest.
//
// They are not in the function table, because "| json" is not a call in LogQL's
// grammar. The language registry still carries their IR mapping, so a
// consistency check comparing the two needs this list to tell that intended
// asymmetry apart from a genuine gap.
func StageKeywordNames() []string {
	var names []string
	for word, tok := range keywords {
		switch tok {
		case JSON, LOGFMT, REGEXP, PATTERN, UNPACK, LINE_FORMAT, LABEL_FORMAT,
			DROP, KEEP, DECOLORIZE, UNWRAP:
			names = append(names, word)
		}
	}
	sort.Strings(names)
	return names
}

// IsStageKeyword reports whether a name is one of LogQL's pipeline stage
// keywords.
func IsStageKeyword(name string) bool {
	for _, known := range StageKeywordNames() {
		if known == strings.ToLower(name) {
			return true
		}
	}
	return false
}

// FunctionNames returns every name the parser accepts as a function: the range
// aggregations, the vector aggregations, label_replace, and the unwrap
// conversions. It gives the language registry a way to check its own coverage
// against what the parser will actually produce.
func FunctionNames() []string {
	names := make([]string, 0, len(rangeOpText)+len(vectorOpText)+4)
	for _, name := range rangeOpText {
		names = append(names, name)
	}
	for _, name := range vectorOpText {
		names = append(names, name)
	}
	names = append(names, "label_replace", "duration", "duration_seconds", "bytes")
	sort.Strings(names)
	return names
}

func (e *LabelReplace) String() string {
	return fmt.Sprintf("label_replace(%s, %s, %s, %s, %s)",
		e.Expr.String(),
		strconv.Quote(e.Dst), strconv.Quote(e.Replacement),
		strconv.Quote(e.Src), strconv.Quote(e.Regex))
}
