package registry

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/polyql/polyql/pkg/compiler/ir"
)

// The raw types mirror the YAML one-to-one, holding every IR reference as the
// symbol the contributor wrote. Resolution into typed IR values happens
// afterwards, in a pass that can report which file and which field is at fault.
type rawDefinition struct {
	DSL                 string                           `yaml:"dsl"`
	SignalTypes         []string                         `yaml:"signal_types"`
	Functions           map[string]rawFunction           `yaml:"functions"`
	Operators           map[string]rawOperator           `yaml:"operators"`
	LogicalOperators    map[string]rawLogicalOperator    `yaml:"logical_operators"`
	StructuralOperators map[string]rawStructuralOperator `yaml:"structural_operators"`
	TypeCoercion        map[string]string                `yaml:"type_coercion"`
	MetricTypes         map[string]string                `yaml:"metric_types"`
	AggregationDefaults *rawAggregationDefaults          `yaml:"aggregation_defaults"`
	Capabilities        *rawCapabilities                 `yaml:"capabilities"`
	Normalizations      *rawNormalizations               `yaml:"normalizations"`
}

type rawNormalizations struct {
	AggregationClausePosition string `yaml:"aggregation_clause_position"`
	DurationFormat            string `yaml:"duration_format"`
	StringQuoting             string `yaml:"string_quoting"`
}

type rawFunction struct {
	IRKind     string   `yaml:"ir_kind"`
	AggScope   string   `yaml:"agg_scope"`
	IRName     string   `yaml:"ir_name"`
	Arity      int      `yaml:"arity"`
	Variadic   int      `yaml:"variadic"`
	ArgTypes   []string `yaml:"arg_types"`
	ReturnType string   `yaml:"return_type"`
}

type rawOperator struct {
	IROp    string `yaml:"ir_op"`
	Context string `yaml:"context"`
	// Symbol overrides the map key as the operator's spelling. It exists
	// because a DSL may spell two different operators the same way — LogQL
	// writes both a label mismatch and a negated line filter as "!=" — and a
	// map cannot hold one key twice. Such an entry is keyed by a descriptive
	// name and states its spelling here.
	Symbol string `yaml:"symbol"`
}

type rawLogicalOperator struct {
	IROp string `yaml:"ir_op"`
	// Symbol overrides the map key as the spelling, for the same reason
	// rawOperator carries one: a DSL may spell two connectives alike.
	Symbol string `yaml:"symbol"`
}

type rawStructuralOperator struct {
	IROp        string `yaml:"ir_op"`
	Symbol      string `yaml:"symbol"`
	Description string `yaml:"description"`
}

type rawAggregationDefaults struct {
	NullSubstituteAdd *float64 `yaml:"null_substitute_add"`
	NullSubstituteMul *float64 `yaml:"null_substitute_mul"`
	NullSubstituteDiv *float64 `yaml:"null_substitute_div"`
	NaNAsSentinel     *bool    `yaml:"nan_as_sentinel"`
}

type rawCapabilities struct {
	Joins            *bool    `yaml:"joins"`
	JoinTypes        []string `yaml:"join_types"`
	Subqueries       *bool    `yaml:"subqueries"`
	PipelineOrdered  *bool    `yaml:"pipeline_ordered"`
	WindowAlignments []string `yaml:"window_alignments"`
	BoolModifier     *bool    `yaml:"bool_modifier"`
	Arithmetic       *bool    `yaml:"arithmetic"`
	TemporalWindows  *bool    `yaml:"temporal_windows"`
	MetricExtraction *bool    `yaml:"metric_extraction"`
	ScopedAttributes *bool    `yaml:"scoped_attributes"`
	BooleanSelectors *bool    `yaml:"boolean_selectors"`
	AttributeCasts   *bool    `yaml:"attribute_casts"`
	LabelFilters     *bool    `yaml:"label_filters"`
	OffsetNeedsRange *bool    `yaml:"offset_needs_range"`

	GroupAggregationNeedsSamples *bool    `yaml:"group_aggregation_needs_samples"`
	StructuralOps                []string `yaml:"structural_ops"`
}

