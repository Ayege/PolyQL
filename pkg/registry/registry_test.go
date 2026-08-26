package registry

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/ir"
)

// loadRegistry reads the definitions shipped with the package. The real YAML is
// the subject of most of these tests: a loader that only ever saw synthetic
// fixtures would not catch a definition file drifting out of step with the IR.
func loadRegistry(t *testing.T) map[string]*DSLDefinition {
	t.Helper()
	defs, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	return defs
}

// withCleanRegistry swaps in an empty installed set for the duration of a test,
// so tests neither see each other's state nor leak into it.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	loaded.Lock()
	saved := loaded.defs
	loaded.defs = make(map[string]*DSLDefinition)
	loaded.Unlock()

	t.Cleanup(func() {
		loaded.Lock()
		loaded.defs = saved
		loaded.Unlock()
	})
}

func TestLoadBothDefinitions(t *testing.T) {
	defs := loadRegistry(t)

	for _, dsl := range []string{"promql", "logql", "traceql"} {
		def, ok := defs[dsl]
		if !ok {
			t.Fatalf("%s is missing from the loaded registry (got %v)", dsl, keysOf(defs))
		}
		if def.DSL != dsl {
			t.Errorf("DSL = %q, want %q", def.DSL, dsl)
		}
		if len(def.Functions) == 0 {
			t.Errorf("%s defines no functions", dsl)
		}
		if len(def.Operators) == 0 {
			t.Errorf("%s defines no operators", dsl)
		}
		if len(def.TypeCoercion) == 0 {
			t.Errorf("%s defines no type coercions", dsl)
		}
		if len(def.SupportedSignalTypes) == 0 {
			t.Errorf("%s declares no signal types", dsl)
		}
		if !strings.HasSuffix(def.SourcePath, dsl+".yaml") {
			t.Errorf("SourcePath = %q, want it to name %s.yaml", def.SourcePath, dsl)
		}
		if !def.IsEmbedded() {
			t.Errorf("SourcePath = %q, want it marked as compiled-in", def.SourcePath)
		}
	}
}

func TestSignalTypes(t *testing.T) {
	defs := loadRegistry(t)

	if !defs["promql"].SupportsSignal(ir.SignalMetric) {
		t.Error("PromQL should query metrics")
	}
	if defs["promql"].SupportsSignal(ir.SignalLog) {
		t.Error("PromQL does not query logs")
	}
	// LogQL's metric functions derive samples from log lines, but the data
	// source is always a log stream.
	if !defs["logql"].SupportsSignal(ir.SignalLog) {
		t.Error("LogQL should query logs")
	}
	if defs["logql"].SupportsSignal(ir.SignalMetric) {
		t.Error("LogQL's data source is logs, not metrics")
	}
	// TraceQL's count() produces a number, but from spans: the data source is
	// still a span set, which is what the signal type records.
	if !defs["traceql"].SupportsSignal(ir.SignalSpan) {
		t.Error("TraceQL should query spans")
	}
	if defs["traceql"].SupportsSignal(ir.SignalMetric) {
		t.Error("TraceQL's data source is spans, not metrics")
	}
}

