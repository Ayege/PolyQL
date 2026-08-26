// Package logql renders TelemetryIR into LogQL text.
//
// LogQL is a pipeline language, so most of the IR's flat stage list maps almost
// directly: the stream selector renders first and each stage appends to the
// right. The one place it does not is the range aggregation, which must enclose
// the whole log pipeline and its window — rate({app="x"} |= "err" [5m]) — so the
// emitter builds the log expression first and closes it when the first temporal
// aggregation arrives. After that the expression yields samples rather than
// lines, and further aggregations wrap it the way PromQL's do.
package logql

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/registry"
)

// DSL is the name this emitter registers under.
const DSL = "logql"

// MetricNameLabel is the label a metric name becomes when a query naming one is
// translated into LogQL. LogQL streams are identified only by labels, so the
// name has nowhere else to go; keeping it as a matcher preserves the intent and
// stays valid LogQL.
const MetricNameLabel = "__name__"

// Emitter renders IR as LogQL. It holds no state, so the registered instance is
// safe to share across concurrent translations.
type Emitter struct{}

// DSL returns "logql".
func (Emitter) DSL() string { return DSL }

// Emit renders a query as LogQL.
func (Emitter) Emit(query *ir.Query, reg *registry.Registry) (string, error) {
	if query == nil {
		return "", fmt.Errorf("logql: cannot emit a nil query")
	}
	if reg == nil {
		return "", fmt.Errorf("logql: a registry is required")
	}
	def, err := reg.Get(DSL)
	if err != nil {
		return "", fmt.Errorf("logql: %w", err)
	}

	w := &writer{def: def, reg: reg}
	text, err := w.emitQuery(query)
	if err != nil {
		return "", err
	}
	return w.notes.Prepend(text), nil
}

func init() { emitter.Register(Emitter{}) }

type writer struct {
	def   *registry.DSLDefinition
	reg   *registry.Registry
	notes emitter.Notes
}

// rendered is an expression together with what phase of the language it is in.
type rendered struct {
	text string
	// logPhase reports whether the text is still a log expression, to which
	// pipeline stages may be appended. Once a range aggregation has closed it,
	// the expression yields samples and only metric operations apply.
	logPhase bool
	// pipelined reports whether any stage has been appended after the stream
	// selector, which decides the spacing before a range bracket.
	pipelined bool
	// atomic reports whether the text needs no parentheses as an operand.
	atomic bool
}

func (w *writer) emitQuery(query *ir.Query) (string, error) {
	if reason, ok := emitter.Unsupported(query); ok {
		w.notes.AddUnsupported(reason)
	}
	// A node carries one reason, so a query the validator downgraded for
	// several causes reports only the first. The emitter therefore also asks
	// directly about what it knows it cannot write, rather than dropping a
	// construct in silence because the flag was already spoken for.
	if query.Output.IsSubquery() {
		w.notes.Addf("UNSUPPORTED: LogQL has no subquery form; the inner expression was " +
			"written without its outer range and resolution")
	}
	// A pinned evaluation instant has no LogQL spelling. Dropping it without a
	// word would change when the query is answered while looking like a clean
	// translation.
	if bounds := query.Output.Range; bounds != nil && !bounds.Start.IsZero() &&
		bounds.Start.UnixNano == bounds.End.UnixNano {
		w.notes.Addf("UNSUPPORTED: LogQL cannot pin evaluation to an instant; the query will be "+
			"answered over the caller's own time range rather than at %s", bounds.Start)
	}

	expr, err := w.emitSource(query)
	if err != nil {
		return "", err
	}

	for _, stage := range query.Pipeline {
		expr, err = w.applyStage(expr, stage, query)
		if err != nil {
			return "", err
		}
	}

	// A log expression that never met a range aggregation is a plain log query,
	// which is valid on its own.
	return expr.text, nil
}

