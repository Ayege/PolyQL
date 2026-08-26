package traceql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/ast"
	"github.com/polyql/polyql/pkg/compiler/parser"
)

// Parser is the TraceQL front end.
type Parser struct{}

// DSL returns "traceql".
func (Parser) DSL() string { return DSL }

// Parse turns TraceQL text into a TraceQL AST.
func (Parser) Parse(input string) (ast.Node, error) {
	expr, err := ParseExpr(input)
	if err != nil {
		return nil, err
	}
	return expr, nil
}

func init() { parser.Register(Parser{}) }

// ParseExpr parses a TraceQL query into its AST.
func ParseExpr(input string) (Expr, error) {
	p := newParser(input)
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.cur.Type != EOF {
		return nil, p.errorf(p.cur, "unexpected %s after the query", describe(p.cur))
	}
	return expr, nil
}

type traceqlParser struct {
	lex  *Lexer
	cur  Token
	peek Token
	// input is kept so an error can quote the offending text in context.
	input string
}

func newParser(input string) *traceqlParser {
	p := &traceqlParser{lex: NewLexer(input), input: input}
	// Prime both slots: the grammar needs one token of lookahead to tell a
	// scope prefix from a bare attribute, and an aggregate call from an
	// attribute that happens to be named "count".
	p.cur = p.lex.Next()
	p.peek = p.lex.Next()
	return p
}

func (p *traceqlParser) next() {
	p.cur = p.peek
	p.peek = p.lex.Next()
}

// describe renders a token for an error message.
func describe(tok Token) string {
	switch tok.Type {
	case EOF:
		return "end of input"
	case ERROR:
		return tok.Val
	case IDENTIFIER, NUMBER, DURATION, STRING:
		return fmt.Sprintf("%q", tok.Val)
	default:
		return fmt.Sprintf("%q", tok.Type.String())
	}
}

func (p *traceqlParser) errorf(tok Token, format string, args ...any) error {
	return fmt.Errorf("traceql: at offset %d: %s", tok.Pos, fmt.Sprintf(format, args...))
}

// check reports a scanning failure as a parse error, so a caller sees one error
// type rather than having to inspect tokens.
func (p *traceqlParser) check() error {
	if p.cur.Type == ERROR {
		return p.errorf(p.cur, "%s", p.cur.Val)
	}
	return nil
}

func (p *traceqlParser) expect(t TokenType) (Token, error) {
	if err := p.check(); err != nil {
		return Token{}, err
	}
	if p.cur.Type != t {
		return Token{}, p.errorf(p.cur, "expected %q but found %s", t.String(), describe(p.cur))
	}
	tok := p.cur
	p.next()
	return tok, nil
}

// parseExpr parses a span-set expression: a chain of structural relationships.
//
// The structural operators are left-associative and share one precedence level,
// so "{a} > {b} > {c}" means "({a} > {b}) > {c}" — the grandchildren of {a}
// filtered by {c}, not {a} related to a nested relationship.
func (p *traceqlParser) parseExpr() (Expr, error) {
	left, err := p.parseUnaryExpr()
	if err != nil {
		return nil, err
	}

	for {
		if err := p.check(); err != nil {
			return nil, err
		}
		op, ok := p.cur.Type.StructuralOp()
		if !ok {
			return left, nil
		}
		p.next()
		right, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		left = &StructuralExpr{Op: op, LHS: left, RHS: right}
	}
}

// parseUnaryExpr parses one span-set operand together with any "as" cast
// following it. A cast binds tighter than a structural operator, so
// "{a} as (span.x: int) > {b}" casts only the left operand.
func (p *traceqlParser) parseUnaryExpr() (Expr, error) {
	expr, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	for p.cur.Type == AS {
		expr, err = p.parseCoercion(expr)
		if err != nil {
			return nil, err
		}
	}
	return expr, nil
}

// parseOperand parses a span set, an aggregate, or a parenthesised expression.
func (p *traceqlParser) parseOperand() (Expr, error) {
	if err := p.check(); err != nil {
		return nil, err
	}

	switch p.cur.Type {
	case LEFT_BRACE:
		return p.parseSpanset()

	case LEFT_PAREN:
		p.next()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(RIGHT_PAREN); err != nil {
			return nil, err
		}
		return &ParenExpr{Inner: inner}, nil

	case IDENTIFIER:
		// An aggregate is the only thing a bare word may start, and only when a
		// "(" follows it. Anything else is a filter written without its braces,
		// which is the commonest TraceQL mistake worth naming precisely.
		if p.peek.Type == LEFT_PAREN {
			return p.parseAggregate()
		}
		return nil, p.errorf(p.cur,
			"expected a span set but found %s; a TraceQL filter is written between braces, as {%s ...}",
			describe(p.cur), p.cur.Val)

	default:
		return nil, p.errorf(p.cur, "expected a span set but found %s", describe(p.cur))
	}
}

