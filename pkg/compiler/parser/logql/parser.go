package logql

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/ast"
	"github.com/polyql/polyql/pkg/compiler/parser"
)

// Parser is the LogQL front end. It holds no state, so the single registered
// instance is safe to share across concurrent translations.
type Parser struct{}

// DSL returns "logql".
func (Parser) DSL() string { return DSL }

// Parse turns LogQL text into a LogQL AST.
func (Parser) Parse(input string) (ast.Node, error) {
	expr, err := Parse(input)
	if err != nil {
		// Returning expr directly would hand back a non-nil interface wrapping
		// a nil pointer.
		return nil, err
	}
	return expr, nil
}

func init() { parser.Register(Parser{}) }

// ParseError reports a lexing or parsing failure along with where in the query
// it occurred.
type ParseError struct {
	Pos   int
	Query string
	Msg   string
}

func (e *ParseError) Error() string {
	line, col := 1, e.Pos+1
	if e.Pos <= len(e.Query) {
		if nl := strings.LastIndexByte(e.Query[:e.Pos], '\n'); nl >= 0 {
			line = strings.Count(e.Query[:e.Pos], "\n") + 1
			col = e.Pos - nl
		}
	}
	return fmt.Sprintf("%d:%d: parse error: %s", line, col, e.Msg)
}

// Binary operator precedence. LogQL uses the same table as PromQL.
const (
	precLowest = iota
	precOr
	precAnd
	precCompare
	precAdd
	precMul
	precPow
)

func binaryPrecedence(t TokenType) (int, bool) {
	switch t {
	case LOR:
		return precOr, true
	case LAND, LUNLESS:
		return precAnd, true
	case EQLC, NEQ, GTR, LSS, GTE, LTE:
		return precCompare, true
	case ADD, SUB:
		return precAdd, true
	case MUL, DIV, MOD:
		return precMul, true
	case POW:
		return precPow, true
	}
	return 0, false
}

// Label filter precedence: "or" binds looser than "and" and the comma.
const (
	filterPrecOr  = 1
	filterPrecAnd = 2
)

// Parse parses a LogQL query into its AST.
func Parse(input string) (expr Expr, err error) {
	tokens := Tokenize(input)
	for _, tok := range tokens {
		if tok.Type == ERROR {
			return nil, &ParseError{Pos: tok.Pos, Query: input, Msg: tok.Val}
		}
	}
	if len(tokens) == 1 && tokens[0].Type == EOF {
		return nil, &ParseError{Pos: 0, Query: input, Msg: "empty query"}
	}

	p := &logqlParser{input: input, tokens: tokens}

	// Errors are reported by panicking with a *ParseError so the
	// recursive-descent code stays free of error plumbing. Any other panic is a
	// genuine bug and is left to propagate.
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(*ParseError)
			if !ok {
				panic(r)
			}
			expr, err = nil, pe
		}
	}()

	result := p.parseExpr(precLowest)
	if tok := p.cur(); tok.Type != EOF {
		p.errorf(tok.Pos, "unexpected %s after the end of the expression", describe(tok))
	}
	return result, nil
}

type logqlParser struct {
	input  string
	tokens []Token
	idx    int
}

func (p *logqlParser) cur() Token { return p.tokens[p.idx] }

func (p *logqlParser) peek() Token {
	if p.idx+1 >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.idx+1]
}

func (p *logqlParser) advance() {
	if p.idx < len(p.tokens)-1 {
		p.idx++
	}
}

func (p *logqlParser) errorf(pos int, format string, args ...any) {
	panic(&ParseError{Pos: pos, Query: p.input, Msg: fmt.Sprintf(format, args...)})
}

func (p *logqlParser) expect(t TokenType) Token {
	tok := p.cur()
	if tok.Type != t {
		p.errorf(tok.Pos, "expected %q, got %s", t.String(), describe(tok))
	}
	p.advance()
	return tok
}