// TestTraceQLDefinition covers the three schema sections only a span language
// uses, and the capability flags that say what TraceQL cannot do. Each of them
// changes a verdict the validator reaches, so a definition drifting from the
// language would show up as a wrong fidelity score rather than as an error.
func TestTraceQLDefinition(t *testing.T) {
	def := loadRegistry(t)["traceql"]

	t.Run("aggregations collapse across spans", func(t *testing.T) {
		// TraceQL has no window to aggregate over, so every aggregation is on
		// the group axis. A temporal one from PromQL is therefore a scope
		// mismatch, which is the honest report.
		for _, name := range []string{"count", "sum", "avg", "min", "max"} {
			fn, err := def.Function(name)
			if err != nil {
				t.Errorf("Function(%q): %v", name, err)
				continue
			}
			if !fn.IsAggregation {
				t.Errorf("%s should map to an IR aggregation operator", name)
			}
			if fn.AggScope != ir.AggScopeGroup {
				t.Errorf("%s scope = %s, want GROUP", name, fn.AggScope)
			}
			if fn.Arity != 0 {
				t.Errorf("%s arity = %d, want 0: TraceQL writes its operand after \"over\"",
					name, fn.Arity)
			}
		}

		count, _ := def.Function("count")
		if count.AggOp != ir.AggCount {
			t.Errorf("count AggOp = %s, want COUNT", count.AggOp)
		}
		// count() counts whole spans, so it is the one aggregate whose result
		// is an integer rather than a double.
		if count.ReturnType != ir.DataTypeSignedInt {
			t.Errorf("count ReturnType = %s, want SIGNED_INT", count.ReturnType)
		}
	})

	t.Run("the metric functions are absent", func(t *testing.T) {
		// Nothing about a span is a rate, and claiming one would let the
		// validator promise a translation the emitter cannot write.
		for _, name := range []string{"rate", "increase", "histogram_quantile", "topk"} {
			if _, err := def.Function(name); err == nil {
				t.Errorf("TraceQL has no %s and should say so", name)
			}
		}
	})

	t.Run("structural operators", func(t *testing.T) {
		want := map[ir.StructuralOp]string{
			ir.StructuralChild:      ">",
			ir.StructuralDescendant: ">>",
			ir.StructuralSibling:    "~",
		}
		for op, symbol := range want {
			got, ok := def.StructuralOperatorForIROp(op)
			if !ok {
				t.Errorf("no spelling for %s", op)
				continue
			}
			if got.Symbol != symbol {
				t.Errorf("%s = %q, want %q", op, got.Symbol, symbol)
			}
			if got.Description == "" {
				t.Errorf("%s should carry a description for a fidelity report to quote", op)
			}
			if !def.Capabilities.SupportsStructuralOp(op) {
				t.Errorf("capabilities should list %s", op)
			}
		}
		if got := def.StructuralOperatorNames(); len(got) != 3 {
			t.Errorf("StructuralOperatorNames() = %v, want three", got)
		}

		// Child and descendant must stay distinct. Rendering both as ">>" would
		// silently widen every child query into a descendant one.
		child, _ := def.StructuralOperatorForIROp(ir.StructuralChild)
		descendant, _ := def.StructuralOperatorForIROp(ir.StructuralDescendant)
		if child.Symbol == descendant.Symbol {
			t.Errorf("child and descendant share the spelling %q", child.Symbol)
		}
	})

	t.Run("logical operators", func(t *testing.T) {
		want := map[ir.LogicalOp]string{
			ir.LogicalAnd: "&&",
			ir.LogicalOr:  "||",
			ir.LogicalNot: "!",
		}
		for op, symbol := range want {
			got, ok := def.LogicalOperatorForIROp(op)
			if !ok {
				t.Errorf("no spelling for %s", op)
				continue
			}
			if got.Symbol != symbol {
				t.Errorf("%s = %q, want %q", op, got.Symbol, symbol)
			}
		}
	})

	t.Run("capabilities", func(t *testing.T) {
		caps := def.Capabilities
		// Each of these is load-bearing: the validator reads it to decide
		// whether a construct is UNSUPPORTED rather than merely awkward.
		if caps.Arithmetic {
			t.Error("a span set is not a number; arithmetic should be false")
		}
		if caps.TemporalWindows {
			t.Error("Tempo takes its time range as request parameters; temporal_windows should be false")
		}
		if caps.Joins {
			t.Error("TraceQL relates spans by the trace tree, not by joining; joins should be false")
		}
		if caps.Subqueries {
			t.Error("TraceQL has no subquery form")
		}
		if !caps.BooleanSelectors {
			t.Error("TraceQL braces hold a full boolean expression")
		}
		if !caps.AttributeCasts {
			t.Error("TraceQL can reinterpret an attribute's type with \"as\"")
		}
		if !caps.ScopedAttributes {
			t.Error("TraceQL addresses attributes through span./resource./intrinsic scopes")
		}
		if !caps.MetricExtraction {
			t.Error("count() over (...) derives a number from records that are not numbers")
		}
	})

	t.Run("the other definitions keep the defaults", func(t *testing.T) {
		// The capability defaults describe the languages already in the
		// registry, so a definition that says nothing keeps its meaning.
		defs := loadRegistry(t)
		for _, dsl := range []string{"promql", "logql"} {
			caps := defs[dsl].Capabilities
			if !caps.Arithmetic {
				t.Errorf("%s has arithmetic and declares nothing, so it should default to true", dsl)
			}
			if !caps.TemporalWindows {
				t.Errorf("%s has a range selector and declares nothing, so it should default to true", dsl)
			}
			if caps.BooleanSelectors {
				t.Errorf("%s has a conjunctive selector, so boolean_selectors should default to false", dsl)
			}
			if len(caps.StructuralOps) != 0 {
				t.Errorf("%s does not query spans and should declare no structural operators", dsl)
			}
		}
	})
}

