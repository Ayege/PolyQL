// Package validator is stage 5 of the compiler pipeline: it judges a
// DSL-neutral IR tree against one target language and records what that target
// cannot express.
//
// # What it does and does not check
//
// The validator checks target compatibility only. Whether the source query was
// well formed — its arity, its argument types, its own semantic rules — was
// settled by the parser, which had the source text and could point at the
// offending token. Re-checking any of that here would duplicate the logic and
// produce worse messages, so the validator never does. A query that failed to
// parse never reaches it.
//
// # How it reports
//
// The validator mutates the tree in place. Every node arrives from the resolver
// flagged FULL; the validator downgrades the ones the target cannot express to
// PARTIAL or UNSUPPORTED and returns the same tree, so the emitter and the
// fidelity reporter read the flags as they stand rather than re-deriving them.
// A node's flag is the worst verdict reached about it, while the returned issue
// list holds every verdict, so nothing is hidden by a later, harsher finding.
//
// Nothing here hard-codes a DSL name. Which functions exist, whether joins or
// subqueries are possible, whether pipeline order is fixed — all of it is read
// from the target's registry definition, so adding a language means adding a
// YAML file rather than a case in a switch.
package validator

import (
	"fmt"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/registry"
)

// ValidationIssue is one finding about one node.
type ValidationIssue struct {
	// Path locates the node in the tree, as a dot-separated path with indices,
	// such as "Query.Pipeline[1].FilterStage.Predicate.Operands[0]".
	Path string
	// Flag is the verdict: PARTIAL when the target can approximate the
	// construct, UNSUPPORTED when it cannot express it at all.
	Flag ir.TranslatabilityFlag
	// Reason explains the finding in terms a query author would recognize.
	Reason string
	// SourceConstruct names what the source wrote, so a fidelity report can
	// point at the part of the query responsible.
	SourceConstruct string
}

func (i ValidationIssue) String() string {
	return fmt.Sprintf("%s: %s: %s", i.Path, i.Flag, i.Reason)
}

// Validate annotates a query with how well targetDSL can express it, and
// returns the same tree along with every finding.
//
// The tree is modified in place; the returned pointer is the argument. Findings
// are ordered by the traversal, so a caller printing them walks the query from
// its source outward.
func Validate(query *ir.Query, targetDSL string, reg *registry.Registry) (
	*ir.Query, []ValidationIssue, *ir.SignalMismatchInfo) {

	if query == nil {
		return nil, nil, nil
	}

	v := &validator{targetDSL: strings.ToLower(strings.TrimSpace(targetDSL))}
	if sourceDSL, ok := query.Hint(ir.HintSourceDSL); ok {
		v.sourceDSL = strings.ToLower(strings.TrimSpace(sourceDSL))
	}

	if reg == nil {
		v.report(query, "Query", ir.TranslatabilityUnsupported,
			"no language registry was supplied, so no target could be checked", "")
		return query, v.issues, nil
	}

	target, err := reg.Get(v.targetDSL)
	if err != nil {
		// Validate returns no error, so an unusable target is reported the same
		// way as any other thing the translation cannot do.
		v.report(query, "Query", ir.TranslatabilityUnsupported,
			fmt.Sprintf("no registry definition for target %q", targetDSL), "")
		return query, v.issues, nil
	}
	v.target = target

	// The source definition is optional: a hand-built tree may not name one,
	// and the checks that need it simply do not run.
	if v.sourceDSL != "" {
		if source, err := reg.Get(v.sourceDSL); err == nil {
			v.source = source
		}
	}

	v.run(query)
	return query, v.issues, v.signalMismatch
}

type validator struct {
	target    *registry.DSLDefinition
	source    *registry.DSLDefinition
	targetDSL string
	sourceDSL string
	issues    []ValidationIssue
	// signalMismatch is set when the query and the target read different
	// classes of telemetry. It is kept apart from issues because it is not a
	// translation loss — see ir.SignalMismatchInfo.
	signalMismatch *ir.SignalMismatchInfo
}

