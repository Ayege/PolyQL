package promql

import (
	"strings"
	"testing"
	"time"
)

// tokenTypes drops positions and values so a test can state the shape of a
// token stream compactly.
func tokenTypes(input string) []TokenType {
	var types []TokenType
	for _, tok := range Tokenize(input) {
		if tok.Type == EOF {
			break
		}
		types = append(types, tok.Type)
	}
	return types
}

func TestLexerTokenStream(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []TokenType
	}{
		{
			name:  "selector with a label matcher",
			input: `http_requests_total{status="500"}`,
			want:  []TokenType{IDENTIFIER, LEFT_BRACE, IDENTIFIER, EQL, STRING, RIGHT_BRACE},
		},
		{
			name:  "range vector",
			input: `foo[5m]`,
			want:  []TokenType{IDENTIFIER, LEFT_BRACKET, DURATION, RIGHT_BRACKET},
		},
		{
			name:  "subquery separates range from resolution with a colon",
			input: `foo[30m:1m]`,
			want:  []TokenType{IDENTIFIER, LEFT_BRACKET, DURATION, COLON, DURATION, RIGHT_BRACKET},
		},
		{
			name:  "a colon outside brackets belongs to the metric name",
			input: `job:requests:rate5m`,
			want:  []TokenType{METRIC_IDENTIFIER},
		},
		{
			name:  "aggregation keywords",
			input: `sum by (job) (foo)`,
			want:  []TokenType{SUM, BY, LEFT_PAREN, IDENTIFIER, RIGHT_PAREN, LEFT_PAREN, IDENTIFIER, RIGHT_PAREN},
		},
		{
			name:  "all label matching operators",
			input: `{a="1",b!="2",c=~"3",d!~"4"}`,
			want: []TokenType{
				LEFT_BRACE,
				IDENTIFIER, EQL, STRING, COMMA,
				IDENTIFIER, NEQ, STRING, COMMA,
				IDENTIFIER, EQL_REGEX, STRING, COMMA,
				IDENTIFIER, NEQ_REGEX, STRING,
				RIGHT_BRACE,
			},
		},
		{
			name:  "arithmetic and comparison operators",
			input: `+ - * / % ^ == != < > <= >=`,
			want:  []TokenType{ADD, SUB, MUL, DIV, MOD, POW, EQLC, NEQ, LSS, GTR, LTE, GTE},
		},
		{
			name:  "set operators are keywords",
			input: `and or unless atan2`,
			want:  []TokenType{LAND, LOR, LUNLESS, ATAN2},
		},
		{
			name:  "matching modifier keywords",
			input: `on ignoring group_left group_right bool offset`,
			want:  []TokenType{ON, IGNORING, GROUP_LEFT, GROUP_RIGHT, BOOL, OFFSET},
		},
		{
			name:  "at modifier",
			input: `foo @ start()`,
			want:  []TokenType{IDENTIFIER, AT, START, LEFT_PAREN, RIGHT_PAREN},
		},
		{
			name:  "punctuation the grammar does not use is still tokenized",
			input: `. ;`,
			want:  []TokenType{DOT, SEMICOLON},
		},
		{
			name:  "comments run to end of line",
			input: "foo # a comment\n+ bar",
			want:  []TokenType{IDENTIFIER, ADD, IDENTIFIER},
		},
		{
			name:  "keywords are matched case-insensitively",
			input: `SUM BY (job) (foo)`,
			want:  []TokenType{SUM, BY, LEFT_PAREN, IDENTIFIER, RIGHT_PAREN, LEFT_PAREN, IDENTIFIER, RIGHT_PAREN},
		},
		{
			name:  "function names lex as identifiers, not keywords",
			input: `rate irate increase histogram_quantile`,
			want:  []TokenType{IDENTIFIER, IDENTIFIER, IDENTIFIER, IDENTIFIER},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenTypes(c.input)
			if len(got) != len(c.want) {
				t.Fatalf("got %d tokens %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("token %d = %s, want %s (full stream: %v)", i, got[i], c.want[i], got)
				}
			}
		})
	}
}