// parseSpanset parses "{ fieldExpr }", including the empty selector "{}".
func (p *traceqlParser) parseSpanset() (Expr, error) {
	if _, err := p.expect(LEFT_BRACE); err != nil {
		return nil, err
	}
	if p.cur.Type == RIGHT_BRACE {
		p.next()
		return &Spanset{}, nil
	}

	filter, err := p.parseFieldExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(RIGHT_BRACE); err != nil {
		return nil, err
	}
	return &Spanset{Filter: filter}, nil
}

// parseFieldExpr parses the boolean expression between a span set's braces.
//
// Precedence follows TraceQL: || binds loosest, then &&, then a leading !, with
// a comparison or a parenthesised group at the leaves.
func (p *traceqlParser) parseFieldExpr() (FieldExpr, error) { return p.parseFieldOr() }

func (p *traceqlParser) parseFieldOr() (FieldExpr, error) {
	left, err := p.parseFieldAnd()
	if err != nil {
		return nil, err
	}
	for p.cur.Type == OR {
		p.next()
		right, err := p.parseFieldAnd()
		if err != nil {
			return nil, err
		}
		left = &FieldBinary{Op: BoolOr, LHS: left, RHS: right}
	}
	return left, nil
}

func (p *traceqlParser) parseFieldAnd() (FieldExpr, error) {
	left, err := p.parseFieldNot()
	if err != nil {
		return nil, err
	}
	for p.cur.Type == AND {
		p.next()
		right, err := p.parseFieldNot()
		if err != nil {
			return nil, err
		}
		left = &FieldBinary{Op: BoolAnd, LHS: left, RHS: right}
	}
	return left, nil
}

func (p *traceqlParser) parseFieldNot() (FieldExpr, error) {
	if p.cur.Type == NOT {
		p.next()
		inner, err := p.parseFieldNot()
		if err != nil {
			return nil, err
		}
		return &FieldNot{Inner: inner}, nil
	}
	return p.parseFieldPrimary()
}

func (p *traceqlParser) parseFieldPrimary() (FieldExpr, error) {
	if err := p.check(); err != nil {
		return nil, err
	}

	if p.cur.Type == LEFT_PAREN {
		p.next()
		inner, err := p.parseFieldExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(RIGHT_PAREN); err != nil {
			return nil, err
		}
		return &FieldParen{Inner: inner}, nil
	}

	attribute, err := p.parseAttribute()
	if err != nil {
		return nil, err
	}

	if err := p.check(); err != nil {
		return nil, err
	}
	op, ok := p.cur.Type.CompareOp()
	if !ok {
		return nil, p.errorf(p.cur, "expected a comparison operator after %s but found %s",
			attribute.String(), describe(p.cur))
	}
	p.next()

	value, err := p.parseValue(attribute, op)
	if err != nil {
		return nil, err
	}
	return &SpansetFilter{Attribute: attribute, Op: op, Value: value}, nil
}

// parseAttribute parses a scoped or bare attribute path.
//
// TraceQL writes three shapes, and they mean different things:
//
//	duration                  a span-model intrinsic
//	.http.status_code         an unscoped attribute, resolved by the backend
//	span.http.status_code     an attribute on the span
//	resource.service.name     an attribute on the resource
//
// A leading "intrinsic." is also accepted. It is not Tempo's own spelling —
// intrinsics are bare there — but it states the scope explicitly, and a query
// arriving from another tool may well use it.
func (p *traceqlParser) parseAttribute() (*Attribute, error) {
	if err := p.check(); err != nil {
		return nil, err
	}

	// The leading-dot form: an unscoped attribute.
	if p.cur.Type == DOT {
		p.next()
		name, err := p.parseAttributePath()
		if err != nil {
			return nil, err
		}
		return &Attribute{Scope: ScopeNone, Name: name}, nil
	}

	if p.cur.Type != IDENTIFIER {
		return nil, p.errorf(p.cur, "expected an attribute name but found %s", describe(p.cur))
	}

	// A scope prefix only counts as one when a "." follows it. Without that,
	// "span" is an ordinary attribute name.
	if scope, ok := scopesByName[strings.ToLower(p.cur.Val)]; ok && p.peek.Type == DOT {
		p.next() // the scope word
		p.next() // the dot
		name, err := p.parseAttributePath()
		if err != nil {
			return nil, err
		}
		return &Attribute{Scope: scope, Name: name, Explicit: true}, nil
	}

	name, err := p.parseAttributePath()
	if err != nil {
		return nil, err
	}
	// A bare word that names a span-model field is an intrinsic; anything else
	// is an unscoped attribute the backend resolves.
	if Intrinsics[name] {
		return &Attribute{Scope: ScopeIntrinsic, Name: name}, nil
	}
	return &Attribute{Scope: ScopeNone, Name: name}, nil
}