// parseDefinition decodes and resolves one registry file.
func parseDefinition(data []byte, path string) (*DSLDefinition, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	// Reject keys the schema does not define. The YAML is the contribution
	// surface, and a silently ignored typo would surface much later as a
	// mistranslation rather than as an error here.
	decoder.KnownFields(true)

	var raw rawDefinition
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("registry: cannot parse %s: %w", path, err)
	}

	def := &DSLDefinition{
		DSL:                 strings.ToLower(strings.TrimSpace(raw.DSL)),
		Functions:           make(map[string]*FunctionDef, len(raw.Functions)),
		Operators:           make(map[string]*OperatorDef, len(raw.Operators)),
		LogicalOperators:    make(map[string]*LogicalOperatorDef, len(raw.LogicalOperators)),
		StructuralOperators: make(map[string]*StructuralOperatorDef, len(raw.StructuralOperators)),
		TypeCoercion:        make(map[string]ir.QlsDataType, len(raw.TypeCoercion)),
		MetricTypes:         make(map[string]ir.QlsMetricType, len(raw.MetricTypes)),
		SourcePath:          path,
		// QLS §Aggregation defaults, which a definition may override.
		AggregationDefaults: AggregationDefaults{
			NullSubstituteAdd: 0,
			NullSubstituteMul: 1,
			NullSubstituteDiv: 0,
			NaNAsSentinel:     false,
		},
		// Capability defaults describe the languages the registry already holds,
		// so an existing definition that says nothing keeps its meaning. A DSL
		// that lacks one of these — TraceQL has neither arithmetic nor a
		// window — has to say so explicitly, which is the safer direction for
		// the omission to fail in.
		Capabilities: Capabilities{
			Arithmetic:      true,
			TemporalWindows: true,
		},
	}

	if def.DSL == "" {
		return nil, fmt.Errorf("registry: %s is missing the required \"dsl\" name", path)
	}

	fail := func(field string, err error) error {
		return fmt.Errorf("registry: %s: %s: %w", path, field, err)
	}

	for i, name := range raw.SignalTypes {
		signal, err := ir.ParseSignalType(name)
		if err != nil {
			return nil, fail(fmt.Sprintf("signal_types[%d]", i), err)
		}
		def.SupportedSignalTypes = append(def.SupportedSignalTypes, signal)
	}
	if len(def.SupportedSignalTypes) == 0 {
		return nil, fmt.Errorf("registry: %s must list at least one entry under \"signal_types\"", path)
	}

	// Type coercion is resolved first: argument and return types may name a DSL
	// type, and resolving those needs this table already built.
	for name, symbol := range raw.TypeCoercion {
		dataType, err := ir.ParseQlsDataType(symbol)
		if err != nil {
			return nil, fail("type_coercion."+name, err)
		}
		def.TypeCoercion[name] = dataType
	}

	for name, symbol := range raw.MetricTypes {
		metricType, err := ir.ParseQlsMetricType(symbol)
		if err != nil {
			return nil, fail("metric_types."+name, err)
		}
		def.MetricTypes[name] = metricType
	}

	for name, rawFn := range raw.Functions {
		fn, err := def.resolveFunction(name, rawFn)
		if err != nil {
			return nil, fail("functions."+name, err)
		}
		def.Functions[name] = fn
	}

	for symbol, rawOp := range raw.Operators {
		op, err := resolveOperator(symbol, rawOp)
		if err != nil {
			return nil, fail("operators."+symbol, err)
		}
		def.Operators[symbol] = op
	}

	for symbol, rawOp := range raw.LogicalOperators {
		op, err := resolveLogicalOperator(symbol, rawOp)
		if err != nil {
			return nil, fail("logical_operators."+symbol, err)
		}
		def.LogicalOperators[symbol] = op
	}

	for name, rawOp := range raw.StructuralOperators {
		op, err := resolveStructuralOperator(name, rawOp)
		if err != nil {
			return nil, fail("structural_operators."+name, err)
		}
		def.StructuralOperators[name] = op
	}

	if agg := raw.AggregationDefaults; agg != nil {
		if agg.NullSubstituteAdd != nil {
			def.AggregationDefaults.NullSubstituteAdd = *agg.NullSubstituteAdd
		}
		if agg.NullSubstituteMul != nil {
			def.AggregationDefaults.NullSubstituteMul = *agg.NullSubstituteMul
		}
		if agg.NullSubstituteDiv != nil {
			def.AggregationDefaults.NullSubstituteDiv = *agg.NullSubstituteDiv
		}
		if agg.NaNAsSentinel != nil {
			def.AggregationDefaults.NaNAsSentinel = *agg.NaNAsSentinel
		}
	}

	if caps := raw.Capabilities; caps != nil {
		if caps.Joins != nil {
			def.Capabilities.Joins = *caps.Joins
		}
		if caps.Subqueries != nil {
			def.Capabilities.Subqueries = *caps.Subqueries
		}
		if caps.PipelineOrdered != nil {
			def.Capabilities.PipelineOrdered = *caps.PipelineOrdered
		}
		if caps.BoolModifier != nil {
			def.Capabilities.BoolModifier = *caps.BoolModifier
		}
		if caps.Arithmetic != nil {
			def.Capabilities.Arithmetic = *caps.Arithmetic
		}
		if caps.TemporalWindows != nil {
			def.Capabilities.TemporalWindows = *caps.TemporalWindows
		}
		if caps.MetricExtraction != nil {
			def.Capabilities.MetricExtraction = *caps.MetricExtraction
		}
		if caps.ScopedAttributes != nil {
			def.Capabilities.ScopedAttributes = *caps.ScopedAttributes
		}
		if caps.BooleanSelectors != nil {
			def.Capabilities.BooleanSelectors = *caps.BooleanSelectors
		}
		if caps.AttributeCasts != nil {
			def.Capabilities.AttributeCasts = *caps.AttributeCasts
		}
		if caps.LabelFilters != nil {
			def.Capabilities.LabelFilters = *caps.LabelFilters
		}
		if caps.OffsetNeedsRange != nil {
			def.Capabilities.OffsetNeedsRange = *caps.OffsetNeedsRange
		}
		if caps.GroupAggregationNeedsSamples != nil {
			def.Capabilities.GroupAggregationNeedsSamples = *caps.GroupAggregationNeedsSamples
		}
		for i, symbol := range caps.StructuralOps {
			op, err := ir.ParseStructuralOp(symbol)
			if err != nil {
				return nil, fail(fmt.Sprintf("capabilities.structural_ops[%d]", i), err)
			}
			def.Capabilities.StructuralOps = append(def.Capabilities.StructuralOps, op)
		}
		for i, symbol := range caps.WindowAlignments {
			alignment, err := ir.ParseWindowAlignment(symbol)
			if err != nil {
				return nil, fail(fmt.Sprintf("capabilities.window_alignments[%d]", i), err)
			}
			def.Capabilities.WindowAlignments = append(def.Capabilities.WindowAlignments, alignment)
		}
		for i, symbol := range caps.JoinTypes {
			joinType, err := ir.ParseJoinType(symbol)
			if err != nil {
				return nil, fail(fmt.Sprintf("capabilities.join_types[%d]", i), err)
			}
			def.Capabilities.JoinTypes = append(def.Capabilities.JoinTypes, joinType)
		}
		if !def.Capabilities.Joins && len(def.Capabilities.JoinTypes) > 0 {
			return nil, fmt.Errorf("registry: %s: capabilities lists join_types but sets joins: false", path)
		}
	}

	if norm := raw.Normalizations; norm != nil {
		if err := resolveNormalizations(norm, &def.Normalizations); err != nil {
			return nil, fmt.Errorf("registry: %s: normalizations: %w", path, err)
		}
	}

	// A capability the DSL claims but has no spelling for would let the
	// validator promise a translation the emitter then cannot write. Catching
	// the disagreement here names the file, rather than leaving it to surface as
	// a structural operator rendered as the empty string.
	for _, op := range def.Capabilities.StructuralOps {
		if _, ok := def.StructuralOperatorForIROp(op); !ok {
			return nil, fmt.Errorf("registry: %s: capabilities lists the structural operator %s "+
				"but structural_operators gives no spelling for it", path, op)
		}
	}
	for name, op := range def.StructuralOperators {
		if !def.Capabilities.SupportsStructuralOp(op.IROp) {
			return nil, fmt.Errorf("registry: %s: structural_operators.%s spells %s "+
				"but capabilities.structural_ops does not list it", path, name, op.IROp)
		}
	}

	return def, nil
}

