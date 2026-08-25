// Package roundtrip exercises the whole compiler: parse, resolve, validate,
// emit, then parse the result in the target language.
//
// It lives apart from the emitter packages because it needs both of them at
// once, and because what it checks is a property of the pipeline rather than of
// any one stage: whatever an emitter produces must be text the target's own
// parser accepts.
package roundtrip

import (
	"strings"
	"testing"
	"time"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/compiler/resolver"
	"github.com/polyql/polyql/pkg/compiler/validator"
	"github.com/polyql/polyql/pkg/registry"

	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	return reg
}

// translate runs the full pipeline and returns the emitted text along with the
// IR it came from.
func translate(t *testing.T, sourceDSL, query, targetDSL string) (string, *ir.Query) {
	t.Helper()
	reg := testRegistry(t)

	p, err := parser.Get(sourceDSL)
	if err != nil {
		t.Fatalf("parser.Get(%q): %v", sourceDSL, err)
	}
	node, err := p.Parse(query)
	if err != nil {
		t.Fatalf("parsing %s: %v", query, err)
	}
	resolved, err := resolver.Resolve(node, sourceDSL, reg)
	if err != nil {
		t.Fatalf("resolving %s: %v", query, err)
	}
	validator.Validate(resolved, targetDSL, reg)

	e, err := emitter.Get(targetDSL)
	if err != nil {
		t.Fatalf("emitter.Get(%q): %v", targetDSL, err)
	}
	text, err := e.Emit(resolved, reg)
	if err != nil {
		t.Fatalf("emitting %s as %s: %v", query, targetDSL, err)
	}
	return text, resolved
}

// assertParses checks that emitted text is accepted by the target's own parser,
// which is the property that makes an emitter usable at all.
func assertParses(t *testing.T, targetDSL, text string) {
	t.Helper()
	p, err := parser.Get(targetDSL)
	if err != nil {
		t.Fatalf("parser.Get(%q): %v", targetDSL, err)
	}
	if _, err := p.Parse(text); err != nil {
		t.Errorf("the emitted %s does not parse: %v\n  %s", targetDSL, err, text)
	}
}

// queryLine strips the leading comment lines, leaving the query itself.
func queryLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "#") {
			return lines[i]
		}
	}
	return ""
}

// TestCrossDSLOutputParses covers the core round trip: what one language's
// parser produced, the other language's parser must accept back.
func TestCrossDSLOutputParses(t *testing.T) {
	cases := []struct {
		name      string
		sourceDSL string
		query     string
		targetDSL string
		// wantContains are fragments the output must carry.
		wantContains []string
	}{
		{
			name:         "promql rate into logql",
			sourceDSL:    "promql",
			query:        `rate(http_requests_total{status="500"}[5m])`,
			targetDSL:    "logql",
			wantContains: []string{`rate(`, `status="500"`, `[5m]`},
		},
		{
			name:         "logql rate into promql",
			sourceDSL:    "logql",
			query:        `rate({app="frontend"} |= "error" [5m])`,
			targetDSL:    "promql",
			wantContains: []string{`rate(`, `app="frontend"`, `[5m]`},
		},
		{
			name:         "promql grouped rate into logql",
			sourceDSL:    "promql",
			query:        `sum by (job) (rate(http_requests_total[5m]))`,
			targetDSL:    "logql",
			wantContains: []string{`sum by (job) (`, `rate(`},
		},
		{
			name:         "logql grouped count into promql",
			sourceDSL:    "logql",
			query:        `sum by (level) (count_over_time({app="frontend"}[1h]))`,
			targetDSL:    "promql",
			wantContains: []string{`sum by (level) (`, `count_over_time(`, `[1h]`},
		},
		{
			name:         "promql histogram_quantile into logql",
			sourceDSL:    "promql",
			query:        `histogram_quantile(0.99, sum by (le) (rate(bucket[5m])))`,
			targetDSL:    "logql",
			wantContains: []string{"# UNSUPPORTED:", "histogram_quantile", `sum by (le) (`},
		},
		{
			name:         "promql join into logql",
			sourceDSL:    "promql",
			query:        `sum(rate(a[5m])) / on (job) group_left (env) sum(rate(b[5m]))`,
			targetDSL:    "logql",
			wantContains: []string{"# UNSUPPORTED:"},
		},
		{
			name:         "logql pipeline into promql",
			sourceDSL:    "logql",
			query:        `sum by (level) (count_over_time({app="x"} | json | status >= 400 [5m]))`,
			targetDSL:    "promql",
			wantContains: []string{"# UNSUPPORTED:", `sum by (level) (`},
		},
		{
			name:         "promql subquery into logql",
			sourceDSL:    "promql",
			query:        `rate(http_requests_total[5m])[30m:1m]`,
			targetDSL:    "logql",
			wantContains: []string{"# UNSUPPORTED:", "subquer"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, _ := translate(t, c.sourceDSL, c.query, c.targetDSL)

			// The whole output, comments included, must parse: both languages
			// take "#" line comments, which is why notes are written that way.
			assertParses(t, c.targetDSL, text)

			for _, want := range c.wantContains {
				if !strings.Contains(text, want) {
					t.Errorf("output should contain %q:\n%s", want, text)
				}
			}
			if queryLine(text) == "" {
				t.Errorf("output has no query line:\n%s", text)
			}
		})
	}
}