// emitSource renders the stream selector.
func (w *writer) emitSource(query *ir.Query) (rendered, error) {
	if query.Source == nil {
		return rendered{logPhase: false, atomic: true}, nil
	}

	matchers := make([]*ir.LabelMatcher, 0, 4)
	// A metric name has no home in a LogQL stream selector, so it becomes a
	// matcher on the conventional name label rather than being dropped.
	if query.Source.Name != "" {
		matchers = append(matchers, &ir.LabelMatcher{
			Key: MetricNameLabel, Op: ir.MatchEQ, Value: query.Source.Name,
		})
		w.notes.Addf("the metric name %q became a %s label matcher; LogQL selects streams by label only",
			query.Source.Name, MetricNameLabel)
	}
	for _, selector := range query.Source.Selectors {
		matchers = append(matchers, selector.Matchers...)
	}
	// A source resolved from TraceQL carries a boolean filter tree rather than a
	// flat selector. LogQL's braces are conjunctive, so only an AND-tree lowers
	// into them; anything else is reported instead of half-written.
	if spanset := query.Source.Spanset; spanset != nil && spanset.Filters != nil {
		lowered, faithful := emitter.ConjunctiveMatchers(spanset.Filters)
		if !faithful {
			w.notes.Addf("UNSUPPORTED: a LogQL stream selector puts an implicit \"and\" between its "+
				"matchers and cannot express %q; that part of the filter was left out",
				spanset.Filters.String())
		}
		// A stream selector admits only =, !=, =~ and !~. An ordered comparison
		// exists in LogQL but only as a label filter after a parser stage, and
		// there is no parser here to put one after.
		spellable, unspellable := emitter.SelectorSpellable(w.def, lowered)
		for _, matcher := range unspellable {
			w.notes.Addf("UNSUPPORTED: a LogQL stream selector cannot compare with %s; the filter "+
				"on %q was left out", matcher.Op.Symbol(), matcher.Key)
		}
		matchers = append(matchers, spellable...)
	}

	parts := make([]string, 0, len(matchers))
	for _, matcher := range matchers {
		if reason, ok := emitter.Unsupported(matcher); ok {
			w.notes.AddUnsupported(reason)
			continue
		}
		text, err := w.matcherText(matcher, registry.OperatorContextSelector)
		if err != nil {
			return rendered{}, err
		}
		parts = append(parts, text)
	}

	if len(parts) == 0 {
		// LogQL requires at least one matcher, so there is nothing valid to
		// write.
		w.notes.Addf("UNSUPPORTED: the query selects no stream labels, and a LogQL selector " +
			"needs at least one matcher")
		return rendered{logPhase: true, atomic: true}, nil
	}
	// LogQL's canonical rendering separates matchers with a comma and a space.
	return rendered{
		text:     "{" + strings.Join(parts, ", ") + "}",
		logPhase: true,
		atomic:   true,
	}, nil
}

// labelName renders an attribute key as a LogQL label name, reporting a rewrite
// where the key was not already one. A span attribute is dotted, and LogQL
// admits no dot in a label name, so a query carrying one would not parse.
func (w *writer) labelName(key string) string {
	safe, changed := ir.FlatLabelName(key)
	if changed {
		w.notes.Addf("PARTIAL: LogQL label names admit no dots, so %q was written as %q",
			key, safe)
	}
	return safe
}

func (w *writer) matcherText(m *ir.LabelMatcher, ctx registry.OperatorContext) (string, error) {
	// LogQL has no IN operator either; an alternation says the same thing.
	if len(m.Values) > 0 {
		op := ir.MatchRegex
		if m.Op == ir.MatchNotIn {
			op = ir.MatchNotRegex
		}
		symbol, err := w.operatorSymbol(op, ctx)
		if err != nil {
			return "", err
		}
		quoted := make([]string, 0, len(m.Values))
		for _, value := range m.Values {
			quoted = append(quoted, regexp.QuoteMeta(value))
		}
		return w.labelName(m.Key) + symbol + emitter.QuoteString(strings.Join(quoted, "|"),
			w.def.Normalizations.StringQuoting), nil
	}

	symbol, err := w.operatorSymbol(m.Op, ctx)
	if err != nil {
		return "", err
	}
	return w.labelName(m.Key) + symbol + emitter.QuoteString(m.Value, w.def.Normalizations.StringQuoting), nil
}