// resolveNormalizations turns the YAML normalizations block into typed values,
// rejecting an unknown spelling rather than leaving an emitter to guess.
func resolveNormalizations(raw *rawNormalizations, out *Normalizations) error {
	if raw.AggregationClausePosition != "" {
		value, err := lookupEnum(aggregationClausePositionNames, raw.AggregationClausePosition)
		if err != nil {
			return fmt.Errorf("aggregation_clause_position: %w", err)
		}
		out.AggregationClausePosition = value
	}
	if raw.DurationFormat != "" {
		value, err := lookupEnum(durationFormatNames, raw.DurationFormat)
		if err != nil {
			return fmt.Errorf("duration_format: %w", err)
		}
		out.DurationFormat = value
	}
	if raw.StringQuoting != "" {
		value, err := lookupEnum(stringQuotingNames, raw.StringQuoting)
		if err != nil {
			return fmt.Errorf("string_quoting: %w", err)
		}
		out.StringQuoting = value
	}
	return nil
}

// lookupEnum resolves a YAML symbol against a name table, listing the valid
// spellings when it does not match.
func lookupEnum[T comparable](names map[T]string, symbol string) (T, error) {
	want := strings.ToLower(strings.TrimSpace(symbol))
	var valid []string
	for value, name := range names {
		if name == want {
			return value, nil
		}
		valid = append(valid, name)
	}
	sort.Strings(valid)
	var zero T
	return zero, fmt.Errorf("%q is not valid (expected one of: %s)", symbol, strings.Join(valid, ", "))
}

