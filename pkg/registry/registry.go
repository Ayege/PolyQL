// Package registry loads the data-driven language definitions that make PolyQL
// extensible without code changes.
//
// Each DSL is described by one YAML file in the registry directory: the
// functions it offers and what each maps to in the QLS-aligned IR, its operator
// spellings, its type coercions, its metric type names, and the aggregation
// semantics it assumes. The resolver and the validator read these definitions
// rather than hard-coding per-DSL knowledge, so adding a language means adding a
// YAML file plus a parser/emitter pair.
//
// The YAML is the contribution surface, so loading is deliberately strict:
// unknown keys and unknown IR symbols are errors that name the file and the
// field, rather than being silently ignored and surfacing later as a wrong
// translation.
package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/polyql/polyql/pkg/compiler/ir"
)

// FunctionDef describes one named operation a DSL offers and how it maps onto
// the IR.
//
// A function maps to the IR in one of two ways. When IRKind names an IR
// aggregation operator, the resolver emits an AggregationStage carrying AggOp
// and AggScope. Otherwise the function has no IR-level equivalent of its own and
// the resolver emits a FunctionStage named IRName, leaving the validator to
// judge how well the target DSL can express it.
type FunctionDef struct {
	// Name is the function's spelling in this DSL.
	Name string
	// IRKind is the IR aggregation operator symbol from the YAML, empty when
	// the function does not map to one.
	IRKind string
	// AggOp is IRKind resolved. It is meaningful only when IsAggregation.
	AggOp ir.AggOp
	// AggScope is the axis the aggregation collapses. It is what distinguishes
	// PromQL's sum from its sum_over_time, which share an IR operator, and so
	// is what makes the reverse mapping used by emitters unambiguous.
	AggScope ir.AggScope
	// IsAggregation reports whether IRKind was set.
	IsAggregation bool
	// IRName is the DSL-neutral function name used when the function becomes a
	// FunctionStage. It defaults to Name.
	IRName string

	// Arity is the number of fixed arguments.
	Arity int
	// Variadic is 0 for a fixed arity, -1 when the final argument type may
	// repeat without limit, and n > 0 when up to n optional arguments may
	// follow.
	Variadic int
	// ArgTypes are the fixed argument types, in order.
	ArgTypes []ArgType
	// ReturnType is the QLS type the function yields.
	ReturnType ir.QlsDataType
}

// ArgType is one argument's declared type. Name is the spelling used in the
// YAML — a DSL type name such as range_vector, or a QLS type such as DOUBLE —
// and Type is that name resolved through the DSL's type coercion table.
//
// Both are kept because they answer different questions: the resolver needs the
// QLS type, while the validator needs the DSL type to explain why a translation
// loses something (a range vector and an instant vector both coerce to DOUBLE
// but are not interchangeable).
type ArgType struct {
	Name string
	Type ir.QlsDataType
}

// OperatorContext narrows where an operator spelling is valid, which is what
// keeps the reverse mapping deterministic when two spellings share an IR
// operator — PromQL writes label-selector equality as "=" and value comparison
// as "==", and both are IR EQ.
type OperatorContext int

const (
	// OperatorContextAny places no restriction.
	OperatorContextAny OperatorContext = iota
	// OperatorContextSelector is a matcher inside a series or stream selector.
	OperatorContextSelector
	// OperatorContextComparison is a value comparison in an expression.
	OperatorContextComparison
	// OperatorContextLineFilter matches against a whole log line rather than an
	// attribute.
	OperatorContextLineFilter
)

var operatorContextNames = map[OperatorContext]string{
	OperatorContextAny:        "any",
	OperatorContextSelector:   "selector",
	OperatorContextComparison: "comparison",
	OperatorContextLineFilter: "line_filter",
}

func (c OperatorContext) String() string {
	if s, ok := operatorContextNames[c]; ok {
		return s
	}
	return fmt.Sprintf("OperatorContext(%d)", int(c))
}

