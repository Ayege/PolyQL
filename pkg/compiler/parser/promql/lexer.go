package promql

import (
	"fmt"
	"strings"
	"time"
)

// TokenType identifies a lexical token in PromQL.
type TokenType int

const (
	// EOF marks the end of input.
	EOF TokenType = iota
	// ERROR carries a scanning failure; Token.Val holds the message.
	ERROR

	// Literals and names.
	IDENTIFIER
	// METRIC_IDENTIFIER is an identifier containing a colon. Recording rule
	// names use colons, but label names may not, so the two are distinguished
	// at scan time and the parser rejects a METRIC_IDENTIFIER where a label
	// name is required.
	METRIC_IDENTIFIER
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
	AT

	// Label matching operators.
	EQL       // =
	NEQ       // != — also the inequality comparison operator
	EQL_REGEX // =~
	NEQ_REGEX // !~

	// Arithmetic operators.
	ADD
	SUB
	MUL
	DIV
	MOD
	POW

	// Comparison operators. NEQ above completes the set.
	EQLC // ==
	GTR  // >
	LSS  // <
	GTE  // >=
	LTE  // <=

	// Logical and set operators, which are keywords rather than symbols.
	LAND    // and
	LOR     // or
	LUNLESS // unless
	ATAN2   // atan2

	// Aggregation operators. These are keywords in PromQL, which is why a
	// series may not be named "sum".
	SUM
	AVG
	COUNT
	MIN
	MAX
	GROUP
	STDDEV
	STDVAR
	TOPK
	BOTTOMK
	COUNT_VALUES
	QUANTILE
	LIMITK
	LIMIT_RATIO

	// Modifier keywords.
	OFFSET
	BY
	WITHOUT
	ON
	IGNORING
	GROUP_LEFT
	GROUP_RIGHT
	BOOL
	START
	END
)

// tokenText maps each token to the source text it is written as, or to a
// descriptive name for the token classes that have no fixed spelling. It backs
// both String and the parser's error messages.
var tokenText = map[TokenType]string{
	EOF:               "end of input",
	ERROR:             "error",
	IDENTIFIER:        "identifier",
	METRIC_IDENTIFIER: "metric identifier",
	STRING:            "string",
	NUMBER:            "number",
	DURATION:          "duration",

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
	AT:            "@",

	EQL:       "=",
	NEQ:       "!=",
	EQL_REGEX: "=~",
	NEQ_REGEX: "!~",

	ADD: "+",
	SUB: "-",
	MUL: "*",
	DIV: "/",
	MOD: "%",
	POW: "^",

	EQLC: "==",
	GTR:  ">",
	LSS:  "<",
	GTE:  ">=",
	LTE:  "<=",

	LAND:    "and",
	LOR:     "or",
	LUNLESS: "unless",
	ATAN2:   "atan2",

	SUM:          "sum",
	AVG:          "avg",
	COUNT:        "count",
	MIN:          "min",
	MAX:          "max",
	GROUP:        "group",
	STDDEV:       "stddev",
	STDVAR:       "stdvar",
	TOPK:         "topk",
	BOTTOMK:      "bottomk",
	COUNT_VALUES: "count_values",
	QUANTILE:     "quantile",
	LIMITK:       "limitk",
	LIMIT_RATIO:  "limit_ratio",

	OFFSET:      "offset",
	BY:          "by",
	WITHOUT:     "without",
	ON:          "on",
	IGNORING:    "ignoring",
	GROUP_LEFT:  "group_left",
	GROUP_RIGHT: "group_right",
	BOOL:        "bool",
	START:       "start",
	END:         "end",
}

func (t TokenType) String() string {
	if s, ok := tokenText[t]; ok {
		return s
	}
	return fmt.Sprintf("TokenType(%d)", int(t))
}