// run applies the checks in order. Earlier checks may leave flags that later
// ones read, so the sequence is deliberate: the whole-query checks come first,
// then the per-node walk, then the ordering pass that may rewrite the pipeline.
func (v *validator) run(query *ir.Query) {
	v.checkSignalType(query)
	v.checkSentinelSemantics(query, "Query")
	v.walk(query, "Query")
}

// report records a finding and downgrades the node's flag.
//
// A node keeps the worst verdict reached about it, while every verdict lands in
// the issue list. A later PARTIAL therefore cannot mask an earlier UNSUPPORTED,
// and neither finding is lost.
func (v *validator) report(node ir.Node, path string, flag ir.TranslatabilityFlag, reason, construct string) {
	base := node.Base()
	if flag > base.Flag {
		base.SetTranslatability(flag, reason)
	}
	v.issues = append(v.issues, ValidationIssue{
		Path:            path,
		Flag:            flag,
		Reason:          reason,
		SourceConstruct: construct,
	})
}

// checkSignalType records a target that reads a different class of telemetry.
//
// This does not flag any node. Whether the constructs translated and whether the
// result can run are separate questions, and answering the second by marking the
// first UNSUPPORTED made every fidelity score meaningless. The finding travels
// beside the issues instead, and the reporter shows it in its own section.
func (v *validator) checkSignalType(query *ir.Query) {
	if v.target.SupportsSignal(query.Signal) {
		return
	}
	supported := make([]string, 0, len(v.target.SupportedSignalTypes))
	for _, signal := range v.target.SupportedSignalTypes {
		supported = append(supported, signal.String())
	}
	joined := strings.Join(supported, ", ")

	v.signalMismatch = &ir.SignalMismatchInfo{
		SourceSignal:  query.Signal,
		TargetSignals: joined,
		Message: fmt.Sprintf("query is %s, target supports %s",
			query.Signal, joined),
	}
}

// checkSubquery covers a target with no way to evaluate an expression over a
// range at its own resolution.
//
// The finding lands on the Output node that carries the subquery, since that is
// the part of the tree the target cannot render.
func (v *validator) checkSubquery(query *ir.Query, path string) {
	if !query.Output.IsSubquery() || v.target.Capabilities.Subqueries {
		return
	}
	v.report(query.Output, path+".Output", ir.TranslatabilityUnsupported,
		fmt.Sprintf("subqueries are not supported in %s", v.targetDSL), "subquery")
	// The query as a whole cannot be rendered either, so the root says so too.
	v.report(query, path, ir.TranslatabilityUnsupported,
		fmt.Sprintf("subqueries are not supported in %s", v.targetDSL), "subquery")
}

// checkSentinelSemantics covers the difference SKILL.md singles out as a known
// translation hazard: PromQL uses NaN to mean "no data" where QLS and LogQL use
// absence. It is a property of the whole translation rather than of any one
// node, so it lands on the root.
func (v *validator) checkSentinelSemantics(query *ir.Query, path string) {
	if v.source == nil || v.source.DSL == v.target.DSL {
		return
	}
	sourceUsesNaN := v.source.AggregationDefaults.NaNAsSentinel
	targetUsesNaN := v.target.AggregationDefaults.NaNAsSentinel
	if sourceUsesNaN == targetUsesNaN {
		return
	}
	v.report(query, path, ir.TranslatabilityPartial,
		fmt.Sprintf("NaN-as-sentinel semantics differ between %s and %s; "+
			"absent-data handling may vary", v.sourceDSL, v.targetDSL),
		"absent data")
}

