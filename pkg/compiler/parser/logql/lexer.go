package logql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TokenType identifies a lexical token in LogQL.
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
	// BYTES is a byte-size literal such as 20MB, used by label filters.
	BYTES

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
	// PIPE is the bare "|" introducing a pipeline stage.
	PIPE

	// Line filter operators. PIPE_EXACT and PIPE_MATCH are single tokens
	// rather than a PIPE followed by an operator, since "|=" may not be split.
	PIPE_EXACT // |=
	PIPE_MATCH // |~

	// Label matching and comparison operators.
	EQL       // =
	EQLC      // ==
	NEQ       // != — also the "line does not contain" filter
	EQL_REGEX // =~
	NEQ_REGEX // !~ — also the "line does not match" filter
	GTR       // >
	LSS       // <
	GTE       // >=
	LTE       // <=

	// Arithmetic operators.
	ADD
	SUB
	MUL
	DIV
	MOD
	POW

	// Logical and set operators.
	LAND    // and
	LOR     // or
	LUNLESS // unless

	// Pipeline stage keywords.
	JSON
	LOGFMT
	REGEXP
	PATTERN
	UNPACK
	LINE_FORMAT
	LABEL_FORMAT
	DROP
	KEEP
	DECOLORIZE
	UNWRAP

	// Modifier keywords.
	BY
	WITHOUT
	OFFSET
	BOOL
	ON
	IGNORING
	GROUP_LEFT
	GROUP_RIGHT
)

var tokenText = map[TokenType]string{
	EOF:        "end of input",
	ERROR:      "error",
	IDENTIFIER: "identifier",
	STRING:     "string",
	NUMBER:     "number",
	DURATION:   "duration",
	BYTES:      "bytes",

	LEFT_PAREN:    "(",
	RIGHT_PAREN:   ")",
	LEFT_BRACE:    "{",
	RIGHT_BRACE:   "}",
	LEFT_BRACKET:  "[",
	RIGHT_BRACKET: "]",
	COMMA:         ",",
	DOT:           ".",
	SEMICOLON:     ";",
	PIPE:          "|",

	PIPE_EXACT: "|=",
	PIPE_MATCH: "|~",

	EQL:       "=",
	EQLC:      "==",
	NEQ:       "!=",
	EQL_REGEX: "=~",
	NEQ_REGEX: "!~",
	GTR:       ">",
	LSS:       "<",
	GTE:       ">=",
	LTE:       "<=",

	ADD: "+",
	SUB: "-",
	MUL: "*",
	DIV: "/",
	MOD: "%",
	POW: "^",

	LAND:    "and",
	LOR:     "or",
	LUNLESS: "unless",

	JSON:         "json",
	LOGFMT:       "logfmt",
	REGEXP:       "regexp",
	PATTERN:      "pattern",
	UNPACK:       "unpack",
	LINE_FORMAT:  "line_format",
	LABEL_FORMAT: "label_format",
	DROP:         "drop",
	KEEP:         "keep",
	DECOLORIZE:   "decolorize",
	UNWRAP:       "unwrap",

	BY:          "by",
	WITHOUT:     "without",
	OFFSET:      "offset",
	BOOL:        "bool",
	ON:          "on",
	IGNORING:    "ignoring",
	GROUP_LEFT:  "group_left",
	GROUP_RIGHT: "group_right",
}

func (t TokenType) String() string {
	if s, ok := tokenText[t]; ok {
		return s
	}
	return fmt.Sprintf("TokenType(%d)", int(t))
}

// keywords are the words LogQL reserves unconditionally: the pipeline stage
// names and the modifiers.
//
// Aggregation and conversion function names are deliberately absent. LogQL only
// treats "sum" or "duration" as a function when the shape that follows says so —
// "(" for a call, or "by (" / "without (" for an aggregation — so those are
// scanned as identifiers and resolved by the parser. That keeps a label named
// "duration" filterable, which "| duration > 1m" depends on.
var keywords = map[string]TokenType{
	"and":    LAND,
	"or":     LOR,
	"unless": LUNLESS,

	"json":         JSON,
	"logfmt":       LOGFMT,
	"regexp":       REGEXP,
	"pattern":      PATTERN,
	"unpack":       UNPACK,
	"line_format":  LINE_FORMAT,
	"label_format": LABEL_FORMAT,
	"drop":         DROP,
	"keep":         KEEP,
	"decolorize":   DECOLORIZE,
	"unwrap":       UNWRAP,

	"by":          BY,
	"without":     WITHOUT,
	"offset":      OFFSET,
	"bool":        BOOL,
	"on":          ON,
	"ignoring":    IGNORING,
	"group_left":  GROUP_LEFT,
	"group_right": GROUP_RIGHT,
}