// keywords maps reserved words to their tokens. PromQL matches these
// case-insensitively, so "SUM by (job) (x)" parses.
//
// Function names are deliberately absent. In PromQL a function name is only a
// function when followed by "(" — "rate" on its own is an ordinary series
// selector — so functions are scanned as identifiers and resolved against the
// function table at parse time. Making them keywords would reject valid
// queries.
var keywords = map[string]TokenType{
	"and":    LAND,
	"or":     LOR,
	"unless": LUNLESS,
	"atan2":  ATAN2,

	"sum":          SUM,
	"avg":          AVG,
	"count":        COUNT,
	"min":          MIN,
	"max":          MAX,
	"group":        GROUP,
	"stddev":       STDDEV,
	"stdvar":       STDVAR,
	"topk":         TOPK,
	"bottomk":      BOTTOMK,
	"count_values": COUNT_VALUES,
	"quantile":     QUANTILE,
	"limitk":       LIMITK,
	"limit_ratio":  LIMIT_RATIO,

	"offset":      OFFSET,
	"by":          BY,
	"without":     WITHOUT,
	"on":          ON,
	"ignoring":    IGNORING,
	"group_left":  GROUP_LEFT,
	"group_right": GROUP_RIGHT,
	"bool":        BOOL,
	"start":       START,
	"end":         END,
}

// IsAggregator reports whether the token is an aggregation operator keyword.
func (t TokenType) IsAggregator() bool {
	switch t {
	case SUM, AVG, COUNT, MIN, MAX, GROUP, STDDEV, STDVAR, TOPK, BOTTOMK,
		COUNT_VALUES, QUANTILE, LIMITK, LIMIT_RATIO:
		return true
	}
	return false
}

// IsAggregatorWithParam reports whether the aggregation operator takes a
// parameter ahead of the expression it aggregates, as in topk(5, x).
func (t TokenType) IsAggregatorWithParam() bool {
	switch t {
	case TOPK, BOTTOMK, QUANTILE, COUNT_VALUES, LIMITK, LIMIT_RATIO:
		return true
	}
	return false
}

// IsComparison reports whether the token is a comparison operator, the only
// class the bool modifier may follow.
func (t TokenType) IsComparison() bool {
	switch t {
	case EQLC, NEQ, GTR, LSS, GTE, LTE:
		return true
	}
	return false
}

// IsSetOperator reports whether the token is a logical/set operator, which
// requires instant vectors on both sides.
func (t TokenType) IsSetOperator() bool {
	switch t {
	case LAND, LOR, LUNLESS:
		return true
	}
	return false
}

// IsLabelMatchOperator reports whether the token may appear inside a selector's
// braces.
func (t TokenType) IsLabelMatchOperator() bool {
	switch t {
	case EQL, NEQ, EQL_REGEX, NEQ_REGEX:
		return true
	}
	return false
}

// Token is one lexical item together with its byte offset in the source, which
// is what lets parse errors point at the offending text.
type Token struct {
	Type TokenType
	Val  string
	Pos  int
}

func (t Token) String() string {
	switch t.Type {
	case EOF:
		return "end of input"
	case ERROR:
		return t.Val
	case IDENTIFIER, METRIC_IDENTIFIER, NUMBER, DURATION, STRING:
		return t.Val
	default:
		return t.Type.String()
	}
}

// Lexer scans PromQL source into tokens.
type Lexer struct {
	input string
	pos   int
	// bracketDepth disambiguates the colon. A metric name may contain colons
	// (job:requests:rate5m), but inside brackets a colon separates a
	// subquery's range from its resolution. Tracking the depth is how the
	// scanner tells "[30m:1m]" from an identifier.
	bracketDepth int
}

// NewLexer returns a Lexer over the given query text.
func NewLexer(input string) *Lexer { return &Lexer{input: input} }

