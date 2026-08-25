// Package ir defines PolyQL's TelemetryIR: the shared, DSL-neutral intermediate
// representation that every parser resolves into and every emitter renders from.
//
// The type system, data models and semantic rules here are drawn from the CNCF
// Observability TAG Query Language Standardization Working Group's DRAFT Semantic
// Specification (Apr 2, 2025). Section references in doc comments ("QLS §...")
// point at that document, a copy of which lives under references/.
//
// The IR is deliberately biased toward the QLS relational-algebra model rather
// than toward any single DSL. Where a source or target DSL cannot express an IR
// construct, the loss is recorded on the node via its TranslatabilityFlag rather
// than being silently dropped.
package ir

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// enumDef holds the name table backing an int-based enum. It centralizes the
// String, Parse and JSON behavior so every QLS enum in the IR serializes as a
// stable, human-readable symbol ("CUMULATIVE_COUNTER") instead of an ordinal,
// which keeps fidelity reports and IR dumps diffable.
type enumDef[T ~int] struct {
	typeName string
	names    []string
	parse    map[string]T
}

func newEnumDef[T ~int](typeName string, names ...string) *enumDef[T] {
	d := &enumDef[T]{typeName: typeName, names: names, parse: make(map[string]T, len(names))}
	for i, n := range names {
		d.parse[n] = T(i)
	}
	return d
}

func (d *enumDef[T]) valid(v T) bool { return int(v) >= 0 && int(v) < len(d.names) }

func (d *enumDef[T]) String(v T) string {
	if !d.valid(v) {
		return fmt.Sprintf("%s(%d)", d.typeName, int(v))
	}
	return d.names[v]
}

func (d *enumDef[T]) Parse(s string) (T, error) {
	if v, ok := d.parse[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return v, nil
	}
	var zero T
	return zero, fmt.Errorf("ir: %q is not a valid %s", s, d.typeName)
}

func (d *enumDef[T]) marshalJSON(v T) ([]byte, error) {
	if !d.valid(v) {
		return nil, fmt.Errorf("ir: cannot marshal %s: ordinal %d out of range", d.typeName, int(v))
	}
	return json.Marshal(d.names[v])
}

func (d *enumDef[T]) unmarshalJSON(data []byte, v *T) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("ir: %s must be a JSON string: %w", d.typeName, err)
	}
	parsed, err := d.Parse(s)
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}

// QlsDataType is the QLS §Data Types type system (§.1.1 through §.1.11).
//
// The zero value is DOUBLE, the most common observability measurement type.
// Nodes that must distinguish "unset" from "double" should track that
// separately rather than relying on the zero value.
type QlsDataType int

const (
	// DataTypeDouble is IEEE 754-1985 double precision floating point. QLS §.1.1.
	DataTypeDouble QlsDataType = iota
	// DataTypeSignedInt is a signed 64-bit integer. QLS §.1.2.
	DataTypeSignedInt
	// DataTypeUnsignedInt is an unsigned 64-bit integer, required for profile
	// memory address referencing. QLS §.1.3.
	DataTypeUnsignedInt
	// DataTypeString is a UTF-8 (ISO/IEC 10646) collated string. QLS §.1.4.
	// The spec prefers NULL over the empty string.
	DataTypeString
	// DataTypeBinaryString is a sequence of octets. QLS §.1.5.
	DataTypeBinaryString
	// DataTypeBoolean is true or false. QLS §.1.6.
	DataTypeBoolean
	// DataTypeTimestamp is a point in time with nanosecond resolution. QLS §.1.7.
	DataTypeTimestamp
	// DataTypeInterval is a duration. QLS §.1.8.
	DataTypeInterval
	// DataTypeMap is an unordered set of key/value pairs, homogeneous in both
	// key and value type, permitting NULL keys and values. QLS §.1.9.
	DataTypeMap
	// DataTypeArray is an ordinal-indexed, homogeneously typed collection. QLS §.1.10.
	DataTypeArray
	// DataTypeJSON is a SQL JSON structure. QLS §.1.11.
	DataTypeJSON
)

var qlsDataTypeEnum = newEnumDef[QlsDataType]("QlsDataType",
	"DOUBLE",
	"SIGNED_INT",
	"UNSIGNED_INT",
	"STRING",
	"BINARY_STRING",
	"BOOLEAN",
	"TIMESTAMP",
	"INTERVAL",
	"MAP",
	"ARRAY",
	"JSON",
)