// TestLookupRate covers the mapping the resolver depends on most: a DSL
// function name resolving to an IR aggregation operator and scope.
func TestLookupRate(t *testing.T) {
	defs := loadRegistry(t)

	for _, dsl := range []string{"promql", "logql"} {
		t.Run(dsl, func(t *testing.T) {
			fn, err := defs[dsl].Function("rate")
			if err != nil {
				t.Fatalf("Function(\"rate\"): %v", err)
			}
			if !fn.IsAggregation {
				t.Fatal("rate should map to an IR aggregation operator")
			}
			if fn.AggOp != ir.AggRate {
				t.Errorf("AggOp = %s, want RATE", fn.AggOp)
			}
			if fn.AggScope != ir.AggScopeTemporal {
				t.Errorf("AggScope = %s, want TEMPORAL", fn.AggScope)
			}
			if fn.Arity != 1 {
				t.Errorf("Arity = %d, want 1", fn.Arity)
			}
			if len(fn.ArgTypes) != 1 {
				t.Fatalf("got %d arg types, want 1", len(fn.ArgTypes))
			}
			// A range vector coerces to DOUBLE, and the DSL-level name is kept
			// so the validator can still explain a lost distinction.
			if got := fn.ArgTypes[0].Type; got != ir.DataTypeDouble {
				t.Errorf("arg type = %s, want DOUBLE", got)
			}
			if fn.ReturnType != ir.DataTypeDouble {
				t.Errorf("ReturnType = %s, want DOUBLE", fn.ReturnType)
			}
		})
	}

	t.Run("promql arg type keeps its DSL name", func(t *testing.T) {
		fn, _ := defs["promql"].Function("rate")
		if got := fn.ArgTypes[0].Name; got != "range_vector" {
			t.Errorf("arg type name = %q, want range_vector", got)
		}
	})
}

// TestTemporalAndGroupScopesAreDistinct covers what keeps sum and sum_over_time
// apart. Both are IR SUM, so only the scope tells an emitter which to write.
func TestTemporalAndGroupScopesAreDistinct(t *testing.T) {
	promql := loadRegistry(t)["promql"]

	group, err := promql.Function("sum")
	if err != nil {
		t.Fatalf("Function(\"sum\"): %v", err)
	}
	temporal, err := promql.Function("sum_over_time")
	if err != nil {
		t.Fatalf("Function(\"sum_over_time\"): %v", err)
	}

	if group.AggOp != ir.AggSum || temporal.AggOp != ir.AggSum {
		t.Fatalf("both should map to IR SUM, got %s and %s", group.AggOp, temporal.AggOp)
	}
	if group.AggScope != ir.AggScopeGroup {
		t.Errorf("sum scope = %s, want GROUP", group.AggScope)
	}
	if temporal.AggScope != ir.AggScopeTemporal {
		t.Errorf("sum_over_time scope = %s, want TEMPORAL", temporal.AggScope)
	}
}

// TestNonAggregationFunctions covers the other mapping path: a function with no
// IR aggregation equivalent becomes a FunctionStage rather than claiming a
// false equivalence.
func TestNonAggregationFunctions(t *testing.T) {
	defs := loadRegistry(t)

	fn, err := defs["promql"].Function("histogram_quantile")
	if err != nil {
		t.Fatalf("Function(\"histogram_quantile\"): %v", err)
	}
	if !fn.IsAggregation || fn.AggOp != ir.AggHistogramQuantile {
		t.Errorf("histogram_quantile should map to IR HISTOGRAM_QUANTILE, got %+v", fn)
	}

	abs, err := defs["promql"].Function("abs")
	if err != nil {
		t.Fatalf("Function(\"abs\"): %v", err)
	}
	if abs.IsAggregation {
		t.Error("abs has no IR aggregation operator and should not claim one")
	}
	if abs.IRName != "abs" {
		t.Errorf("IRName = %q, want it to default to the function name", abs.IRName)
	}

	// LogQL's bytes_rate measures payload size, not entry count, so mapping it
	// to IR RATE would hide translation loss.
	bytesRate, err := defs["logql"].Function("bytes_rate")
	if err != nil {
		t.Fatalf("Function(\"bytes_rate\"): %v", err)
	}
	if bytesRate.IsAggregation {
		t.Error("bytes_rate should not claim to be IR RATE")
	}
}

func TestLookupUnknownFunction(t *testing.T) {
	defs := loadRegistry(t)

	fn, err := defs["promql"].Function("no_such_function")
	if err == nil {
		t.Fatalf("expected an error, got %+v", fn)
	}
	if fn != nil {
		t.Errorf("definition should be nil on error, got %+v", fn)
	}
	for _, want := range []string{"promql", "no_such_function"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}

	// A function one DSL has and the other does not must not leak across.
	if _, err := defs["logql"].Function("histogram_quantile"); err == nil {
		t.Error("LogQL has no histogram_quantile and should say so")
	}
	if _, err := defs["promql"].Function("line_format"); err == nil {
		t.Error("PromQL has no line_format and should say so")
	}
}