// walk visits every node under a query, checking each against the target.
//
// The traversal itself is ir.InspectPath, shared with the fidelity reporter so
// the two cannot drift: a node type added to the IR and not to that traversal
// fails ir's own test rather than going quietly unchecked here and unreported
// there.
func (v *validator) walk(root *ir.Query, path string) {
	ir.InspectPath(root, path, func(path string, node ir.Node) bool {
		switch n := node.(type) {
		case *ir.Query:
			// Ordering runs before the descent so that a stage's reported path
			// matches its final position in the pipeline.
			v.checkPipelineOrder(n, path)
			v.checkSubquery(n, path)
			// This one reads the pipeline as a whole rather than one stage, so
			// it runs here rather than in the AggregationStage case below. It
			// must follow the ordering pass, which may have moved the very
			// stages it counts.
			v.checkAggregationOperand(n, path)
			v.checkOffset(n, path)

		case *ir.LabelMatcher:
			// A matcher inside a selector is flagged in its own right; one
			// inside a predicate is reached through the MatchPredicate case
			// below, which flags the predicate instead.
			v.checkMatcher(n, n, path)

		case *ir.MatchPredicate:
			if n.Matcher != nil {
				// The predicate is what a report names, so the verdict goes
				// there rather than on the matcher it wraps.
				v.checkMatcher(n, n.Matcher, path)
			}
			// Its only child is that matcher, already accounted for.
			return false

		case *ir.AggregationStage:
			v.checkAggregation(n, path)

		case *ir.FunctionStage:
			v.checkFunction(n, path)

		case *ir.FilterStage:
			v.checkFilter(n, path)

		case *ir.JoinStage:
			v.checkJoin(n, path)

		case *ir.Window:
			v.checkWindow(n, path)

		case *ir.SpansetSelector:
			v.checkSpanset(n, path)

		case *ir.StructuralStage:
			v.checkStructural(n, path)

		case *ir.CoercionStage:
			v.checkCoercion(n, path)

		case *ir.BinaryOpStage:
			// Most targets have arithmetic, and where they do, what may not be
			// expressible is what the operator combines rather than the operator
			// — the traversal descends into those operands by itself. A target
			// with no arithmetic at all is the exception, and has to say so.
			v.checkArithmetic(n, path, strings.ToLower(n.Op.String()))

		case *ir.UnaryOpStage:
			v.checkArithmetic(n, path, strings.ToLower(n.Op.String()))
		}
		return true
	})
}

// checkSpanset covers a span set filter the target's selector cannot hold.
//
// Two things can go wrong, and they are different. A selector that puts an
// implicit "and" between its matchers cannot express a disjunction at all, so an
// OR or a NOT anywhere in the tree is a refusal. And an operator the target has
// but cannot write in a selector — LogQL's ">", which is a label filter rather
// than a stream matcher — is a refusal for that matcher alone.
func (v *validator) checkSpanset(spanset *ir.SpansetSelector, path string) {
	if spanset.Filters == nil {
		return
	}

	if !v.target.Capabilities.BooleanSelectors && !isConjunctive(spanset.Filters) {
		v.report(spanset, path, ir.TranslatabilityUnsupported,
			fmt.Sprintf("a %s selector puts an implicit \"and\" between its matchers, so it cannot "+
				"express the \"or\" in this span set filter", v.targetDSL),
			"spanset filter")
	}

	// The per-matcher check only applies where the filter sits in a selector,
	// which is what a spanset is. A predicate in a pipeline filter is judged by
	// checkMatcher instead, and correctly does not care about context.
	ir.Inspect(spanset.Filters, func(node ir.Node) bool {
		matcher, ok := node.(*ir.LabelMatcher)
		if !ok {
			return true
		}
		op := matcher.Op
		switch {
		case op == ir.MatchIn || op == ir.MatchContains:
			op = ir.MatchRegex
		case op == ir.MatchNotIn || op == ir.MatchNotContains:
			op = ir.MatchNotRegex
		}
		if v.target.SupportsIROpInContext(op, registry.OperatorContextSelector) {
			return true
		}
		// An operator the selector cannot spell may still be writable as a
		// label filter after it, which is how LogQL takes an ordered
		// comparison. The emitter moves it there rather than dropping it, so
		// calling it unsupported here would put the score at odds with the
		// output.
		if v.target.Capabilities.LabelFilters &&
			v.target.SupportsIROpInContext(op, registry.OperatorContextComparison) {
			return true
		}
		v.report(matcher, path, ir.TranslatabilityUnsupported,
			fmt.Sprintf("%s cannot compare with %s inside a selector, so the filter on %q "+
				"cannot be written", v.targetDSL, matcher.Op.Symbol(), matcher.Key),
			matcher.Op.String())
		return true
	})
}

