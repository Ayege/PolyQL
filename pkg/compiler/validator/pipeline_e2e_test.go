package validator_test

import (
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/compiler/resolver"
	"github.com/polyql/polyql/pkg/compiler/validator"
	"github.com/polyql/polyql/pkg/registry"

	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
)

// TestParseResolveValidate drives the real pipeline end to end. The unit tests
// build IR by hand to keep the validator isolated; this one checks that what the
// resolver actually produces is what the validator expects to see, which no
// isolated test can.
func TestParseResolveValidate(t *testing.T) {
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	cases := []struct {
		name        string
		sourceDSL   string
		query       string
		targetDSL   string
		wantWorst   ir.TranslatabilityFlag
		wantReasons []string
	}{
		{
			name:      "promql rate into logql is expressible",
			sourceDSL: "promql",
			query:     `rate(http_requests_total[5m])`,
			targetDSL: "logql",
			// The shapes match, but signal mismatch is separate from construct fidelity.
			wantWorst:   ir.TranslatabilityPartial,
			wantReasons: []string{"NaN-as-sentinel semantics differ between promql and logql"},
		},
		{
			name:        "logql rate into promql",
			sourceDSL:   "logql",
			query:       `rate({app="frontend"}[5m])`,
			targetDSL:   "promql",
			wantWorst:   ir.TranslatabilityPartial,
			wantReasons: []string{"NaN-as-sentinel semantics differ between logql and promql"},
		},
		{
			name:      "logql parser stage into promql",
			sourceDSL: "logql",
			query:     `sum by (level) (count_over_time({app="frontend"} | json | status >= 400 [5m]))`,
			targetDSL: "promql",
			wantWorst: ir.TranslatabilityUnsupported,
			wantReasons: []string{
				"NaN-as-sentinel semantics differ between logql and promql",
				`function "parse_json" is not available in promql`,
			},
		},
		{
			name:      "promql join into logql",
			sourceDSL: "promql",
			query:     `sum(rate(a[5m])) / on (job) group_left (env) sum(rate(b[5m]))`,
			targetDSL: "logql",
			wantWorst: ir.TranslatabilityUnsupported,
			wantReasons: []string{
				"NaN-as-sentinel semantics differ between promql and logql",
				"joins are not supported in logql",
			},
		},
		{
			name:      "promql subquery into logql",
			sourceDSL: "promql",
			query:     `rate(http_requests_total[5m])[30m:1m]`,
			targetDSL: "logql",
			wantWorst: ir.TranslatabilityUnsupported,
			wantReasons: []string{
				"NaN-as-sentinel semantics differ between promql and logql",
				"subqueries are not supported in logql",
			},
		},
		{
			name:      "promql histogram_quantile into logql",
			sourceDSL: "promql",
			query:     `histogram_quantile(0.99, sum by (le) (rate(bucket[5m])))`,
			targetDSL: "logql",
			wantWorst: ir.TranslatabilityUnsupported,
			wantReasons: []string{
				"NaN-as-sentinel semantics differ between promql and logql",
				`aggregation "histogram_quantile" is not available in logql`,
			},
		},
		{
			name:      "promql back into promql is clean",
			sourceDSL: "promql",
			query:     `sum by (job) (rate(http_requests_total{status="500"}[5m]))`,
			targetDSL: "promql",
			wantWorst: ir.TranslatabilityFull,
		},
		{
			name:      "logql back into logql is clean",
			sourceDSL: "logql",
			query:     `sum by (level) (count_over_time({app="frontend"} |= "error" | json [1h]))`,
			targetDSL: "logql",
			wantWorst: ir.TranslatabilityFull,
		},
		{
			name:      "logql regex into logql crosses no dialect",
			sourceDSL: "logql",
			query:     `{app=~"front.*"} |~ "err.*"`,
			targetDSL: "logql",
			wantWorst: ir.TranslatabilityFull,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parser.Get(c.sourceDSL)
			if err != nil {
				t.Fatalf("parser.Get: %v", err)
			}
			node, err := p.Parse(c.query)
			if err != nil {
				t.Fatalf("parsing %s: %v", c.query, err)
			}
			query, err := resolver.Resolve(node, c.sourceDSL, reg)
			if err != nil {
				t.Fatalf("resolving %s: %v", c.query, err)
			}

			// The resolver hands over a tree flagged FULL throughout.
			if worst, _ := ir.WorstTranslatability(query); worst != ir.TranslatabilityFull {
				t.Fatalf("the resolver should produce an all-FULL tree, got %s", worst)
			}

			validated, issues, _ := validator.Validate(query, c.targetDSL, reg)

			worst, _ := ir.WorstTranslatability(validated)
			if worst != c.wantWorst {
				var b strings.Builder
				for _, issue := range issues {
					b.WriteString("\n  " + issue.String())
				}
				t.Errorf("worst = %s, want %s; issues:%s", worst, c.wantWorst, b.String())
			}

			for _, want := range c.wantReasons {
				found := false
				for _, issue := range issues {
					if strings.Contains(issue.Reason, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no issue mentioned %q; got %v", want, issues)
				}
			}

			if c.wantWorst == ir.TranslatabilityFull && len(issues) != 0 {
				t.Errorf("expected no issues for a clean translation, got %v", issues)
			}
		})
	}
}

// TestValidateIsIdempotent covers running the validator twice: the flags are
// already set, so the second pass must reach the same verdict rather than
// compounding.
func TestValidateIsIdempotent(t *testing.T) {
	reg, err := registry.Open("")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := parser.Get("promql")
	node, err := p.Parse(`sum(rate(a[5m])) / on (job) group_left (env) sum(rate(b[5m]))`)
	if err != nil {
		t.Fatal(err)
	}
	query, err := resolver.Resolve(node, "promql", reg)
	if err != nil {
		t.Fatal(err)
	}

	_, first, _ := validator.Validate(query, "logql", reg)
	worstFirst, _ := ir.WorstTranslatability(query)

	_, second, _ := validator.Validate(query, "logql", reg)
	worstSecond, _ := ir.WorstTranslatability(query)

	if worstFirst != worstSecond {
		t.Errorf("worst flag changed on a second pass: %s then %s", worstFirst, worstSecond)
	}
	if len(first) != len(second) {
		t.Errorf("issue count changed on a second pass: %d then %d", len(first), len(second))
	}
}