func describe(tok Token) string {
	switch tok.Type {
	case EOF:
		return "end of input"
	case IDENTIFIER, NUMBER, DURATION, BYTES:
		return fmt.Sprintf("%q", tok.Val)
	case STRING:
		return "string " + tok.Val
	default:
		return fmt.Sprintf("%q", tok.Type.String())
	}
}

// parseExpr is the precedence-climbing core for the metric layer.
func (p *logqlParser) parseExpr(minPrec int) Expr {
	left := p.parseUnaryExpr()

	for {
		tok := p.cur()
		prec, isBinary := binaryPrecedence(tok.Type)
		if !isBinary || prec < minPrec {
			return left
		}
		op, opPos := tok.Type, tok.Pos
		p.advance()

		returnBool := false
		if p.cur().Type == BOOL {
			if !op.IsComparison() {
				p.errorf(p.cur().Pos, "bool modifier can only follow a comparison operator, not %q", op)
			}
			returnBool = true
			p.advance()
		}
		matching := p.parseVectorMatching(op)

		nextMin := prec + 1
		if op == POW {
			nextMin = prec // right-associative
		}
		right := p.parseExpr(nextMin)

		p.checkBinaryOperands(op, opPos, left, right)
		left = &BinaryExpr{Op: op, LHS: left, RHS: right, VectorMatching: matching, ReturnBool: returnBool}
	}
}

func (p *logqlParser) parseVectorMatching(op TokenType) *VectorMatching {
	var vm *VectorMatching

	if t := p.cur().Type; t == ON || t == IGNORING {
		p.advance()
		vm = &VectorMatching{Card: CardOneToOne, On: t == ON}
		vm.MatchingLabels = p.parseLabelList()
	}

	if t := p.cur().Type; t == GROUP_LEFT || t == GROUP_RIGHT {
		groupPos := p.cur().Pos
		p.advance()
		if op.IsSetOperator() {
			p.errorf(groupPos, "no grouping allowed for %q operation", op)
		}
		if vm == nil {
			vm = &VectorMatching{}
		}
		if t == GROUP_LEFT {
			vm.Card = CardManyToOne
		} else {
			vm.Card = CardOneToMany
		}
		if p.cur().Type == LEFT_PAREN {
			vm.Include = p.parseLabelList()
		}
	}

	if vm != nil && op.IsSetOperator() {
		vm.Card = CardManyToMany
	}
	return vm
}

func (p *logqlParser) checkBinaryOperands(op TokenType, pos int, lhs, rhs Expr) {
	if lhs.Type() == ExprTypeLog || rhs.Type() == ExprTypeLog {
		p.errorf(pos, "binary operator %q requires metric expressions on both sides; "+
			"a log expression must be wrapped in a range aggregation first", op)
	}
}

func (p *logqlParser) parseUnaryExpr() Expr {
	tok := p.cur()
	if tok.Type != ADD && tok.Type != SUB {
		return p.parseAtom()
	}
	p.advance()

	// A unary sign binds more tightly than multiplication but less tightly than
	// exponentiation, so -2^2 is -(2^2).
	operand := p.parseExpr(precMul + 1)
	if operand.Type() == ExprTypeLog {
		p.errorf(tok.Pos, "a unary sign cannot be applied to a log expression")
	}
	return &UnaryExpr{Op: tok.Type, Expr: operand}
}

func (p *logqlParser) parseAtom() Expr {
	tok := p.cur()

	switch tok.Type {
	case NUMBER:
		p.advance()
		return &NumberLiteral{Val: p.numberValue(tok)}

	case LEFT_PAREN:
		p.advance()
		inner := p.parseExpr(precLowest)
		p.expect(RIGHT_PAREN)
		return &ParenExpr{Expr: inner}

	case LEFT_BRACE:
		// A log expression: a stream selector plus any pipeline stages.
		return p.parseLogExpr(false)

	case IDENTIFIER:
		return p.parseNamedExpr()
	}

	p.errorf(tok.Pos, "unexpected %s", describe(tok))
	return nil
}