// TestSameDSLRoundTripIsStable covers translating a query back into its own
// language: the pipeline must be able to reproduce what it read.
func TestSameDSLRoundTripIsStable(t *testing.T) {
	cases := []struct{ dsl, query, want string }{
		{"promql", `up`, `up`},
		{"promql", `http_requests_total{status="500"}`, `http_requests_total{status="500"}`},
		{"promql", `rate(http_requests_total{status="500"}[5m])`, `rate(http_requests_total{status="500"}[5m])`},
		{"promql", `sum by (job) (rate(http_requests_total[5m]))`, `sum by (job) (rate(http_requests_total[5m]))`},
		{"promql", `topk(5, http_requests_total)`, `topk(5, http_requests_total)`},
		{"promql", `histogram_quantile(0.99, sum by (le) (rate(bucket[5m])))`,
			`histogram_quantile(0.99, sum by (le) (rate(bucket[5m])))`},
		{"promql", `rate(a[5m]) / rate(b[5m])`, `rate(a[5m]) / rate(b[5m])`},
		{"promql", `rate(x[5m] offset 1h)`, `rate(x[5m] offset 1h)`},
		{"promql", `abs(x)`, `abs(x)`},
		{"promql", `label_replace(up, "d", "$1", "s", "(.*)")`, `label_replace(up, "d", "$1", "s", "(.*)")`},
		{"promql", `up > 5`, `up > 5`},
		{"promql", `up > bool 5`, `up > bool 5`},
		{"promql", `rate(a[5m]) + rate(b[5m])`, `rate(a[5m]) + rate(b[5m])`},
		{"promql", `a and b`, `a and b`},
		{"promql", `a unless b`, `a unless b`},
		{"promql", `(a + b) * 2`, `(a + b) * 2`},
		{"promql", `a - b - c`, `a - b - c`},
		{"promql", `a - (b - c)`, `a - (b - c)`},
		{"promql", `a / on (job) group_left (env) b`, `a / on (job) group_left (env) b`},
		{"promql", `rate(x[5m])[30m:1m]`, `rate(x[5m])[30m:1m]`},
		{"promql", `sum without (pod) (rate(x[5m]))`, `sum without (pod) (rate(x[5m]))`},

		{"logql", `{app="frontend"}`, `{app="frontend"}`},
		{"logql", `{app="frontend"} |= "error"`, `{app="frontend"} |= "error"`},
		{"logql", `{app="frontend"} |= "error.log"`, `{app="frontend"} |= "error.log"`},
		{"logql", `{app="frontend"} != "debug"`, `{app="frontend"} != "debug"`},
		{"logql", `{app="frontend"} |~ "err.*"`, `{app="frontend"} |~ "err.*"`},
		{"logql", `count_over_time({a="b"}[90m])`, `count_over_time({a="b"}[90m])`},
		{"logql", `count_over_time({a="b"}[1h30m])`, `count_over_time({a="b"}[1h30m])`},
		{"logql", `sum(rate({a="b"}[5m])) / sum(rate({c="d"}[5m]))`,
			`sum(rate({a="b"}[5m])) / sum(rate({c="d"}[5m]))`},
		{"logql", `{app="frontend"} | json | status >= 400`, `{app="frontend"} | json | status >= 400`},
		{"logql", `{app="frontend"} | logfmt | level="error"`, `{app="frontend"} | logfmt | level="error"`},
		{"logql", `rate({app="frontend"} |= "error" [5m])`, `rate({app="frontend"} |= "error" [5m])`},
		{"logql", `sum by (level) (count_over_time({app="frontend"}[1h]))`,
			`sum by (level) (count_over_time({app="frontend"}[1h]))`},
		{"logql", `avg_over_time({app="frontend"} | json | unwrap duration [5m])`,
			`avg_over_time({app="frontend"} | json | unwrap duration [5m])`},
		{"logql", `{app="x"} |= "e" | json | line_format "{{.m}}"`, `{app="x"} |= "e" | json | line_format "{{.m}}"`},
		{"logql", `{a="b"} | duration > 1m and size > 20MB`, `{a="b"} | duration > 1m and size > 20MB`},
		{"logql", `topk(3, sum by (x) (rate({a="b"}[5m])))`, `topk(3, sum by (x) (rate({a="b"}[5m])))`},
		{"logql", `max_over_time({a="b"} | unwrap y [5m]) by (pod)`, `max_over_time({a="b"} | unwrap y [5m]) by (pod)`},
		{"logql", `bytes_rate({a="b"}[5m])`, `bytes_rate({a="b"}[5m])`},
		{"logql", `{a="b"} | drop level, method="GET"`, `{a="b"} | drop level, method="GET"`},
	}

	for _, c := range cases {
		t.Run(c.dsl+" "+c.query, func(t *testing.T) {
			text, _ := translate(t, c.dsl, c.query, c.dsl)
			assertParses(t, c.dsl, text)

			if got := queryLine(text); got != c.want {
				t.Errorf("round trip:\n  in   %s\n  out  %s\n  want %s", c.query, got, c.want)
			}
			// Translating a query into its own language loses nothing, so there
			// should be nothing to report.
			if strings.Contains(text, "#") {
				t.Errorf("a same-language round trip should need no notes:\n%s", text)
			}
		})
	}
}

