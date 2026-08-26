package ir

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

var (
	fixtureStart = time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC)
	fixtureEnd   = fixtureStart.Add(time.Hour)
)

// newFixtureQuery builds a query that exercises every IR node type at least
// once. It is roughly the IR a resolver would produce for
//
//	topk(3, histogram_quantile(0.99, sum by (job) (rate(http_requests_total{...}[5m])))) / on (job) up
//
// with a boolean filter that PromQL itself could not express, so the tree covers
// the QLS constructs the IR must carry even when a given DSL cannot.
func newFixtureQuery() *Query {
	return &Query{
		Signal: SignalMetric,
		Source: &DataSource{
			Name:  "http_requests_total",
			Scope: ScopeResource,
			Selectors: []*Selector{{
				Matchers: []*LabelMatcher{
					{Key: "status", Op: MatchEQ, Value: "500"},
					{Key: "job", Op: MatchRegex, Value: "api.*"},
				},
			}},
		},
		Pipeline: Pipeline{
			&FilterStage{Predicate: &LogicalPredicate{
				Op: LogicalAnd,
				Operands: []Predicate{
					&MatchPredicate{Matcher: &LabelMatcher{
						Key: "method", Op: MatchIn, Values: []string{"GET", "POST"},
					}},
					&MatchPredicate{Matcher: &LabelMatcher{
						Key: "region", Op: MatchIsNotNull,
					}},
				},
			}},
			&AggregationStage{Op: AggRate, Scope: AggScopeTemporal},
			&AggregationStage{Op: AggSum, GroupBy: []string{"job"}, Scope: AggScopeGroup},
			&FunctionStage{
				Name:       "histogram_quantile",
				ReturnType: DataTypeDouble,
				Args: []IRExpr{
					NewNumberLiteral(0.99),
					&RefExpr{Name: "le", Scope: ScopeUnscoped, Type: DataTypeDouble},
					&QueryExpr{Query: &Query{
						Signal: SignalMetric,
						Source: &DataSource{Name: "http_request_duration_bucket", Scope: ScopeUnscoped},
					}},
				},
			},
			&AggregationStage{
				Op:        AggTopK,
				Scope:     AggScopeGroup,
				Parameter: NewNumberLiteral(3),
			},
			&JoinStage{
				JoinType:      JoinInner,
				OnLabels:      []string{"job"},
				IncludeLabels: []string{"env"},
				RightSide: &Query{
					Signal: SignalMetric,
					Source: &DataSource{Name: "up", Scope: ScopeUnscoped},
				},
			},
			&BinaryOpStage{
				Op:    ArithDiv,
				Left:  &Query{Signal: SignalMetric, Source: &DataSource{Name: "num"}},
				Right: &Query{Signal: SignalMetric, Source: &DataSource{Name: "den"}},
			},
		},
		Output: &Output{
			Range: &TimeRange{
				Start: NewTimestamp(fixtureStart),
				End:   NewTimestamp(fixtureEnd),
			},
			Window: &Window{
				Step:      NewInterval(time.Minute),
				Offset:    NewInterval(30 * time.Second),
				Alignment: WindowUTCNormalized,
			},
			GroupBy: []string{"job"},
			Sort:    SortDesc,
			Limit:   10,
		},
		Hints: map[string]string{"promql.metric_type": "counter"},
	}
}

// TestQueryConstructsEveryNodeType asserts the fixture reaches every node type
// the IR defines, so that a gap in Walk or in the fixture shows up here rather
// than in a downstream component.
func TestQueryConstructsEveryNodeType(t *testing.T) {
	want := []string{
		"*ir.AggregationStage",
		"*ir.BinaryOpStage",
		"*ir.DataSource",
		"*ir.FilterStage",
		"*ir.FunctionStage",
		"*ir.JoinStage",
		"*ir.LabelMatcher",
		"*ir.LiteralExpr",
		"*ir.LogicalPredicate",
		"*ir.MatchPredicate",
		"*ir.Output",
		"*ir.Query",
		"*ir.QueryExpr",
		"*ir.RefExpr",
		"*ir.Selector",
		"*ir.TimeRange",
		"*ir.Window",
	}

	seen := map[string]int{}
	Inspect(newFixtureQuery(), func(n Node) bool {
		seen[fmt.Sprintf("%T", n)]++
		return true
	})

	got := make([]string, 0, len(seen))
	for k := range seen {
		got = append(got, k)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("fixture node types:\n got %v\nwant %v", got, want)
	}
}

// TestWalkIsDepthFirstPreOrder pins the traversal contract the validator,
// emitters and fidelity reporter all depend on.
func TestWalkIsDepthFirstPreOrder(t *testing.T) {
	q := &Query{
		Signal: SignalLog,
		Source: &DataSource{
			Name:      "app_logs",
			Selectors: []*Selector{{Matchers: []*LabelMatcher{{Key: "app", Op: MatchEQ, Value: "api"}}}},
		},
		Output: &Output{Window: &Window{Step: NewInterval(time.Minute)}},
	}

	var got []string
	Walk(VisitorFunc(func(n Node) {
		if n != nil {
			got = append(got, fmt.Sprintf("%T", n))
		}
	}), q)

	want := []string{
		"*ir.Query",
		"*ir.DataSource",
		"*ir.Selector",
		"*ir.LabelMatcher",
		"*ir.Output",
		"*ir.Window",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("visit order:\n got %v\nwant %v", got, want)
	}
}

// TestWalkNilVisitStopsDescent covers the pruning half of the Visitor contract.
func TestWalkNilVisitStopsDescent(t *testing.T) {
	q := newFixtureQuery()

	var visited []string
	var matcherKeys []string
	Inspect(q, func(n Node) bool {
		visited = append(visited, fmt.Sprintf("%T", n))
		if m, ok := n.(*LabelMatcher); ok {
			matcherKeys = append(matcherKeys, m.Key)
		}
		// Refuse to descend into the data source.
		_, isSource := n.(*DataSource)
		return !isSource
	})

	if len(visited) == 0 || visited[0] != "*ir.Query" {
		t.Fatalf("expected the root to be visited first, got %v", visited)
	}
	for _, name := range visited {
		if name == "*ir.Selector" {
			t.Errorf("visitor descended past a pruned *ir.DataSource: saw %s", name)
		}
	}
	// The selector's own matchers must be skipped, while matchers reached by
	// other paths — the filter predicate here — are still visited.
	for _, key := range matcherKeys {
		if key == "status" || key == "job" {
			t.Errorf("matcher %q lives under the pruned data source and should not be visited", key)
		}
	}
	if !reflect.DeepEqual(matcherKeys, []string{"method", "region"}) {
		t.Errorf("matcher keys = %v, want the two predicate matchers", matcherKeys)
	}
}

// TestWalkToleratesPartialTrees guards the nil checks in Walk: a resolver that
// has not finished building a query must still be walkable.
func TestWalkToleratesPartialTrees(t *testing.T) {
	partial := &Query{
		Signal:   SignalSpan,
		Pipeline: Pipeline{&FilterStage{}, &FunctionStage{Name: "noop"}, &JoinStage{}},
	}

	count := 0
	Inspect(partial, func(Node) bool {
		count++
		return true
	})
	if want := 4; count != want {
		t.Errorf("visited %d nodes, want %d", count, want)
	}
}