// startsFunctionCall reports whether the identifier at the cursor is being used
// as a function. LogQL writes an aggregation either as "sum(" or as
// "sum by (...) (", so both shapes count.
func (p *logqlParser) startsFunctionCall() bool {
	switch p.peek().Type {
	case LEFT_PAREN, BY, WITHOUT:
		return true
	}
	return false
}

// parseNamedExpr handles the identifier-headed forms: range aggregations,
// vector aggregations and label_replace.
func (p *logqlParser) parseNamedExpr() Expr {
	tok := p.cur()
	name := strings.ToLower(tok.Val)

	if !p.startsFunctionCall() {
		p.errorf(tok.Pos, "unexpected %q: a LogQL query starts with a stream selector in braces "+
			"or a metric function", tok.Val)
	}

	if op, ok := rangeOpsByName[name]; ok {
		return p.parseRangeAggregation(op)
	}
	if op, ok := vectorOpsByName[name]; ok {
		return p.parseVectorAggregation(op)
	}
	if name == "label_replace" {
		return p.parseLabelReplace()
	}
	p.errorf(tok.Pos, "unknown function %q", tok.Val)
	return nil
}

// parseLogExpr parses a stream selector and the pipeline stages chained after
// it. This is the heart of the language: stages are read left to right and
// appended in order, rather than nesting the way PromQL's calls do.
//
// The unwrap stage is deliberately not consumed here. It belongs to the log
// range rather than to the pipeline, so parseLogRange handles it after this
// returns.
func (p *logqlParser) parseLogExpr(inRange bool) Expr {
	selector := p.parseStreamSelector()
	stages := p.parsePipelineStages()

	// parsePipelineStages stops at "| unwrap" and leaves it for the caller. Only
	// a log range knows what to do with one, so anywhere else it is an error
	// worth naming precisely.
	if !inRange && p.cur().Type == PIPE && p.peek().Type == UNWRAP {
		p.errorf(p.peek().Pos, "unwrap is only allowed inside a range aggregation, "+
			"immediately before the range")
	}

	if len(stages) == 0 {
		return selector
	}
	return &PipelineExpr{Selector: selector, Stages: stages}
}

func (p *logqlParser) parsePipelineStages() []PipelineStage {
	var stages []PipelineStage
	for {
		tok := p.cur()
		switch {
		case tok.Type.IsLineFilterOperator():
			stages = append(stages, p.parseLineFilter())
		case tok.Type == PIPE:
			// "| unwrap" ends the pipeline; the caller decides what to do with
			// it, so leave both tokens in place.
			if p.peek().Type == UNWRAP {
				return stages
			}
			p.advance()
			stages = append(stages, p.parseStage())
		default:
			return stages
		}
	}
}

func (p *logqlParser) parseStreamSelector() *LogStreamSelector {
	openPos := p.cur().Pos
	p.expect(LEFT_BRACE)

	matchers := []*LabelMatcher{}
	if p.cur().Type == RIGHT_BRACE {
		p.advance()
		p.errorf(openPos, "a stream selector must contain at least one label matcher")
	}
	for {
		name := p.parseLabelName()

		opTok := p.cur()
		matchType, ok := matchTypeForToken(opTok.Type)
		if !ok {
			p.errorf(opTok.Pos, "expected a label matching operator (=, !=, =~, !~), got %s", describe(opTok))
		}
		p.advance()

		valueTok := p.cur()
		if valueTok.Type != STRING {
			p.errorf(valueTok.Pos, "expected a quoted string as the matcher value, got %s", describe(valueTok))
		}
		p.advance()

		matcher := &LabelMatcher{Name: name, Type: matchType, Value: p.stringValue(valueTok)}
		if matchType == MatchRegexp || matchType == MatchNotRegexp {
			if _, err := regexp.Compile("^(?:" + matcher.Value + ")$"); err != nil {
				p.errorf(valueTok.Pos, "invalid regular expression in matcher for %q: %s", name, err)
			}
		}
		matchers = append(matchers, matcher)

		if p.cur().Type != COMMA {
			break
		}
		p.advance()
		if p.cur().Type == RIGHT_BRACE {
			break // tolerate a trailing comma
		}
	}
	p.expect(RIGHT_BRACE)

	return &LogStreamSelector{Matchers: matchers}
}

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