func (t QlsDataType) String() string { return qlsDataTypeEnum.String(t) }

// ParseQlsDataType resolves a QLS type symbol, case-insensitively.
func ParseQlsDataType(s string) (QlsDataType, error) { return qlsDataTypeEnum.Parse(s) }

func (t QlsDataType) MarshalJSON() ([]byte, error)  { return qlsDataTypeEnum.marshalJSON(t) }
func (t *QlsDataType) UnmarshalJSON(b []byte) error { return qlsDataTypeEnum.unmarshalJSON(b, t) }

// IsNumeric reports whether the type participates in QLS §Aggregation math.
func (t QlsDataType) IsNumeric() bool {
	switch t {
	case DataTypeDouble, DataTypeSignedInt, DataTypeUnsignedInt:
		return true
	default:
		return false
	}
}

// IsScalar reports whether the type holds a single value rather than a
// collection. Scalar-to-set operations are the only case in which QLS §Joins
// permits a cross join.
func (t QlsDataType) IsScalar() bool {
	switch t {
	case DataTypeMap, DataTypeArray, DataTypeJSON:
		return false
	default:
		return true
	}
}

// QlsMetricType is the metric type enum from QLS §.3.0 Metrics > Type. It exists
// so functions can be validated against the shape of the data they consume.
//
// The zero value is UNKNOWN, matching the spec's stated default.
type QlsMetricType int

const (
	// MetricTypeUnknown is the spec default, used when the type is not known.
	MetricTypeUnknown QlsMetricType = iota
	// MetricTypeCumulativeCounter is a cumulative monotonic counter.
	MetricTypeCumulativeCounter
	// MetricTypeDeltaCounter measures the change between reporting intervals.
	MetricTypeDeltaCounter
	// MetricTypeGauge is a state measurement that may move up or down.
	MetricTypeGauge
	// MetricTypeRate is a delta converted to increments per second.
	MetricTypeRate
	// MetricTypeHistogram is a bucketed histogram.
	MetricTypeHistogram
	// MetricTypeApproximateCount is an approximate distinct-count structure
	// (HLL and similar).
	MetricTypeApproximateCount
	// MetricTypeApproximateDistribution is an approximate distribution structure
	// (t-digest and similar), typically used to extract quantiles.
	MetricTypeApproximateDistribution
	// MetricTypeCustom covers non-standard types from implementations that do
	// not conform to the spec.
	MetricTypeCustom
)

// Note: the draft spec spells the two approximate members "APROXIMATE_COUNT" and
// "APROXIMATE_DISTRIBUTION". PolyQL uses the corrected spelling; a registry
// alias can map the spec's spelling if a backend emits it verbatim.
var qlsMetricTypeEnum = newEnumDef[QlsMetricType]("QlsMetricType",
	"UNKNOWN",
	"CUMULATIVE_COUNTER",
	"DELTA_COUNTER",
	"GAUGE",
	"RATE",
	"HISTOGRAM",
	"APPROXIMATE_COUNT",
	"APPROXIMATE_DISTRIBUTION",
	"CUSTOM",
)

func (t QlsMetricType) String() string { return qlsMetricTypeEnum.String(t) }

// ParseQlsMetricType resolves a QLS metric type symbol, case-insensitively.
func ParseQlsMetricType(s string) (QlsMetricType, error) { return qlsMetricTypeEnum.Parse(s) }

func (t QlsMetricType) MarshalJSON() ([]byte, error)  { return qlsMetricTypeEnum.marshalJSON(t) }
func (t *QlsMetricType) UnmarshalJSON(b []byte) error { return qlsMetricTypeEnum.unmarshalJSON(b, t) }

// IsCounter reports whether the metric accumulates, and therefore whether rate
// and increase style functions are meaningful over it.
func (t QlsMetricType) IsCounter() bool {
	return t == MetricTypeCumulativeCounter || t == MetricTypeDeltaCounter
}

// QlsLogSeverity is the log severity enum from QLS §.4 Logs/Events > Severity,
// which follows the OpenTelemetry severity definitions.
//
// Ordinals ascend with urgency precisely so that ordered predicates such as
// "severity > INFO" work, as called out by the spec. Note that the zero value is
// TRACE while the spec's default for a log *record* is INFO; construct records
// with DefaultLogSeverity rather than relying on the zero value.
type QlsLogSeverity int