// TestTranslatabilityOnEveryNode covers the flag being settable and readable on
// any node, which is what lets the validator annotate an arbitrary subtree.
func TestTranslatabilityOnEveryNode(t *testing.T) {
	q := newFixtureQuery()

	nodes := 0
	Inspect(q, func(n Node) bool {
		flag, reason := n.Base().Translatability()
		if flag != TranslatabilityFull || reason != "" {
			t.Errorf("%T: fresh node should default to FULL with no reason, got %s %q", n, flag, reason)
		}
		n.Base().Annotate(TranslatabilityPartial, "no %s equivalent", "LogQL")
		nodes++
		return true
	})
	if nodes == 0 {
		t.Fatal("walked no nodes")
	}

	Inspect(q, func(n Node) bool {
		flag, reason := n.Base().Translatability()
		if flag != TranslatabilityPartial {
			t.Errorf("%T: got flag %s, want PARTIAL", n, flag)
		}
		if reason != "no LogQL equivalent" {
			t.Errorf("%T: got reason %q, want %q", n, reason, "no LogQL equivalent")
		}
		return true
	})

	// A single UNSUPPORTED node anywhere dominates the subtree's summary.
	q.Pipeline[len(q.Pipeline)-1].Base().SetTranslatability(
		TranslatabilityUnsupported, "LogQL has no join")
	flag, reason := WorstTranslatability(q)
	if flag != TranslatabilityUnsupported {
		t.Errorf("worst flag = %s, want UNSUPPORTED", flag)
	}
	if reason != "LogQL has no join" {
		t.Errorf("worst reason = %q, want %q", reason, "LogQL has no join")
	}

	q.ResetTranslatability()
	if flag, reason := q.Translatability(); flag != TranslatabilityFull || reason != "" {
		t.Errorf("after reset: got %s %q, want FULL and no reason", flag, reason)
	}
}