func parseOperatorContext(s string) (OperatorContext, error) {
	for ctx, name := range operatorContextNames {
		if name == strings.ToLower(strings.TrimSpace(s)) {
			return ctx, nil
		}
	}
	return 0, fmt.Errorf("%q is not a valid operator context (any, selector, comparison, line_filter)", s)
}

// OperatorDef maps one DSL operator spelling onto an IR match operator.
type OperatorDef struct {
	// Symbol is the operator as written in this DSL.
	Symbol string
	// IROp is the QLS selection predicate it corresponds to.
	IROp ir.MatchOp
	// Context narrows where the spelling applies.
	Context OperatorContext
}

// AggregationDefaults records the arithmetic semantics a DSL assumes, so the
// resolver can normalise them onto the QLS rules and the fidelity reporter can
// say what changed.
//
// QLS §Aggregation requires NULL, never NaN, as the sentinel for absent data,
// and specifies substituting 0 for NULL in addition and subtraction, 1 in
// multiplication, and 0 for a NULL divisor.
type AggregationDefaults struct {
	NullSubstituteAdd float64
	NullSubstituteMul float64
	// NullSubstituteDiv is the substitution for a NULL divisor. QLS specifies
	// it alongside the other two, so the registry must be able to express it.
	NullSubstituteDiv float64
	// NaNAsSentinel reports whether this DSL uses NaN to mean "no data". PromQL
	// does; QLS does not. Where this is true the resolver must convert the
	// NaN-sentinel pattern into an IR NULL, and the emitter must convert it
	// back.
	NaNAsSentinel bool
}

// AggregationClausePosition records where a DSL writes an aggregation's
// by/without clause relative to the expression it aggregates.
type AggregationClausePosition int

const (
	// ClauseBeforeOperand is "sum by (job) (foo)".
	ClauseBeforeOperand AggregationClausePosition = iota
	// ClauseAfterOperand is "sum(foo) by (job)".
	ClauseAfterOperand
)

var aggregationClausePositionNames = map[AggregationClausePosition]string{
	ClauseBeforeOperand: "before_operand",
	ClauseAfterOperand:  "after_operand",
}

func (p AggregationClausePosition) String() string {
	if s, ok := aggregationClausePositionNames[p]; ok {
		return s
	}
	return fmt.Sprintf("AggregationClausePosition(%d)", int(p))
}

// DurationFormat records how a DSL writes a duration back out.
type DurationFormat int

const (
	// DurationLargestUnit decomposes into the largest units that divide the
	// value, so seven days is written as one week.
	DurationLargestUnit DurationFormat = iota
	// DurationVerbatim reproduces the units the query was written with.
	DurationVerbatim
)

var durationFormatNames = map[DurationFormat]string{
	DurationLargestUnit: "largest_unit",
	DurationVerbatim:    "verbatim",
}

func (f DurationFormat) String() string {
	if s, ok := durationFormatNames[f]; ok {
		return s
	}
	return fmt.Sprintf("DurationFormat(%d)", int(f))
}

// StringQuoting records which quote a DSL emits string literals with.
type StringQuoting int

const (
	StringQuoteDouble StringQuoting = iota
	StringQuoteSingle
	StringQuoteBacktick
)

var stringQuotingNames = map[StringQuoting]string{
	StringQuoteDouble:   "double",
	StringQuoteSingle:   "single",
	StringQuoteBacktick: "backtick",
}

func (q StringQuoting) String() string {
	if s, ok := stringQuotingNames[q]; ok {
		return s
	}
	return fmt.Sprintf("StringQuoting(%d)", int(q))
}

// Normalizations records the canonical form a DSL's parser renders back to when
// a query could have been written more than one way.
//
// The parsers normalise while rendering — PromQL accepts "sum(foo) by (job)" and
// writes it back as "sum by (job) (foo)" — and the resolver works from that
// normalised tree rather than undoing it. Recording the choices here is what
// lets an emitter produce the form its target expects instead of inferring it,
// and lets a round-trip test know which differences are canonicalisation rather
// than translation loss.
type Normalizations struct {
	AggregationClausePosition AggregationClausePosition
	DurationFormat            DurationFormat
	StringQuoting             StringQuoting
}

