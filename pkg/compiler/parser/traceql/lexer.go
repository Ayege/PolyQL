package traceql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TokenType identifies a lexical token in TraceQL.
type TokenType int

const (
	// EOF marks the end of input.
	EOF TokenType = iota
	// ERROR carries a scanning failure; Token.Val holds the message.
	ERROR

	// Literals and names.
	IDENTIFIER
	STRING
	NUMBER
	DURATION

	// Punctuation.
	LEFT_PAREN
	RIGHT_PAREN
	LEFT_BRACE
	RIGHT_BRACE
	LEFT_BRACKET
	RIGHT_BRACKET
	COMMA
	DOT
	SEMICOLON
	COLON
	QUESTION
	// PIPE is the bare "|", which TraceQL uses to introduce a scalar filter on
	// an aggregate. It is scanned so that a query using one fails in the parser
	// with a message about the construct rather than in the lexer with one about
	// an unexpected character.
	PIPE

	// Comparison operators.
	EQ        // =
	NEQ       // !=
	LT        // <
	GT        // >
	LTE       // <=
	GTE       // >=
	REGEX     // =~
	NOT_REGEX // !~

	// Boolean connectives.
	AND // &&
	OR  // ||
	NOT // !

	// Structural operators. TILDE and RSHIFT relate two span sets; GT above
	// doubles as the child operator, which the parser tells apart by position.
	TILDE  // ~
	RSHIFT // >>

	// Keywords.
	AS
	BY
	OVER
)

var tokenText = map[TokenType]string{
	EOF:        "end of input",
	ERROR:      "error",
	IDENTIFIER: "identifier",
	STRING:     "string",
	NUMBER:     "number",
	DURATION:   "duration",

	LEFT_PAREN:    "(",
	RIGHT_PAREN:   ")",
	LEFT_BRACE:    "{",
	RIGHT_BRACE:   "}",
	LEFT_BRACKET:  "[",
	RIGHT_BRACKET: "]",
	COMMA:         ",",
	DOT:           ".",
	SEMICOLON:     ";",
	COLON:         ":",
	QUESTION:      "?",
	PIPE:          "|",

	EQ:        "=",
	NEQ:       "!=",
	LT:        "<",
	GT:        ">",
	LTE:       "<=",
	GTE:       ">=",
	REGEX:     "=~",
	NOT_REGEX: "!~",

	AND: "&&",
	OR:  "||",
	NOT: "!",

	TILDE:  "~",
	RSHIFT: ">>",

	AS:   "as",
	BY:   "by",
	OVER: "over",
}

func (t TokenType) String() string {
	if s, ok := tokenText[t]; ok {
		return s
	}
	return fmt.Sprintf("TokenType(%d)", int(t))
}

// keywords are the words TraceQL reserves unconditionally.
//
// The aggregate names — count, sum, avg, min, max — and the scope prefixes —
// span, resource, intrinsic — are deliberately absent. TraceQL only reads
// "count" as an aggregate when a "(" follows, and only reads "span" as a scope
// when a "." does, so scanning them as identifiers keeps an attribute genuinely
// named "count" filterable. That is the same choice the LogQL lexer makes for
// its own function names, and for the same reason.
var keywords = map[string]TokenType{
	"as":   AS,
	"by":   BY,
	"over": OVER,
}

// IsComparison reports whether the token compares an attribute against a value.
func (t TokenType) IsComparison() bool {
	switch t {
	case EQ, NEQ, LT, GT, LTE, GTE, REGEX, NOT_REGEX:
		return true
	}
	return false
}

// CompareOp maps a comparison token onto the AST operator it spells.
func (t TokenType) CompareOp() (CompareOp, bool) {
	switch t {
	case EQ:
		return OpEqual, true
	case NEQ:
		return OpNotEqual, true
	case LT:
		return OpLess, true
	case GT:
		return OpGreater, true
	case LTE:
		return OpLessEqual, true
	case GTE:
		return OpGreaterEqual, true
	case REGEX:
		return OpRegex, true
	case NOT_REGEX:
		return OpNotRegex, true
	}
	return 0, false
}

// IsStructural reports whether the token relates two span sets.
//
// GT is included because TraceQL spells the child operator ">", the same as the
// greater-than comparison. Only position tells them apart: a comparison follows
// an attribute inside braces, a structural operator follows a whole span set.
// The parser resolves that; the lexer does not try to.
func (t TokenType) IsStructural() bool {
	switch t {
	case GT, RSHIFT, TILDE:
		return true
	}
	return false
}