// isConjunctive reports whether a predicate tree is a shape a conjunctive
// selector can hold.
//
// That is a pure AND of comparisons, plus one exception: a disjunction over a
// single attribute is set membership, which every conjunctive target writes as
// an anchored regex alternation. The exception is tested with the same function
// the emitters fold with, so the two cannot disagree — a validator calling this
// unsupported while the emitter wrote it would put the score and the output at
// odds, which is the failure the fidelity report exists to prevent.
func isConjunctive(predicate ir.Predicate) bool {
	switch node := predicate.(type) {
	case *ir.MatchPredicate:
		return true
	case *ir.LogicalPredicate:
		if node.Op == ir.LogicalOr {
			if _, foldable := ir.FoldSameKeyDisjunction(node); foldable {
				return true
			}
		}
		if node.Op != ir.LogicalAnd {
			return false
		}
		for _, operand := range node.Operands {
			if !isConjunctive(operand) {
				return false
			}
		}
		return true
	default:
		return predicate == nil
	}
}

// checkStructural covers a trace-tree relationship the target cannot express.
//
// A structural operator is not a join, so a target with joins is no help: there
// is no attribute recording which span is whose parent, and correlating on one
// would be a different query. This is a refusal wherever the target lacks the
// operator.
func (v *validator) checkStructural(stage *ir.StructuralStage, path string) {
	if v.target.Capabilities.SupportsStructuralOp(stage.Op) {
		return
	}
	if len(v.target.Capabilities.StructuralOps) == 0 {
		v.report(stage, path, ir.TranslatabilityUnsupported,
			fmt.Sprintf("%s has no way to relate records by their position in a trace; "+
				"the %s relationship cannot be written",
				v.targetDSL, strings.ToLower(stage.Op.String())),
			stage.Op.String())
		return
	}
	supported := make([]string, 0, len(v.target.Capabilities.StructuralOps))
	for _, op := range v.target.Capabilities.StructuralOps {
		supported = append(supported, strings.ToLower(op.String()))
	}
	v.report(stage, path, ir.TranslatabilityUnsupported,
		fmt.Sprintf("%s cannot express a %s relationship (it has %s)",
			v.targetDSL, strings.ToLower(stage.Op.String()), strings.Join(supported, ", ")),
		stage.Op.String())
}

// checkCoercion covers a target that cannot reinterpret an attribute's type.
func (v *validator) checkCoercion(stage *ir.CoercionStage, path string) {
	if v.target.Capabilities.AttributeCasts {
		return
	}
	v.report(stage, path, ir.TranslatabilityUnsupported,
		fmt.Sprintf("%s cannot reinterpret %q as %s; the cast has no equivalent",
			v.targetDSL, stage.Attribute, stage.TargetType),
		"coercion")
}

// checkArithmetic covers a target that cannot compute over whole result sets.
//
// TraceQL is the case: a span set is not a number, so "a / b" has no spelling.
// Every other language in the registry has arithmetic, which is why the
// capability defaults to true and this usually returns at once.
func (v *validator) checkArithmetic(stage ir.PipelineStage, path, op string) {
	if v.target.Capabilities.Arithmetic {
		return
	}
	v.report(stage, path, ir.TranslatabilityUnsupported,
		fmt.Sprintf("%s has no arithmetic between result sets, so %q cannot be written",
			v.targetDSL, op),
		op)
}

// checkFilter covers what a filter stage carries beyond its predicate.
//
// A comparison that yields 0 or 1 for every record is a different operation from
// one that drops the records failing it. A target without the modifier can still
// write the filter, so this is an approximation rather than a refusal — but the
// result set differs, and saying so is the point.
func (v *validator) checkFilter(stage *ir.FilterStage, path string) {
	if !stage.ReturnsBool {
		return
	}
	if v.target.Capabilities.BoolModifier {
		return
	}
	v.report(stage, path, ir.TranslatabilityPartial,
		fmt.Sprintf("%s has no bool modifier: the comparison will drop the records that fail it "+
			"rather than returning 0 or 1 for every one", v.targetDSL),
		"bool")
}

// checkFunction covers a function stage the target has no name for.
func (v *validator) checkFunction(stage *ir.FunctionStage, path string) {
	// Structural operations describe the IR rather than any DSL's vocabulary,
	// so no registry names them and every target can write them.
	if ir.StructuralFunctions[stage.Name] {
		return
	}
	if _, ok := v.target.FunctionByIRName(stage.Name); ok {
		return
	}
	v.report(stage, path, ir.TranslatabilityUnsupported,
		fmt.Sprintf("function %q is not available in %s", stage.Name, v.targetDSL),
		stage.Name)
}

