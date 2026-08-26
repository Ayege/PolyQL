package traceql

import (
	"testing"
	"time"
)

// types strips a token slice down to its types, for comparison.
func types(tokens []Token) []TokenType {
	out := make([]TokenType, 0, len(tokens))
	for _, tok := range tokens {
		out = append(out, tok.Type)
	}
	return out
}

func equalTypes(a, b []TokenType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTokenizeSpanset(t *testing.T) {
	got := types(Tokenize(`{span.http.status_code = 500}`))
	want := []TokenType{
		LEFT_BRACE, IDENTIFIER, DOT, IDENTIFIER, DOT, IDENTIFIER,
		EQ, NUMBER, RIGHT_BRACE, EOF,
	}
	if !equalTypes(got, want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
}

// TestTokenizeOperators covers the multi-character operators, where a greedy
// mis-scan would silently change a query's meaning — ">>" read as two ">" would
// turn a descendant relationship into a comparison.
func TestTokenizeOperators(t *testing.T) {
	cases := []struct {
		input string
		want  TokenType
	}{
		{"=", EQ},
		{"==", EQ},
		{"!=", NEQ},
		{"<", LT},
		{">", GT},
		{"<=", LTE},
		{">=", GTE},
		{"=~", REGEX},
		{"!~", NOT_REGEX},
		{"&&", AND},
		{"||", OR},
		{"!", NOT},
		{"~", TILDE},
		{">>", RSHIFT},
		{"|", PIPE},
		{":", COLON},
		{"?", QUESTION},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			tokens := Tokenize(c.input)
			if len(tokens) != 2 {
				t.Fatalf("Tokenize(%q) = %d tokens, want one plus EOF", c.input, len(tokens))
			}
			if got := tokens[0].Type; got != c.want {
				t.Errorf("type = %v, want %v", got, c.want)
			}
		})
	}
}

// TestGreaterThanVersusShift pins the boundary between the two: the lexer is
// greedy, so ">>" is one token and "> >" is two.
func TestGreaterThanVersusShift(t *testing.T) {
	if got := types(Tokenize(">>")); !equalTypes(got, []TokenType{RSHIFT, EOF}) {
		t.Errorf(`">>" = %v, want RSHIFT`, got)
	}
	if got := types(Tokenize("> >")); !equalTypes(got, []TokenType{GT, GT, EOF}) {
		t.Errorf(`"> >" = %v, want two GT`, got)
	}
	if got := types(Tokenize(">=")); !equalTypes(got, []TokenType{GTE, EOF}) {
		t.Errorf(`">=" = %v, want GTE`, got)
	}
}

// TestSingleAmpersandIsAnError covers the one place a near-miss is worth
// naming: "&" alone is almost certainly a typo for "&&".
func TestSingleAmpersandIsAnError(t *testing.T) {
	tokens := Tokenize("&")
	if tokens[0].Type != ERROR {
		t.Fatalf("Tokenize(%q) = %v, want ERROR", "&", tokens[0].Type)
	}
	if got := tokens[0].Val; got == "" {
		t.Error("the error should carry a message")
	}
}

func TestTokenizeKeywords(t *testing.T) {
	cases := []struct {
		input string
		want  TokenType
	}{
		{"as", AS},
		{"by", BY},
		{"over", OVER},
		// The aggregate names and scope prefixes are deliberately not keywords,
		// so an attribute of the same name stays filterable.
		{"count", IDENTIFIER},
		{"sum", IDENTIFIER},
		{"span", IDENTIFIER},
		{"resource", IDENTIFIER},
		{"intrinsic", IDENTIFIER},
		{"duration", IDENTIFIER},
		{"status", IDENTIFIER},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			if got := Tokenize(c.input)[0].Type; got != c.want {
				t.Errorf("Tokenize(%q)[0] = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

// TestTokenizeNumbersAndDurations covers the suffix deciding which a numeric
// literal is.
func TestTokenizeNumbersAndDurations(t *testing.T) {
	cases := []struct {
		input string
		want  TokenType
	}{
		{"500", NUMBER},
		{"0.99", NUMBER},
		{"1e3", NUMBER},
		{"1e-3", NUMBER},
		{"100ms", DURATION},
		{"2s", DURATION},
		{"1h30m", DURATION},
		{"500us", DURATION},
		{"1.5s", DURATION},
		{"100ns", DURATION},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			tokens := Tokenize(c.input)
			if got := tokens[0].Type; got != c.want {
				t.Errorf("Tokenize(%q)[0] = %v, want %v", c.input, got, c.want)
			}
			if got := tokens[0].Val; got != c.input {
				t.Errorf("Val = %q, want the source text %q", got, c.input)
			}
		})
	}

	t.Run("an unknown unit is rejected", func(t *testing.T) {
		if got := Tokenize("5furlongs")[0].Type; got != ERROR {
			t.Errorf("Tokenize(%q)[0] = %v, want ERROR", "5furlongs", got)
		}
	})
}

func TestTokenizeStrings(t *testing.T) {
	for _, input := range []string{`"web"`, `'web'`, "`web`"} {
		tokens := Tokenize(input)
		if got := tokens[0].Type; got != STRING {
			t.Errorf("Tokenize(%q)[0] = %v, want STRING", input, got)
		}
	}

	t.Run("unterminated", func(t *testing.T) {
		for _, input := range []string{`"web`, `"web` + "\n" + `"`} {
			if got := Tokenize(input)[0].Type; got != ERROR {
				t.Errorf("Tokenize(%q)[0] = %v, want ERROR", input, got)
			}
		}
	})
}

func TestSkipsWhitespaceAndComments(t *testing.T) {
	got := types(Tokenize("  # a note\n  {  }  "))
	want := []TokenType{LEFT_BRACE, RIGHT_BRACE, EOF}
	if !equalTypes(got, want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
}

// TestTokenPositions covers the offsets an error message points with.
func TestTokenPositions(t *testing.T) {
	tokens := Tokenize(`{a = 1}`)
	wantPos := []int{0, 1, 3, 5, 6, 7}
	for i, want := range wantPos {
		if got := tokens[i].Pos; got != want {
			t.Errorf("tokens[%d].Pos = %d, want %d", i, got, want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"100ms", 100 * time.Millisecond},
		{"2s", 2 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"1.5s", 1500 * time.Millisecond},
		{"500us", 500 * time.Microsecond},
		{"250ns", 250 * time.Nanosecond},
		{"1w", 7 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, err := ParseDuration(c.input)
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", c.input, err)
			}
			if got != c.want {
				t.Errorf("= %v, want %v", got, c.want)
			}
		})
	}

	t.Run("errors", func(t *testing.T) {
		for _, input := range []string{"", "5", "ms", "5furlongs", "5m3"} {
			if _, err := ParseDuration(input); err == nil {
				t.Errorf("ParseDuration(%q) should have failed", input)
			}
		}
	})

	// "ms" must not be read as "m" followed by stray text, which a
	// shortest-match unit table would do.
	t.Run("longer units win", func(t *testing.T) {
		got, err := ParseDuration("5ms")
		if err != nil {
			t.Fatal(err)
		}
		if got != 5*time.Millisecond {
			t.Errorf("5ms = %v, want 5ms not 5m", got)
		}
	})
}