// TestNaNSentinelDiffers covers the difference SKILL.md calls out as a known
// translation hazard: PromQL uses NaN to mean "no data", LogQL returns nothing,
// and QLS mandates NULL.
func TestNaNSentinelDiffers(t *testing.T) {
	defs := loadRegistry(t)

	if !defs["promql"].AggregationDefaults.NaNAsSentinel {
		t.Error("PromQL uses NaN as its missing-data sentinel; nan_as_sentinel should be true")
	}
	if defs["logql"].AggregationDefaults.NaNAsSentinel {
		t.Error("LogQL returns empty results rather than NaN; nan_as_sentinel should be false")
	}
}

// TestAggregationDefaultsFollowQLS covers the NULL substitution rules from QLS
// §Aggregation, which every DSL shares.
func TestAggregationDefaultsFollowQLS(t *testing.T) {
	defs := loadRegistry(t)

	for dsl, def := range defs {
		agg := def.AggregationDefaults
		if agg.NullSubstituteAdd != 0 {
			t.Errorf("%s: null_substitute_add = %v, want 0 per QLS", dsl, agg.NullSubstituteAdd)
		}
		if agg.NullSubstituteMul != 1 {
			t.Errorf("%s: null_substitute_mul = %v, want 1 per QLS", dsl, agg.NullSubstituteMul)
		}
		if agg.NullSubstituteDiv != 0 {
			t.Errorf("%s: null_substitute_div = %v, want 0 per QLS", dsl, agg.NullSubstituteDiv)
		}
	}
}

// TestJoinCapability covers the capability SKILL.md singles out: LogQL cannot
// join, so an IR JoinStage translated into it must be flagged UNSUPPORTED.
func TestJoinCapability(t *testing.T) {
	defs := loadRegistry(t)

	promql := defs["promql"].Capabilities
	if !promql.Joins {
		t.Error("PromQL joins instant vectors through vector matching")
	}
	if !promql.SupportsJoinType(ir.JoinInner) {
		t.Error("PromQL's default vector matching is an inner equi-join")
	}
	if promql.SupportsJoinType(ir.JoinCross) {
		t.Error("PromQL has no cross join, so it should not claim one")
	}

	logql := defs["logql"].Capabilities
	if logql.Joins {
		t.Error("LogQL has no join")
	}
	if len(logql.JoinTypes) != 0 {
		t.Errorf("JoinTypes = %v, want empty for a DSL with no join", logql.JoinTypes)
	}
	for _, jt := range []ir.JoinType{ir.JoinInner, ir.JoinLeftOuter, ir.JoinFullOuter, ir.JoinCross} {
		if logql.SupportsJoinType(jt) {
			t.Errorf("LogQL should not support %s", jt)
		}
	}
	if defs["logql"].Capabilities.Subqueries {
		t.Error("LogQL has no subquery syntax")
	}
}

func TestOperatorMapping(t *testing.T) {
	defs := loadRegistry(t)

	cases := []struct {
		dsl, symbol string
		want        ir.MatchOp
		context     OperatorContext
	}{
		{"promql", "=", ir.MatchEQ, OperatorContextSelector},
		{"promql", "=~", ir.MatchRegex, OperatorContextSelector},
		{"promql", "!~", ir.MatchNotRegex, OperatorContextSelector},
		// "!=" is the one PromQL spelling used both as a label matcher and as a
		// value comparison, so it is not tied to either context.
		{"promql", "!=", ir.MatchNEQ, OperatorContextAny},
		// PromQL spells equality "=" in a selector and "==" in a comparison;
		// both are IR EQ, and the context is what tells them apart.
		{"promql", "==", ir.MatchEQ, OperatorContextComparison},
		{"promql", ">=", ir.MatchGTE, OperatorContextComparison},
		// LogQL writes the same four matchers inside a stream selector and as
		// label filters, so their context is "any".
		{"logql", "=~", ir.MatchRegex, OperatorContextAny},
		{"logql", "=", ir.MatchEQ, OperatorContextAny},
		{"logql", "<", ir.MatchLT, OperatorContextComparison},
		// A LogQL line filter tests the raw line. "|=" is containment, which the
		// IR names outright rather than lowering to a regex; the entries are
		// keyed by name because LogQL spells the negated forms the same way as
		// its selector matchers.
		{"logql", "line_contains", ir.MatchContains, OperatorContextLineFilter},
		{"logql", "line_matches_regex", ir.MatchRegex, OperatorContextLineFilter},
		{"logql", "line_not_contains", ir.MatchNotContains, OperatorContextLineFilter},
	}

	for _, c := range cases {
		t.Run(c.dsl+" "+c.symbol, func(t *testing.T) {
			op, err := defs[c.dsl].Operator(c.symbol)
			if err != nil {
				t.Fatalf("Operator(%q): %v", c.symbol, err)
			}
			if op.IROp != c.want {
				t.Errorf("IROp = %s, want %s", op.IROp, c.want)
			}
			if op.Context != c.context {
				t.Errorf("Context = %s, want %s", op.Context, c.context)
			}
		})
	}

	if _, err := defs["promql"].Operator("line_contains"); err == nil {
		t.Error("PromQL has no line filter operator and should say so")
	}

	t.Run("an entry keyed by name still spells itself", func(t *testing.T) {
		op, err := defs["logql"].Operator("line_not_contains")
		if err != nil {
			t.Fatalf("Operator: %v", err)
		}
		// LogQL writes a negated line filter "!=", the same as a label
		// mismatch, so the entry is keyed by name and states its spelling.
		if op.Symbol != "!=" {
			t.Errorf("Symbol = %q, want !=", op.Symbol)
		}
	})
}