// checkAggregation covers an aggregation the target cannot express, or can only
// express on the other axis.
//
// The lookup is by IR operator rather than by name, because the same operator
// has different spellings per DSL and per axis: IR SUM is PromQL's sum on the
// group axis and its sum_over_time on the temporal one.
func (v *validator) checkAggregation(stage *ir.AggregationStage, path string) {
	if _, ok := v.target.FunctionForAggregation(stage.Op, stage.Scope); ok {
		return
	}

	// The operator exists but only on the other axis. That is expressible, but
	// not identically, so it is an approximation rather than a refusal.
	if alternatives := v.target.FunctionsForAggOp(stage.Op); len(alternatives) > 0 {
		names := make([]string, 0, len(alternatives))
		scopes := make([]string, 0, len(alternatives))
		for _, fn := range alternatives {
			names = append(names, fn.Name)
			scopes = append(scopes, fn.AggScope.String())
		}
		v.report(stage, path, ir.TranslatabilityPartial,
			fmt.Sprintf("%q has a different aggregation scope in %s: the query aggregates over the %s axis, "+
				"and %s offers it only as %s (%s)",
				strings.ToLower(stage.Op.String()), v.targetDSL, strings.ToLower(stage.Scope.String()),
				v.targetDSL, strings.Join(names, ", "), strings.Join(scopes, ", ")),
			stage.Op.String())
		return
	}

	v.report(stage, path, ir.TranslatabilityUnsupported,
		fmt.Sprintf("aggregation %q is not available in %s",
			strings.ToLower(stage.Op.String()), v.targetDSL),
		stage.Op.String())
}

// checkAggregationOperand covers a target whose group aggregations reduce
// samples rather than records.
//
// LogQL is the case: "sum({app=\"x\"})" is not a LogQL query, because a stream
// selector yields log lines and sum reduces samples. Something has to turn one
// into the other first, and only a range aggregation can.
//
// The IR cannot see this from a single stage. An AggregationStage says "collapse
// across groups" and nothing about what it is collapsing, so checkAggregation
// finds COUNT on the group axis, sees that LogQL has count, and calls it FULL.
// The emitter then reaches the same stage, finds it applied to raw log lines,
// and drops it — leaving a report claiming full fidelity for a query that lost
// its aggregation. Reading the pipeline as a whole is what closes that gap.
func (v *validator) checkAggregationOperand(query *ir.Query, path string) {
	if !v.target.Capabilities.GroupAggregationNeedsSamples {
		return
	}
	// A query already reading samples needs nothing to convert: the target's own
	// signal is what its aggregations consume.
	if v.target.SupportsSignal(query.Signal) && query.Signal == ir.SignalMetric {
		return
	}

	rangeTypes := v.target.RangeOperandTypes()
	samples := false
	for i, stage := range query.Pipeline {
		switch node := stage.(type) {
		case *ir.AggregationStage:
			if node.Scope == ir.AggScopeTemporal {
				// A temporal aggregation reduces the window into samples, which
				// is exactly the conversion a group aggregation is waiting for.
				samples = true
				continue
			}
			if samples {
				continue
			}
			v.report(node, fmt.Sprintf("%s.Pipeline[%d].%s", path, i, ir.NodeTypeName(node)),
				ir.TranslatabilityUnsupported,
				fmt.Sprintf("%s aggregates across groups over samples, and nothing here has produced "+
					"any: %q would be applied to %s records, which %s cannot write",
					v.targetDSL, strings.ToLower(node.Op.String()),
					strings.ToLower(query.Signal.String()), v.targetDSL),
				node.Op.String())

		case *ir.FunctionStage:
			// A function the target declares as consuming a range is one of its
			// range aggregations written as a plain call, so it converts too.
			if fn, ok := v.target.FunctionByIRName(node.Name); ok && fn.ReadsRangeOperand(rangeTypes) {
				samples = true
			}
		}
	}
}