// parseLabelName reads a label name. Stage keywords are accepted because a log
// stream may legitimately carry a label named "drop" or "by".
func (p *logqlParser) parseLabelName() string {
	tok := p.cur()
	if tok.Type == IDENTIFIER {
		p.advance()
		return tok.Val
	}
	if _, isKeyword := keywords[tok.Type.String()]; isKeyword {
		p.advance()
		return tok.Type.String()
	}
	p.errorf(tok.Pos, "expected a label name, got %s", describe(tok))
	return ""
}

func (p *logqlParser) parseLineFilter() PipelineStage {
	tok := p.cur()
	var op LineFilterOp
	switch tok.Type {
	case PIPE_EXACT:
		op = LineContains
	case PIPE_MATCH:
		op = LineMatchesRegex
	case NEQ:
		op = LineNotContains
	case NEQ_REGEX:
		op = LineNotMatchesRegex
	}
	p.advance()

	valueTok := p.cur()
	if valueTok.Type != STRING {
		p.errorf(valueTok.Pos, "expected a quoted string after %q, got %s", tok.Type, describe(valueTok))
	}
	p.advance()

	value := p.stringValue(valueTok)
	if op.IsRegex() {
		// Line filter regexes are unanchored, unlike label matcher regexes.
		if _, err := regexp.Compile(value); err != nil {
			p.errorf(valueTok.Pos, "invalid regular expression in line filter: %s", err)
		}
	}
	return &LineFilter{Op: op, Match: value}
}

// parseStage parses one pipeline stage, with the leading "|" already consumed.
func (p *logqlParser) parseStage() PipelineStage {
	tok := p.cur()
	switch tok.Type {
	case JSON:
		p.advance()
		return &ParserStage{Kind: ParserJSON, Params: p.parseParserParams()}
	case LOGFMT:
		p.advance()
		flags := p.parseParserFlags()
		return &ParserStage{Kind: ParserLogfmt, Flags: flags, Params: p.parseParserParams()}
	case UNPACK:
		p.advance()
		return &ParserStage{Kind: ParserUnpack}
	case REGEXP:
		p.advance()
		return &ParserStage{Kind: ParserRegexp, Pattern: p.parseQuotedOperand("regexp")}
	case PATTERN:
		p.advance()
		return &ParserStage{Kind: ParserPattern, Pattern: p.parseQuotedOperand("pattern")}
	case LINE_FORMAT:
		p.advance()
		return &FormatterStage{Kind: FormatLine, Template: p.parseQuotedOperand("line_format")}
	case LABEL_FORMAT:
		p.advance()
		return &FormatterStage{Kind: FormatLabel, Params: p.parseLabelFormatParams()}
	case DROP:
		p.advance()
		return &DropStage{Labels: p.parseLabelRefs("drop")}
	case KEEP:
		p.advance()
		return &KeepStage{Labels: p.parseLabelRefs("keep")}
	case DECOLORIZE:
		p.advance()
		return &DecolorizeStage{}
	case UNWRAP:
		p.errorf(tok.Pos, "unwrap is only allowed inside a range aggregation, immediately before the range")
	}
	// Anything else must be a label filter expression.
	return &LabelFilter{Predicate: p.parseLabelFilterExpr(filterPrecOr)}
}

func (p *logqlParser) parseQuotedOperand(stage string) string {
	tok := p.cur()
	if tok.Type != STRING {
		p.errorf(tok.Pos, "expected a quoted string after %s, got %s", stage, describe(tok))
	}
	p.advance()
	return p.stringValue(tok)
}

// parseParserFlags reads logfmt's --strict and --keep-empty flags.
func (p *logqlParser) parseParserFlags() []string {
	var flags []string
	for p.cur().Type == IDENTIFIER && strings.HasPrefix(p.cur().Val, "--") {
		flags = append(flags, p.cur().Val)
		p.advance()
	}
	return flags
}