func (w *writer) operatorSymbol(op ir.MatchOp, ctx registry.OperatorContext) (string, error) {
	candidates := w.def.OperatorsForIROp(op)
	if len(candidates) == 0 {
		return "", fmt.Errorf("logql: no operator spells %s", op)
	}
	for _, candidate := range candidates {
		if candidate.Context == ctx || candidate.Context == registry.OperatorContextAny {
			return candidate.Symbol, nil
		}
	}
	return candidates[0].Symbol, nil
}

func (w *writer) applyStage(expr rendered, stage ir.PipelineStage, query *ir.Query) (rendered, error) {
	if reason, ok := emitter.Unsupported(stage); ok {
		w.notes.AddUnsupported(reason)
		return expr, nil
	}

	switch node := stage.(type) {
	case *ir.AggregationStage:
		return w.applyAggregation(expr, node, query)
	case *ir.FunctionStage:
		return w.applyFunction(expr, node, query)
	case *ir.FilterStage:
		return w.applyFilter(expr, node)
	case *ir.JoinStage:
		return w.applyJoin(expr, node)
	case *ir.BinaryOpStage:
		return w.applyBinaryOp(expr, node)
	case *ir.UnaryOpStage:
		return w.applyUnaryOp(node)
	default:
		return expr, fmt.Errorf("logql: no rule for pipeline stage type %T", stage)
	}
}

func (w *writer) applyAggregation(expr rendered, stage *ir.AggregationStage, query *ir.Query) (rendered, error) {
	fn, ok := w.def.FunctionForAggregation(stage.Op, stage.Scope)
	if !ok {
		w.notes.Addf("UNSUPPORTED: LogQL has no %s aggregation on the %s axis",
			strings.ToLower(stage.Op.String()), strings.ToLower(stage.Scope.String()))
		return expr, nil
	}
	if stage.Scope == ir.AggScopeTemporal {
		return w.applyRangeAggregation(expr, fn, stage, query)
	}
	return w.applyVectorAggregation(expr, fn, stage)
}

// applyRangeAggregation closes the log expression with its window and wraps it.
func (w *writer) applyRangeAggregation(expr rendered, fn *registry.FunctionDef,
	stage *ir.AggregationStage, query *ir.Query) (rendered, error) {

	operand := expr.text
	if !expr.logPhase {
		// A range aggregation reads a log range, and LogQL has no subquery to
		// take a range over an already-aggregated expression.
		w.notes.Addf("UNSUPPORTED: LogQL cannot take a range over an aggregated expression; " +
			"it has no subquery form")
		return expr, nil
	}

	// A bare selector abuts its range; once a pipeline follows, a space keeps
	// the bracket from reading as an index on the preceding word.
	if expr.pipelined {
		operand += " "
	}
	operand += "[" + w.windowStep(query) + "]" + w.offsetSuffix(query)

	text := fn.Name + "("
	if stage.Parameter != nil {
		text += w.exprText(stage.Parameter) + ", "
	}
	text += operand + ")"
	if clause := groupingClause(stage); clause != "" {
		// LogQL writes a range aggregation's grouping after the call.
		text += " " + clause
	}
	return rendered{text: text, logPhase: false, atomic: true}, nil
}

func (w *writer) applyVectorAggregation(expr rendered, fn *registry.FunctionDef,
	stage *ir.AggregationStage) (rendered, error) {

	if expr.logPhase {
		w.notes.Addf("UNSUPPORTED: LogQL cannot aggregate log lines across streams without a " +
			"range aggregation first")
		return expr, nil
	}

	var b strings.Builder
	b.WriteString(fn.Name)
	if clause := groupingClause(stage); clause != "" {
		b.WriteString(" " + clause + " ")
	}
	b.WriteString("(")
	if stage.Parameter != nil {
		b.WriteString(w.exprText(stage.Parameter) + ", ")
	}
	b.WriteString(expr.text + ")")
	return rendered{text: b.String(), logPhase: false, atomic: true}, nil
}