// checkOffset covers an offset the target has nowhere to put.
//
// LogQL attaches one to a range, and has no bare instant vector to attach it to,
// so a PromQL "x offset 1h" arrives with an offset and no range to carry it.
// Dropping it unremarked would present a query answered over the wrong period as
// a clean translation — a difference in results, not in spelling.
func (v *validator) checkOffset(query *ir.Query, path string) {
	if !v.target.Capabilities.OffsetNeedsRange {
		return
	}
	if query.Output == nil || query.Output.Window == nil {
		return
	}
	window := query.Output.Window
	if window.Offset.IsZero() || v.producesRange(query) {
		return
	}
	v.report(window, path+".Output.Window", ir.TranslatabilityUnsupported,
		fmt.Sprintf("%s attaches an offset to a range, and this query has none; the %s offset "+
			"cannot be written, so the result would cover the caller's own time range rather "+
			"than one shifted back by it", v.targetDSL, window.Offset),
		window.Offset.String())
}

// producesRange reports whether anything in the pipeline narrows the source to a
// window, which is what an offset and a group aggregation both need.
//
// A temporal aggregation does. So does a function the target declares as
// consuming a range — LogQL's bytes_rate has no IR aggregation operator but is
// still a range aggregation, and the registry says so through the argument type
// it takes.
func (v *validator) producesRange(query *ir.Query) bool {
	rangeTypes := v.target.RangeOperandTypes()
	for _, stage := range query.Pipeline {
		switch node := stage.(type) {
		case *ir.AggregationStage:
			if node.Scope == ir.AggScopeTemporal {
				return true
			}
		case *ir.FunctionStage:
			if fn, ok := v.target.FunctionByIRName(node.Name); ok && fn.ReadsRangeOperand(rangeTypes) {
				return true
			}
		}
	}
	return false
}

// checkJoin covers a target that cannot correlate two result sets.
func (v *validator) checkJoin(stage *ir.JoinStage, path string) {
	if !v.target.Capabilities.Joins {
		v.report(stage, path, ir.TranslatabilityUnsupported,
			fmt.Sprintf("joins are not supported in %s", v.targetDSL), stage.JoinType.String())
	} else if !v.target.Capabilities.SupportsJoinType(stage.JoinType) {
		supported := make([]string, 0, len(v.target.Capabilities.JoinTypes))
		for _, jt := range v.target.Capabilities.JoinTypes {
			supported = append(supported, jt.String())
		}
		v.report(stage, path, ir.TranslatabilityPartial,
			fmt.Sprintf("%s cannot express a %s join (it supports %s)",
				v.targetDSL, stage.JoinType, strings.Join(supported, ", ")),
			stage.JoinType.String())
	}

	// The joined query is a query in its own right, and the traversal descends
	// into it on its own.
}

