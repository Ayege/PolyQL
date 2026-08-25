package logql

import (
	"strings"
	"testing"
	"time"
)

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
			name:  "stream selector",
			input: `{app="frontend"}`,
			want:  []TokenType{LEFT_BRACE, IDENTIFIER, EQL, STRING, RIGHT_BRACE},
		},
		{
			name:  "line filter operators are single tokens",
			input: `|= |~ != !~`,
			want:  []TokenType{PIPE_EXACT, PIPE_MATCH, NEQ, NEQ_REGEX},
		},
		{
			name:  "a bare pipe introduces a stage",
			input: `| json`,
			want:  []TokenType{PIPE, JSON},
		},
		{
			name:  "pipe is not split from its operator",
			input: `|="error"`,
			want:  []TokenType{PIPE_EXACT, STRING},
		},
		{
			name:  "stage keywords",
			input: `json logfmt regexp pattern unpack line_format label_format drop keep decolorize unwrap`,
			want: []TokenType{
				JSON, LOGFMT, REGEXP, PATTERN, UNPACK, LINE_FORMAT,
				LABEL_FORMAT, DROP, KEEP, DECOLORIZE, UNWRAP,
			},
		},
		{
			name:  "modifier keywords",
			input: `by without offset bool on ignoring group_left group_right and or unless`,
			want: []TokenType{
				BY, WITHOUT, OFFSET, BOOL, ON, IGNORING, GROUP_LEFT, GROUP_RIGHT,
				LAND, LOR, LUNLESS,
			},
		},
		{
			name:  "aggregation names lex as identifiers, not keywords",
			input: `sum rate count_over_time quantile_over_time duration bytes`,
			want:  []TokenType{IDENTIFIER, IDENTIFIER, IDENTIFIER, IDENTIFIER, IDENTIFIER, IDENTIFIER},
		},
		{
			name:  "comparison operators",
			input: `= == != =~ !~ > >= < <=`,
			want:  []TokenType{EQL, EQLC, NEQ, EQL_REGEX, NEQ_REGEX, GTR, GTE, LSS, LTE},
		},
		{
			name:  "arithmetic operators",
			input: `+ - * / % ^`,
			want:  []TokenType{ADD, SUB, MUL, DIV, MOD, POW},
		},
		{
			name:  "range brackets",
			input: `[5m]`,
			want:  []TokenType{LEFT_BRACKET, DURATION, RIGHT_BRACKET},
		},
		{
			name:  "logfmt flags",
			input: `| logfmt --strict --keep-empty`,
			want:  []TokenType{PIPE, LOGFMT, IDENTIFIER, IDENTIFIER},
		},
		{
			name:  "comments run to end of line",
			input: "{a=\"b\"} # comment\n| json",
			want:  []TokenType{LEFT_BRACE, IDENTIFIER, EQL, STRING, RIGHT_BRACE, PIPE, JSON},
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

// TestLexerNumericLiterals covers the three-way split between plain numbers,
// durations and byte sizes, which share a numeric prefix and are told apart by
// their unit suffix.
func TestLexerNumericLiterals(t *testing.T) {
	cases := []struct {
		input string
		want  TokenType
	}{
		{`400`, NUMBER},
		{`0.99`, NUMBER},
		{`1.5e3`, NUMBER},
		{`.5`, NUMBER},
		{`5m`, DURATION},
		{`1h30m`, DURATION},
		{`300ms`, DURATION},
		{`1.5h`, DURATION},
		{`20ns`, DURATION},
		{`20us`, DURATION},
		{`1d`, DURATION},
		{`2w`, DURATION},
		{`20MB`, BYTES},
		{`20KB`, BYTES},
		{`5B`, BYTES},
		{`1.5GiB`, BYTES},
		{`42mb`, BYTES},
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
		{"unterminated backtick", "`abc", "unterminated string"},
		{"newline inside a string", "\"abc\ndef\"", "unterminated string"},
		{"lone bang", `a ! b`, "expected != or !~"},
		{"unknown character", `a $ b`, "unexpected character"},
		{"unknown unit", `5qq`, "unknown unit"},
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

func TestParseDuration(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"1ns", time.Nanosecond},
		{"20us", 20 * time.Microsecond},
		{"300ms", 300 * time.Millisecond},
		{"1s", time.Second},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		// LogQL admits fractional components, unlike PromQL.
		{"1.5h", 90 * time.Minute},
		{"0.5s", 500 * time.Millisecond},
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

	for _, bad := range []string{"", "5", "m", "1x"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			if d, err := ParseDuration(bad); err == nil {
				t.Errorf("ParseDuration(%q) = %s, want an error", bad, d)
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	cases := []struct {
		input string
		want  uint64
	}{
		{"5B", 5},
		{"20KB", 20000},
		{"20MB", 20000000},
		{"1GB", 1000000000},
		{"1KiB", 1024},
		{"1MiB", 1024 * 1024},
		{"1.5GiB", 1610612736},
		{"42mb", 42000000},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, err := ParseBytes(c.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}

	for _, bad := range []string{"", "MB", "5QB", "5"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			if b, err := ParseBytes(bad); err == nil {
				t.Errorf("ParseBytes(%q) = %d, want an error", bad, b)
			}
		})
	}
}

func TestStringUnquoting(t *testing.T) {
	cases := []struct{ input, want string }{
		{`"plain"`, "plain"},
		{`"escaped: \n \\ \t"`, "escaped: \n \\ \t"},
		{`'single quoted'`, "single quoted"},
		{`'it\'s escaped'`, "it's escaped"},
		// Templates are commonly backquoted so their quotes need no escaping.
		{"`{{.status}} \"{{.method}}\"`", `{{.status}} "{{.method}}"`},
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
