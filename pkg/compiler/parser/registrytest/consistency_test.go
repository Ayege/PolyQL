package registrytest

import (
	"testing"

	"github.com/polyql/polyql/pkg/compiler/parser/logql"
	"github.com/polyql/polyql/pkg/compiler/parser/promql"
	"github.com/polyql/polyql/pkg/compiler/parser/traceql"
	"github.com/polyql/polyql/pkg/registry"
)

// TestRegistryCoversEveryParserFunction guards the seam between stages 1 and 3
// of the pipeline. A function the parser accepts but the registry does not
// describe parses fine and then fails at resolve time, which is exactly the kind
// of drift that a data-driven registry invites as the parsers grow.
func TestRegistryCoversEveryParserFunction(t *testing.T) {
	defs, err := registry.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	cases := []struct {
		dsl   string
		names []string
	}{
		{"promql", promql.FunctionNames()},
		{"logql", logql.FunctionNames()},
		{"traceql", traceql.FunctionNames()},
	}

	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			def, ok := defs[c.dsl]
			if !ok {
				t.Fatalf("no registry definition for %s", c.dsl)
			}
			if len(c.names) == 0 {
				t.Fatal("the parser reports no functions")
			}
			for _, name := range c.names {
				if _, err := def.Function(name); err != nil {
					t.Errorf("the %s parser accepts %q but %s has no entry for it; "+
						"the resolver would fail on any query using it", c.dsl, name, def.SourcePath)
				}
			}
		})
	}
}

// TestRegistryAggregationOperatorsAreNotParserFunctions documents the one
// intended asymmetry: PromQL writes sum, topk and friends as operators rather
// than calls, so its parser handles them as keywords while the registry still
// carries their IR mapping.
func TestRegistryAggregationOperatorsAreNotParserFunctions(t *testing.T) {
	defs, err := registry.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	for _, name := range []string{"sum", "avg", "min", "max", "count", "topk", "bottomk", "quantile"} {
		if _, isParserFunction := promql.LookupFunction(name); isParserFunction {
			t.Errorf("%q should be an aggregation keyword in the PromQL parser, not a function", name)
		}
		fn, err := defs["promql"].Function(name)
		if err != nil {
			t.Errorf("the registry should still map the aggregation operator %q: %v", name, err)
			continue
		}
		if !fn.IsAggregation {
			t.Errorf("%q should map to an IR aggregation operator", name)
		}
	}
}

// TestParserRecognizesEveryRegistryFunction is the reverse direction: a registry
// entry naming something the parser cannot produce is dead weight at best, and
// at worst a promise the round trip cannot keep — an emitter could render a call
// the source parser would then reject.
//
// The one intended asymmetry is PromQL's aggregation operators, which its
// grammar spells as keywords rather than calls; those are allowed through
// explicitly rather than by loosening the check.
func TestParserRecognizesEveryRegistryFunction(t *testing.T) {
	defs, err := registry.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	cases := []struct {
		dsl string
		// recognized reports whether the DSL's parser can produce a call to
		// this name in any form.
		recognized func(name string) bool
	}{
		{
			dsl: "promql",
			recognized: func(name string) bool {
				if _, ok := promql.LookupFunction(name); ok {
					return true
				}
				return promql.IsAggregatorName(name)
			},
		},
		{
			dsl: "logql",
			recognized: func(name string) bool {
				for _, known := range logql.FunctionNames() {
					if known == name {
						return true
					}
				}
				// LogQL spells its pipeline stages as keywords, the same
				// intended asymmetry as PromQL's aggregation operators.
				return logql.IsStageKeyword(name)
			},
		},
		{
			dsl: "traceql",
			recognized: func(name string) bool {
				// TraceQL writes every one of its functions as an aggregate
				// call, so there is no asymmetry to allow for here.
				_, ok := traceql.LookupAggregate(name)
				return ok
			},
		},
	}

	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			def, ok := defs[c.dsl]
			if !ok {
				t.Fatalf("no registry definition for %s", c.dsl)
			}
			for _, name := range def.FunctionNames() {
				if !c.recognized(name) {
					t.Errorf("%s lists %q but the %s parser cannot produce it; "+
						"either add parser support or drop the entry",
						def.SourcePath, name, c.dsl)
				}
			}
		})
	}
}