func TestLexerNumbersAndDurations(t *testing.T) {
	cases := []struct {
		input string
		want  TokenType
	}{
		{`23`, NUMBER},
		{`2.43`, NUMBER},
		{`3.4e-9`, NUMBER},
		{`3.4E+9`, NUMBER},
		{`0x8f`, NUMBER},
		{`1_000_000`, NUMBER},
		{`.123`, NUMBER},
		{`NaN`, NUMBER},
		{`nan`, NUMBER},
		{`Inf`, NUMBER},
		{`INF`, NUMBER},
		{`1s`, DURATION},
		{`2m`, DURATION},
		{`1ms`, DURATION},
		{`1h30m`, DURATION},
		{`12h34m56s`, DURATION},
		{`54s321ms`, DURATION},
		{`1y2w3d4h5m6s7ms`, DURATION},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got := tokenTypes(c.input)
			if len(got) != 1 {
				t.Fatalf("got %d tokens %v, want exactly 1", len(got), got)
			}
			if got[0] != c.want {
				t.Errorf("got %s, want %s", got[0], c.want)
			}
		})
	}
}

func TestLexerErrors(t *testing.T) {
	cases := []struct{ name, input, wantMsg string }{
		{"unterminated double quote", `"abc`, "unterminated string"},
		{"unterminated single quote", `'abc`, "unterminated string"},
		{"newline inside a string", "\"abc\ndef\"", "unterminated string"},
		{"lone bang", `foo ! bar`, "expected != or !~"},
		{"unknown character", `foo $ bar`, "unexpected character"},
		{"duration units out of order", `1s2h`, "longest to shortest"},
		{"repeated duration unit", `1m1m`, "longest to shortest"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tokens := Tokenize(c.input)
			last := tokens[len(tokens)-1]
			if last.Type != ERROR {
				t.Fatalf("expected an ERROR token, got %v", tokens)
			}
			if !strings.Contains(last.Val, c.wantMsg) {
				t.Errorf("error %q should contain %q", last.Val, c.wantMsg)
			}
		})
	}
}

// TestUnitlessNumberIsNotADuration covers the boundary between the two: "1q"
// is not a malformed duration but a number followed by an identifier, which the
// lexer accepts and the parser rejects.
func TestUnitlessNumberIsNotADuration(t *testing.T) {
	if got, want := tokenTypes(`1q`), []TokenType{NUMBER, IDENTIFIER}; len(got) != 2 ||
		got[0] != want[0] || got[1] != want[1] {
		t.Errorf("tokens = %v, want %v", got, want)
	}
	if _, err := Parse(`foo[1q]`); err == nil {
		t.Error("foo[1q] should fail to parse")
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"1ms", time.Millisecond},
		{"1s", time.Second},
		{"2m", 2 * time.Minute},
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1y", 365 * 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"54s321ms", 54*time.Second + 321*time.Millisecond},
		{"12h34m56s", 12*time.Hour + 34*time.Minute + 56*time.Second},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, err := ParseDuration(c.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}

	for _, bad := range []string{"", "5", "m", "1x", "1s1s", "1s1h", "5m3"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			if d, err := ParseDuration(bad); err == nil {
				t.Errorf("ParseDuration(%q) = %s, want an error", bad, d)
			}
		})
	}
}

// TestFormatDurationRoundTrip checks that rendering a duration produces text
// ParseDuration reads back to the same value, which is what keeps AST.String
// output re-parseable.
func TestFormatDurationRoundTrip(t *testing.T) {
	cases := []time.Duration{
		time.Millisecond,
		time.Second,
		5 * time.Minute,
		90 * time.Minute,
		24 * time.Hour,
		7 * 24 * time.Hour,
		365 * 24 * time.Hour,
		54*time.Second + 321*time.Millisecond,
		12*time.Hour + 34*time.Minute + 56*time.Second,
	}
	for _, d := range cases {
		text := FormatDuration(d)
		got, err := ParseDuration(text)
		if err != nil {
			t.Errorf("FormatDuration(%s) = %q, which does not parse: %v", d, text, err)
			continue
		}
		if got != d {
			t.Errorf("round trip: %s -> %q -> %s", d, text, got)
		}
	}

	// Equivalent spellings canonicalise to the largest units that divide them.
	if got, want := FormatDuration(7*24*time.Hour), "1w"; got != want {
		t.Errorf("FormatDuration(7d) = %q, want %q", got, want)
	}
	if got, want := FormatDuration(0), "0s"; got != want {
		t.Errorf("FormatDuration(0) = %q, want %q", got, want)
	}
}

func TestStringUnquoting(t *testing.T) {
	cases := []struct{ input, want string }{
		{`"plain"`, "plain"},
		{`"escaped: \n \\ \t"`, "escaped: \n \\ \t"},
		{`'single quoted'`, "single quoted"},
		{`'it\'s escaped'`, "it's escaped"},
		{`'has "double" quotes'`, `has "double" quotes`},
		{"`raw: \\n not unescaped`", `raw: \n not unescaped`},
		{`"é"`, "é"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, err := unquote(c.input)
			if err != nil {
				t.Fatalf("unquote(%s): %v", c.input, err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