func TestTypeCoercion(t *testing.T) {
	defs := loadRegistry(t)

	cases := []struct {
		dsl, name string
		want      ir.QlsDataType
	}{
		{"promql", "instant_vector", ir.DataTypeDouble},
		{"promql", "range_vector", ir.DataTypeDouble},
		{"promql", "scalar", ir.DataTypeDouble},
		{"promql", "string", ir.DataTypeString},
		{"logql", "log_stream", ir.DataTypeString},
		{"logql", "unwrapped_range", ir.DataTypeDouble},
		{"logql", "duration", ir.DataTypeInterval},
		{"logql", "bytes", ir.DataTypeUnsignedInt},
	}
	for _, c := range cases {
		t.Run(c.dsl+" "+c.name, func(t *testing.T) {
			got, err := defs[c.dsl].CoerceType(c.name)
			if err != nil {
				t.Fatalf("CoerceType(%q): %v", c.name, err)
			}
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}

	if _, err := defs["promql"].CoerceType("log_stream"); err == nil {
		t.Error("PromQL has no log_stream type and should say so")
	}
}

func TestMetricTypes(t *testing.T) {
	promql := loadRegistry(t)["promql"]

	cases := []struct {
		name string
		want ir.QlsMetricType
	}{
		{"counter", ir.MetricTypeCumulativeCounter},
		{"gauge", ir.MetricTypeGauge},
		{"histogram", ir.MetricTypeHistogram},
		{"summary", ir.MetricTypeApproximateDistribution},
		{"untyped", ir.MetricTypeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := promql.MetricType(c.name)
			if err != nil {
				t.Fatalf("MetricType(%q): %v", c.name, err)
			}
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}

	if _, err := promql.MetricType("no_such_type"); err == nil {
		t.Error("an unknown metric type should be an error")
	}
}

// TestLoadInstallsForGet covers the coupling between the two entry points: Load
// installs the process-wide set that Get reads.
func TestLoadInstallsForGet(t *testing.T) {
	withCleanRegistry(t)

	if _, err := Get("promql"); err == nil {
		t.Fatal("Get should fail before Load has run")
	} else if !strings.Contains(err.Error(), "Load has not been called") {
		t.Errorf("error %q should explain that the registry is empty", err)
	}

	// An empty directory selects the compiled-in definitions.
	if _, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	}

	def, err := Get("promql")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if def.DSL != "promql" {
		t.Errorf("DSL = %q", def.DSL)
	}
	// Lookup is case-insensitive, since a CLI flag may arrive spelled anyhow.
	if _, err := Get("  PromQL "); err != nil {
		t.Errorf("Get should normalise the name: %v", err)
	}

	// The set is asserted by membership rather than by length, so adding a
	// language does not break a test that is about installation rather than
	// about how many definitions ship.
	assertContainsDSLs(t, List(), "logql", "promql", "traceql")

	// A name no definition claims, so the error path stays reachable however
	// many languages the registry grows.
	_, err = Get("nonsuchql")
	if err == nil {
		t.Fatal("expected an error for an unloaded DSL")
	}
	if !strings.Contains(err.Error(), "logql, promql") {
		t.Errorf("error %q should list what is loaded", err)
	}
}

// assertContainsDSLs checks that a sorted DSL list holds each wanted name.
func assertContainsDSLs(t *testing.T, got []string, want ...string) {
	t.Helper()
	have := make(map[string]bool, len(got))
	for _, name := range got {
		have[name] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("%v is missing %q", got, name)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("%v should be sorted", got)
	}
}

// TestLoadRejectsBadDefinitions covers the loader's strictness. The YAML is the
// contribution surface, so a mistake in it must fail loudly at load rather than
// silently produce a wrong translation later.
func TestLoadRejectsBadDefinitions(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantMsg string
	}{
		{
			name:    "missing dsl name",
			yaml:    "signal_types: [metric]\n",
			wantMsg: `missing the required "dsl"`,
		},
		{
			name:    "no signal types",
			yaml:    "dsl: testql\n",
			wantMsg: `at least one entry under "signal_types"`,
		},
		{
			name:    "unknown signal type",
			yaml:    "dsl: testql\nsignal_types: [telepathy]\n",
			wantMsg: "not a valid SignalType",
		},
		{
			name:    "misspelled top-level key",
			yaml:    "dsl: testql\nsignal_types: [metric]\nfunctionz: {}\n",
			wantMsg: "field functionz not found",
		},
		{
			name: "misspelled function key",
			yaml: "dsl: testql\nsignal_types: [metric]\nfunctions:\n  rate:\n" +
				"    ir_knid: RATE\n    arity: 0\n    return_type: DOUBLE\n",
			wantMsg: "field ir_knid not found",
		},
		{
			name: "unknown ir_kind",
			yaml: "dsl: testql\nsignal_types: [metric]\nfunctions:\n  rate:\n" +
				"    ir_kind: TELEPORT\n    arity: 0\n    return_type: DOUBLE\n",
			wantMsg: "not a valid AggOp",
		},
		{
			name: "unknown return type",
			yaml: "dsl: testql\nsignal_types: [metric]\nfunctions:\n  rate:\n" +
				"    arity: 0\n    return_type: VIBES\n",
			wantMsg: "neither a QLS data type nor a type_coercion entry",
		},
		{
			name:    "missing return type",
			yaml:    "dsl: testql\nsignal_types: [metric]\nfunctions:\n  rate:\n    arity: 0\n",
			wantMsg: "return_type is required",
		},
		{
			name: "arity disagrees with arg_types",
			yaml: "dsl: testql\nsignal_types: [metric]\nfunctions:\n  rate:\n" +
				"    arity: 3\n    arg_types: [DOUBLE]\n    return_type: DOUBLE\n",
			wantMsg: "arity is 3 but arg_types lists 1",
		},
		{
			name: "agg_scope without ir_kind",
			yaml: "dsl: testql\nsignal_types: [metric]\nfunctions:\n  abs:\n" +
				"    agg_scope: GROUP\n    arity: 0\n    return_type: DOUBLE\n",
			wantMsg: "only meaningful alongside ir_kind",
		},
		{
			name: "temporal-only operator given a group scope",
			yaml: "dsl: testql\nsignal_types: [metric]\nfunctions:\n  rate:\n" +
				"    ir_kind: RATE\n    agg_scope: GROUP\n    arity: 0\n    return_type: DOUBLE\n",
			wantMsg: "only aggregates over time",
		},
		{
			name:    "unknown ir_op",
			yaml:    "dsl: testql\nsignal_types: [metric]\noperators:\n  \"=\": { ir_op: EQUALSISH }\n",
			wantMsg: "not a valid MatchOp",
		},
		{
			name:    "missing ir_op",
			yaml:    "dsl: testql\nsignal_types: [metric]\noperators:\n  \"=\": {}\n",
			wantMsg: "ir_op is required",
		},
		{
			name:    "unknown operator context",
			yaml:    "dsl: testql\nsignal_types: [metric]\noperators:\n  \"=\": { ir_op: EQ, context: vibes }\n",
			wantMsg: "not a valid operator context",
		},
		{
			name:    "unknown metric type",
			yaml:    "dsl: testql\nsignal_types: [metric]\nmetric_types:\n  counter: SPINNER\n",
			wantMsg: "not a valid QlsMetricType",
		},
		{
			name: "join types listed but joins disabled",
			yaml: "dsl: testql\nsignal_types: [metric]\ncapabilities:\n" +
				"  joins: false\n  join_types: [INNER]\n",
			wantMsg: "lists join_types but sets joins: false",
		},
		{
			name: "unknown normalization value",
			yaml: "dsl: testql\nsignal_types: [metric]\nnormalizations:\n" +
				"  duration_format: largest_unti\n",
			wantMsg: "expected one of: largest_unit, verbatim",
		},
		{
			name: "misspelled normalization key",
			yaml: "dsl: testql\nsignal_types: [metric]\nnormalizations:\n" +
				"  duration_fmt: verbatim\n",
			wantMsg: "field duration_fmt not found",
		},
		{
			name:    "malformed yaml",
			yaml:    "dsl: testql\nsignal_types: [metric\n",
			wantMsg: "cannot parse",
		},
		{
			name:    "wrong type for a field",
			yaml:    "dsl: testql\nsignal_types: [metric]\nfunctions: not-a-mapping\n",
			wantMsg: "cannot parse",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "testql.yaml")
			if err := os.WriteFile(path, []byte(c.yaml), 0o600); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}

			_, err := LoadDir(dir)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error %q should contain %q", err, c.wantMsg)
			}
			// Every error should name the file, so a contributor knows where to
			// look.
			if !strings.Contains(err.Error(), "testql.yaml") {
				t.Errorf("error %q should name the offending file", err)
			}
		})
	}
}