// parseParserParams reads the optional label extraction list of a json or
// logfmt stage, accepting both the bare "servers" and the assigned
// `first="servers[0]"` forms.
func (p *logqlParser) parseParserParams() []*ParserParam {
	if p.cur().Type != IDENTIFIER {
		return nil
	}
	var params []*ParserParam
	for {
		nameTok := p.cur()
		if nameTok.Type != IDENTIFIER {
			p.errorf(nameTok.Pos, "expected a label name in the extraction list, got %s", describe(nameTok))
		}
		p.advance()

		param := &ParserParam{Name: nameTok.Val}
		if p.cur().Type == EQL {
			p.advance()
			valueTok := p.cur()
			if valueTok.Type != STRING {
				p.errorf(valueTok.Pos, "expected a quoted extraction expression, got %s", describe(valueTok))
			}
			p.advance()
			param.Expression = p.stringValue(valueTok)
		}
		params = append(params, param)

		if p.cur().Type != COMMA {
			return params
		}
		p.advance()
	}
}

// parseLabelFormatParams reads label_format's entries, which are either a
// rename (dst=src) or a template (dst="...").
func (p *logqlParser) parseLabelFormatParams() []*LabelFormatParam {
	var params []*LabelFormatParam
	for {
		dst := p.parseLabelName()
		p.expect(EQL)

		tok := p.cur()
		switch tok.Type {
		case STRING:
			p.advance()
			params = append(params, &LabelFormatParam{Dst: dst, Template: p.stringValue(tok), IsTemplate: true})
		case IDENTIFIER:
			p.advance()
			params = append(params, &LabelFormatParam{Dst: dst, Src: tok.Val})
		default:
			p.errorf(tok.Pos, "expected a source label or a quoted template after %s=, got %s", dst, describe(tok))
		}

		if p.cur().Type != COMMA {
			return params
		}
		p.advance()
	}
}

// parseLabelRefs reads the operand list of drop and keep, whose entries are
// either bare label names or matchers.
func (p *logqlParser) parseLabelRefs(stage string) []*LabelRef {
	var refs []*LabelRef
	for {
		tok := p.cur()
		name := p.parseLabelName()

		if matchType, ok := matchTypeForToken(p.cur().Type); ok {
			p.advance()
			valueTok := p.cur()
			if valueTok.Type != STRING {
				p.errorf(valueTok.Pos, "expected a quoted string in the %s matcher, got %s", stage, describe(valueTok))
			}
			p.advance()
			refs = append(refs, &LabelRef{Matcher: &LabelMatcher{
				Name: name, Type: matchType, Value: p.stringValue(valueTok),
			}})
		} else {
			refs = append(refs, &LabelRef{Name: name})
		}
		_ = tok

		if p.cur().Type != COMMA {
			return refs
		}
		p.advance()
	}
}

// parseLabelFilterExpr parses a boolean tree of label predicates. LogQL joins
// them with "and", "or", a comma, or bare juxtaposition; the last three all mean
// conjunction.
func (p *logqlParser) parseLabelFilterExpr(minPrec int) LabelFilterExpr {
	left := p.parseLabelFilterOperand()

	for {
		var op LabelFilterBoolOp
		var prec int
		switch p.cur().Type {
		case LOR:
			op, prec = FilterOr, filterPrecOr
		case LAND:
			op, prec = FilterAnd, filterPrecAnd
		case COMMA:
			op, prec = FilterComma, filterPrecAnd
		case IDENTIFIER:
			// Juxtaposition also means conjunction: | level="error" status=500.
			op, prec = FilterAnd, filterPrecAnd
		default:
			return left
		}
		if prec < minPrec {
			return left
		}
		if p.cur().Type != IDENTIFIER {
			p.advance()
		}
		right := p.parseLabelFilterExpr(prec + 1)
		left = &LabelFilterBinary{Op: op, LHS: left, RHS: right}
	}
}