func TestTranslatabilityFlagWorst(t *testing.T) {
	cases := []struct {
		a, b, want TranslatabilityFlag
	}{
		{TranslatabilityFull, TranslatabilityFull, TranslatabilityFull},
		{TranslatabilityFull, TranslatabilityPartial, TranslatabilityPartial},
		{TranslatabilityPartial, TranslatabilityFull, TranslatabilityPartial},
		{TranslatabilityPartial, TranslatabilityUnsupported, TranslatabilityUnsupported},
		{TranslatabilityUnsupported, TranslatabilityFull, TranslatabilityUnsupported},
	}
	for _, c := range cases {
		if got := c.a.Worst(c.b); got != c.want {
			t.Errorf("%s.Worst(%s) = %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

// Compile-time proof that every node type satisfies Node, so a new node cannot
// be added without translatability state and a String method.
var _ = []Node{
	(*Query)(nil), (*DataSource)(nil), (*Selector)(nil), (*LabelMatcher)(nil),
	(*FilterStage)(nil), (*AggregationStage)(nil), (*FunctionStage)(nil), (*JoinStage)(nil),
	(*BinaryOpStage)(nil), (*MatchPredicate)(nil), (*LogicalPredicate)(nil),
	(*LiteralExpr)(nil), (*RefExpr)(nil), (*QueryExpr)(nil),
	(*Output)(nil), (*TimeRange)(nil), (*Window)(nil),
}

var (
	_ = []PipelineStage{(*FilterStage)(nil), (*AggregationStage)(nil), (*FunctionStage)(nil),
		(*JoinStage)(nil), (*BinaryOpStage)(nil)}
	_ = []Predicate{(*MatchPredicate)(nil), (*LogicalPredicate)(nil)}
	_ = []IRExpr{(*LiteralExpr)(nil), (*RefExpr)(nil), (*QueryExpr)(nil)}
)

// enumCase describes one enum for the shared Stringer, Parse and JSON tests.
type enumCase struct {
	typeName string
	values   []fmt.Stringer
	names    []string
	parse    func(string) (fmt.Stringer, error)
}

func allEnums() []enumCase {
	return []enumCase{
		{
			typeName: "QlsDataType",
			values: []fmt.Stringer{DataTypeDouble, DataTypeSignedInt, DataTypeUnsignedInt,
				DataTypeString, DataTypeBinaryString, DataTypeBoolean, DataTypeTimestamp,
				DataTypeInterval, DataTypeMap, DataTypeArray, DataTypeJSON},
			names: []string{"DOUBLE", "SIGNED_INT", "UNSIGNED_INT", "STRING", "BINARY_STRING",
				"BOOLEAN", "TIMESTAMP", "INTERVAL", "MAP", "ARRAY", "JSON"},
			parse: func(s string) (fmt.Stringer, error) { return ParseQlsDataType(s) },
		},
		{
			typeName: "QlsMetricType",
			values: []fmt.Stringer{MetricTypeUnknown, MetricTypeCumulativeCounter,
				MetricTypeDeltaCounter, MetricTypeGauge, MetricTypeRate, MetricTypeHistogram,
				MetricTypeApproximateCount, MetricTypeApproximateDistribution, MetricTypeCustom},
			names: []string{"UNKNOWN", "CUMULATIVE_COUNTER", "DELTA_COUNTER", "GAUGE", "RATE",
				"HISTOGRAM", "APPROXIMATE_COUNT", "APPROXIMATE_DISTRIBUTION", "CUSTOM"},
			parse: func(s string) (fmt.Stringer, error) { return ParseQlsMetricType(s) },
		},
		{
			typeName: "QlsLogSeverity",
			values: []fmt.Stringer{SeverityTrace, SeverityDebug, SeverityInfo, SeverityWarn,
				SeverityError, SeverityFatal},
			names: []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL"},
			parse: func(s string) (fmt.Stringer, error) { return ParseQlsLogSeverity(s) },
		},
		{
			typeName: "QlsSpanKind",
			values: []fmt.Stringer{SpanKindClient, SpanKindServer, SpanKindProducer,
				SpanKindConsumer, SpanKindInternal},
			names: []string{"CLIENT", "SERVER", "PRODUCER", "CONSUMER", "INTERNAL"},
			parse: func(s string) (fmt.Stringer, error) { return ParseQlsSpanKind(s) },
		},
		{
			typeName: "TranslatabilityFlag",
			values:   []fmt.Stringer{TranslatabilityFull, TranslatabilityPartial, TranslatabilityUnsupported},
			names:    []string{"FULL", "PARTIAL", "UNSUPPORTED"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseTranslatabilityFlag(s) },
		},
		{
			typeName: "SignalType",
			values:   []fmt.Stringer{SignalMetric, SignalLog, SignalSpan, SignalProfile},
			names:    []string{"METRIC", "LOG", "SPAN", "PROFILE"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseSignalType(s) },
		},
		{
			typeName: "Scope",
			values:   []fmt.Stringer{ScopeUnscoped, ScopeResource, ScopeSpan, ScopeIntrinsic},
			names:    []string{"UNSCOPED", "RESOURCE", "SPAN", "INTRINSIC"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseScope(s) },
		},
		{
			typeName: "ArithOp",
			values: []fmt.Stringer{ArithAdd, ArithSub, ArithMul, ArithDiv, ArithMod, ArithPow,
				ArithAnd, ArithOr, ArithUnless,
				ArithEQ, ArithNEQ, ArithGT, ArithGTE, ArithLT, ArithLTE},
			names: []string{"ADD", "SUB", "MUL", "DIV", "MOD", "POW",
				"AND", "OR", "UNLESS",
				"EQ", "NEQ", "GT", "GTE", "LT", "LTE"},
			parse: func(s string) (fmt.Stringer, error) { return ParseArithOp(s) },
		},
		{
			typeName: "MatchOp",
			values: []fmt.Stringer{MatchEQ, MatchNEQ, MatchRegex, MatchNotRegex, MatchGT,
				MatchGTE, MatchLT, MatchLTE, MatchIn, MatchNotIn, MatchIsNull, MatchIsNotNull,
				MatchContains, MatchNotContains},
			names: []string{"EQ", "NEQ", "REGEX", "NOT_REGEX", "GT", "GTE", "LT", "LTE",
				"IN", "NOT_IN", "IS_NULL", "IS_NOT_NULL", "CONTAINS", "NOT_CONTAINS"},
			parse: func(s string) (fmt.Stringer, error) { return ParseMatchOp(s) },
		},
		{
			typeName: "PredicateKind",
			values:   []fmt.Stringer{PredicateKindMatch, PredicateKindLogical},
			names:    []string{"MATCH", "LOGICAL"},
			parse:    func(s string) (fmt.Stringer, error) { return ParsePredicateKind(s) },
		},
		{
			typeName: "LogicalOp",
			values:   []fmt.Stringer{LogicalAnd, LogicalOr, LogicalNot},
			names:    []string{"AND", "OR", "NOT"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseLogicalOp(s) },
		},
		{
			typeName: "ExprKind",
			values:   []fmt.Stringer{ExprKindLiteral, ExprKindRef, ExprKindQuery},
			names:    []string{"LITERAL", "REF", "QUERY"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseExprKind(s) },
		},
		{
			typeName: "StageKind",
			values: []fmt.Stringer{StageKindFilter, StageKindAggregation, StageKindFunction,
				StageKindJoin, StageKindBinaryOp},
			names: []string{"FILTER", "AGGREGATION", "FUNCTION", "JOIN", "BINARY_OP"},
			parse: func(s string) (fmt.Stringer, error) { return ParseStageKind(s) },
		},
		{
			typeName: "AggOp",
			values: []fmt.Stringer{AggSum, AggAvg, AggMin, AggMax, AggCount, AggCountDistinct,
				AggStddev, AggQuantile, AggRate, AggIncrease, AggDelta, AggIrate,
				AggHistogramQuantile, AggTopK, AggBottomK},
			names: []string{"SUM", "AVG", "MIN", "MAX", "COUNT", "COUNT_DISTINCT", "STDDEV",
				"QUANTILE", "RATE", "INCREASE", "DELTA", "IRATE", "HISTOGRAM_QUANTILE",
				"TOPK", "BOTTOMK"},
			parse: func(s string) (fmt.Stringer, error) { return ParseAggOp(s) },
		},
		{
			typeName: "AggScope",
			values:   []fmt.Stringer{AggScopeTemporal, AggScopeGroup},
			names:    []string{"TEMPORAL", "GROUP"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseAggScope(s) },
		},
		{
			typeName: "JoinType",
			values:   []fmt.Stringer{JoinInner, JoinLeftOuter, JoinRightOuter, JoinFullOuter, JoinCross},
			names:    []string{"INNER", "LEFT_OUTER", "RIGHT_OUTER", "FULL_OUTER", "CROSS"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseJoinType(s) },
		},
		{
			typeName: "SortOrder",
			values:   []fmt.Stringer{SortNone, SortAsc, SortDesc},
			names:    []string{"NONE", "ASC", "DESC"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseSortOrder(s) },
		},
		{
			typeName: "WindowAlignment",
			values:   []fmt.Stringer{WindowUTCNormalized, WindowCalendarAligned, WindowQueryStartAligned},
			names:    []string{"UTC_NORMALIZED", "CALENDAR_ALIGNED", "QUERY_START_ALIGNED"},
			parse:    func(s string) (fmt.Stringer, error) { return ParseWindowAlignment(s) },
		},
	}
}

// TestEnumStringer checks that every enum renders the QLS symbol its spec
// section names, in declaration order.
func TestEnumStringer(t *testing.T) {
	for _, ec := range allEnums() {
		t.Run(ec.typeName, func(t *testing.T) {
			if len(ec.values) != len(ec.names) {
				t.Fatalf("test table mismatch: %d values, %d names", len(ec.values), len(ec.names))
			}
			for i, v := range ec.values {
				if got := v.String(); got != ec.names[i] {
					t.Errorf("ordinal %d: String() = %q, want %q", i, got, ec.names[i])
				}
			}
		})
	}
}

// TestEnumParseRoundTrip covers the registry loading enum symbols out of YAML.
func TestEnumParseRoundTrip(t *testing.T) {
	for _, ec := range allEnums() {
		t.Run(ec.typeName, func(t *testing.T) {
			for _, v := range ec.values {
				// Parsing is case-insensitive and tolerates surrounding space.
				for _, in := range []string{v.String(), strings.ToLower(v.String()), "  " + v.String() + " "} {
					got, err := ec.parse(in)
					if err != nil {
						t.Errorf("parse(%q): unexpected error %v", in, err)
						continue
					}
					if got.String() != v.String() {
						t.Errorf("parse(%q) = %s, want %s", in, got, v)
					}
				}
			}
			if _, err := ec.parse("NOT_A_REAL_SYMBOL"); err == nil {
				t.Error("parsing an unknown symbol should fail")
			} else if !strings.Contains(err.Error(), ec.typeName) {
				t.Errorf("error %q should name the type %s", err, ec.typeName)
			}
		})
	}
}

// TestEnumJSONRoundTrip checks that enums serialize as stable symbols rather
// than ordinals, which is what keeps fidelity reports readable and diffable.
func TestEnumJSONRoundTrip(t *testing.T) {
	for _, ec := range allEnums() {
		t.Run(ec.typeName, func(t *testing.T) {
			for _, v := range ec.values {
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatalf("marshal %s: %v", v, err)
				}
				want := `"` + v.String() + `"`
				if string(b) != want {
					t.Errorf("marshal %s = %s, want %s", v, b, want)
				}
			}
		})
	}
}

// TestEnumOutOfRangeIsDiagnosable covers a corrupt or future ordinal: String
// must stay debuggable and JSON must refuse rather than emit a bad symbol.
func TestEnumOutOfRangeIsDiagnosable(t *testing.T) {
	bogus := AggOp(999)
	if got, want := bogus.String(), "AggOp(999)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if _, err := json.Marshal(bogus); err == nil {
		t.Error("marshaling an out-of-range enum should fail")
	}
}

func TestMatchOpHelpers(t *testing.T) {
	for _, op := range []MatchOp{MatchIsNull, MatchIsNotNull} {
		if !op.IsUnary() {
			t.Errorf("%s should be unary", op)
		}
	}
	for _, op := range []MatchOp{MatchIn, MatchNotIn} {
		if !op.IsSetOp() {
			t.Errorf("%s should be a set operator", op)
		}
	}
	if MatchEQ.IsUnary() || MatchEQ.IsSetOp() {
		t.Error("EQ is neither unary nor a set operator")
	}
	if got, want := MatchRegex.Symbol(), "=~"; got != want {
		t.Errorf("Symbol() = %q, want %q", got, want)
	}
}

func TestAggOpHelpers(t *testing.T) {
	for _, op := range []AggOp{AggRate, AggIncrease, AggDelta, AggIrate} {
		if !op.IsTemporalOnly() {
			t.Errorf("%s should be temporal-only", op)
		}
	}
	if AggSum.IsTemporalOnly() {
		t.Error("SUM aggregates over either axis, so it is not temporal-only")
	}
	for _, op := range []AggOp{AggQuantile, AggHistogramQuantile, AggTopK, AggBottomK} {
		if !op.RequiresParameter() {
			t.Errorf("%s should require a parameter", op)
		}
	}
	if AggAvg.RequiresParameter() {
		t.Error("AVG takes no extra parameter")
	}
}

func TestQlsDataTypeHelpers(t *testing.T) {
	for _, dt := range []QlsDataType{DataTypeDouble, DataTypeSignedInt, DataTypeUnsignedInt} {
		if !dt.IsNumeric() {
			t.Errorf("%s should be numeric", dt)
		}
	}
	if DataTypeString.IsNumeric() {
		t.Error("STRING is not numeric")
	}
	for _, dt := range []QlsDataType{DataTypeMap, DataTypeArray, DataTypeJSON} {
		if dt.IsScalar() {
			t.Errorf("%s should not be scalar", dt)
		}
	}
	if !DataTypeDouble.IsScalar() {
		t.Error("DOUBLE is scalar")
	}
	if !MetricTypeCumulativeCounter.IsCounter() || !MetricTypeDeltaCounter.IsCounter() {
		t.Error("both counter types should report IsCounter")
	}
	if MetricTypeGauge.IsCounter() {
		t.Error("GAUGE is not a counter")
	}
}

func TestTimestampAndInterval(t *testing.T) {
	ts := NewTimestamp(fixtureStart)
	if got, want := ts.String(), "2025-04-02T00:00:00Z"; got != want {
		t.Errorf("Timestamp.String() = %q, want %q", got, want)
	}
	if !ts.Time().Equal(fixtureStart) {
		t.Errorf("round trip through time.Time lost precision: %v", ts.Time())
	}
	// The spec requires nanosecond resolution.
	nano := NewTimestamp(fixtureStart.Add(1))
	if nano.UnixNano-ts.UnixNano != 1 {
		t.Error("timestamps must preserve nanosecond resolution")
	}
	if !ts.Before(nano) {
		t.Error("Before should order timestamps")
	}
	if !(Timestamp{}).IsZero() {
		t.Error("the zero Timestamp should report IsZero")
	}
	if got, want := (Timestamp{}).String(), "<unset>"; got != want {
		t.Errorf("zero Timestamp.String() = %q, want %q", got, want)
	}

	iv := NewInterval(90 * time.Second)
	if got, want := iv.String(), "1m30s"; got != want {
		t.Errorf("Interval.String() = %q, want %q", got, want)
	}
	if iv.Duration() != 90*time.Second {
		t.Errorf("Interval.Duration() = %v", iv.Duration())
	}
	if !(Interval{}).IsZero() {
		t.Error("the zero Interval should report IsZero")
	}
}

// TestNodeStringOutput pins the debug rendering of each node type. These strings
// are what a developer sees in a failing test or an IR dump, so they are worth
// keeping readable.
func TestNodeStringOutput(t *testing.T) {
	cases := []struct {
		name string
		node Node
		want string
	}{
		{
			name: "equality matcher",
			node: &LabelMatcher{Key: "status", Op: MatchEQ, Value: "500"},
			want: `status="500"`,
		},
		{
			name: "regex matcher",
			node: &LabelMatcher{Key: "job", Op: MatchRegex, Value: "api.*"},
			want: `job=~"api.*"`,
		},
		{
			name: "set matcher reads Values",
			node: &LabelMatcher{Key: "method", Op: MatchIn, Values: []string{"GET", "POST"}},
			want: `method IN ("GET", "POST")`,
		},
		{
			name: "null matcher takes no operand",
			node: &LabelMatcher{Key: "region", Op: MatchIsNotNull},
			want: "region IS NOT NULL",
		},
		{
			name: "selector",
			node: &Selector{Matchers: []*LabelMatcher{
				{Key: "a", Op: MatchEQ, Value: "1"},
				{Key: "b", Op: MatchNEQ, Value: "2"},
			}},
			want: `{a="1", b!="2"}`,
		},
		{
			name: "scoped data source",
			node: &DataSource{Name: "http_requests_total", Scope: ScopeResource},
			want: "resource:http_requests_total",
		},
		{
			name: "unscoped data source omits the scope prefix",
			node: &DataSource{Name: "up", Scope: ScopeUnscoped},
			want: "up",
		},
		{
			name: "temporal aggregation",
			node: &AggregationStage{Op: AggRate, Scope: AggScopeTemporal},
			want: "rate [TEMPORAL]",
		},
		{
			name: "group aggregation with by",
			node: &AggregationStage{Op: AggSum, GroupBy: []string{"job", "instance"}, Scope: AggScopeGroup},
			want: "sum by (job, instance) [GROUP]",
		},
		{
			name: "group aggregation with without",
			node: &AggregationStage{Op: AggAvg, Without: []string{"pod"}, Scope: AggScopeGroup},
			want: "avg without (pod) [GROUP]",
		},
		{
			name: "parameterised aggregation",
			node: &AggregationStage{Op: AggTopK, Scope: AggScopeGroup, Parameter: NewNumberLiteral(3)},
			want: "topk(3) [GROUP]",
		},
		{
			name: "function stage shows its return type",
			node: &FunctionStage{
				Name:       "histogram_quantile",
				Args:       []IRExpr{NewNumberLiteral(0.99), &RefExpr{Name: "le"}},
				ReturnType: DataTypeDouble,
			},
			want: "histogram_quantile(0.99, le) -> DOUBLE",
		},
		{
			name: "filter stage",
			node: &FilterStage{Predicate: &MatchPredicate{
				Matcher: &LabelMatcher{Key: "level", Op: MatchEQ, Value: "error"},
			}},
			want: `filter(level="error")`,
		},
		{
			name: "boolean predicate",
			node: &LogicalPredicate{Op: LogicalOr, Operands: []Predicate{
				&MatchPredicate{Matcher: &LabelMatcher{Key: "a", Op: MatchEQ, Value: "1"}},
				&MatchPredicate{Matcher: &LabelMatcher{Key: "b", Op: MatchEQ, Value: "2"}},
			}},
			want: `(a="1" OR b="2")`,
		},
		{
			name: "negated predicate",
			node: &LogicalPredicate{Op: LogicalNot, Operands: []Predicate{
				&MatchPredicate{Matcher: &LabelMatcher{Key: "a", Op: MatchEQ, Value: "1"}},
			}},
			want: `NOT (a="1")`,
		},
		{
			name: "join stage",
			node: &JoinStage{
				JoinType:  JoinInner,
				OnLabels:  []string{"job"},
				RightSide: &Query{Signal: SignalMetric, Source: &DataSource{Name: "up"}},
			},
			want: "join INNER on (job) with (METRIC up)",
		},
		{
			name: "null literal",
			node: NewNullLiteral(DataTypeDouble),
			want: "NULL",
		},
		{
			name: "string literal is quoted",
			node: NewStringLiteral("500"),
			want: `"500"`,
		},
		{
			name: "scoped reference",
			node: &RefExpr{Name: "service.name", Scope: ScopeResource, Type: DataTypeString},
			want: "resource.service.name",
		},
		{
			name: "time range",
			node: &TimeRange{Start: NewTimestamp(fixtureStart), End: NewTimestamp(fixtureEnd)},
			want: "[2025-04-02T00:00:00Z, 2025-04-02T01:00:00Z]",
		},
		{
			name: "window",
			node: &Window{Step: NewInterval(time.Minute), Offset: NewInterval(30 * time.Second)},
			want: "step=1m0s offset=30s align=UTC_NORMALIZED",
		},
		{
			name: "window omits a zero offset",
			node: &Window{Step: NewInterval(5 * time.Minute), Alignment: WindowCalendarAligned},
			want: "step=5m0s align=CALENDAR_ALIGNED",
		},
		{
			name: "output",
			node: &Output{GroupBy: []string{"job"}, Sort: SortDesc, Limit: 5},
			want: "by (job) sort=DESC limit=5",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.node.String(); got != c.want {
				t.Errorf("String():\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestQueryStringOutput checks the whole-query rendering, which is the form that
// shows up in CLI debug output.
func TestQueryStringOutput(t *testing.T) {
	q := &Query{
		Signal: SignalMetric,
		Source: &DataSource{
			Name:      "http_requests_total",
			Scope:     ScopeUnscoped,
			Selectors: []*Selector{{Matchers: []*LabelMatcher{{Key: "status", Op: MatchEQ, Value: "500"}}}},
		},
		Pipeline: Pipeline{
			&AggregationStage{Op: AggRate, Scope: AggScopeTemporal},
			&AggregationStage{Op: AggSum, GroupBy: []string{"job"}, Scope: AggScopeGroup},
		},
		Output: &Output{
			Window: &Window{Step: NewInterval(5 * time.Minute)},
			Sort:   SortDesc,
			Limit:  5,
		},
	}

	want := `METRIC http_requests_total{status="500"} | rate [TEMPORAL] | sum by (job) [GROUP] ` +
		`step=5m0s align=UTC_NORMALIZED sort=DESC limit=5`
	if got := q.String(); got != want {
		t.Errorf("Query.String():\n got %s\nwant %s", got, want)
	}

	// The pipeline renders on its own too, for stage-level diagnostics.
	if got, want := q.Pipeline.String(), "rate [TEMPORAL] | sum by (job) [GROUP]"; got != want {
		t.Errorf("Pipeline.String():\n got %s\nwant %s", got, want)
	}
}

// TestDumpRendersAnnotatedTree covers the indented debug view, including the
// flag annotation the fidelity reporter relies on.
func TestDumpRendersAnnotatedTree(t *testing.T) {
	q := &Query{
		Signal: SignalMetric,
		Source: &DataSource{Name: "up"},
		Pipeline: Pipeline{
			&JoinStage{JoinType: JoinInner, OnLabels: []string{"job"}},
		},
	}
	q.Pipeline[0].Base().SetTranslatability(TranslatabilityUnsupported, "LogQL has no join")

	want := strings.Join([]string{
		`*ir.Query METRIC up | join INNER on (job)`,
		`  *ir.DataSource up`,
		`  *ir.JoinStage join INNER on (job)  !! UNSUPPORTED: LogQL has no join`,
		``,
	}, "\n")

	if got := Dump(q); got != want {
		t.Errorf("Dump():\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestJSONRoundTrip covers serializing the IR and reading it back. The stage,
// predicate and expression interfaces each carry a "kind" discriminator so the
// concrete types can be reconstructed.
func TestJSONRoundTrip(t *testing.T) {
	original := newFixtureQuery()

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored Query
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got, want := Dump(&restored), Dump(original); got != want {
		t.Errorf("round trip changed the tree:\n got:\n%s\nwant:\n%s", got, want)
	}
	if !reflect.DeepEqual(restored.Hints, original.Hints) {
		t.Errorf("hints = %v, want %v", restored.Hints, original.Hints)
	}

	// Enums must serialize as symbols, not ordinals.
	for _, symbol := range []string{`"METRIC"`, `"RESOURCE"`, `"TEMPORAL"`, `"INNER"`, `"UTC_NORMALIZED"`} {
		if !strings.Contains(string(data), symbol) {
			t.Errorf("serialized IR should contain %s", symbol)
		}
	}
}

// TestJSONTranslatabilitySurvivesRoundTrip covers the validator's annotations
// making it into a serialized fidelity report.
func TestJSONTranslatabilitySurvivesRoundTrip(t *testing.T) {
	q := newFixtureQuery()
	q.Pipeline[len(q.Pipeline)-1].Base().SetTranslatability(
		TranslatabilityUnsupported, "LogQL has no join")

	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored Query
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	flag, reason := WorstTranslatability(&restored)
	if flag != TranslatabilityUnsupported || reason != "LogQL has no join" {
		t.Errorf("after round trip: got %s %q, want UNSUPPORTED and the original reason", flag, reason)
	}
}

// TestJSONUnknownKindIsRejected covers a malformed or future IR document.
func TestJSONUnknownKindIsRejected(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"unknown stage kind", `{"signal":"METRIC","pipeline":[{"kind":"TELEPORT"}]}`},
		{"missing stage kind", `{"signal":"METRIC","pipeline":[{"op":"SUM"}]}`},
		{"unknown predicate kind", `{"signal":"METRIC","pipeline":[{"kind":"FILTER","predicate":{"kind":"VIBES"}}]}`},
		{"unknown expression kind", `{"signal":"METRIC","pipeline":[{"kind":"FUNCTION","args":[{"kind":"ORACLE"}]}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var q Query
			if err := json.Unmarshal([]byte(c.doc), &q); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

// unknownNode stands in for a node type someone adds without updating Walk.
type unknownNode struct{ IRNode }

func (u *unknownNode) String() string { return "unknown" }

// TestWalkPanicsOnUnhandledNodeType makes the failure mode loud: a node type
// Walk does not know must not be silently skipped, because every downstream
// component would then quietly ignore it.
func TestWalkPanicsOnUnhandledNodeType(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic for an unhandled node type")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "unknownNode") {
			t.Errorf("panic message %q should name the offending type", msg)
		}
	}()
	Inspect(&unknownNode{}, func(Node) bool { return true })
}

// TestWalkIgnoresNilInputs keeps callers from having to guard.
func TestWalkIgnoresNilInputs(t *testing.T) {
	Walk(nil, newFixtureQuery())
	Walk(VisitorFunc(func(Node) {}), nil)
	Inspect(nil, func(Node) bool { t.Fatal("should not be called"); return false })
}

// TestZeroValuesAreSafeDefaults pins the defaults a partially built node falls
// back to. Each of these is load-bearing: a wrong default here would silently
// change what a translated query means rather than failing loudly.
func TestZeroValuesAreSafeDefaults(t *testing.T) {
	cases := []struct {
		name string
		got  fmt.Stringer
		want string
		why  string
	}{
		{"Scope", Scope(0), "UNSCOPED",
			"an unset scope must not claim to be resource-scoped"},
		{"TranslatabilityFlag", TranslatabilityFlag(0), "FULL",
			"nodes are translatable until the validator downgrades them"},
		{"QlsMetricType", QlsMetricType(0), "UNKNOWN",
			"QLS §.3.0 makes UNKNOWN the metric type default"},
		{"WindowAlignment", WindowAlignment(0), "UTC_NORMALIZED",
			"QLS §Time Based Windowing requires UTC normalization by default"},
		{"SortOrder", SortOrder(0), "NONE",
			"an unset sort leaves the QLS default ordering in place"},
	}
	for _, c := range cases {
		if got := c.got.String(); got != c.want {
			t.Errorf("zero %s = %s, want %s (%s)", c.name, got, c.want, c.why)
		}
	}

	// The severity and span kind ordinals do not carry the spec's record
	// defaults, so the package exposes those explicitly.
	if DefaultLogSeverity != SeverityInfo {
		t.Errorf("DefaultLogSeverity = %s, want INFO per QLS §.4", DefaultLogSeverity)
	}
	if DefaultSpanKind != SpanKindInternal {
		t.Errorf("DefaultSpanKind = %s, want INTERNAL per QLS §.5", DefaultSpanKind)
	}
	// Severity ordinals must ascend so that "severity > INFO" works, as QLS §.4
	// explicitly calls for.
	ordered := []QlsLogSeverity{SeverityTrace, SeverityDebug, SeverityInfo,
		SeverityWarn, SeverityError, SeverityFatal}
	for i := 1; i < len(ordered); i++ {
		if !(ordered[i-1] < ordered[i]) {
			t.Errorf("severity %s should sort below %s", ordered[i-1], ordered[i])
		}
	}
}

// TestBinaryOpStage covers the node that replaced the stringly-typed
// binary_op function stage.
func TestBinaryOpStage(t *testing.T) {
	left := &Query{Signal: SignalMetric, Source: &DataSource{Name: "a"}}
	right := &Query{Signal: SignalMetric, Source: &DataSource{Name: "b"}}
	stage := &BinaryOpStage{Op: ArithDiv, Left: left, Right: right}

	t.Run("it is a pipeline stage", func(t *testing.T) {
		if stage.StageKind() != StageKindBinaryOp {
			t.Errorf("StageKind() = %s, want BINARY_OP", stage.StageKind())
		}
		if got, want := stage.String(), "(METRIC a DIV METRIC b)"; got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	})

	t.Run("Walk reaches both operands", func(t *testing.T) {
		query := &Query{Signal: SignalMetric, Pipeline: Pipeline{stage}}
		var names []string
		Inspect(query, func(n Node) bool {
			if source, ok := n.(*DataSource); ok {
				names = append(names, source.Name)
			}
			return true
		})
		if len(names) != 2 || names[0] != "a" || names[1] != "b" {
			t.Errorf("walked sources = %v, want [a b]", names)
		}
	})

	t.Run("operands may be absent", func(t *testing.T) {
		// A join supplies the operands, so the stage carries only the operator.
		joined := &BinaryOpStage{Op: ArithDiv}
		if got, want := joined.String(), "binary DIV"; got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
		count := 0
		Inspect(&Query{Signal: SignalMetric, Pipeline: Pipeline{joined}}, func(Node) bool {
			count++
			return true
		})
		if count != 2 {
			t.Errorf("walked %d nodes, want the query and the stage", count)
		}
	})

	t.Run("it dumps", func(t *testing.T) {
		got := Dump(&Query{Signal: SignalMetric, Pipeline: Pipeline{stage}})
		if !strings.Contains(got, "*ir.BinaryOpStage") {
			t.Errorf("Dump():\n%s", got)
		}
	})

	t.Run("it round-trips through JSON", func(t *testing.T) {
		query := &Query{Signal: SignalMetric, Pipeline: Pipeline{stage}}
		data, err := json.Marshal(query)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(data), `"BINARY_OP"`) {
			t.Errorf("the discriminator should be a symbol: %s", data)
		}
		if !strings.Contains(string(data), `"DIV"`) {
			t.Errorf("the operator should be a symbol: %s", data)
		}
		var restored Query
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got, want := Dump(&restored), Dump(query); got != want {
			t.Errorf("round trip changed the tree:\n got:\n%s\nwant:\n%s", got, want)
		}
	})
}

// TestArithOpPrecedence covers the table an emitter uses to decide grouping.
func TestArithOpPrecedence(t *testing.T) {
	tighter := []struct{ looser, tighter ArithOp }{
		{ArithOr, ArithAnd},
		{ArithAnd, ArithEQ},
		{ArithEQ, ArithAdd},
		{ArithAdd, ArithMul},
		{ArithMul, ArithPow},
	}
	for _, c := range tighter {
		if c.looser.Precedence() >= c.tighter.Precedence() {
			t.Errorf("%s should bind less tightly than %s", c.looser, c.tighter)
		}
	}
	if !ArithPow.IsRightAssociative() {
		t.Error("exponentiation groups to the right")
	}
	if ArithSub.IsRightAssociative() {
		t.Error("subtraction groups to the left")
	}

	if !ArithAdd.IsArithmetic() || ArithAnd.IsArithmetic() {
		t.Error("IsArithmetic should cover only the six arithmetic operators")
	}
	if !ArithUnless.IsSetOperator() || ArithAdd.IsSetOperator() {
		t.Error("IsSetOperator should cover only and/or/unless")
	}
	if !ArithGTE.IsComparison() || ArithMul.IsComparison() {
		t.Error("IsComparison should cover only the comparison operators")
	}
}

// TestIntervalSourceText covers a duration keeping the units it was written
// with, which is what stops a translated query changing shape under its author.
func TestIntervalSourceText(t *testing.T) {
	written := NewIntervalFromSource(90*time.Minute, "90m")

	if got := written.Duration(); got != 90*time.Minute {
		t.Errorf("Duration() = %s, want 90m", got)
	}
	if got, want := written.String(), "90m"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	// Without a recorded spelling, the value falls back to Go's duration
	// format. This is the IR's own debug rendering; an emitter has its own,
	// which writes the units the target language accepts.
	derived := NewInterval(90 * time.Minute)
	if got, want := derived.String(), "1h30m0s"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	// The spelling is advisory: two intervals of one length are one length.
	if !written.Equal(derived) {
		t.Error("Equal should compare length, not spelling")
	}
	if written.IsZero() {
		t.Error("IsZero should read the length")
	}
	if got := written.WithSourceText("1.5h").String(); got != "1.5h" {
		t.Errorf("WithSourceText: got %q", got)
	}

	t.Run("both fields survive JSON", func(t *testing.T) {
		data, err := json.Marshal(written)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var restored Interval
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if restored.Nanos != written.Nanos || restored.SourceText != "90m" {
			t.Errorf("round trip = %+v, want %+v", restored, written)
		}
	})

	t.Run("an absent spelling is omitted", func(t *testing.T) {
		data, err := json.Marshal(derived)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "source_text") {
			t.Errorf("an empty spelling should be omitted: %s", data)
		}
	})
}

// TestContainmentOperators covers the predicate that fills the gap between an
// exact match and a pattern.
func TestContainmentOperators(t *testing.T) {
	for _, op := range []MatchOp{MatchContains, MatchNotContains} {
		if !op.IsContainment() {
			t.Errorf("%s should report as containment", op)
		}
		if op.IsUnary() || op.IsSetOp() {
			t.Errorf("%s reads Value like the scalar operators", op)
		}
	}
	for _, op := range []MatchOp{MatchEQ, MatchRegex, MatchIn} {
		if op.IsContainment() {
			t.Errorf("%s is not containment", op)
		}
	}
}

// TestFilterStageReturnsBool covers the modifier that distinguishes a filtering
// comparison from one returning 0/1.
func TestFilterStageReturnsBool(t *testing.T) {
	plain := &FilterStage{Predicate: &MatchPredicate{
		Matcher: &LabelMatcher{Key: "value", Op: MatchGT, Value: "5"},
	}}
	if strings.Contains(plain.String(), "bool") {
		t.Errorf("String() = %q, want no modifier", plain.String())
	}

	returning := &FilterStage{Predicate: plain.Predicate, ReturnsBool: true}
	if !strings.Contains(returning.String(), "[bool]") {
		t.Errorf("String() = %q, want the modifier shown", returning.String())
	}

	data, err := json.Marshal(returning)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"returns_bool":true`) {
		t.Errorf("the modifier should serialize: %s", data)
	}
}

// TestOutputSubquery covers the two windows a subquery nests, which used to be
// carried as hints.
func TestOutputSubquery(t *testing.T) {
	var plain Output
	if plain.IsSubquery() {
		t.Error("an output with no subquery range is not a subquery")
	}
	var absent *Output
	if absent.IsSubquery() {
		t.Error("a nil output is not a subquery")
	}

	outer := NewIntervalFromSource(30*time.Minute, "30m")
	step := NewIntervalFromSource(time.Minute, "1m")
	output := &Output{
		// The inner aggregation keeps its own window alongside these.
		Window:        &Window{Step: NewIntervalFromSource(5*time.Minute, "5m")},
		SubqueryRange: &outer,
		SubqueryStep:  &step,
	}
	if !output.IsSubquery() {
		t.Error("IsSubquery should follow SubqueryRange")
	}
	if got := output.String(); !strings.Contains(got, "subquery=[30m:1m]") {
		t.Errorf("String() = %q", got)
	}
	// All three durations survive, which is the point of the change.
	if got := output.Window.Step.String(); got != "5m" {
		t.Errorf("the inner window = %q, want 5m", got)
	}
}

// TestInspectPathMatchesWalk pins the two traversals to the same node set.
//
// Walk and InspectPath each have their own type switch, and both the validator
// and the fidelity reporter depend on the second reaching everything the first
// does. A node type added to one and not the other would be silently unchecked
// and unreported, which is exactly the failure this catches.
func TestInspectPathMatchesWalk(t *testing.T) {
	query := newFixtureQuery()

	var walked []string
	Inspect(query, func(n Node) bool {
		walked = append(walked, fmt.Sprintf("%T", n))
		return true
	})

	var pathed []string
	InspectPath(query, "Query", func(_ string, n Node) bool {
		pathed = append(pathed, fmt.Sprintf("%T", n))
		return true
	})

	if !reflect.DeepEqual(walked, pathed) {
		t.Errorf("the two traversals disagree:\n  Walk:        %v\n  InspectPath: %v", walked, pathed)
	}
	if len(walked) == 0 {
		t.Fatal("the fixture walked nothing")
	}
}

// TestInspectPathNames covers the path convention both callers rely on.
func TestInspectPathNames(t *testing.T) {
	query := &Query{
		Signal: SignalMetric,
		Source: &DataSource{
			Name:      "up",
			Selectors: []*Selector{{Matchers: []*LabelMatcher{{Key: "job", Op: MatchEQ, Value: "api"}}}},
		},
		Pipeline: Pipeline{
			&FilterStage{Predicate: &LogicalPredicate{
				Op: LogicalAnd,
				Operands: []Predicate{
					&MatchPredicate{Matcher: &LabelMatcher{Key: "a", Op: MatchEQ, Value: "1"}},
				},
			}},
			&BinaryOpStage{
				Op:    ArithDiv,
				Left:  &Query{Signal: SignalMetric},
				Right: &Query{Signal: SignalMetric},
			},
		},
		Output: &Output{Window: &Window{}},
	}

	paths := map[string]bool{}
	InspectPath(query, "Query", func(path string, _ Node) bool {
		paths[path] = true
		return true
	})

	for _, want := range []string{
		"Query",
		"Query.Source",
		"Query.Source.Selectors[0]",
		"Query.Source.Selectors[0].Matchers[0]",
		"Query.Pipeline[0].FilterStage",
		"Query.Pipeline[0].FilterStage.Predicate",
		"Query.Pipeline[0].FilterStage.Predicate.Operands[0]",
		"Query.Pipeline[0].FilterStage.Predicate.Operands[0].Matcher",
		"Query.Pipeline[1].BinaryOpStage",
		"Query.Pipeline[1].BinaryOpStage.Left",
		"Query.Pipeline[1].BinaryOpStage.Right",
		"Query.Output",
		"Query.Output.Window",
	} {
		if !paths[want] {
			t.Errorf("no node reached at %q", want)
		}
	}
}

// TestInspectPathPruning covers the callback stopping a descent.
func TestInspectPathPruning(t *testing.T) {
	query := newFixtureQuery()

	visited := 0
	InspectPath(query, "Query", func(_ string, node Node) bool {
		visited++
		_, isSource := node.(*DataSource)
		return !isSource
	})

	full := 0
	InspectPath(query, "Query", func(string, Node) bool {
		full++
		return true
	})

	if visited >= full {
		t.Errorf("pruning visited %d of %d nodes, want fewer", visited, full)
	}
}

// TestInspectPathPanicsOnUnhandledNodeType matches Walk's contract: a node type
// it does not know must fail loudly rather than be skipped.
func TestInspectPathPanicsOnUnhandledNodeType(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic for an unhandled node type")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "unknownNode") {
			t.Errorf("panic %q should name the offending type", msg)
		}
	}()
	InspectPath(&unknownNode{}, "Query", func(string, Node) bool { return true })
}

func TestNodeTypeName(t *testing.T) {
	cases := []struct {
		node Node
		want string
	}{
		{&Query{}, "Query"},
		{&BinaryOpStage{}, "BinaryOpStage"},
		{&MatchPredicate{}, "MatchPredicate"},
		{&LabelMatcher{}, "LabelMatcher"},
		{&Output{}, "Output"},
	}
	for _, c := range cases {
		if got := NodeTypeName(c.node); got != c.want {
			t.Errorf("NodeTypeName(%T) = %q, want %q", c.node, got, c.want)
		}
	}
}

// TestSpansetSelector covers the node that exists because a Selector cannot
// stand in for it: a Selector's matchers are conjunctive, and a span set filter
// is a full boolean expression.
func TestSpansetSelector(t *testing.T) {
	t.Run("an empty selector still selects", func(t *testing.T) {
		var empty SpansetSelector
		if got, want := empty.String(), "{}"; got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	})

	t.Run("a disjunction survives", func(t *testing.T) {
		spanset := &SpansetSelector{Filters: &LogicalPredicate{
			Op: LogicalOr,
			Operands: []Predicate{
				&MatchPredicate{Matcher: &LabelMatcher{Key: "span.a", Op: MatchEQ, Value: "1"}},
				&MatchPredicate{Matcher: &LabelMatcher{Key: "span.b", Op: MatchEQ, Value: "2"}},
			},
		}}
		if got := spanset.String(); !strings.Contains(got, "OR") {
			t.Errorf("String() = %q, want the disjunction shown", got)
		}
	})

	t.Run("round trips through JSON", func(t *testing.T) {
		// The Filters field is an interface, so it needs the discriminator the
		// predicate types marshal. Losing it would silently drop every filter.
		spanset := &SpansetSelector{Filters: &LogicalPredicate{
			Op: LogicalAnd,
			Operands: []Predicate{
				&MatchPredicate{Matcher: &LabelMatcher{Key: "span.a", Op: MatchEQ, Value: "1"}},
			},
		}}
		data, err := json.Marshal(spanset)
		if err != nil {
			t.Fatal(err)
		}
		var restored SpansetSelector
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got, want := restored.String(), spanset.String(); got != want {
			t.Errorf("round trip = %q, want %q", got, want)
		}
	})

	t.Run("reached by both traversals through a data source", func(t *testing.T) {
		query := &Query{
			Signal: SignalSpan,
			Source: &DataSource{Spanset: &SpansetSelector{Filters: &MatchPredicate{
				Matcher: &LabelMatcher{Key: "span.a", Op: MatchEQ, Value: "1"},
			}}},
		}
		var paths []string
		InspectPath(query, "Query", func(path string, _ Node) bool {
			paths = append(paths, path)
			return true
		})
		for _, want := range []string{
			"Query.Source.Spanset",
			"Query.Source.Spanset.Filters",
			"Query.Source.Spanset.Filters.Matcher",
		} {
			if !slices.Contains(paths, want) {
				t.Errorf("no node reached at %q; got %v", want, paths)
			}
		}
	})
}

// TestStructuralStage covers the trace-tree relationship, which is not a join:
// a join correlates on values the query names, this on structure nothing
// records.
func TestStructuralStage(t *testing.T) {
	for _, op := range []StructuralOp{StructuralChild, StructuralDescendant, StructuralSibling} {
		t.Run(op.String(), func(t *testing.T) {
			parsed, err := ParseStructuralOp(strings.ToLower(op.String()))
			if err != nil {
				t.Fatalf("ParseStructuralOp: %v", err)
			}
			if parsed != op {
				t.Errorf("ParseStructuralOp round trip = %v, want %v", parsed, op)
			}

			stage := &StructuralStage{Op: op, Right: &Query{Signal: SignalSpan}}
			if stage.StageKind() != StageKindStructural {
				t.Errorf("StageKind() = %v", stage.StageKind())
			}
			if got := stage.String(); !strings.Contains(got, op.String()) {
				t.Errorf("String() = %q, want it to name the operator", got)
			}

			data, err := json.Marshal(stage)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"kind":"STRUCTURAL"`) {
				t.Errorf("the stage should carry its discriminator: %s", data)
			}
			if !strings.Contains(string(data), `"op":"`+op.String()+`"`) {
				t.Errorf("the operator should serialize by name: %s", data)
			}
		})
	}

	t.Run("child and descendant are distinct", func(t *testing.T) {
		// The transitive closure is a different relationship, and conflating
		// them would widen every child query.
		if StructuralChild == StructuralDescendant {
			t.Fatal("child and descendant must not share an ordinal")
		}
	})

	t.Run("a pipeline round trips through JSON", func(t *testing.T) {
		pipeline := Pipeline{&StructuralStage{Op: StructuralSibling}}
		data, err := json.Marshal(pipeline)
		if err != nil {
			t.Fatal(err)
		}
		var restored Pipeline
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		stage, ok := restored[0].(*StructuralStage)
		if !ok {
			t.Fatalf("restored[0] = %T, want *StructuralStage", restored[0])
		}
		if stage.Op != StructuralSibling {
			t.Errorf("Op = %v, want SIBLING", stage.Op)
		}
	})
}

// TestCoercionStage covers the explicit cast QLS Attributes describes.
func TestCoercionStage(t *testing.T) {
	stage := &CoercionStage{Attribute: "span.http.status_code", TargetType: DataTypeSignedInt}

	if stage.StageKind() != StageKindCoercion {
		t.Errorf("StageKind() = %v", stage.StageKind())
	}
	if got := stage.String(); !strings.Contains(got, "span.http.status_code") ||
		!strings.Contains(got, "SIGNED_INT") {
		t.Errorf("String() = %q, want the attribute and the target type", got)
	}

	data, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"kind":"COERCION"`) {
		t.Errorf("the stage should carry its discriminator: %s", data)
	}

	var restored Pipeline
	if err := json.Unmarshal([]byte("["+string(data)+"]"), &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, ok := restored[0].(*CoercionStage); !ok || got.Attribute != stage.Attribute {
		t.Errorf("restored[0] = %#v, want the stage back", restored[0])
	}
}

// TestQlsSpanKindCoversOTel pins the enum against the OpenTelemetry model the
// QLS spec follows, and against the zero value the IR relies on.
func TestQlsSpanKindCoversOTel(t *testing.T) {
	for _, name := range []string{
		"UNSPECIFIED", "INTERNAL", "SERVER", "CLIENT", "PRODUCER", "CONSUMER",
	} {
		kind, err := ParseQlsSpanKind(name)
		if err != nil {
			t.Errorf("ParseQlsSpanKind(%q): %v", name, err)
			continue
		}
		if got := kind.String(); got != name {
			t.Errorf("round trip = %q, want %q", got, name)
		}
	}

	// The zero value must be UNSPECIFIED. A span whose kind was never stated is
	// not a client span, and making the zero value say so would have a TraceQL
	// emitter write "kind = client" for a query that named no kind at all.
	var zero QlsSpanKind
	if zero != SpanKindUnspecified {
		t.Errorf("the zero value is %s, want UNSPECIFIED", zero)
	}
	// The spec's own default for a constructed span is still INTERNAL.
	if DefaultSpanKind != SpanKindInternal {
		t.Errorf("DefaultSpanKind = %s, want INTERNAL", DefaultSpanKind)
	}
}

// TestFlatLabelName covers the fold both the validator and the emitters depend
// on. They have to agree: one reports the rewrite and the other performs it, and
// a disagreement would mean a note describing something other than what was
// written.
func TestFlatLabelName(t *testing.T) {
	cases := []struct {
		key     string
		want    string
		changed bool
	}{
		{"span.http.status_code", "span_http_status_code", true},
		{"resource.service.name", "resource_service_name", true},
		{"duration", "duration", false},
		{"job", "job", false},
		{"__name__", "__name__", false},
		{"a-b", "a_b", true},
		{"1st", "_1st", true},
		{"", "", false},
	}

	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			got, changed := FlatLabelName(c.key)
			if got != c.want || changed != c.changed {
				t.Errorf("FlatLabelName(%q) = (%q, %v), want (%q, %v)",
					c.key, got, changed, c.want, c.changed)
			}
		})
	}
}