func TestLoadDirErrors(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		if _, err := LoadDir(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("expected an error for a missing directory")
		}
	})

	t.Run("no definitions", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadDir(dir)
		if err == nil || !strings.Contains(err.Error(), "no DSL definitions found") {
			t.Errorf("got %v, want a no-definitions error", err)
		}
	})

	t.Run("duplicate DSL", func(t *testing.T) {
		dir := t.TempDir()
		body := "dsl: testql\nsignal_types: [metric]\n"
		for _, name := range []string{"a.yaml", "b.yaml"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := LoadDir(dir)
		if err == nil || !strings.Contains(err.Error(), "defined twice") {
			t.Errorf("got %v, want a duplicate-DSL error", err)
		}
	})
}

// TestLoadIsAllOrNothing covers the installed set surviving a bad reload, so a
// broken edit cannot leave the process on a half-applied registry.
func TestLoadIsAllOrNothing(t *testing.T) {
	withCleanRegistry(t)

	if _, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	}

	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "bad.yaml"), []byte("dsl: [not a string]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(broken); err == nil {
		t.Fatal("expected the bad load to fail")
	}

	if _, err := Get("promql"); err != nil {
		t.Errorf("the previously loaded registry should survive a failed reload: %v", err)
	}
}

// TestFunctionNamesAreSorted covers the deterministic listing the CLI and tests
// rely on.
func TestFunctionNamesAreSorted(t *testing.T) {
	names := loadRegistry(t)["promql"].FunctionNames()
	if len(names) == 0 {
		t.Fatal("no function names")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("FunctionNames is not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

func keysOf(defs map[string]*DSLDefinition) []string {
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	// Sorted so that a failure message reads the same on every run, and so a
	// caller comparing against List's own ordering is comparing like with like.
	sort.Strings(names)
	return names
}

// TestLoadFallsBackToEmbedded covers the distribution decision: polyql ships as
// a single binary, so an empty directory means "use the definitions compiled
// in", while a directory overrides them entirely.
func TestLoadFallsBackToEmbedded(t *testing.T) {
	t.Run("empty directory selects the embedded set", func(t *testing.T) {
		withCleanRegistry(t)
		for _, dir := range []string{"", "   "} {
			defs, err := Load(dir)
			if err != nil {
				t.Fatalf("Load(%q): %v", dir, err)
			}
			assertContainsDSLs(t, keysOf(defs), "logql", "promql", "traceql")
			if !defs["promql"].IsEmbedded() {
				t.Errorf("SourcePath = %q, want the embedded set", defs["promql"].SourcePath)
			}
		}
	})

	t.Run("a directory overrides the embedded set", func(t *testing.T) {
		withCleanRegistry(t)

		dir := t.TempDir()
		override := "dsl: promql\nsignal_types: [log]\n"
		if err := os.WriteFile(filepath.Join(dir, "promql.yaml"), []byte(override), 0o600); err != nil {
			t.Fatal(err)
		}

		defs, err := Load(dir)
		if err != nil {
			t.Fatalf("Load(%s): %v", dir, err)
		}
		// The override replaces the whole set rather than merging into it, so
		// logql is absent and promql is the local file.
		if len(defs) != 1 {
			t.Errorf("loaded %v, want only the overriding definition", keysOf(defs))
		}
		if defs["promql"].IsEmbedded() {
			t.Error("the on-disk definition should not be marked embedded")
		}
		if !defs["promql"].SupportsSignal(ir.SignalLog) {
			t.Error("the override's contents should win over the embedded definition")
		}
	})
}

// TestLoadEmbedded covers the definitions a built binary carries.
func TestLoadEmbedded(t *testing.T) {
	defs, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	for _, dsl := range []string{"promql", "logql"} {
		def, ok := defs[dsl]
		if !ok {
			t.Fatalf("%s is missing from the embedded set (got %v)", dsl, keysOf(defs))
		}
		if !def.IsEmbedded() {
			t.Errorf("%s: SourcePath = %q, want it marked as compiled-in", dsl, def.SourcePath)
		}

		// Spot-check a function every definition must carry, so an empty or
		// truncated embed fails here rather than at translation time.
		fn, err := def.Function("rate")
		if err != nil {
			t.Fatalf("%s has no rate: %v", dsl, err)
		}
		if !fn.IsAggregation || fn.AggOp != ir.AggRate || fn.AggScope != ir.AggScopeTemporal {
			t.Errorf("%s: rate = %+v, want the temporal RATE aggregation", dsl, fn)
		}
	}

	t.Run("DefaultRegistry wraps it", func(t *testing.T) {
		reg, err := DefaultRegistry()
		if err != nil {
			t.Fatalf("DefaultRegistry: %v", err)
		}
		assertContainsDSLs(t, reg.List(), "logql", "promql", "traceql")
		if _, err := reg.Get("promql"); err != nil {
			t.Errorf("Get: %v", err)
		}
	})

	t.Run("DefaultRegistry does not install globally", func(t *testing.T) {
		withCleanRegistry(t)
		if _, err := DefaultRegistry(); err != nil {
			t.Fatal(err)
		}
		// Installing the process-wide set is Load's business, not this one's.
		if _, err := Get("promql"); err == nil {
			t.Error("DefaultRegistry should leave the installed set untouched")
		}
	})
}

// sourceOfTruthDir is where contributors edit the language definitions. The
// copies under data/ exist only because go:embed cannot reach outside its own
// package directory.
const sourceOfTruthDir = "../../registry"

// TestEmbeddedMatchesDisk is the sync check.
//
// A definition edited under registry/ and not copied into data/ would leave a
// built binary running on stale rules while the repository looked correct. That
// is a silent wrong-translation bug, so it fails here — and in CI, which runs
// the same comparison through "make check-generate".
func TestEmbeddedMatchesDisk(t *testing.T) {
	embedded, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	onDisk, err := LoadDir(sourceOfTruthDir)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", sourceOfTruthDir, err)
	}

	if len(embedded) != len(onDisk) {
		t.Fatalf("embedded has %v, %s has %v", keysOf(embedded), sourceOfTruthDir, keysOf(onDisk))
	}

	for dsl, want := range onDisk {
		got, ok := embedded[dsl]
		if !ok {
			t.Errorf("%s is on disk but not embedded; run 'make generate'", dsl)
			continue
		}
		if diff := describeDefinitionDiff(got, want); diff != "" {
			t.Errorf("%s has drifted from %s/%s.yaml; run 'make generate':\n%s",
				dsl, sourceOfTruthDir, dsl, diff)
		}
	}

	t.Run("the bytes are identical", func(t *testing.T) {
		// Comparing the parsed definitions catches every difference that
		// matters, but comparing the bytes also catches a comment or an
		// ordering change, which is what a contributor actually edited.
		entries, err := os.ReadDir(sourceOfTruthDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !isDefinitionFile(entry.Name()) {
				continue
			}
			source, err := os.ReadFile(filepath.Join(sourceOfTruthDir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(filepath.Join("data", entry.Name()))
			if err != nil {
				t.Errorf("%s has no embedded copy; run 'make generate': %v", entry.Name(), err)
				continue
			}
			if !bytes.Equal(source, copied) {
				t.Errorf("data/%s differs from %s/%s; run 'make generate'",
					entry.Name(), sourceOfTruthDir, entry.Name())
			}
		}
	})
}

// describeDefinitionDiff reports how two definitions differ, or "" when they
// agree on everything the compiler reads.
func describeDefinitionDiff(got, want *DSLDefinition) string {
	var diffs []string

	if len(got.Functions) != len(want.Functions) {
		diffs = append(diffs, fmt.Sprintf("  functions: %d vs %d", len(got.Functions), len(want.Functions)))
	}
	for name, wantFn := range want.Functions {
		gotFn, ok := got.Functions[name]
		if !ok {
			diffs = append(diffs, "  missing function "+name)
			continue
		}
		if gotFn.IRName != wantFn.IRName || gotFn.IsAggregation != wantFn.IsAggregation ||
			gotFn.AggOp != wantFn.AggOp || gotFn.AggScope != wantFn.AggScope ||
			gotFn.ReturnType != wantFn.ReturnType {
			diffs = append(diffs, "  function "+name+" differs")
		}
	}
	for symbol, wantOp := range want.Operators {
		gotOp, ok := got.Operators[symbol]
		if !ok {
			diffs = append(diffs, "  missing operator "+symbol)
			continue
		}
		if *gotOp != *wantOp {
			diffs = append(diffs, "  operator "+symbol+" differs")
		}
	}
	if got.AggregationDefaults != want.AggregationDefaults {
		diffs = append(diffs, "  aggregation defaults differ")
	}
	if got.Normalizations != want.Normalizations {
		diffs = append(diffs, "  normalizations differ")
	}
	if got.Capabilities.Joins != want.Capabilities.Joins ||
		got.Capabilities.Subqueries != want.Capabilities.Subqueries ||
		got.Capabilities.PipelineOrdered != want.Capabilities.PipelineOrdered ||
		got.Capabilities.BoolModifier != want.Capabilities.BoolModifier {
		diffs = append(diffs, "  capabilities differ")
	}

	sort.Strings(diffs)
	return strings.Join(diffs, "\n")
}