func groupingClause(stage *ir.AggregationStage) string {
	switch {
	case len(stage.Without) > 0:
		return "without (" + strings.Join(stage.Without, ", ") + ")"
	case len(stage.GroupBy) > 0:
		return "by (" + strings.Join(stage.GroupBy, ", ") + ")"
	}
	return ""
}

// stageSyntax maps an IR function name to the LogQL pipeline stage that writes
// it, for the stages LogQL spells with "|" rather than as a call.
var stageSyntax = map[string]string{
	"parse_json":    "json",
	"parse_logfmt":  "logfmt",
	"parse_regexp":  "regexp",
	"parse_pattern": "pattern",
	"parse_unpack":  "unpack",
	"line_format":   "line_format",
	"label_format":  "label_format",
	"drop_labels":   "drop",
	"keep_labels":   "keep",
	"decolorize":    "decolorize",
	"unwrap":        "unwrap",
}

func (w *writer) applyFunction(expr rendered, stage *ir.FunctionStage, query *ir.Query) (rendered, error) {
	switch stage.Name {
	case ir.FuncLiteral:
		if len(stage.Args) > 0 {
			return rendered{text: w.exprText(stage.Args[0]), atomic: true}, nil
		}
		return expr, nil
	}

	if keyword, ok := stageSyntax[stage.Name]; ok {
		return w.applyPipelineStage(expr, stage, keyword)
	}

	// Anything else is a call around the expression.
	fn, ok := w.def.FunctionByIRName(stage.Name)
	if !ok {
		w.notes.Addf("UNSUPPORTED: function %q has no LogQL equivalent", stage.Name)
		return expr, nil
	}
	args := make([]string, 0, len(stage.Args)+1)
	if expr.text != "" {
		args = append(args, expr.text)
	}
	for _, arg := range stage.Args {
		args = append(args, w.exprText(arg))
	}
	// A range function without an IR aggregation still reads a log range, so it
	// closes the log expression the same way one with an operator does.
	if expr.logPhase && readsLogRange(fn) {
		operand := expr.text
		if expr.pipelined {
			operand += " "
		}
		operand += "[" + w.windowStep(query) + "]" + w.offsetSuffix(query)
		return rendered{text: fn.Name + "(" + operand + ")", logPhase: false, atomic: true}, nil
	}
	return rendered{text: fn.Name + "(" + strings.Join(args, ", ") + ")",
		logPhase: expr.logPhase, atomic: true}, nil
}

// readsLogRange reports whether a signature takes a range rather than an instant
// vector, which decides whether the call closes the log expression.
func readsLogRange(fn *registry.FunctionDef) bool {
	for _, arg := range fn.ArgTypes {
		if arg.Name == "range_vector" || arg.Name == "unwrapped_range" {
			return true
		}
	}
	return false
}

// applyPipelineStage appends a "|"-introduced stage to the log expression.
func (w *writer) applyPipelineStage(expr rendered, stage *ir.FunctionStage, keyword string) (rendered, error) {
	if !expr.logPhase {
		w.notes.Addf("UNSUPPORTED: the %q stage reads log lines, and the expression has already "+
			"been aggregated into samples", keyword)
		return expr, nil
	}

	var b strings.Builder
	b.WriteString(expr.text + " | " + keyword)

	switch keyword {
	case "regexp", "pattern", "line_format":
		// These take a single quoted operand.
		if len(stage.Args) > 0 {
			b.WriteString(" " + w.exprText(stage.Args[0]))
		}
	case "unwrap":
		// The unwrap names a label, optionally through a conversion.
		if len(stage.Args) == 0 {
			w.notes.Addf("UNSUPPORTED: an unwrap with no label to read cannot be written")
			return expr, nil
		}
		label := w.exprText(stage.Args[0])
		if len(stage.Args) > 1 {
			conversion := strings.Trim(w.exprText(stage.Args[1]), `"`)
			b.WriteString(" " + conversion + "(" + label + ")")
		} else {
			b.WriteString(" " + label)
		}
	case "json", "logfmt":
		if params := w.parserParams(stage); params != "" {
			b.WriteString(" " + params)
		}
	case "label_format", "drop", "keep":
		if params := w.labelParams(stage, keyword); params != "" {
			b.WriteString(" " + params)
		}
	}

	return rendered{text: b.String(), logPhase: true, pipelined: true, atomic: true}, nil
}