const (
	// SeverityTrace is a fine grained event.
	SeverityTrace QlsLogSeverity = iota
	// SeverityDebug is a debugging event.
	SeverityDebug
	// SeverityInfo records that an event occurred.
	SeverityInfo
	// SeverityWarn may require remediation but allows operations to continue.
	SeverityWarn
	// SeverityError should not have occurred and requires remediation, but
	// allows operations to continue.
	SeverityError
	// SeverityFatal should not have occurred and prevented operations from
	// continuing.
	SeverityFatal
)

// DefaultLogSeverity is the spec default for a log record (QLS §.4 > Severity).
const DefaultLogSeverity = SeverityInfo

var qlsLogSeverityEnum = newEnumDef[QlsLogSeverity]("QlsLogSeverity",
	"TRACE",
	"DEBUG",
	"INFO",
	"WARN",
	"ERROR",
	"FATAL",
)

func (s QlsLogSeverity) String() string { return qlsLogSeverityEnum.String(s) }

// ParseQlsLogSeverity resolves a QLS severity symbol, case-insensitively.
func ParseQlsLogSeverity(s string) (QlsLogSeverity, error) { return qlsLogSeverityEnum.Parse(s) }

func (s QlsLogSeverity) MarshalJSON() ([]byte, error) { return qlsLogSeverityEnum.marshalJSON(s) }
func (s *QlsLogSeverity) UnmarshalJSON(b []byte) error {
	return qlsLogSeverityEnum.unmarshalJSON(b, s)
}

// QlsSpanKind is the span kind enum from QLS §.5 Spans > Kind, which follows the
// OpenTelemetry model.
//
// The zero value is CLIENT while the spec's default for a span is INTERNAL; use
// DefaultSpanKind when constructing span records.
type QlsSpanKind int

const (
	// SpanKindClient is an outgoing request.
	SpanKindClient QlsSpanKind = iota
	// SpanKindServer is an incoming request.
	SpanKindServer
	// SpanKindProducer is an outgoing event.
	SpanKindProducer
	// SpanKindConsumer is an incoming event.
	SpanKindConsumer
	// SpanKindInternal is an internal or unknown operation.
	SpanKindInternal
)

// DefaultSpanKind is the spec default for a span record (QLS §.5 > Kind).
const DefaultSpanKind = SpanKindInternal

var qlsSpanKindEnum = newEnumDef[QlsSpanKind]("QlsSpanKind",
	"CLIENT",
	"SERVER",
	"PRODUCER",
	"CONSUMER",
	"INTERNAL",
)

func (k QlsSpanKind) String() string { return qlsSpanKindEnum.String(k) }

// ParseQlsSpanKind resolves a QLS span kind symbol, case-insensitively.
func ParseQlsSpanKind(s string) (QlsSpanKind, error) { return qlsSpanKindEnum.Parse(s) }

func (k QlsSpanKind) MarshalJSON() ([]byte, error)  { return qlsSpanKindEnum.marshalJSON(k) }
func (k *QlsSpanKind) UnmarshalJSON(b []byte) error { return qlsSpanKindEnum.unmarshalJSON(b, k) }

// TranslatabilityFlag records how faithfully an IR node can be rendered into a
// given target DSL. The validator sets it; the emitter reads it; the fidelity
// reporter aggregates it.
//
// The zero value is FULL: a node is assumed translatable until something
// downgrades it. Ordinals ascend with severity so Worst can combine flags with a
// simple comparison.
type TranslatabilityFlag int

const (
	// TranslatabilityFull means the target DSL expresses the node exactly.
	TranslatabilityFull TranslatabilityFlag = iota
	// TranslatabilityPartial means the target DSL expresses an approximation.
	// The node's Reason must explain what changes.
	TranslatabilityPartial
	// TranslatabilityUnsupported means the target DSL cannot express the node at
	// all. The node's Reason must explain why.
	TranslatabilityUnsupported
)

var translatabilityFlagEnum = newEnumDef[TranslatabilityFlag]("TranslatabilityFlag",
	"FULL",
	"PARTIAL",
	"UNSUPPORTED",
)

func (f TranslatabilityFlag) String() string { return translatabilityFlagEnum.String(f) }

// ParseTranslatabilityFlag resolves a translatability symbol, case-insensitively.
func ParseTranslatabilityFlag(s string) (TranslatabilityFlag, error) {
	return translatabilityFlagEnum.Parse(s)
}