// IsLineFilterOperator reports whether the token opens a line filter stage.
func (t TokenType) IsLineFilterOperator() bool {
	switch t {
	case PIPE_EXACT, PIPE_MATCH, NEQ, NEQ_REGEX:
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

// IsSetOperator reports whether the token is a logical/set operator.
func (t TokenType) IsSetOperator() bool {
	switch t {
	case LAND, LOR, LUNLESS:
		return true
	}
	return false
}

// Token is one lexical item together with its byte offset in the source.
type Token struct {
	Type TokenType
	Val  string
	Pos  int
}

// Lexer scans LogQL source into tokens.
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
	case c == '-' && l.at(1) == '-' && isAlpha(l.at(2)):
		// A parser flag such as --strict or --keep-empty.
		return l.scanFlag()
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
	case '|':
		switch l.at(0) {
		case '=':
			l.pos++
			return l.token(PIPE_EXACT, start)
		case '~':
			l.pos++
			return l.token(PIPE_MATCH, start)
		}
		return l.token(PIPE, start)
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

// scanFlag scans a parser flag such as --strict.
func (l *Lexer) scanFlag() Token {
	start := l.pos
	l.pos += 2
	for l.pos < len(l.input) && (isAlphaNumeric(l.input[l.pos]) || l.input[l.pos] == '-') {
		l.pos++
	}
	return l.token(IDENTIFIER, start)
}

// byteUnits are the suffixes that make a numeric literal a byte size. They are
// matched case-insensitively and longest-first so that "MiB" is not read as "M"
// followed by stray text.
var byteUnits = []string{
	"kib", "mib", "gib", "tib", "pib", "eib",
	"kb", "mb", "gb", "tb", "pb", "eb",
	"b",
}

// scanNumeric scans a number, a duration or a byte size. All three share a
// numeric prefix, so the unit suffix decides which it is.
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
	unit := strings.ToLower(l.input[unitStart:l.pos])
	for _, u := range byteUnits {
		if unit == u {
			if _, err := ParseBytes(text); err != nil {
				return l.errorf(start, "%s", err)
			}
			return l.token(BYTES, start)
		}
	}
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

// durationUnits lists LogQL's time units from longest to shortest. LogQL accepts
// both the Prometheus-style units used in ranges and the Go-style sub-second
// units used in label filters.
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
	{"us", time.Microsecond},
	{"µs", time.Microsecond},
	{"ns", time.Nanosecond},
}

// ParseDuration converts LogQL duration text such as "5m", "1h30m" or "1.5h"
// into a Duration. Unlike PromQL, LogQL admits fractional components, so "1.5h"
// is valid and means ninety minutes.
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
			return 0, fmt.Errorf("missing unit after %s in duration %q", strconv.FormatFloat(value, 'g', -1, 64), s)
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

// byteUnitSizes maps a byte suffix to its multiplier. LogQL follows the usual
// convention where kB is decimal and KiB is binary.
var byteUnitSizes = map[string]uint64{
	"b":   1,
	"kb":  1000,
	"mb":  1000 * 1000,
	"gb":  1000 * 1000 * 1000,
	"tb":  1000 * 1000 * 1000 * 1000,
	"pb":  1000 * 1000 * 1000 * 1000 * 1000,
	"eb":  1000 * 1000 * 1000 * 1000 * 1000 * 1000,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
	"pib": 1 << 50,
	"eib": 1 << 60,
}

// ParseBytes converts LogQL byte-size text such as "20MB" or "1.5GiB" into a
// count of bytes.
func ParseBytes(s string) (uint64, error) {
	i := 0
	for i < len(s) && (isDigit(s[i]) || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("missing number in byte size %q", s)
	}
	value, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q in byte size %q", s[:i], s)
	}
	unit := strings.ToLower(s[i:])
	size, ok := byteUnitSizes[unit]
	if !ok {
		return 0, fmt.Errorf("unknown byte unit %q in %q", s[i:], s)
	}
	if value < 0 {
		return 0, fmt.Errorf("byte size %q must not be negative", s)
	}
	return uint64(value * float64(size)), nil
}
