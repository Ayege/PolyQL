package promql

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ast"
	"github.com/polyql/polyql/pkg/compiler/parser"
)

// Parser is the PromQL front end. It holds no state, so the single registered
// instance is safe to share across concurrent translations.
type Parser struct{}

// DSL returns "promql".
func (Parser) DSL() string { return DSL }

// Parse turns PromQL text into a PromQL AST.
func (Parser) Parse(input string) (ast.Node, error) {
	expr, err := Parse(input)
	if err != nil {
		// Returning expr directly here would hand back a non-nil interface
		// wrapping a nil pointer.
		return nil, err
	}
	return expr, nil
}

func init() { parser.Register(Parser{}) }

// ParseError reports a lexing or parsing failure along with where in the query
// it occurred.
type ParseError struct {
	// Pos is the byte offset of the offending token.
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

// Binary operator precedence, lowest binding first. PromQL's table runs
// or < and/unless < comparison < +,- < *,/,%,atan2 < ^.
const (
	precLowest = iota
	precOr
	precAnd
	precCompare
	precAdd
	precMul
	precPow
)

// binaryPrecedence returns the operator's precedence, and whether the token is
// a binary operator at all.
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
	case MUL, DIV, MOD, ATAN2:
		return precMul, true
	case POW:
		return precPow, true
	}
	return 0, false
}

// Parse parses a PromQL query into its AST.
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

	p := &promqlParser{input: input, tokens: tokens}

	// The parser reports errors by panicking with a *ParseError so that the
	// recursive-descent code can stay free of error plumbing. Any other panic
	// is a genuine bug and is left to propagate.
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

// promqlParser walks the token stream. Lexing happens up front because a few
// constructs — telling a range vector apart from a subquery, or an aggregation
// clause written before its parentheses from one written after — need lookahead
// past the current token.
type promqlParser struct {
	input  string
	tokens []Token
	idx    int
}

func (p *promqlParser) cur() Token { return p.tokens[p.idx] }

func (p *promqlParser) peek() Token {
	if p.idx+1 >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.idx+1]
}

func (p *promqlParser) advance() {
	if p.idx < len(p.tokens)-1 {
		p.idx++
	}
}

func (p *promqlParser) errorf(pos int, format string, args ...any) {
	panic(&ParseError{Pos: pos, Query: p.input, Msg: fmt.Sprintf(format, args...)})
}

// expect consumes the current token if it has the wanted type, and fails
// otherwise.
func (p *promqlParser) expect(t TokenType) Token {
	tok := p.cur()
	if tok.Type != t {
		p.errorf(tok.Pos, "expected %q, got %s", t.String(), describe(tok))
	}
	p.advance()
	return tok
}

// describe renders a token for an error message.
func describe(tok Token) string {
	switch tok.Type {
	case EOF:
		return "end of input"
	case IDENTIFIER, METRIC_IDENTIFIER, NUMBER, DURATION:
		return fmt.Sprintf("%q", tok.Val)
	case STRING:
		return "string " + tok.Val
	default:
		return fmt.Sprintf("%q", tok.Type.String())
	}
}

// parseExpr is the precedence-climbing core. It parses a unary expression and
// then absorbs binary operators whose precedence binds at least as tightly as
// minPrec.
func (p *promqlParser) parseExpr(minPrec int) Expr {
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

		// Recursing at prec+1 leaves an operator of equal precedence for the
		// enclosing loop, making it left-associative. Power recurses at its own
		// precedence so the right-hand side absorbs it instead, which is what
		// makes ^ right-associative.
		nextMin := prec + 1
		if op == POW {
			nextMin = prec
		}
		right := p.parseExpr(nextMin)

		p.checkBinaryOperands(op, opPos, left, right, matching, returnBool)
		left = &BinaryExpr{Op: op, LHS: left, RHS: right, VectorMatching: matching, ReturnBool: returnBool}
	}
}