func (p *logqlParser) parseLabelFilterOperand() LabelFilterExpr {
	if p.cur().Type == LEFT_PAREN {
		p.advance()
		inner := p.parseLabelFilterExpr(filterPrecOr)
		p.expect(RIGHT_PAREN)
		return &LabelFilterParen{Inner: inner}
	}

	nameTok := p.cur()
	name := p.parseLabelName()

	opTok := p.cur()
	op, ok := labelFilterOpForToken(opTok.Type)
	if !ok {
		p.errorf(opTok.Pos, "expected a comparison operator after the label %q, got %s", name, describe(opTok))
	}
	p.advance()

	value := p.parseFilterValue(op, name)
	_ = nameTok
	return &LabelPredicate{Name: name, Op: op, Value: value}
}

func labelFilterOpForToken(t TokenType) (LabelFilterOp, bool) {
	switch t {
	case EQL:
		return FilterEq, true
	case EQLC:
		return FilterEqEq, true
	case NEQ:
		return FilterNeq, true
	case EQL_REGEX:
		return FilterRegex, true
	case NEQ_REGEX:
		return FilterNotRegex, true
	case GTR:
		return FilterGT, true
	case GTE:
		return FilterGTE, true
	case LSS:
		return FilterLT, true
	case LTE:
		return FilterLTE, true
	}
	return 0, false
}

func (p *logqlParser) parseFilterValue(op LabelFilterOp, name string) *FilterValue {
	tok := p.cur()
	switch tok.Type {
	case STRING:
		p.advance()
		if op.IsOrdered() {
			p.errorf(tok.Pos, "the ordered comparison %q cannot be applied to a string", op)
		}
		return &FilterValue{Kind: FilterValueString, Text: tok.Val, Str: p.stringValue(tok)}

	case NUMBER:
		p.advance()
		return &FilterValue{Kind: FilterValueNumber, Text: tok.Val, Number: p.numberValue(tok)}

	case DURATION:
		p.advance()
		d, err := ParseDuration(tok.Val)
		if err != nil {
			p.errorf(tok.Pos, "%s", err)
		}
		return &FilterValue{Kind: FilterValueDuration, Text: tok.Val, Duration: d}

	case BYTES:
		p.advance()
		b, err := ParseBytes(tok.Val)
		if err != nil {
			p.errorf(tok.Pos, "%s", err)
		}
		return &FilterValue{Kind: FilterValueBytes, Text: tok.Val, Bytes: b}

	case SUB:
		// A negative number, as in | offset < -5.
		p.advance()
		numTok := p.cur()
		if numTok.Type != NUMBER {
			p.errorf(numTok.Pos, "expected a number after the sign, got %s", describe(numTok))
		}
		p.advance()
		return &FilterValue{Kind: FilterValueNumber, Text: "-" + numTok.Val, Number: -p.numberValue(numTok)}
	}

	p.errorf(tok.Pos, "expected a string, number, duration or byte size for the label filter on %q, got %s",
		name, describe(tok))
	return nil
}

// parseRangeAggregation parses a range function over a log range.
func (p *logqlParser) parseRangeAggregation(op RangeOp) Expr {
	opTok := p.cur()
	p.advance()
	p.expect(LEFT_PAREN)

	var param *NumberLiteral
	if op.TakesParameter() {
		tok := p.cur()
		negative := false
		if tok.Type == SUB {
			negative = true
			p.advance()
			tok = p.cur()
		}
		if tok.Type != NUMBER {
			p.errorf(tok.Pos, "%s expects a scalar parameter first, as in %s(0.99, ...)", op, op)
		}
		p.advance()
		value := p.numberValue(tok)
		if negative {
			value = -value
		}
		param = &NumberLiteral{Val: value}
		p.expect(COMMA)
	}

	logRange := p.parseLogRange()
	p.expect(RIGHT_PAREN)

	grouping := p.parseOptionalGrouping()

	switch {
	case op.RequiresUnwrap() && logRange.Unwrap == nil:
		p.errorf(opTok.Pos, "%s aggregates extracted values, so its range must end with | unwrap", op)
	case op.RejectsUnwrap() && logRange.Unwrap != nil:
		p.errorf(opTok.Pos, "%s aggregates log lines, so its range must not use | unwrap", op)
	}

	return &RangeAggregation{Op: op, Range: logRange, Param: param, Grouping: grouping}
}