// resolveFunction turns one YAML function entry into a typed definition.
func (d *DSLDefinition) resolveFunction(name string, raw rawFunction) (*FunctionDef, error) {
	fn := &FunctionDef{
		Name:     name,
		IRKind:   raw.IRKind,
		IRName:   raw.IRName,
		Arity:    raw.Arity,
		Variadic: raw.Variadic,
	}
	if fn.IRName == "" {
		fn.IRName = name
	}

	if raw.IRKind != "" {
		aggOp, err := ir.ParseAggOp(raw.IRKind)
		if err != nil {
			return nil, fmt.Errorf("ir_kind: %w (omit ir_kind for a function that has no IR "+
				"aggregation operator; it becomes a FunctionStage instead)", err)
		}
		fn.AggOp = aggOp
		fn.IsAggregation = true

		// A temporal aggregation collapses points within one series over time;
		// a group aggregation collapses across series. Some operators only make
		// sense on one axis, so the default follows the operator when the file
		// does not say.
		switch {
		case raw.AggScope != "":
			scope, err := ir.ParseAggScope(raw.AggScope)
			if err != nil {
				return nil, fmt.Errorf("agg_scope: %w", err)
			}
			fn.AggScope = scope
		case aggOp.IsTemporalOnly():
			fn.AggScope = ir.AggScopeTemporal
		default:
			fn.AggScope = ir.AggScopeGroup
		}

		if aggOp.IsTemporalOnly() && fn.AggScope != ir.AggScopeTemporal {
			return nil, fmt.Errorf("agg_scope: %s only aggregates over time, so it cannot have scope %s",
				aggOp, fn.AggScope)
		}
	} else if raw.AggScope != "" {
		return nil, fmt.Errorf("agg_scope is only meaningful alongside ir_kind")
	}

	for i, typeName := range raw.ArgTypes {
		resolved, err := d.resolveTypeName(typeName)
		if err != nil {
			return nil, fmt.Errorf("arg_types[%d]: %w", i, err)
		}
		fn.ArgTypes = append(fn.ArgTypes, ArgType{Name: typeName, Type: resolved})
	}

	if raw.ReturnType == "" {
		return nil, fmt.Errorf("return_type is required")
	}
	returnType, err := d.resolveTypeName(raw.ReturnType)
	if err != nil {
		return nil, fmt.Errorf("return_type: %w", err)
	}
	fn.ReturnType = returnType

	// Arity and arg_types describe the same signature, so a disagreement means
	// one of them is stale.
	if raw.Arity != len(raw.ArgTypes) {
		return nil, fmt.Errorf("arity is %d but arg_types lists %d entries", raw.Arity, len(raw.ArgTypes))
	}
	if raw.Variadic < -1 {
		return nil, fmt.Errorf("variadic must be -1, 0 or a positive count, got %d", raw.Variadic)
	}
	if raw.Variadic != 0 && raw.Arity == 0 {
		return nil, fmt.Errorf("variadic is set but the function declares no argument types to repeat")
	}

	return fn, nil
}