// Capabilities records what a DSL can express, so the validator can flag an IR
// construct the target has no way to represent.
type Capabilities struct {
	// Joins reports whether the DSL can correlate two result sets at all.
	// LogQL cannot, so any IR JoinStage translated into it is UNSUPPORTED.
	Joins bool
	// JoinTypes are the QLS join types the DSL supports, empty when Joins is
	// false.
	JoinTypes []ir.JoinType
	// Subqueries reports whether the DSL can evaluate an expression over a
	// range at its own resolution.
	Subqueries bool
	// PipelineOrdered reports whether the DSL's syntax fixes the order of
	// pipeline stages. LogQL does — a label filter cannot precede the parser
	// that extracts the label — while PromQL nests its operations and imposes
	// no order. A validator uses this to decide whether an IR pipeline needs
	// reordering for the target.
	PipelineOrdered bool
	// WindowAlignments are the windowing alignments the DSL can express
	// (QLS §Time Based Windowing). An IR window asking for an alignment absent
	// from this list cannot be rendered faithfully.
	WindowAlignments []ir.WindowAlignment
	// BoolModifier reports whether the DSL can write a comparison that yields
	// 0 or 1 for every record instead of dropping the ones that fail it —
	// PromQL's bool. A target without it can still write the filtering form,
	// but the result set differs.
	BoolModifier bool
}

// SupportsWindowAlignment reports whether the DSL can express an alignment.
// A definition that lists none is taken to support only the QLS default, since
// that is what an implementation gets by following the spec.
func (c *Capabilities) SupportsWindowAlignment(a ir.WindowAlignment) bool {
	if len(c.WindowAlignments) == 0 {
		return a == ir.WindowUTCNormalized
	}
	for _, supported := range c.WindowAlignments {
		if supported == a {
			return true
		}
	}
	return false
}

// SupportsJoinType reports whether the DSL can express a given join.
func (c *Capabilities) SupportsJoinType(t ir.JoinType) bool {
	if !c.Joins {
		return false
	}
	for _, jt := range c.JoinTypes {
		if jt == t {
			return true
		}
	}
	return false
}

// DSLDefinition is one language's complete registry entry.
type DSLDefinition struct {
	// DSL is the language's canonical lowercase name, matching the name its
	// parser and emitter register under.
	DSL string
	// SupportedSignalTypes are the telemetry classes this DSL queries.
	SupportedSignalTypes []ir.SignalType
	// Functions is keyed by the function's spelling in this DSL.
	Functions map[string]*FunctionDef
	// Operators is keyed by the operator's spelling in this DSL.
	Operators map[string]*OperatorDef
	// TypeCoercion maps this DSL's type names onto QLS types.
	TypeCoercion map[string]ir.QlsDataType
	// MetricTypes maps this DSL's metric type names onto QLS metric types.
	MetricTypes         map[string]ir.QlsMetricType
	AggregationDefaults AggregationDefaults
	Capabilities        Capabilities
	Normalizations      Normalizations
	// SourcePath is the file the definition was read from, for error messages.
	SourcePath string
}

// Function returns a function definition by its DSL spelling.
func (d *DSLDefinition) Function(name string) (*FunctionDef, error) {
	if f, ok := d.Functions[name]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("registry: %s has no function %q", d.DSL, name)
}

// Operator returns an operator definition by its DSL spelling.
func (d *DSLDefinition) Operator(symbol string) (*OperatorDef, error) {
	if op, ok := d.Operators[symbol]; ok {
		return op, nil
	}
	return nil, fmt.Errorf("registry: %s has no operator %q", d.DSL, symbol)
}

// CoerceType resolves a DSL type name to its QLS type.
func (d *DSLDefinition) CoerceType(name string) (ir.QlsDataType, error) {
	if t, ok := d.TypeCoercion[name]; ok {
		return t, nil
	}
	return 0, fmt.Errorf("registry: %s has no type coercion for %q", d.DSL, name)
}