// Tokenize scans the whole input. The returned slice always ends with an EOF
// token, and stops at the first ERROR token, so a caller can report the first
// scanning failure without re-scanning.
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
func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func isAlpha(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isAlphaNumeric(c byte) bool { return isAlpha(c) || isDigit(c) }

// at returns the byte n positions ahead, or 0 past the end. Zero is not a valid
// PromQL character, so callers can compare against it safely.
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

// skipSpaceAndComments advances past whitespace and # line comments.
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
	case isAlpha(c) || (c == ':' && l.bracketDepth == 0):
		return l.scanIdentifier()
	case isDigit(c):
		return l.scanNumberOrDuration()
	case c == '.' && isDigit(l.at(1)):
		return l.scanNumberOrDuration()
	case c == '"' || c == '\'' || c == '`':
		return l.scanString()
	}

	// Two-character operators must be tested before their one-character
	// prefixes.
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
		l.bracketDepth++
		return l.token(LEFT_BRACKET, start)
	case ']':
		if l.bracketDepth > 0 {
			l.bracketDepth--
		}
		return l.token(RIGHT_BRACKET, start)
	case ':':
		return l.token(COLON, start)
	case ',':
		return l.token(COMMA, start)
	case '.':
		return l.token(DOT, start)
	case ';':
		return l.token(SEMICOLON, start)
	case '@':
		return l.token(AT, start)
	case '+':
		return l.token(ADD, start)
	case '-':
		return l.token(SUB, start)
	case '*':
		return l.token(MUL, start)
	case '/':
		return l.token(DIV, start)
	case '%':
		return l.token(MOD, start)
	case '^':
		return l.token(POW, start)
	case '=':
		switch l.at(0) {
		case '=':
			l.pos++
			return l.token(EQLC, start)
		case '~':
			l.pos++
			return l.token(EQL_REGEX, start)
		}
		return l.token(EQL, start)
	case '!':
		switch l.at(0) {
		case '=':
			l.pos++
			return l.token(NEQ, start)
		case '~':
			l.pos++
			return l.token(NEQ_REGEX, start)
		}
		return l.errorf(start, "unexpected %q: expected != or !~", "!")
	case '<':
		if l.at(0) == '=' {
			l.pos++
			return l.token(LTE, start)
		}
		return l.token(LSS, start)
	case '>':
		if l.at(0) == '=' {
			l.pos++
			return l.token(GTE, start)
		}
		return l.token(GTR, start)
	}

	return l.errorf(start, "unexpected character %q", string(rune(c)))
}

// scanIdentifier scans a word: a keyword, the float words NaN and Inf, a metric
// identifier if it contains a colon, or a plain identifier.
func (l *Lexer) scanIdentifier() Token {
	start := l.pos
	hasColon := false
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if c == ':' {
			if l.bracketDepth > 0 {
				break
			}
			hasColon = true
		} else if !isAlphaNumeric(c) {
			break
		}
		l.pos++
	}
	word := l.input[start:l.pos]

	// NaN and Inf are float literals spelled as words, and PromQL matches them
	// case-insensitively.
	if !hasColon {
		switch strings.ToLower(word) {
		case "nan", "inf":
			return l.token(NUMBER, start)
		}
		if kw, ok := keywords[strings.ToLower(word)]; ok {
			return l.token(kw, start)
		}
		return l.token(IDENTIFIER, start)
	}
	return l.token(METRIC_IDENTIFIER, start)
}