func (f TranslatabilityFlag) MarshalJSON() ([]byte, error) {
	return translatabilityFlagEnum.marshalJSON(f)
}
func (f *TranslatabilityFlag) UnmarshalJSON(b []byte) error {
	return translatabilityFlagEnum.unmarshalJSON(b, f)
}

// Worst returns the more severe of two flags. A subtree's fidelity is the worst
// fidelity found anywhere within it, so the reporter folds with this.
func (f TranslatabilityFlag) Worst(other TranslatabilityFlag) TranslatabilityFlag {
	if other > f {
		return other
	}
	return f
}

// Timestamp is a specific point in time (QLS §.1.7). The spec requires
// nanosecond resolution, so the IR stores nanoseconds since the Unix epoch
// rather than a wall-clock struct, keeping serialization exact and comparison
// cheap.
type Timestamp struct {
	UnixNano int64 `json:"unix_nano"`
}

// NewTimestamp converts a time.Time into a QLS Timestamp.
func NewTimestamp(t time.Time) Timestamp { return Timestamp{UnixNano: t.UnixNano()} }

// Time converts back to a time.Time in UTC. QLS §Time Based Windowing normalises
// on UTC boundaries by default, so UTC is the right lens for IR timestamps.
func (t Timestamp) Time() time.Time { return time.Unix(0, t.UnixNano).UTC() }

// IsZero reports whether the timestamp is unset.
func (t Timestamp) IsZero() bool { return t.UnixNano == 0 }

// Before reports whether t precedes other.
func (t Timestamp) Before(other Timestamp) bool { return t.UnixNano < other.UnixNano }

func (t Timestamp) String() string {
	if t.IsZero() {
		return "<unset>"
	}
	return t.Time().Format(time.RFC3339Nano)
}

// Interval is a duration (QLS §.1.8). An Interval of zero nanoseconds is
// meaningful in the spec: a temporally aggregated metric whose step varies
// across values (calendar alignment, for instance) carries a zero step and
// records its real boundaries separately.
// SourceText, when set, is the duration exactly as the query was written. Nanos
// is what everything computes with; SourceText only decides how the value goes
// back out. Keeping it means a LogQL "[90m]" returns as "90m" rather than as the
// equal but unfamiliar "1h30m" — a translated query its author still recognizes
// is worth more than a canonical one.
type Interval struct {
	Nanos      int64  `json:"nanos"`
	SourceText string `json:"source_text,omitempty"`
}

// NewInterval converts a time.Duration into a QLS Interval, recording no source
// spelling.
func NewInterval(d time.Duration) Interval { return Interval{Nanos: int64(d)} }

// NewIntervalFromSource converts a duration and records the text it was written
// as, so an emitter can reproduce the author's units.
func NewIntervalFromSource(d time.Duration, text string) Interval {
	return Interval{Nanos: int64(d), SourceText: text}
}

// WithSourceText returns a copy carrying the given source spelling.
func (i Interval) WithSourceText(text string) Interval {
	i.SourceText = text
	return i
}

// Duration converts back to a time.Duration.
func (i Interval) Duration() time.Duration { return time.Duration(i.Nanos) }

// IsZero reports whether the interval has zero length. The source spelling is
// advisory and plays no part.
func (i Interval) IsZero() bool { return i.Nanos == 0 }

// Equal compares two intervals by length, ignoring how each was written.
func (i Interval) Equal(other Interval) bool { return i.Nanos == other.Nanos }

func (i Interval) String() string {
	if i.SourceText != "" {
		return i.SourceText
	}
	return i.Duration().String()
}

// ArithOp is the operator a BinaryOpStage applies.
//
// The six arithmetic operators come first. The logical and comparison operators
// follow because they occupy the same position in both target grammars — "a and
// b" and "a > b" are binary expressions exactly as "a + b" is — and a
// BinaryOpStage has to be able to say which one it holds. Leaving those on a
// stringly-typed function stage would have defeated the point of naming the
// arithmetic ones.
//
// These are operators over whole result sets, which is what distinguishes them
// from MatchOp: a predicate over one attribute of one record.
type ArithOp int