// TestBidirectionalIRIsStable covers the deeper property: translating into
// another language and resolving the result must land on the same IR shape, for
// the constructs both languages have.
func TestBidirectionalIRIsStable(t *testing.T) {
	reg := testRegistry(t)

	cases := []struct{ sourceDSL, query, targetDSL string }{
		{"promql", `sum by (job) (rate(x[5m]))`, "logql"},
		{"logql", `sum by (level) (count_over_time({app="frontend"}[1h]))`, "promql"},
		{"logql", `rate({app="frontend"}[5m])`, "promql"},
	}

	for _, c := range cases {
		t.Run(c.sourceDSL+"->"+c.targetDSL+" "+c.query, func(t *testing.T) {
			text, first := translate(t, c.sourceDSL, c.query, c.targetDSL)

			// Resolve the emitted text back into IR through the target's parser.
			p, _ := parser.Get(c.targetDSL)
			node, err := p.Parse(text)
			if err != nil {
				t.Fatalf("the emitted %s does not parse: %v\n  %s", c.targetDSL, err, text)
			}
			second, err := resolver.Resolve(node, c.targetDSL, reg)
			if err != nil {
				t.Fatalf("re-resolving: %v", err)
			}

			if got, want := pipelineShape(second), pipelineShape(first); got != want {
				t.Errorf("the pipeline changed shape across the translation:\n"+
					"  source IR: %s\n  target IR: %s\n  emitted:   %s", want, got, text)
			}
		})
	}
}