// parseLogRange parses a log expression followed by an optional unwrap, the
// range in brackets, and an optional offset.
func (p *logqlParser) parseLogRange() *LogRange {
	selector := p.parseLogExpr(true)

	var unwrap *UnwrapExpr
	if p.cur().Type == PIPE && p.peek().Type == UNWRAP {
		unwrap = p.parseUnwrap()
	}

	openPos := p.cur().Pos
	p.expect(LEFT_BRACKET)
	durTok := p.cur()
	if durTok.Type != DURATION {
		p.errorf(durTok.Pos, "expected a duration inside [], got %s", describe(durTok))
	}
	p.advance()
	interval, err := ParseDuration(durTok.Val)
	if err != nil {
		p.errorf(durTok.Pos, "%s", err)
	}
	p.expect(RIGHT_BRACKET)
	_ = openPos

	var offset *Duration
	if p.cur().Type == OFFSET {
		p.advance()
		offTok := p.cur()
		if offTok.Type != DURATION {
			p.errorf(offTok.Pos, "expected a duration after offset, got %s", describe(offTok))
		}
		p.advance()
		value, err := ParseDuration(offTok.Val)
		if err != nil {
			p.errorf(offTok.Pos, "%s", err)
		}
		offset = &Duration{Text: offTok.Val, Value: value}
	}

	return &LogRange{
		Selector: selector,
		Interval: Duration{Text: durTok.Val, Value: interval},
		Offset:   offset,
		Unwrap:   unwrap,
	}
}

// parseUnwrap parses "| unwrap label", "| unwrap duration(label)" and the label
// filters that may follow.
func (p *logqlParser) parseUnwrap() *UnwrapExpr {
	p.expect(PIPE)
	p.expect(UNWRAP)

	tok := p.cur()
	if tok.Type != IDENTIFIER {
		p.errorf(tok.Pos, "expected a label name after unwrap, got %s", describe(tok))
	}
	p.advance()

	unwrap := &UnwrapExpr{Identifier: tok.Val}
	if p.cur().Type == LEFT_PAREN {
		conversion, ok := conversionOpForName(strings.ToLower(tok.Val))
		if !ok {
			p.errorf(tok.Pos, "unknown unwrap conversion %q: expected duration, duration_seconds or bytes", tok.Val)
		}
		p.advance()
		inner := p.cur()
		if inner.Type != IDENTIFIER {
			p.errorf(inner.Pos, "expected a label name inside %s(), got %s", tok.Val, describe(inner))
		}
		p.advance()
		p.expect(RIGHT_PAREN)
		unwrap.Conversion = conversion
		unwrap.Identifier = inner.Val
	}

	// Label filters written after the unwrap, commonly | __error__="".
	for p.cur().Type == PIPE {
		if p.peek().Type == UNWRAP {
			p.errorf(p.peek().Pos, "a range may only be unwrapped once")
		}
		p.advance()
		unwrap.PostFilters = append(unwrap.PostFilters,
			&LabelFilter{Predicate: p.parseLabelFilterExpr(filterPrecOr)})
	}
	return unwrap
}

func conversionOpForName(name string) (ConversionOp, bool) {
	switch name {
	case "duration":
		return ConvDuration, true
	case "duration_seconds":
		return ConvDurationSeconds, true
	case "bytes":
		return ConvBytes, true
	}
	return ConvNone, false
}