// parserParams renders a json or logfmt extraction list, where a flag and a bare
// reference pass through and an assigned pair rejoins with "=".
func (w *writer) parserParams(stage *ir.FunctionStage) string {
	var parts []string
	for i := 0; i < len(stage.Args); i++ {
		switch arg := stage.Args[i].(type) {
		case *ir.RefExpr:
			parts = append(parts, arg.Name)
		case *ir.LiteralExpr:
			text := emitter.LiteralText(arg, w.def.Normalizations.StringQuoting)
			raw := strings.Trim(text, `"`)
			if strings.HasPrefix(raw, "--") {
				// A parser flag stands alone.
				parts = append(parts, raw)
				continue
			}
			// A name and its extraction expression arrive as a pair.
			if i+1 < len(stage.Args) {
				if value, ok := stage.Args[i+1].(*ir.LiteralExpr); ok {
					parts = append(parts, raw+"="+emitter.LiteralText(value,
						w.def.Normalizations.StringQuoting))
					i++
					continue
				}
			}
			parts = append(parts, raw)
		}
	}
	return strings.Join(parts, ", ")
}

// labelParams renders a label_format, drop or keep operand list.
func (w *writer) labelParams(stage *ir.FunctionStage, keyword string) string {
	var parts []string
	for i := 0; i < len(stage.Args); i++ {
		switch arg := stage.Args[i].(type) {
		case *ir.RefExpr:
			// A reference followed by an operator and a value is a matcher.
			if keyword != "label_format" && i+2 < len(stage.Args) {
				op, opOK := stage.Args[i+1].(*ir.LiteralExpr)
				value, valueOK := stage.Args[i+2].(*ir.LiteralExpr)
				if opOK && valueOK {
					symbol := strings.Trim(emitter.LiteralText(op, w.def.Normalizations.StringQuoting), `"`)
					parts = append(parts, arg.Name+symbol+
						emitter.LiteralText(value, w.def.Normalizations.StringQuoting))
					i += 2
					continue
				}
			}
			parts = append(parts, arg.Name)

		case *ir.LiteralExpr:
			name := strings.Trim(emitter.LiteralText(arg, w.def.Normalizations.StringQuoting), `"`)
			if i+1 >= len(stage.Args) {
				parts = append(parts, name)
				continue
			}
			switch value := stage.Args[i+1].(type) {
			case *ir.RefExpr:
				// A rename names a source label.
				parts = append(parts, name+"="+value.Name)
			case *ir.LiteralExpr:
				parts = append(parts, name+"="+emitter.LiteralText(value,
					w.def.Normalizations.StringQuoting))
			}
			i++
		}
	}
	return strings.Join(parts, ", ")
}

// applyFilter renders a filter as a line filter, a label filter, or a value
// comparison, according to what it addresses and what phase the expression is
// in.
func (w *writer) applyFilter(expr rendered, stage *ir.FilterStage) (rendered, error) {
	if !expr.logPhase {
		return w.applyValueComparison(expr, stage)
	}
	text, err := w.filterText(stage.Predicate, expr)
	if err != nil {
		return expr, err
	}
	if text == "" {
		return expr, nil
	}
	return rendered{text: expr.text + text, logPhase: true, pipelined: true, atomic: true}, nil
}