// parseVectorMatching reads the on/ignoring and group_left/group_right clauses
// that may follow a binary operator.
func (p *promqlParser) parseVectorMatching(op TokenType) *VectorMatching {
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
		// The label list is optional: "foo * group_left bar" is valid.
		if p.cur().Type == LEFT_PAREN {
			vm.Include = p.parseLabelList()
		}
	}

	if vm != nil && op.IsSetOperator() {
		// Set operations match many-to-many by definition.
		vm.Card = CardManyToMany
	}
	return vm
}

// parseLabelList reads a parenthesised, comma-separated list of label names.
func (p *promqlParser) parseLabelList() []string {
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

// parseLabelName reads a single label name. Keywords are accepted here because
// a series may legitimately carry a label called "by" or "count"; only the
// colon-bearing metric identifiers are rejected, since label names may not
// contain colons.
func (p *promqlParser) parseLabelName() string {
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

// parseUnaryExpr handles a leading sign, then falls through to an atom.
func (p *promqlParser) parseUnaryExpr() Expr {
	tok := p.cur()
	if tok.Type != ADD && tok.Type != SUB {
		return p.parseAtom()
	}
	p.advance()

	// A unary sign binds more tightly than multiplication but less tightly than
	// exponentiation, so -2^2 is -(2^2) rather than (-2)^2.
	operand := p.parseExpr(precMul + 1)
	if t := operand.Type(); t != ValueTypeScalar && t != ValueTypeVector {
		p.errorf(tok.Pos, "unary expression only allowed on scalar or instant vector, got %s", t)
	}
	return &UnaryExpr{Op: tok.Type, Expr: operand}
}

// parseAtom parses a primary expression and then any suffixes that attach to
// it: a range or subquery bracket, an offset, or an @ modifier.
func (p *promqlParser) parseAtom() Expr {
	expr := p.parsePrimaryExpr()
	for {
		switch p.cur().Type {
		case LEFT_BRACKET:
			expr = p.parseBracketSuffix(expr)
		case OFFSET:
			p.parseOffset(expr)
		case AT:
			p.parseAt(expr)
		default:
			return expr
		}
	}
}

func (p *promqlParser) parsePrimaryExpr() Expr {
	tok := p.cur()

	switch tok.Type {
	case NUMBER:
		p.advance()
		return &NumberLiteral{Val: p.numberValue(tok)}

	case STRING:
		p.advance()
		return &StringLiteral{Val: p.stringValue(tok)}

	case LEFT_PAREN:
		p.advance()
		inner := p.parseExpr(precLowest)
		p.expect(RIGHT_PAREN)
		return &ParenExpr{Expr: inner}

	case LEFT_BRACE:
		return p.parseVectorSelector()

	case IDENTIFIER, METRIC_IDENTIFIER:
		// A name is a function call only when it is immediately followed by an
		// opening parenthesis; otherwise it names a series.
		if tok.Type == IDENTIFIER && p.peek().Type == LEFT_PAREN {
			fn, ok := LookupFunction(tok.Val)
			if !ok {
				p.errorf(tok.Pos, "unknown function %q", tok.Val)
			}
			return p.parseCall(fn)
		}
		return p.parseVectorSelector()
	}

	if tok.Type.IsAggregator() {
		return p.parseAggregateExpr()
	}
	p.errorf(tok.Pos, "unexpected %s", describe(tok))
	return nil
}

// parseVectorSelector reads a metric name, a brace-enclosed matcher set, or
// both.
func (p *promqlParser) parseVectorSelector() Expr {
	tok := p.cur()
	pos := tok.Pos

	name := ""
	if tok.Type == IDENTIFIER || tok.Type == METRIC_IDENTIFIER {
		name = tok.Val
		p.advance()
	}

	var matchers []*LabelMatcher
	if p.cur().Type == LEFT_BRACE {
		matchers = p.parseLabelMatchers()
	}

	selector := &VectorSelector{Name: name, LabelMatchers: matchers}
	p.checkSelector(selector, pos)
	return selector
}

func (p *promqlParser) parseLabelMatchers() []*LabelMatcher {
	p.expect(LEFT_BRACE)
	matchers := []*LabelMatcher{}
	if p.cur().Type == RIGHT_BRACE {
		p.advance()
		return matchers
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
			if _, err := compileAnchoredRegexp(matcher.Value); err != nil {
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
	return matchers
}

// checkSelector enforces PromQL's rule that a selector must match something:
// it needs a metric name, or a matcher that cannot be satisfied by a series
// simply lacking the label.
func (p *promqlParser) checkSelector(vs *VectorSelector, pos int) {
	if vs.Name != "" {
		return
	}
	for _, m := range vs.LabelMatchers {
		if !matcherMatchesEmpty(m) {
			return
		}
	}
	p.errorf(pos, "vector selector must contain at least one non-empty matcher")
}

// compileAnchoredRegexp compiles a matcher's pattern the way PromQL evaluates
// it: RE2, anchored at both ends.
func compileAnchoredRegexp(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("^(?:" + pattern + ")$")
}

// matcherMatchesEmpty reports whether the matcher is satisfied by a series that
// does not carry the label at all.
func matcherMatchesEmpty(m *LabelMatcher) bool {
	switch m.Type {
	case MatchEqual:
		return m.Value == ""
	case MatchNotEqual:
		return m.Value != ""
	case MatchRegexp, MatchNotRegexp:
		re, err := compileAnchoredRegexp(m.Value)
		if err != nil {
			return false
		}
		if m.Type == MatchRegexp {
			return re.MatchString("")
		}
		return !re.MatchString("")
	}
	return false
}

func (p *promqlParser) parseCall(fn *Function) Expr {
	nameTok := p.cur()
	p.advance()
	p.expect(LEFT_PAREN)

	args := []Expr{}
	if p.cur().Type == RIGHT_PAREN {
		p.advance()
	} else {
		for {
			args = append(args, p.parseExpr(precLowest))
			if p.cur().Type != COMMA {
				break
			}
			p.advance()
		}
		p.expect(RIGHT_PAREN)
	}

	p.checkCall(fn, args, nameTok.Pos)
	return &Call{Func: fn, Args: args}
}

// checkCall validates a call's arity and argument types. PromQL's grammar is
// type-directed, so catching rate(foo) here gives a far better message than
// letting it reach the resolver.
func (p *promqlParser) checkCall(fn *Function, args []Expr, pos int) {
	fixed := len(fn.ArgTypes)
	switch {
	case fn.Variadic == 0:
		if len(args) != fixed {
			p.errorf(pos, "expected %d argument(s) in call to %q, got %d", fixed, fn.Name, len(args))
		}
	default:
		required := fixed - 1
		if len(args) < required {
			p.errorf(pos, "expected at least %d argument(s) in call to %q, got %d", required, fn.Name, len(args))
		}
		if fn.Variadic > 0 {
			if max := required + fn.Variadic; len(args) > max {
				p.errorf(pos, "expected at most %d argument(s) in call to %q, got %d", max, fn.Name, len(args))
			}
		}
	}

	for i, arg := range args {
		want := fn.ArgTypes[min(i, fixed-1)]
		if got := arg.Type(); got != want {
			p.errorf(pos, "argument %d of %q expects type %s, got %s", i+1, fn.Name, want, got)
		}
	}
}

// parseAggregateExpr handles both spellings PromQL allows: the grouping clause
// may precede or follow the parenthesised expression.
func (p *promqlParser) parseAggregateExpr() Expr {
	opTok := p.cur()
	op := opTok.Type
	p.advance()

	var grouping []string
	without := false
	haveGrouping := false

	if t := p.cur().Type; t == BY || t == WITHOUT {
		without = t == WITHOUT
		p.advance()
		grouping = p.parseLabelList()
		haveGrouping = true
	}

	p.expect(LEFT_PAREN)
	first := p.parseExpr(precLowest)
	var param Expr
	expr := first
	if p.cur().Type == COMMA {
		p.advance()
		param = first
		expr = p.parseExpr(precLowest)
	}
	p.expect(RIGHT_PAREN)

	if !haveGrouping {
		if t := p.cur().Type; t == BY || t == WITHOUT {
			without = t == WITHOUT
			p.advance()
			grouping = p.parseLabelList()
		}
	}

	switch {
	case op.IsAggregatorWithParam() && param == nil:
		p.errorf(opTok.Pos, "aggregation %q requires a parameter, as in %s(<param>, <vector>)", op, op)
	case !op.IsAggregatorWithParam() && param != nil:
		p.errorf(opTok.Pos, "aggregation %q does not take a parameter", op)
	}
	if t := expr.Type(); t != ValueTypeVector {
		p.errorf(opTok.Pos, "aggregation %q expects an instant vector, got %s", op, t)
	}
	if param != nil {
		wantParam := ValueTypeScalar
		if op == COUNT_VALUES {
			wantParam = ValueTypeString
		}
		if t := param.Type(); t != wantParam {
			p.errorf(opTok.Pos, "parameter of aggregation %q expects type %s, got %s", op, wantParam, t)
		}
	}

	return &AggregateExpr{Op: op, Expr: expr, Param: param, Grouping: grouping, Without: without}
}

// parseBracketSuffix handles both bracket forms: [5m] turns a selector into a
// range vector, and [30m:1m] turns any instant-vector expression into a
// subquery.
func (p *promqlParser) parseBracketSuffix(expr Expr) Expr {
	openPos := p.cur().Pos
	p.advance()

	durTok := p.cur()
	if durTok.Type != DURATION {
		p.errorf(durTok.Pos, "expected a duration inside [], got %s", describe(durTok))
	}
	rangeDur := mustParseDuration(durTok.Val)
	p.advance()

	if p.cur().Type == COLON {
		p.advance()
		step := time.Duration(0)
		if p.cur().Type == DURATION {
			step = mustParseDuration(p.cur().Val)
			p.advance()
		}
		p.expect(RIGHT_BRACKET)
		if t := expr.Type(); t != ValueTypeVector {
			p.errorf(openPos, "subquery is only allowed on an instant vector, got %s", t)
		}
		return &SubqueryExpr{Expr: expr, Range: rangeDur, Step: step}
	}
	p.expect(RIGHT_BRACKET)

	selector, ok := expr.(*VectorSelector)
	if !ok {
		p.errorf(openPos, "a range must follow a series selector; to take a range of an expression, "+
			"write a subquery such as [%s:]", FormatDuration(rangeDur))
	}
	if selector.OriginalOffset != 0 || selector.At != nil {
		p.errorf(openPos, "the offset and @ modifiers must be written after the range, not before it")
	}
	return &MatrixSelector{VectorSelector: selector, Range: rangeDur}
}

func (p *promqlParser) parseOffset(expr Expr) {
	offsetPos := p.cur().Pos
	p.advance()

	negative := false
	switch p.cur().Type {
	case SUB:
		negative = true
		p.advance()
	case ADD:
		p.advance()
	}

	durTok := p.cur()
	if durTok.Type != DURATION {
		p.errorf(durTok.Pos, "expected a duration after offset, got %s", describe(durTok))
	}
	p.advance()

	offset := mustParseDuration(durTok.Val)
	if negative {
		offset = -offset
	}

	switch node := expr.(type) {
	case *VectorSelector:
		p.setOffset(&node.OriginalOffset, offset, offsetPos)
	case *MatrixSelector:
		// The offset belongs to the underlying selector; it is only written
		// after the range.
		p.setOffset(&node.VectorSelector.OriginalOffset, offset, offsetPos)
	case *SubqueryExpr:
		p.setOffset(&node.OriginalOffset, offset, offsetPos)
	default:
		p.errorf(offsetPos, "the offset modifier must follow a selector or a subquery")
	}
}

func (p *promqlParser) setOffset(dst *time.Duration, offset time.Duration, pos int) {
	if *dst != 0 {
		p.errorf(pos, "the offset modifier may only be set once per selector")
	}
	*dst = offset
}

func (p *promqlParser) parseAt(expr Expr) {
	atPos := p.cur().Pos
	p.advance()

	var modifier *AtModifier
	switch p.cur().Type {
	case START, END:
		preset := AtStart
		if p.cur().Type == END {
			preset = AtEnd
		}
		p.advance()
		p.expect(LEFT_PAREN)
		p.expect(RIGHT_PAREN)
		modifier = &AtModifier{Preset: preset}
	default:
		negative := false
		switch p.cur().Type {
		case SUB:
			negative = true
			p.advance()
		case ADD:
			p.advance()
		}
		numTok := p.cur()
		if numTok.Type != NUMBER {
			p.errorf(numTok.Pos, "expected a timestamp, start() or end() after @, got %s", describe(numTok))
		}
		p.advance()
		value := p.numberValue(numTok)
		if negative {
			value = -value
		}
		modifier = &AtModifier{Preset: AtTimestamp, Timestamp: value}
	}

	var dst **AtModifier
	switch node := expr.(type) {
	case *VectorSelector:
		dst = &node.At
	case *MatrixSelector:
		dst = &node.VectorSelector.At
	case *SubqueryExpr:
		dst = &node.At
	default:
		p.errorf(atPos, "the @ modifier must follow a selector or a subquery")
	}
	if *dst != nil {
		p.errorf(atPos, "the @ modifier may only be set once per selector")
	}
	*dst = modifier
}

// mustParseDuration re-parses duration text the lexer already validated.
func mustParseDuration(text string) time.Duration {
	d, err := ParseDuration(text)
	if err != nil {
		// Unreachable: the lexer rejects malformed durations before emitting a
		// DURATION token.
		panic(fmt.Sprintf("promql: lexer produced an invalid duration %q: %s", text, err))
	}
	return d
}

// numberValue converts a NUMBER token's text to its value, covering PromQL's
// decimal, hexadecimal, NaN and Inf spellings and the underscores it allows as
// digit separators.
func (p *promqlParser) numberValue(tok Token) float64 {
	text := strings.ReplaceAll(tok.Val, "_", "")
	switch strings.ToLower(text) {
	case "nan":
		return math.NaN()
	case "inf":
		return math.Inf(1)
	}
	if strings.HasPrefix(text, "0x") || strings.HasPrefix(text, "0X") {
		n, err := strconv.ParseInt(text, 0, 64)
		if err != nil {
			p.errorf(tok.Pos, "invalid hexadecimal number %q: %s", tok.Val, err)
		}
		return float64(n)
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		p.errorf(tok.Pos, "invalid number %q", tok.Val)
	}
	return value
}

// stringValue unquotes a STRING token. PromQL takes Go escape sequences in
// double- and single-quoted strings, and treats backquoted strings as raw.
func (p *promqlParser) stringValue(tok Token) string {
	value, err := unquote(tok.Val)
	if err != nil {
		p.errorf(tok.Pos, "invalid string literal: %s", err)
	}
	return value
}

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
		// strconv.Unquote treats single quotes as rune literals, so rewrite the
		// literal as an equivalent double-quoted one first.
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

// checkBinaryOperands enforces PromQL's typing rules for binary operators.
func (p *promqlParser) checkBinaryOperands(op TokenType, pos int, lhs, rhs Expr, vm *VectorMatching, returnBool bool) {
	lt, rt := lhs.Type(), rhs.Type()

	for _, t := range []ValueType{lt, rt} {
		if t != ValueTypeScalar && t != ValueTypeVector {
			p.errorf(pos, "binary expression operands must be scalar or instant vector, got %s", t)
		}
	}
	if op.IsSetOperator() && (lt != ValueTypeVector || rt != ValueTypeVector) {
		p.errorf(pos, "set operator %q requires instant vectors on both sides", op)
	}
	if op.IsComparison() && !returnBool && lt == ValueTypeScalar && rt == ValueTypeScalar {
		p.errorf(pos, "comparison between two scalars must use the bool modifier")
	}
	if vm != nil && (lt == ValueTypeScalar || rt == ValueTypeScalar) {
		p.errorf(pos, "vector matching is only allowed between two instant vectors")
	}
}