// parseAttributePath reads a dotted attribute name. Keywords are accepted inside
// a path, so "span.by" and "resource.as" remain addressable.
func (p *traceqlParser) parseAttributePath() (string, error) {
	var parts []string
	for {
		if err := p.check(); err != nil {
			return "", err
		}
		switch p.cur.Type {
		case IDENTIFIER, AS, BY, OVER:
			parts = append(parts, p.cur.Val)
			p.next()
		default:
			if len(parts) == 0 {
				return "", p.errorf(p.cur, "expected an attribute name but found %s", describe(p.cur))
			}
			return "", p.errorf(p.cur, "expected an attribute name after %q but found %s",
				strings.Join(parts, ".")+".", describe(p.cur))
		}
		if p.cur.Type != DOT {
			return strings.Join(parts, "."), nil
		}
		p.next()
	}
}

// statusWords and kindWords are the bare words TraceQL admits as operands of the
// status and kind intrinsics. They are unquoted in the source, so without this
// list they would scan as attribute names and fail as a missing operator.
var statusWords = map[string]bool{"ok": true, "error": true, "unset": true}

var kindWords = map[string]bool{
	"server": true, "client": true, "producer": true,
	"consumer": true, "internal": true, "unspecified": true,
}

// parseValue parses a comparison's right-hand side. The attribute is passed in
// so that a bare word can be read as a status or kind keyword where one of those
// intrinsics is what is being compared.
func (p *traceqlParser) parseValue(attribute *Attribute, op CompareOp) (*Value, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	tok := p.cur

	switch tok.Type {
	case STRING:
		unquoted, err := unquote(tok.Val)
		if err != nil {
			return nil, p.errorf(tok, "%s", err)
		}
		p.next()
		return &Value{Kind: ValueString, Text: tok.Val, Str: unquoted}, nil

	case NUMBER:
		value, err := strconv.ParseFloat(strings.ReplaceAll(tok.Val, "_", ""), 64)
		if err != nil {
			return nil, p.errorf(tok, "invalid number %q", tok.Val)
		}
		p.next()
		return &Value{Kind: ValueNumber, Text: tok.Val, Number: value}, nil

	case DURATION:
		d, err := ParseDuration(tok.Val)
		if err != nil {
			return nil, p.errorf(tok, "%s", err)
		}
		p.next()
		return &Value{Kind: ValueDuration, Text: tok.Val, Duration: d}, nil

	case IDENTIFIER:
		word := strings.ToLower(tok.Val)
		switch {
		case word == "true" || word == "false":
			p.next()
			return &Value{Kind: ValueBool, Text: tok.Val, Bool: word == "true"}, nil
		case attribute.Scope == ScopeIntrinsic && attribute.Name == "status" && statusWords[word]:
			p.next()
			return &Value{Kind: ValueStatus, Text: tok.Val, Str: word}, nil
		case attribute.Scope == ScopeIntrinsic && attribute.Name == "kind" && kindWords[word]:
			p.next()
			return &Value{Kind: ValueSpanKind, Text: tok.Val, Str: word}, nil
		}
		return nil, p.errorf(tok,
			"expected a value after %s %s but found the bare word %q; "+
				"string operands are quoted in TraceQL",
			attribute.String(), op, tok.Val)

	default:
		return nil, p.errorf(tok, "expected a value after %s %s but found %s",
			attribute.String(), op, describe(tok))
	}
}