// MetricType resolves a DSL metric type name to its QLS metric type.
func (d *DSLDefinition) MetricType(name string) (ir.QlsMetricType, error) {
	if t, ok := d.MetricTypes[name]; ok {
		return t, nil
	}
	return ir.MetricTypeUnknown, fmt.Errorf("registry: %s has no metric type %q", d.DSL, name)
}

// SupportsSignal reports whether the DSL queries a given telemetry class.
func (d *DSLDefinition) SupportsSignal(s ir.SignalType) bool {
	for _, supported := range d.SupportedSignalTypes {
		if supported == s {
			return true
		}
	}
	return false
}

// FunctionByIRName finds the function whose IR-neutral name matches, which is
// how a caller holding an IR FunctionStage asks whether this DSL can express it.
//
// The Functions map is keyed by the DSL's own spelling, so this is a scan rather
// than a lookup: LogQL writes "| json" for what the IR calls parse_json.
func (d *DSLDefinition) FunctionByIRName(irName string) (*FunctionDef, bool) {
	for _, fn := range d.Functions {
		if fn.IRName == irName {
			return fn, true
		}
	}
	return nil, false
}

// FunctionsForAggOp returns every function mapping to an IR aggregation
// operator, sorted by DSL name for determinism.
//
// More than one can match: PromQL's sum and sum_over_time are both IR SUM, and
// only their scope tells them apart. A caller that needs one specific spelling
// filters the result by AggScope.
func (d *DSLDefinition) FunctionsForAggOp(op ir.AggOp) []*FunctionDef {
	var matches []*FunctionDef
	for _, fn := range d.Functions {
		if fn.IsAggregation && fn.AggOp == op {
			matches = append(matches, fn)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	return matches
}

// FunctionForAggregation finds the function expressing an IR aggregation on a
// given axis, which is the exact pairing an emitter needs.
func (d *DSLDefinition) FunctionForAggregation(op ir.AggOp, scope ir.AggScope) (*FunctionDef, bool) {
	for _, fn := range d.FunctionsForAggOp(op) {
		if fn.AggScope == scope {
			return fn, true
		}
	}
	return nil, false
}

// OperatorsForIROp returns every DSL spelling of an IR selection predicate,
// sorted for determinism.
func (d *DSLDefinition) OperatorsForIROp(op ir.MatchOp) []*OperatorDef {
	var matches []*OperatorDef
	for _, def := range d.Operators {
		if def.IROp == op {
			matches = append(matches, def)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Symbol < matches[j].Symbol })
	return matches
}

// SupportsIROp reports whether the DSL has any spelling of an IR selection
// predicate.
//
// Context is deliberately not part of this question. The IR records what a
// predicate compares — an attribute, the log body, the metric value — but not
// whether the source wrote it inside a selector or as a filter, and those are
// not the same distinction. Answering "can this DSL express IN" with a context
// mismatch would report an operator as missing when the target has it. Context
// earns its keep on the emitter's side, where OperatorsForIROp lets it choose
// between "=" and "==".
func (d *DSLDefinition) SupportsIROp(op ir.MatchOp) bool {
	return len(d.OperatorsForIROp(op)) > 0
}

// SupportsIROpInContext reports whether the DSL can express an IR predicate with
// a spelling valid in the given context, which is the stricter question an
// emitter asks when choosing how to write one.
func (d *DSLDefinition) SupportsIROpInContext(op ir.MatchOp, ctx OperatorContext) bool {
	for _, def := range d.OperatorsForIROp(op) {
		if ctx == OperatorContextAny || def.Context == OperatorContextAny || def.Context == ctx {
			return true
		}
	}
	return false
}

// FunctionNames returns every function name in the definition, sorted.
func (d *DSLDefinition) FunctionNames() []string {
	names := make([]string, 0, len(d.Functions))
	for name := range d.Functions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// normalizeDSL puts a DSL name into the canonical form used as a registry key,
// so that a caller passing "PromQL" reaches the definition stored as "promql".
func normalizeDSL(dsl string) string {
	return strings.ToLower(strings.TrimSpace(dsl))
}

// loaded holds the definitions installed by Load, which Get reads.
var loaded = struct {
	sync.RWMutex
	defs map[string]*DSLDefinition
}{defs: make(map[string]*DSLDefinition)}

// sourceFile is one definition's bytes together with the name to blame in an
// error. It lets the on-disk and compiled-in loaders share everything after the
// read.
type sourceFile struct {
	path string
	data []byte
}

// isDefinitionFile reports whether a filename is a registry definition.
func isDefinitionFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// Load returns the language definitions and installs them as the process-wide
// set that Get reads.
//
// An empty dir selects the definitions compiled into the binary, which is the
// normal case: polyql ships as a single executable that needs no files beside
// it. Passing a directory overrides that set entirely, which is how a
// contributor iterates on a DSL, or an operator supplies a private vendor
// definition, without rebuilding.
//
// Loading is all-or-nothing: if any definition is malformed the installed set is
// left untouched, so a bad edit cannot leave the process running on a
// half-applied registry.
func Load(dir string) (map[string]*DSLDefinition, error) {
	var defs map[string]*DSLDefinition
	var err error
	if strings.TrimSpace(dir) == "" {
		defs, err = LoadEmbedded()
	} else {
		defs, err = LoadDir(dir)
	}
	if err != nil {
		return nil, err
	}

	loaded.Lock()
	loaded.defs = defs
	loaded.Unlock()

	return defs, nil
}

// LoadDir reads the definitions in dir without installing them, which lets a
// caller inspect or validate a candidate registry before adopting it.
func LoadDir(dir string) (map[string]*DSLDefinition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("registry: cannot read directory %s: %w", dir, err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !isDefinitionFile(entry.Name()) {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	// Sort so that a duplicate-DSL error names the same pair of files every
	// time, whatever order the filesystem hands them back in.
	sort.Strings(paths)

	files := make([]sourceFile, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("registry: cannot read %s: %w", path, err)
		}
		files = append(files, sourceFile{path: path, data: data})
	}

	defs, err := loadFiles(files)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("registry: no DSL definitions found in %s", dir)
	}
	return defs, nil
}

// LoadFile reads one definition file.
func LoadFile(path string) (*DSLDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry: cannot read %s: %w", path, err)
	}
	return parseDefinition(data, path)
}

// loadFiles parses an already-read set of definitions, rejecting a set in which
// two files claim the same DSL.
func loadFiles(files []sourceFile) (map[string]*DSLDefinition, error) {
	defs := make(map[string]*DSLDefinition, len(files))
	for _, file := range files {
		def, err := parseDefinition(file.data, file.path)
		if err != nil {
			return nil, err
		}
		if existing, ok := defs[def.DSL]; ok {
			return nil, fmt.Errorf("registry: DSL %q is defined twice, in %s and %s",
				def.DSL, existing.SourcePath, file.path)
		}
		defs[def.DSL] = def
	}
	return defs, nil
}

// Get returns an installed definition by DSL name, case-insensitively.
func Get(dsl string) (*DSLDefinition, error) {
	name := normalizeDSL(dsl)

	loaded.RLock()
	defer loaded.RUnlock()

	if def, ok := loaded.defs[name]; ok {
		return def, nil
	}
	if len(loaded.defs) == 0 {
		return nil, fmt.Errorf("registry: no definition for DSL %q: the registry is empty, "+
			"which usually means Load has not been called", dsl)
	}
	return nil, fmt.Errorf("registry: no definition for DSL %q (loaded: %s)",
		dsl, strings.Join(listLocked(), ", "))
}

// List returns the installed DSL names in sorted order.
func List() []string {
	loaded.RLock()
	defer loaded.RUnlock()
	return listLocked()
}

// listLocked returns the sorted DSL names. The caller must hold at least a read
// lock.
func listLocked() []string {
	names := make([]string, 0, len(loaded.defs))
	for name := range loaded.defs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