// resolveTypeName resolves a type as written in the YAML. A definition may name
// either one of its own DSL types, which the type_coercion table maps onto QLS,
// or a QLS type directly.
func (d *DSLDefinition) resolveTypeName(name string) (ir.QlsDataType, error) {
	if coerced, ok := d.TypeCoercion[name]; ok {
		return coerced, nil
	}
	dataType, err := ir.ParseQlsDataType(name)
	if err != nil {
		known := make([]string, 0, len(d.TypeCoercion))
		for typeName := range d.TypeCoercion {
			known = append(known, typeName)
		}
		sort.Strings(known)
		return 0, fmt.Errorf("%q is neither a QLS data type nor a type_coercion entry (known: %s)",
			name, strings.Join(known, ", "))
	}
	return dataType, nil
}

func resolveOperator(symbol string, raw rawOperator) (*OperatorDef, error) {
	if raw.IROp == "" {
		return nil, fmt.Errorf("ir_op is required")
	}
	matchOp, err := ir.ParseMatchOp(raw.IROp)
	if err != nil {
		return nil, fmt.Errorf("ir_op: %w", err)
	}

	context := OperatorContextAny
	if raw.Context != "" {
		context, err = parseOperatorContext(raw.Context)
		if err != nil {
			return nil, fmt.Errorf("context: %w", err)
		}
	}
	spelling := symbol
	if raw.Symbol != "" {
		spelling = raw.Symbol
	}
	return &OperatorDef{Symbol: spelling, IROp: matchOp, Context: context}, nil
}

func resolveLogicalOperator(symbol string, raw rawLogicalOperator) (*LogicalOperatorDef, error) {
	if raw.IROp == "" {
		return nil, fmt.Errorf("ir_op is required")
	}
	logicalOp, err := ir.ParseLogicalOp(raw.IROp)
	if err != nil {
		return nil, fmt.Errorf("ir_op: %w (a logical operator composes predicates; "+
			"a comparison against one attribute belongs under \"operators\")", err)
	}
	spelling := symbol
	if raw.Symbol != "" {
		spelling = raw.Symbol
	}
	return &LogicalOperatorDef{Symbol: spelling, IROp: logicalOp}, nil
}

func resolveStructuralOperator(name string, raw rawStructuralOperator) (*StructuralOperatorDef, error) {
	if raw.IROp == "" {
		return nil, fmt.Errorf("ir_op is required")
	}
	structuralOp, err := ir.ParseStructuralOp(raw.IROp)
	if err != nil {
		return nil, fmt.Errorf("ir_op: %w", err)
	}
	// A structural operator is keyed by a readable name precisely so that its
	// spelling can be stated, since ">" and ">>" would otherwise be easy to key
	// wrongly. Leaving the spelling out would make the emitter write the name.
	if raw.Symbol == "" {
		return nil, fmt.Errorf("symbol is required: a structural operator is keyed by name, " +
			"so the spelling it is written with has to be given explicitly")
	}
	return &StructuralOperatorDef{
		Name:        name,
		Symbol:      raw.Symbol,
		IROp:        structuralOp,
		Description: raw.Description,
	}, nil
}