// parseAggregate parses "op(attr?) over (expr) [by (attrs)]".
//
// TraceQL writes aggregation prefix, with its operand introduced by "over",
// where PromQL and LogQL both wrap the operand in the call itself. The shape is
// the reason this is its own rule rather than a case of a general call parser.
func (p *traceqlParser) parseAggregate() (Expr, error) {
	nameTok := p.cur
	op, ok := LookupAggregate(nameTok.Val)
	if !ok {
		return nil, p.errorf(nameTok, "unknown aggregate %q (TraceQL has %s)",
			nameTok.Val, strings.Join(FunctionNames(), ", "))
	}
	p.next()

	if _, err := p.expect(LEFT_PAREN); err != nil {
		return nil, err
	}

	var attribute *Attribute
	if p.cur.Type != RIGHT_PAREN {
		parsed, err := p.parseAttribute()
		if err != nil {
			return nil, err
		}
		attribute = parsed
	}
	if _, err := p.expect(RIGHT_PAREN); err != nil {
		return nil, err
	}

	// count() counts spans and takes nothing; the others need to know which
	// attribute's values they are combining. Catching the mismatch here means
	// the message can point at the call rather than at a later type error.
	switch {
	case op.TakesAttribute() && attribute == nil:
		return nil, p.errorf(nameTok, "%s() needs the attribute to aggregate, as %s(span.duration)",
			op, op)
	case !op.TakesAttribute() && attribute != nil:
		return nil, p.errorf(nameTok, "%s() counts spans and takes no attribute", op)
	}

	if _, err := p.expect(OVER); err != nil {
		return nil, err
	}
	if _, err := p.expect(LEFT_PAREN); err != nil {
		return nil, err
	}
	operand, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(RIGHT_PAREN); err != nil {
		return nil, err
	}

	aggregate := &AggregateExpr{Op: op, Attribute: attribute, Operand: operand}

	if p.cur.Type == BY {
		grouping, err := p.parseGrouping()
		if err != nil {
			return nil, err
		}
		aggregate.Grouping = grouping
	}
	return aggregate, nil
}

func (p *traceqlParser) parseGrouping() (*Grouping, error) {
	if _, err := p.expect(BY); err != nil {
		return nil, err
	}
	if _, err := p.expect(LEFT_PAREN); err != nil {
		return nil, err
	}

	var attributes []*Attribute
	for {
		attribute, err := p.parseAttribute()
		if err != nil {
			return nil, err
		}
		attributes = append(attributes, attribute)
		if p.cur.Type != COMMA {
			break
		}
		p.next()
	}

	if _, err := p.expect(RIGHT_PAREN); err != nil {
		return nil, err
	}
	return &Grouping{Attributes: attributes}, nil
}

// parseCoercion parses "as (attribute: type)".
//
// Tempo spells its cast inside the comparison; writing it as a suffix on the
// span set is what lets the IR carry it as a stage that applies to everything
// after it, rather than as a corner of one predicate. The attribute is named
// explicitly for the same reason: a cast with no subject cannot be translated
// into a language that casts per reference.
func (p *traceqlParser) parseCoercion(expr Expr) (Expr, error) {
	if _, err := p.expect(AS); err != nil {
		return nil, err
	}
	if _, err := p.expect(LEFT_PAREN); err != nil {
		return nil, err
	}

	attribute, err := p.parseAttribute()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(COLON); err != nil {
		return nil, err
	}

	if err := p.check(); err != nil {
		return nil, err
	}
	if p.cur.Type != IDENTIFIER {
		return nil, p.errorf(p.cur, "expected a type name but found %s (one of %s)",
			describe(p.cur), strings.Join(CoercionTypeNames(), ", "))
	}
	target, ok := coercionTypesByName[strings.ToLower(p.cur.Val)]
	if !ok {
		return nil, p.errorf(p.cur, "unknown cast target %q (one of %s)",
			p.cur.Val, strings.Join(CoercionTypeNames(), ", "))
	}
	p.next()

	if _, err := p.expect(RIGHT_PAREN); err != nil {
		return nil, err
	}
	return &AttributeCoercion{Expr: expr, Attribute: attribute, AsType: target}, nil
}

// unquote resolves a TraceQL string literal's escapes. Backquoted strings are
// raw, matching Go's own rule and TraceQL's use of them for regex patterns.
func unquote(text string) (string, error) {
	if len(text) < 2 {
		return "", fmt.Errorf("malformed string literal %q", text)
	}
	switch text[0] {
	case '`':
		return text[1 : len(text)-1], nil
	case '\'':
		// Go's strconv reads single quotes as a rune literal, so a
		// single-quoted string is re-quoted before being unquoted.
		inner := text[1 : len(text)-1]
		return strconv.Unquote(`"` + strings.ReplaceAll(inner, `"`, `\"`) + `"`)
	default:
		return strconv.Unquote(text)
	}
}