// filterText renders a predicate as the pipeline syntax it needs.
func (w *writer) filterText(predicate ir.Predicate, expr rendered) (string, error) {
	switch node := predicate.(type) {
	case *ir.MatchPredicate:
		if node.Matcher == nil {
			return "", nil
		}
		if node.Matcher.Key == ir.FieldBody {
			return w.lineFilterText(node.Matcher)
		}
		text, err := w.labelFilterText(node.Matcher)
		if err != nil {
			return "", err
		}
		return " | " + text, nil

	case *ir.LogicalPredicate:
		// A boolean label filter is written as one stage with "and" or "or"
		// between its parts, which is LogQL's own form.
		parts := make([]string, 0, len(node.Operands))
		for _, operand := range node.Operands {
			match, ok := operand.(*ir.MatchPredicate)
			if !ok || match.Matcher == nil {
				w.notes.Addf("UNSUPPORTED: a nested boolean label filter could not be written")
				continue
			}
			if match.Matcher.Key == ir.FieldBody {
				w.notes.Addf("UNSUPPORTED: a line filter cannot be combined with a boolean " +
					"operator inside one label filter stage")
				continue
			}
			text, err := w.labelFilterText(match.Matcher)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		if len(parts) == 0 {
			return "", nil
		}
		joiner := " and "
		if node.Op == ir.LogicalOr {
			joiner = " or "
		}
		return " | " + strings.Join(parts, joiner), nil
	}
	return "", nil
}

// lineFilterText renders a filter over the whole log line.
func (w *writer) lineFilterText(m *ir.LabelMatcher) (string, error) {
	// The IR names containment outright, so no guessing from the pattern is
	// needed: |= is CONTAINS and |~ is a genuine regex, whatever text each
	// carries.
	var symbol string
	switch m.Op {
	case ir.MatchContains:
		symbol = "|="
	case ir.MatchNotContains:
		symbol = "!="
	case ir.MatchRegex:
		symbol = "|~"
	case ir.MatchNotRegex:
		symbol = "!~"
	case ir.MatchEQ:
		// An exact match on a whole log line is not something LogQL writes; the
		// closest form is an anchored pattern.
		symbol = "|~"
		return " " + symbol + " " + emitter.QuoteString("^"+regexp.QuoteMeta(m.Value)+"$",
			w.def.Normalizations.StringQuoting), nil
	default:
		w.notes.Addf("UNSUPPORTED: LogQL has no line filter for %s", m.Op)
		return "", nil
	}
	return " " + symbol + " " + emitter.QuoteString(m.Value, w.def.Normalizations.StringQuoting), nil
}

// labelFilterText renders a filter over an extracted label.
//
// LogQL's two idioms differ in more than spacing: a comparison against a number,
// a duration or a byte size is written unquoted and spaced — "status >= 400",
// "duration > 1m" — while a string matcher is quoted and tight. Quoting a
// numeric operand does not merely look wrong, it changes the comparison from
// numeric to lexical.
func (w *writer) labelFilterText(m *ir.LabelMatcher) (string, error) {
	symbol, err := w.operatorSymbol(m.Op, registry.OperatorContextComparison)
	if err != nil {
		return "", err
	}

	if len(m.Values) > 0 {
		quoted := make([]string, 0, len(m.Values))
		for _, value := range m.Values {
			quoted = append(quoted, regexp.QuoteMeta(value))
		}
		op := ir.MatchRegex
		if m.Op == ir.MatchNotIn {
			op = ir.MatchNotRegex
		}
		symbol, err = w.operatorSymbol(op, registry.OperatorContextComparison)
		if err != nil {
			return "", err
		}
		return m.Key + symbol + emitter.QuoteString(strings.Join(quoted, "|"),
			w.def.Normalizations.StringQuoting), nil
	}

	if isOrderedComparison(m.Op) {
		return m.Key + " " + symbol + " " + m.Value, nil
	}
	return m.Key + symbol + emitter.QuoteString(m.Value, w.def.Normalizations.StringQuoting), nil
}

// isOrderedComparison reports whether an operator compares magnitude, which only
// a number, duration or byte size supports — and so tells the operand apart from
// a string without the IR having to record its kind.
func isOrderedComparison(op ir.MatchOp) bool {
	switch op {
	case ir.MatchGT, ir.MatchGTE, ir.MatchLT, ir.MatchLTE:
		return true
	}
	return false
}

// applyValueComparison renders a filter over an aggregated value as a binary
// comparison.
func (w *writer) applyValueComparison(expr rendered, stage *ir.FilterStage) (rendered, error) {
	match, ok := stage.Predicate.(*ir.MatchPredicate)
	if !ok || match.Matcher == nil {
		w.notes.Addf("UNSUPPORTED: LogQL cannot express this filter over an aggregated expression")
		return expr, nil
	}
	if match.Matcher.Key != ir.FieldValue {
		w.notes.Addf("UNSUPPORTED: LogQL cannot filter on %q once the expression has been aggregated",
			match.Matcher.Key)
		return expr, nil
	}
	symbol, err := w.operatorSymbol(match.Matcher.Op, registry.OperatorContextComparison)
	if err != nil {
		return expr, err
	}
	if stage.ReturnsBool {
		// LogQL has no bool modifier, so the comparison drops the series that
		// fail it instead of returning 0 for them.
		w.notes.Addf("PARTIAL: LogQL has no bool modifier; the comparison was written as a " +
			"filter, so series failing it are dropped rather than returned as 0")
	}
	return rendered{text: expr.text + " " + symbol + " " + match.Matcher.Value, atomic: false}, nil
}

func (w *writer) applyJoin(expr rendered, stage *ir.JoinStage) (rendered, error) {
	// LogQL has no join at all, so this only arrives when a validator has not
	// run. The left side is still valid on its own.
	w.notes.Addf("UNSUPPORTED: LogQL has no join; only the left-hand side was written")
	if len(stage.IncludeLabels) > 0 {
		w.notes.Addf("UNSUPPORTED: the labels %s copied by the join were lost with it",
			strings.Join(stage.IncludeLabels, ", "))
	}
	return expr, nil
}

func (w *writer) applyBinaryOp(expr rendered, stage *ir.BinaryOpStage) (rendered, error) {
	symbol, ok := arithSymbol(stage.Op)
	if !ok {
		w.notes.Addf("UNSUPPORTED: operator %s has no LogQL spelling", stage.Op)
		return expr, nil
	}
	if stage.Left == nil || stage.Right == nil {
		// The operands belong to a join, which LogQL cannot write; the join
		// stage has already reported that.
		return expr, nil
	}

	left := w.operandText(stage.Left, stage.Op, false)
	right := w.operandText(stage.Right, stage.Op, true)
	return rendered{text: left + " " + symbol + " " + right, atomic: false}, nil
}

// applyUnaryOp renders a leading sign, grouping the operand only when it binds
// less tightly than the sign does.
func (w *writer) applyUnaryOp(stage *ir.UnaryOpStage) (rendered, error) {
	symbol, ok := arithSymbol(stage.Op)
	if !ok {
		w.notes.Addf("UNSUPPORTED: sign %s has no LogQL spelling", stage.Op)
		return rendered{}, nil
	}
	if stage.Operand == nil {
		return rendered{}, fmt.Errorf("logql: a %s stage has no operand", stage.Op)
	}
	return rendered{text: symbol + w.operandText(stage.Operand, stage.Op, true), atomic: false}, nil
}

func (w *writer) windowStep(query *ir.Query) string {
	if query.Output == nil || query.Output.Window == nil || query.Output.Window.Step.IsZero() {
		w.notes.Addf("no window was recorded for a range aggregation; [5m] was assumed")
		return "5m"
	}
	return emitter.FormatDuration(query.Output.Window.Step, w.def.Normalizations.DurationFormat)
}

func (w *writer) offsetSuffix(query *ir.Query) string {
	if query.Output == nil || query.Output.Window == nil || query.Output.Window.Offset.IsZero() {
		return ""
	}
	return " offset " + emitter.FormatDuration(query.Output.Window.Offset,
		w.def.Normalizations.DurationFormat)
}

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

// operandText renders one side of a binary operator, grouping it only when the
// reconstructed precedence would otherwise differ from the tree's.
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

// arithSymbols maps the IR's binary operators to LogQL spellings. LogQL borrows
// PromQL's operator set, so the two tables agree.
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