const (
	ArithAdd ArithOp = iota
	ArithSub
	ArithMul
	ArithDiv
	ArithMod
	ArithPow
	// ArithAnd, ArithOr and ArithUnless combine two sets by membership.
	ArithAnd
	ArithOr
	ArithUnless
	// ArithEQ through ArithLTE compare two sets element-wise. A comparison
	// against a constant is a FilterStage instead, since that selects records
	// rather than combining two sets.
	ArithEQ
	ArithNEQ
	ArithGT
	ArithGTE
	ArithLT
	ArithLTE
	// ArithNeg and ArithPos are the unary signs. They take one operand rather
	// than two, which is what separates a UnaryOpStage from a BinaryOpStage;
	// they live in the same enum because both languages spell them with the
	// same symbols as subtraction and addition.
	ArithNeg
	ArithPos
)

var arithOpEnum = newEnumDef[ArithOp]("ArithOp",
	"ADD", "SUB", "MUL", "DIV", "MOD", "POW",
	"AND", "OR", "UNLESS",
	"EQ", "NEQ", "GT", "GTE", "LT", "LTE",
	"NEG", "POS",
)

func (o ArithOp) String() string { return arithOpEnum.String(o) }

// ParseArithOp resolves a binary operator symbol, case-insensitively.
func ParseArithOp(s string) (ArithOp, error) { return arithOpEnum.Parse(s) }

func (o ArithOp) MarshalJSON() ([]byte, error)  { return arithOpEnum.marshalJSON(o) }
func (o *ArithOp) UnmarshalJSON(b []byte) error { return arithOpEnum.unmarshalJSON(b, o) }

// IsArithmetic reports whether the operator computes a value rather than
// combining or comparing sets.
func (o ArithOp) IsArithmetic() bool { return o >= ArithAdd && o <= ArithPow }

// IsSetOperator reports whether the operator combines two sets by membership.
func (o ArithOp) IsSetOperator() bool { return o == ArithAnd || o == ArithOr || o == ArithUnless }

// IsComparison reports whether the operator compares two sets element-wise.
func (o ArithOp) IsComparison() bool { return o >= ArithEQ && o <= ArithLTE }

// IsUnary reports whether the operator takes a single operand.
func (o ArithOp) IsUnary() bool { return o == ArithNeg || o == ArithPos }

// arithPrecedence is the binding strength both target languages give these
// operators, lowest first. An emitter reads it to decide when an operand needs
// parentheses, using the same table the parsers use.
var arithPrecedence = map[ArithOp]int{
	ArithOr:     1,
	ArithAnd:    2,
	ArithUnless: 2,
	ArithEQ:     3, ArithNEQ: 3, ArithGT: 3, ArithGTE: 3, ArithLT: 3, ArithLTE: 3,
	ArithAdd: 4, ArithSub: 4,
	ArithMul: 5, ArithDiv: 5, ArithMod: 5,
	ArithPow: 6,
	// A unary sign binds more tightly than multiplication and less tightly than
	// exponentiation, which is why -2^2 is -(2^2) rather than (-2)^2.
	ArithNeg: 6,
	ArithPos: 6,
}

// Precedence returns the operator's binding strength; a higher number binds more
// tightly.
func (o ArithOp) Precedence() int {
	if p, ok := arithPrecedence[o]; ok {
		return p
	}
	return 0
}

// IsRightAssociative reports whether equal precedence groups to the right, which
// only exponentiation does.
func (o ArithOp) IsRightAssociative() bool { return o == ArithPow }

// SignalMismatchInfo records that a query reads one class of telemetry and the
// target language reads another.
//
// It is deliberately not a TranslatabilityFlag on any node, because it answers a
// different question. "Did every construct translate?" and "can the result run
// on the target backend?" are separate, and a query whose every construct
// carries over exactly still cannot execute against a backend that holds no
// metrics. Flagging the root UNSUPPORTED for this conflated the two and made
// every score meaningless — a perfectly translated rate() scored 0.67 for a
// reason that had nothing to do with the translation.
//
// Someone migrating dashboards between backends already knows the signal types
// differ; that is why they are translating. What they need to know is what the
// translation cost, which is what the fidelity score now measures alone.
//
// It lives here because SignalType does, and because both the validator that
// discovers it and the reporter that renders it already depend on this package.
type SignalMismatchInfo struct {
	// SourceSignal is the class of telemetry the query reads.
	SourceSignal SignalType `json:"source_signal"`
	// TargetSignals names what the target language reads, comma-separated when
	// it reads more than one.
	TargetSignals string `json:"target_signals"`
	// Message states the mismatch in a form a person can read.
	Message string `json:"message"`
}

// String renders the mismatch for a log line or an error.
func (m *SignalMismatchInfo) String() string {
	if m == nil {
		return ""
	}
	return m.Message
}