// parseVectorAggregation parses an aggregation across streams. LogQL allows the
// grouping clause on either side of the parenthesised expression.
func (p *logqlParser) parseVectorAggregation(op VectorOp) Expr {
	opTok := p.cur()
	p.advance()

	grouping := p.parseOptionalGrouping()

	p.expect(LEFT_PAREN)
	var param *NumberLiteral
	if op.TakesParameter() {
		tok := p.cur()
		if tok.Type != NUMBER {
			p.errorf(tok.Pos, "%s expects a numeric parameter first, as in %s(5, ...)", op, op)
		}
		p.advance()
		param = &NumberLiteral{Val: p.numberValue(tok)}
		p.expect(COMMA)
	}
	inner := p.parseExpr(precLowest)
	p.expect(RIGHT_PAREN)

	if grouping == nil {
		grouping = p.parseOptionalGrouping()
	}

	if inner.Type() != ExprTypeMetric {
		p.errorf(opTok.Pos, "%s expects a metric expression, got a %s", op, inner.Type())
	}
	return &VectorAggregation{Op: op, Expr: inner, Param: param, Grouping: grouping}
}

func (p *logqlParser) parseOptionalGrouping() *Grouping {
	t := p.cur().Type
	if t != BY && t != WITHOUT {
		return nil
	}
	p.advance()
	return &Grouping{Without: t == WITHOUT, Labels: p.parseLabelList()}
}

func (p *logqlParser) parseLabelList() []string {
	p.expect(LEFT_PAREN)
	labels := []string{}
	if p.cur().Type == RIGHT_PAREN {
		p.advance()
		return labels
	}
	for {
		labels = append(labels, p.parseLabelName())
		if p.cur().Type != COMMA {
			break
		}
		p.advance()
		if p.cur().Type == RIGHT_PAREN {
			break // tolerate a trailing comma
		}
	}
	p.expect(RIGHT_PAREN)
	return labels
}

func (p *logqlParser) parseLabelReplace() Expr {
	namePos := p.cur().Pos
	p.advance()
	p.expect(LEFT_PAREN)

	inner := p.parseExpr(precLowest)
	if inner.Type() != ExprTypeMetric {
		p.errorf(namePos, "label_replace expects a metric expression as its first argument")
	}

	args := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		p.expect(COMMA)
		tok := p.cur()
		if tok.Type != STRING {
			p.errorf(tok.Pos, "argument %d of label_replace must be a quoted string, got %s", i+2, describe(tok))
		}
		p.advance()
		args = append(args, p.stringValue(tok))
	}
	p.expect(RIGHT_PAREN)

	if _, err := regexp.Compile("^(?:" + args[3] + ")$"); err != nil {
		p.errorf(namePos, "invalid regular expression in label_replace: %s", err)
	}
	return &LabelReplace{
		Expr: inner, Dst: args[0], Replacement: args[1], Src: args[2], Regex: args[3],
	}
}

func (p *logqlParser) numberValue(tok Token) float64 {
	text := strings.ReplaceAll(tok.Val, "_", "")
	switch strings.ToLower(text) {
	case "nan":
		return math.NaN()
	case "inf":
		return math.Inf(1)
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		p.errorf(tok.Pos, "invalid number %q", tok.Val)
	}
	return value
}

func (p *logqlParser) stringValue(tok Token) string {
	value, err := unquote(tok.Val)
	if err != nil {
		p.errorf(tok.Pos, "invalid string literal: %s", err)
	}
	return value
}

// unquote resolves a LogQL string literal. Double- and single-quoted strings
// take Go escape sequences; backquoted strings are raw, which is why templates
// are commonly written that way.
func unquote(s string) (string, error) {
	if len(s) < 2 {
		return "", fmt.Errorf("string literal is too short")
	}
	switch s[0] {
	case '`':
		return s[1 : len(s)-1], nil
	case '"':
		return strconv.Unquote(s)
	case '\'':
		inner := s[1 : len(s)-1]
		var b strings.Builder
		b.WriteByte('"')
		for i := 0; i < len(inner); i++ {
			switch c := inner[i]; {
			case c == '\\' && i+1 < len(inner):
				if inner[i+1] == '\'' {
					b.WriteByte('\'')
				} else {
					b.WriteByte(c)
					b.WriteByte(inner[i+1])
				}
				i++
			case c == '"':
				b.WriteString(`\"`)
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('"')
		return strconv.Unquote(b.String())
	}
	return "", fmt.Errorf("string literal is not quoted")
}