// scanNumberOrDuration scans a numeric literal, or a duration when the digits
// are followed by a time unit. The two share a prefix, so the decision can only
// be made after the mantissa has been read.
func (l *Lexer) scanNumberOrDuration() Token {
	start := l.pos

	// Hexadecimal integers cannot carry a unit or an exponent.
	if l.at(0) == '0' && (l.at(1) == 'x' || l.at(1) == 'X') {
		l.pos += 2
		for l.pos < len(l.input) && (isHexDigit(l.input[l.pos]) || l.input[l.pos] == '_') {
			l.pos++
		}
		return l.token(NUMBER, start)
	}

	isFloat := false
	l.scanDigits()
	if l.at(0) == '.' && isDigit(l.at(1)) {
		isFloat = true
		l.pos++
		l.scanDigits()
	} else if l.at(0) == '.' && l.pos == start {
		// A literal written as ".5".
		isFloat = true
		l.pos++
		l.scanDigits()
	}
	if c := l.at(0); c == 'e' || c == 'E' {
		// Only treat this as an exponent if digits actually follow, so that the
		// "e" of an adjacent identifier is not swallowed.
		if isDigit(l.at(1)) || ((l.at(1) == '+' || l.at(1) == '-') && isDigit(l.at(2))) {
			isFloat = true
			l.pos += 2
			l.scanDigits()
		}
	}

	// An integer mantissa followed by unit characters is a duration.
	if !isFloat {
		end := l.pos
		for end < len(l.input) && (isDigit(l.input[end]) || strings.IndexByte("smhdwy", l.input[end]) >= 0) {
			end++
		}
		if end > l.pos {
			l.pos = end
			text := l.input[start:l.pos]
			if _, err := ParseDuration(text); err != nil {
				return l.errorf(start, "%s", err)
			}
			return l.token(DURATION, start)
		}
	}

	return l.token(NUMBER, start)
}

func (l *Lexer) scanDigits() {
	for l.pos < len(l.input) && (isDigit(l.input[l.pos]) || l.input[l.pos] == '_') {
		l.pos++
	}
}

// scanString scans a quoted string. Double and single quotes take Go escape
// sequences; backticks are raw.
func (l *Lexer) scanString() Token {
	start := l.pos
	quote := l.input[l.pos]
	l.pos++
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		switch {
		case c == '\\' && quote != '`':
			// Skip the escaped character so that a quote cannot close the
			// literal.
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

// durationUnits lists PromQL's time units from longest to shortest. A duration
// writes its units in this order, each at most once.
var durationUnits = []struct {
	name string
	size time.Duration
}{
	{"y", 365 * 24 * time.Hour},
	{"w", 7 * 24 * time.Hour},
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
	{"ms", time.Millisecond},
}

// ParseDuration converts PromQL duration text such as "5m", "1h30m" or
// "54s321ms" into a Duration. Units must run from longest to shortest and may
// each appear only once, which PromQL requires and which makes the text form
// canonical.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	rest := s
	total := time.Duration(0)
	lastUnit := -1

	for len(rest) > 0 {
		digits := 0
		for digits < len(rest) && isDigit(rest[digits]) {
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("missing number before unit in duration %q", s)
		}
		var value int64
		for i := 0; i < digits; i++ {
			value = value*10 + int64(rest[i]-'0')
			if value > 1<<40 {
				return 0, fmt.Errorf("duration %q is out of range", s)
			}
		}
		rest = rest[digits:]
		if rest == "" {
			return 0, fmt.Errorf("missing unit after %d in duration %q", value, s)
		}

		// "ms" must be tested before "m".
		name := rest[:1]
		if strings.HasPrefix(rest, "ms") {
			name = "ms"
		}
		unit := -1
		for i, u := range durationUnits {
			if u.name == name {
				unit = i
				break
			}
		}
		if unit < 0 {
			return 0, fmt.Errorf("unknown unit %q in duration %q", name, s)
		}
		if unit <= lastUnit {
			return 0, fmt.Errorf("units in duration %q must be ordered from longest to shortest "+
				"and may each appear only once", s)
		}
		lastUnit = unit
		total += time.Duration(value) * durationUnits[unit].size
		rest = rest[len(name):]
	}
	return total, nil
}

// FormatDuration renders a Duration in PromQL syntax, decomposing it into the
// largest units that divide it. Durations only ever enter the AST through
// ParseDuration, so they are always whole milliseconds and nothing is lost.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	if d < 0 {
		b.WriteByte('-')
		d = -d
	}
	for _, u := range durationUnits {
		if d >= u.size {
			n := d / u.size
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.size
		}
	}
	return b.String()
}