// pipelineShape summarises a pipeline as the operations it performs, ignoring
// the details that legitimately differ between languages — the data source's
// name, and which spelling each language uses.
func pipelineShape(query *ir.Query) string {
	parts := make([]string, 0, len(query.Pipeline))
	for _, stage := range query.Pipeline {
		switch node := stage.(type) {
		case *ir.AggregationStage:
			part := node.Op.String() + "/" + node.Scope.String()
			if len(node.GroupBy) > 0 {
				part += " by(" + strings.Join(node.GroupBy, ",") + ")"
			}
			if len(node.Without) > 0 {
				part += " without(" + strings.Join(node.Without, ",") + ")"
			}
			parts = append(parts, part)
		case *ir.FunctionStage:
			parts = append(parts, "fn:"+node.Name)
		case *ir.FilterStage:
			parts = append(parts, "filter")
		case *ir.JoinStage:
			parts = append(parts, "join:"+node.JoinType.String())
		case *ir.BinaryOpStage:
			parts = append(parts, "binary:"+node.Op.String())
		}
	}
	shape := strings.Join(parts, " | ")
	if query.Output != nil && query.Output.Window != nil && !query.Output.Window.Step.IsZero() {
		shape += " @step=" + query.Output.Window.Step.String()
	}
	return shape
}

// TestEmitterRegistryHasBothTargets covers the init-time registration seeing
// more than one language, which no single emitter package's tests can.
func TestEmitterRegistryHasBothTargets(t *testing.T) {
	got := emitter.List()
	want := []string{"logql", "promql"}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v", got, want)
		}
	}

	for _, dsl := range want {
		e, err := emitter.Get(dsl)
		if err != nil {
			t.Errorf("Get(%q): %v", dsl, err)
			continue
		}
		if e.DSL() != dsl {
			t.Errorf("Get(%q).DSL() = %q", dsl, e.DSL())
		}
	}

	if _, err := emitter.Get("traceql"); err == nil {
		t.Error("expected an error for an unregistered target")
	} else if !strings.Contains(err.Error(), "logql, promql") {
		t.Errorf("error %q should list what is registered", err)
	}
}