// StructuralOp maps a structural token onto the AST operator it spells.
func (t TokenType) StructuralOp() (StructuralOp, bool) {
	switch t {
	case GT:
		return StructChild, true
	case RSHIFT:
		return StructDescendant, true
	case TILDE:
		return StructSibling, true
	}
	return 0, false
}

// Token is one lexical item together with its byte offset in the source.
type Token struct {
	Type TokenType
	Val  string
	Pos  int
}

// Lexer scans TraceQL source into tokens.
type Lexer struct {
	input string
	pos   int
}

// NewLexer returns a Lexer over the given query text.
func NewLexer(input string) *Lexer { return &Lexer{input: input} }

// Tokenize scans the whole input, stopping at the first ERROR token. The
// returned slice always ends with EOF or ERROR.
func Tokenize(input string) []Token {
	lex := NewLexer(input)
	var tokens []Token
	for {
		tok := lex.Next()
		tokens = append(tokens, tok)
		if tok.Type == EOF || tok.Type == ERROR {
			return tokens
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isAlphaNumeric(c byte) bool { return isAlpha(c) || isDigit(c) }

func (l *Lexer) at(n int) byte {
	if l.pos+n >= len(l.input) {
		return 0
	}
	return l.input[l.pos+n]
}

func (l *Lexer) token(t TokenType, start int) Token {
	return Token{Type: t, Val: l.input[start:l.pos], Pos: start}
}

func (l *Lexer) errorf(start int, format string, args ...any) Token {
	return Token{Type: ERROR, Val: fmt.Sprintf(format, args...), Pos: start}
}

func (l *Lexer) skipSpaceAndComments() {
	for l.pos < len(l.input) {
		switch c := l.input[l.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.pos++
		case c == '#':
			for l.pos < len(l.input) && l.input[l.pos] != '\n' {
				l.pos++
			}
		default:
			return
		}
	}
}

// Next returns the next token.
func (l *Lexer) Next() Token {
	l.skipSpaceAndComments()
	start := l.pos
	if l.pos >= len(l.input) {
		return Token{Type: EOF, Pos: start}
	}

	c := l.input[l.pos]
	switch {
	case isAlpha(c):
		return l.scanIdentifier()
	case isDigit(c):
		return l.scanNumeric()
	case c == '.' && isDigit(l.at(1)):
		return l.scanNumeric()
	case c == '"' || c == '\'' || c == '`':
		return l.scanString()
	}

	l.pos++
	switch c {
	case '(':
		return l.token(LEFT_PAREN, start)
	case ')':
		return l.token(RIGHT_PAREN, start)
	case '{':
		return l.token(LEFT_BRACE, start)
	case '}':
		return l.token(RIGHT_BRACE, start)
	case '[':
		return l.token(LEFT_BRACKET, start)
	case ']':
		return l.token(RIGHT_BRACKET, start)
	case ',':
		return l.token(COMMA, start)
	case '.':
		return l.token(DOT, start)
	case ';':
		return l.token(SEMICOLON, start)
	case ':':
		return l.token(COLON, start)
	case '?':
		return l.token(QUESTION, start)
	case '~':
		return l.token(TILDE, start)
	case '&':
		if l.at(0) == '&' {
			l.pos++
			return l.token(AND, start)
		}
		return l.errorf(start, "unexpected %q: TraceQL spells conjunction %q", "&", "&&")
	case '|':
		if l.at(0) == '|' {
			l.pos++
			return l.token(OR, start)
		}
		return l.token(PIPE, start)
	case '=':
		if l.at(0) == '~' {
			l.pos++
			return l.token(REGEX, start)
		}
		// TraceQL accepts "==" as a synonym for "=", so accepting it here keeps
		// a query written that way from failing on a stray second character.
		if l.at(0) == '=' {
			l.pos++
			return l.token(EQ, start)
		}
		return l.token(EQ, start)
	case '!':
		switch l.at(0) {
		case '=':
			l.pos++
			return l.token(NEQ, start)
		case '~':
			l.pos++
			return l.token(NOT_REGEX, start)
		}
		return l.token(NOT, start)
	case '<':
		if l.at(0) == '=' {
			l.pos++
			return l.token(LTE, start)
		}
		return l.token(LT, start)
	case '>':
		switch l.at(0) {
		case '=':
			l.pos++
			return l.token(GTE, start)
		case '>':
			l.pos++
			return l.token(RSHIFT, start)
		}
		return l.token(GT, start)
	}

	return l.errorf(start, "unexpected character %q", string(rune(c)))
}

func (l *Lexer) scanIdentifier() Token {
	start := l.pos
	for l.pos < len(l.input) && isAlphaNumeric(l.input[l.pos]) {
		l.pos++
	}
	word := l.input[start:l.pos]
	if kw, ok := keywords[strings.ToLower(word)]; ok {
		return l.token(kw, start)
	}
	return l.token(IDENTIFIER, start)
}

// scanNumeric scans a number or a duration. Both share a numeric prefix, so the
// unit suffix decides which it is.
func (l *Lexer) scanNumeric() Token {
	start := l.pos
	l.scanDigits()
	if l.at(0) == '.' && isDigit(l.at(1)) {
		l.pos++
		l.scanDigits()
	} else if l.pos == start && l.at(0) == '.' {
		l.pos++
		l.scanDigits()
	}
	if c := l.at(0); c == 'e' || c == 'E' {
		if isDigit(l.at(1)) || ((l.at(1) == '+' || l.at(1) == '-') && isDigit(l.at(2))) {
			l.pos += 2
			l.scanDigits()
			return l.token(NUMBER, start)
		}
	}

	// Collect the unit suffix: letters, plus the micro sign used by "µs".
	unitStart := l.pos
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if isAlpha(c) || isDigit(c) {
			l.pos++
			continue
		}
		if c == 0xC2 && l.at(1) == 0xB5 { // the two UTF-8 bytes of µ
			l.pos += 2
			continue
		}
		break
	}
	if l.pos == unitStart {
		return l.token(NUMBER, start)
	}

	text := l.input[start:l.pos]
	if _, err := ParseDuration(text); err != nil {
		return l.errorf(start, "%s", err)
	}
	return l.token(DURATION, start)
}

func (l *Lexer) scanDigits() {
	for l.pos < len(l.input) && (isDigit(l.input[l.pos]) || l.input[l.pos] == '_') {
		l.pos++
	}
}

func (l *Lexer) scanString() Token {
	start := l.pos
	quote := l.input[l.pos]
	l.pos++
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		switch {
		case c == '\\' && quote != '`':
			l.pos++
			if l.pos >= len(l.input) {
				return l.errorf(start, "unterminated string literal")
			}
			l.pos++
			continue
		case c == quote:
			l.pos++
			return l.token(STRING, start)
		case c == '\n' && quote != '`':
			return l.errorf(start, "unterminated string literal")
		}
		l.pos++
	}
	return l.errorf(start, "unterminated string literal")
}

// durationUnits lists TraceQL's time units from longest to shortest. Span
// durations are routinely sub-second, so the small units matter here in a way
// they do not for a metric range.
var durationUnits = []struct {
	name string
	size time.Duration
}{
	{"w", 7 * 24 * time.Hour},
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
	{"ms", time.Millisecond},
	{"us", time.Microsecond},
	{"µs", time.Microsecond},
	{"ns", time.Nanosecond},
}

// ParseDuration converts TraceQL duration text such as "100ms", "2s" or "1h30m"
// into a Duration. Fractional components are accepted, so "1.5s" is valid.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	rest := s
	total := time.Duration(0)
	components := 0

	for len(rest) > 0 {
		digits := 0
		for digits < len(rest) && (isDigit(rest[digits]) || rest[digits] == '.') {
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("missing number before unit in duration %q", s)
		}
		value, err := strconv.ParseFloat(rest[:digits], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number %q in duration %q", rest[:digits], s)
		}
		rest = rest[digits:]
		if rest == "" {
			return 0, fmt.Errorf("missing unit after %s in duration %q",
				strconv.FormatFloat(value, 'g', -1, 64), s)
		}

		matched := ""
		for _, u := range durationUnits {
			// Longer units must be tested first so that "ms" is not read as "m".
			if strings.HasPrefix(rest, u.name) && len(u.name) > len(matched) {
				matched = u.name
			}
		}
		if matched == "" {
			return 0, fmt.Errorf("unknown unit %q in duration %q", rest, s)
		}
		for _, u := range durationUnits {
			if u.name == matched {
				total += time.Duration(value * float64(u.size))
				break
			}
		}
		rest = rest[len(matched):]
		components++
	}
	if components == 0 {
		return 0, fmt.Errorf("empty duration %q", s)
	}
	return total, nil
}