// checkMatcher covers an operator the target cannot spell, and warns about
// regular expressions.
//
// target is the node the verdict lands on, which is not always the matcher: a
// matcher inside a predicate is an implementation detail of that predicate, and
// naming the predicate is what makes a report readable.
func (v *validator) checkMatcher(target ir.Node, matcher *ir.LabelMatcher, path string) {
	// A predicate over the log body has nowhere to land in a target that does
	// not query logs — there is no field for it to address, whatever operator it
	// uses. Saying PARTIAL here would promise an approximation the emitter
	// cannot deliver.
	if matcher.Key == ir.FieldBody && !v.target.SupportsSignal(ir.SignalLog) {
		v.report(target, path, ir.TranslatabilityUnsupported,
			fmt.Sprintf("%s has no log body to filter on; the filter cannot be written at all",
				v.targetDSL),
			matcher.Op.String())
		return
	}

	// A scoped attribute key — "span.http.status_code" — has to be folded into
	// one flat name for a target with a single label namespace, because neither
	// PromQL nor LogQL admits a dot in a label name. The rewrite is faithful in
	// meaning but changes what the query asks for by name, and a reader
	// comparing the two queries side by side deserves to be told.
	if strings.Contains(matcher.Key, ".") && !v.target.Capabilities.ScopedAttributes {
		if safe, changed := ir.FlatLabelName(matcher.Key); changed {
			v.report(target, path, ir.TranslatabilityPartial,
				fmt.Sprintf("%s has one flat label namespace and admits no dot in a name, so the "+
					"scoped attribute %q becomes %q", v.targetDSL, matcher.Key, safe),
				matcher.Key)
		}
	}

	if !v.target.SupportsIROp(matcher.Op) {
		// Containment is the one predicate with a faithful fallback: an escaped
		// pattern wrapped in .* says the same thing, so a target without a
		// containment operator approximates rather than refuses.
		if matcher.Op.IsContainment() {
			v.report(target, path, ir.TranslatabilityPartial,
				fmt.Sprintf("%s has no containment operator; the test will be written as a "+
					"regular expression over the escaped text", v.targetDSL),
				matcher.Op.String())
			return
		}
		v.report(target, path, ir.TranslatabilityUnsupported,
			fmt.Sprintf("%s has no operator for %s", v.targetDSL, matcher.Op),
			matcher.Op.String())
		return
	}

	// Regular expressions are the classic silent divergence between these
	// languages: both use RE2, but they differ in what they anchor and in how a
	// pattern must be escaped once it is embedded in the target's syntax. This
	// is a warning rather than a refusal, and it only fires across languages —
	// a query translated back into its own DSL has no dialect to cross.
	if v.sourceDSL == "" || v.sourceDSL == v.targetDSL {
		return
	}
	if matcher.Op == ir.MatchRegex || matcher.Op == ir.MatchNotRegex {
		v.report(target, path, ir.TranslatabilityPartial,
			fmt.Sprintf("regex dialect may differ between %s and %s; "+
				"anchoring and escaping conventions are not identical", v.sourceDSL, v.targetDSL),
			matcher.Op.String())
	}
}

// checkWindow covers QLS §Time Based Windowing: whether the target can express
// the window's step and its alignment.
func (v *validator) checkWindow(w *ir.Window, path string) {
	// A target with no window syntax at all cannot narrow a query in time from
	// inside the query text. TraceQL is the case: Tempo takes start and end as
	// request parameters, so a translated query silently reads whatever range
	// the caller asks for — a difference in results, not just in spelling.
	if !v.target.Capabilities.TemporalWindows {
		if !w.Step.IsZero() || !w.Offset.IsZero() {
			v.report(w, path, ir.TranslatabilityUnsupported,
				fmt.Sprintf("%s has no range selector: the time window has to travel outside the "+
					"query, so %s cannot be written into it", v.targetDSL, w.Step),
				w.Step.String())
		}
		return
	}

	if reason, ok := durationExpressible(v.target, w.Step); !ok {
		v.report(w, path, ir.TranslatabilityPartial,
			fmt.Sprintf("window step %s is not expressible in %s: %s", w.Step, v.targetDSL, reason),
			w.Step.String())
	}
	if reason, ok := durationExpressible(v.target, w.Offset); !ok {
		v.report(w, path, ir.TranslatabilityPartial,
			fmt.Sprintf("window offset %s is not expressible in %s: %s", w.Offset, v.targetDSL, reason),
			w.Offset.String())
	}

	if !v.target.Capabilities.SupportsWindowAlignment(w.Alignment) {
		supported := make([]string, 0, len(v.target.Capabilities.WindowAlignments))
		for _, a := range v.target.Capabilities.WindowAlignments {
			supported = append(supported, a.String())
		}
		if len(supported) == 0 {
			supported = append(supported, ir.WindowUTCNormalized.String())
		}
		v.report(w, path, ir.TranslatabilityPartial,
			fmt.Sprintf("%s cannot align windows as %s (it supports %s); "+
				"bucket boundaries may fall elsewhere",
				v.targetDSL, w.Alignment, strings.Join(supported, ", ")),
			w.Alignment.String())
	}
}

// durationExpressible reports whether a target can write a duration.
//
// Every DSL in the MVP shares PromQL's duration units, so this passes
// everything through. It exists as the seam for a DSL that cannot: one with a
// coarser smallest unit than the interval asks for, or with no way to write a
// negative offset. Adding such a language means extending this and the
// capability it reads, not the callers.
func durationExpressible(def *registry.DSLDefinition, interval ir.Interval) (string, bool) {
	_ = def
	if interval.IsZero() {
		return "", true
	}
	return "", true
}