// TestFixesSurviveTranslation covers the six IR-level gaps end to end: each was
// a place a translation quietly lost or changed something, so each is checked
// against the text that comes out the other side.
func TestFixesSurviveTranslation(t *testing.T) {
	t.Run("arithmetic is a typed stage", func(t *testing.T) {
		text, query := translate(t, "promql", `http_requests_total / http_requests_failed`, "promql")

		stage, ok := query.Pipeline[0].(*ir.BinaryOpStage)
		if !ok {
			t.Fatalf("stage is %T, want *ir.BinaryOpStage", query.Pipeline[0])
		}
		if stage.Op != ir.ArithDiv {
			t.Errorf("Op = %s, want DIV", stage.Op)
		}
		if queryLine(text) != `http_requests_total / http_requests_failed` {
			t.Errorf("round trip = %s", queryLine(text))
		}
		assertParses(t, "promql", text)
	})

	t.Run("arithmetic crosses to logql", func(t *testing.T) {
		text, _ := translate(t, "promql", `rate(a[5m]) + rate(b[5m])`, "logql")
		assertParses(t, "logql", text)
	})

	t.Run("group_left keeps its labels", func(t *testing.T) {
		text, query := translate(t, "promql",
			`sum(rate(a[5m])) / on (job) group_left (env) sum(rate(b[5m]))`, "promql")

		join, ok := query.Pipeline[2].(*ir.JoinStage)
		if !ok {
			t.Fatalf("stage is %T, want *ir.JoinStage", query.Pipeline[2])
		}
		if got := join.IncludeLabels; len(got) != 1 || got[0] != "env" {
			t.Fatalf("IncludeLabels = %v, want [env]", got)
		}
		if !strings.Contains(text, "group_left (env)") {
			t.Errorf("the copied label should survive:\n%s", text)
		}
		assertParses(t, "promql", text)
	})

	t.Run("containment keeps a metacharacter literal", func(t *testing.T) {
		// The old lowering to a regex made the dot mean "any character", so
		// this query would also have matched "errorXlog".
		text, query := translate(t, "logql", `{app="x"} |= "error.log"`, "logql")

		filter := query.Pipeline[0].(*ir.FilterStage)
		matcher := filter.Predicate.(*ir.MatchPredicate).Matcher
		if matcher.Op != ir.MatchContains {
			t.Errorf("Op = %s, want CONTAINS", matcher.Op)
		}
		if got := queryLine(text); got != `{app="x"} |= "error.log"` {
			t.Errorf("round trip = %s, want the containment filter unchanged", got)
		}
		assertParses(t, "logql", text)
	})

	t.Run("a line filter crossing to promql is refused", func(t *testing.T) {
		// PromQL has no log body to filter on, so the escaped-pattern fallback
		// does not apply here — there is no field to match it against. The
		// fallback covers a containment matcher inside a selector, which
		// TestContainmentBecomesAnEscapedPattern exercises.
		text, _ := translate(t, "logql", `{app="x"} |= "error.log"`, "promql")

		if !strings.Contains(text, "UNSUPPORTED") {
			t.Errorf("a line filter has no PromQL form:\n%s", text)
		}
		assertParses(t, "promql", text)
	})

	t.Run("a duration keeps the units it was written with", func(t *testing.T) {
		text, query := translate(t, "logql", `count_over_time({a="b"}[90m])`, "logql")

		if got := query.Output.Window.Step.SourceText; got != "90m" {
			t.Errorf("SourceText = %q, want 90m", got)
		}
		if got := queryLine(text); got != `count_over_time({a="b"}[90m])` {
			t.Errorf("round trip = %s, want [90m] rather than [1h30m]", got)
		}
	})

	t.Run("a fractional duration falls back for promql", func(t *testing.T) {
		// LogQL writes 1.5h; PromQL has no fractional component, so the value
		// is decomposed rather than copied.
		text, _ := translate(t, "logql", `count_over_time({a="b"}[1.5h])`, "promql")

		if strings.Contains(queryLine(text), "1.5h") {
			t.Errorf("PromQL cannot write a fractional duration:\n%s", text)
		}
		if !strings.Contains(queryLine(text), "1h30m") {
			t.Errorf("the value should be decomposed:\n%s", text)
		}
		assertParses(t, "promql", text)
	})

	t.Run("the bool modifier survives promql to promql", func(t *testing.T) {
		text, query := translate(t, "promql", `up > bool 5`, "promql")

		if !query.Pipeline[0].(*ir.FilterStage).ReturnsBool {
			t.Error("ReturnsBool should be set")
		}
		if got := queryLine(text); got != `up > bool 5` {
			t.Errorf("round trip = %s", got)
		}
	})

	t.Run("the bool modifier is reported crossing to logql", func(t *testing.T) {
		text, _ := translate(t, "promql", `rate(a[5m]) > bool 5`, "logql")

		if !strings.Contains(text, "bool modifier") {
			t.Errorf("the difference should be reported:\n%s", text)
		}
		assertParses(t, "logql", text)
	})

	t.Run("a subquery keeps all three durations", func(t *testing.T) {
		text, query := translate(t, "promql", `rate(http_requests_total[5m])[30m:1m]`, "promql")

		output := query.Output
		if !output.IsSubquery() {
			t.Fatal("Output should report a subquery")
		}
		if got := output.Window.Step.Duration(); got != 5*time.Minute {
			t.Errorf("the inner window = %s, want 5m", got)
		}
		if got := output.SubqueryRange.Duration(); got != 30*time.Minute {
			t.Errorf("SubqueryRange = %s, want 30m", got)
		}
		if got := output.SubqueryStep.Duration(); got != time.Minute {
			t.Errorf("SubqueryStep = %s, want 1m", got)
		}
		if got := queryLine(text); got != `rate(http_requests_total[5m])[30m:1m]` {
			t.Errorf("round trip = %s", got)
		}
	})

	t.Run("a subquery is refused by logql", func(t *testing.T) {
		text, _ := translate(t, "promql", `rate(x[5m])[30m:1m]`, "logql")
		if !strings.Contains(text, "subquer") {
			t.Errorf("the missing construct should be reported:\n%s", text)
		}
		assertParses(t, "logql", text)
	})
}
