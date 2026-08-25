package validator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/ir"
)

// stageRank is a stage's position in the order a pipeline-ordered DSL requires.
//
// LogQL fixes this order because each step depends on the last: a line filter
// reads the raw line, a parser turns that line into labels, a label filter can
// only name a label once the parser has produced it, formatters rewrite what the
// earlier steps produced, and the unwrap coerces a label into the number the
// aggregation then reduces.
type stageRank int

const (
	rankLineFilter stageRank = iota
	rankParser
	rankLabelFilter
	rankFormatter
	rankUnwrap
	rankAggregation
	// rankValueFilter is a filter on the aggregated value rather than on an
	// attribute. It can only run once an aggregation has produced that value,
	// so it sorts after one — moving it earlier would leave it filtering on
	// something that does not exist yet.
	rankValueFilter
	rankJoin
	rankOther
)

var stageRankNames = map[stageRank]string{
	rankLineFilter:  "line filter",
	rankParser:      "parser",
	rankLabelFilter: "label filter",
	rankFormatter:   "formatter",
	rankUnwrap:      "unwrap",
	rankAggregation: "aggregation",
	rankValueFilter: "value filter",
	rankJoin:        "join",
	rankOther:       "stage",
}

func (r stageRank) String() string {
	if s, ok := stageRankNames[r]; ok {
		return s
	}
	return fmt.Sprintf("stageRank(%d)", int(r))
}

// parserIRNames are the IR function names that turn a log line into labels.
var parserIRNames = map[string]bool{
	"parse_json":    true,
	"parse_logfmt":  true,
	"parse_regexp":  true,
	"parse_pattern": true,
	"parse_unpack":  true,
}

// formatterIRNames are the IR function names that rewrite the line or its
// labels.
var formatterIRNames = map[string]bool{
	"line_format":  true,
	"label_format": true,
	"drop_labels":  true,
	"keep_labels":  true,
	"decolorize":   true,
}

// changesLabels reports whether a stage alters which attributes exist. Anything
// reading an attribute — a label filter, an unwrap — sees a different world on
// either side of one of these.
func changesLabels(name string) bool {
	switch {
	case parserIRNames[name]:
		return true
	case name == "label_format", name == "drop_labels", name == "keep_labels":
		return true
	}
	return false
}

// changesLine reports whether a stage rewrites the log line itself. A parser
// does not: it reads the line and adds attributes, leaving the text alone, so a
// line filter may cross one freely. A line_format or a decolorize does.
func changesLine(name string) bool {
	return name == "line_format" || name == "decolorize"
}

// rankOf classifies a stage.
//
// A filter is ranked by what it addresses: one over the log body is a line
// filter and reads the raw line, while one over an attribute is a label filter
// and needs whatever produced that attribute to have run already.
func rankOf(stage ir.PipelineStage) stageRank {
	switch node := stage.(type) {
	case *ir.FilterStage:
		switch {
		case predicateAddressesBody(node.Predicate):
			return rankLineFilter
		case predicateAddressesValue(node.Predicate):
			return rankValueFilter
		}
		return rankLabelFilter
	case *ir.FunctionStage:
		switch {
		case parserIRNames[node.Name]:
			return rankParser
		case formatterIRNames[node.Name]:
			return rankFormatter
		case node.Name == "unwrap":
			return rankUnwrap
		}
		return rankOther
	case *ir.AggregationStage:
		return rankAggregation
	case *ir.JoinStage:
		return rankJoin
	}
	return rankOther
}

// predicateAddressesValue reports whether any leaf tests the aggregated value,
// which only exists once an aggregation has produced it.
func predicateAddressesValue(predicate ir.Predicate) bool {
	switch node := predicate.(type) {
	case *ir.MatchPredicate:
		return node.Matcher != nil && node.Matcher.Key == ir.FieldValue
	case *ir.LogicalPredicate:
		for _, operand := range node.Operands {
			if predicateAddressesValue(operand) {
				return true
			}
		}
	}
	return false
}

// predicateAddressesBody reports whether every leaf of a predicate tests the log
// body. A predicate mixing the body with attributes is not a line filter, since
// it cannot run before the attributes exist.
func predicateAddressesBody(predicate ir.Predicate) bool {
	switch node := predicate.(type) {
	case *ir.MatchPredicate:
		return node.Matcher != nil && node.Matcher.Key == ir.FieldBody
	case *ir.LogicalPredicate:
		if len(node.Operands) == 0 {
			return false
		}
		for _, operand := range node.Operands {
			if !predicateAddressesBody(operand) {
				return false
			}
		}
		return true
	}
	return false
}

// checkPipelineOrder reorders a pipeline for a target whose syntax fixes the
// order of stages, and reports when doing so may not preserve meaning.
//
// A reorder that only moves stages within their own kind, or that moves a stage
// past ones it cannot interact with, is a transparent fix and leaves the flags
// alone. A reorder that carries a filter across a parser or a formatter is not:
// what that filter sees changes, so the affected stages are flagged PARTIAL and
// the author is asked to confirm. The pipeline is still reordered — an emitter
// targeting LogQL cannot write the original order at all — but the report says
// so rather than presenting the result as equivalent.
func (v *validator) checkPipelineOrder(query *ir.Query, path string) {
	if !v.target.Capabilities.PipelineOrdered || len(query.Pipeline) < 2 {
		return
	}

	original := make([]ir.PipelineStage, len(query.Pipeline))
	copy(original, query.Pipeline)

	ranks := make([]stageRank, len(original))
	for i, stage := range original {
		ranks[i] = rankOf(stage)
	}
	if sort.SliceIsSorted(ranks, func(i, j int) bool { return ranks[i] < ranks[j] }) {
		return
	}

	// A stable sort keeps stages of the same kind in the order they were
	// written, which matters: two line filters compose, and swapping them would
	// be a change the author never asked for.
	indices := make([]int, len(original))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool { return ranks[indices[a]] < ranks[indices[b]] })

	reordered := make([]ir.PipelineStage, len(original))
	for newPos, oldPos := range indices {
		reordered[newPos] = original[oldPos]
	}
	query.Pipeline = reordered

	// Work out which moves could have changed meaning. A filter that crossed a
	// parser or a formatter now sees a different set of attributes.
	unsafe := crossedProducer(original, indices, ranks)

	for newPos, oldPos := range indices {
		if newPos == oldPos {
			continue
		}
		stage := reordered[newPos]
		stagePath := fmt.Sprintf("%s.Pipeline[%d]", path, newPos)
		if !unsafe[oldPos] {
			continue
		}
		v.report(stage, stagePath, ir.TranslatabilityPartial,
			fmt.Sprintf("pipeline stage order adjusted for %s: this %s moved from position %d to %d, "+
				"crossing a stage that changes which attributes exist; verify semantic equivalence",
				v.targetDSL, ranks[oldPos], oldPos, newPos),
			ranks[oldPos].String())
	}
}

// crossedProducer marks the stages whose move changed what they can see.
//
// Not every reorder does. A stage only cares about crossing another if that
// other one rewrites the thing it reads: a label filter and an unwrap read
// attributes, so they are affected by anything that adds or removes attributes;
// a line filter reads the raw line, so it is affected only by something that
// rewrites the line. A line filter may therefore move freely across a parser,
// which reads the line without changing it — and saying otherwise would flag a
// transparent fix as a possible semantic change.
func crossedProducer(original []ir.PipelineStage, indices []int, ranks []stageRank) map[int]bool {
	newPosition := make(map[int]int, len(indices))
	for newPos, oldPos := range indices {
		newPosition[oldPos] = newPos
	}

	// Classify what each stage rewrites.
	rewritesLabels := make(map[int]bool)
	rewritesLine := make(map[int]bool)
	for i, stage := range original {
		fn, ok := stage.(*ir.FunctionStage)
		if !ok {
			continue
		}
		rewritesLabels[i] = changesLabels(fn.Name)
		rewritesLine[i] = changesLine(fn.Name)
	}

	unsafe := make(map[int]bool)
	for oldPos, rank := range ranks {
		var affectedBy map[int]bool
		switch rank {
		case rankLabelFilter, rankUnwrap:
			affectedBy = rewritesLabels
		case rankLineFilter:
			affectedBy = rewritesLine
		default:
			continue
		}

		for producer := range affectedBy {
			if !affectedBy[producer] {
				continue
			}
			wasBefore := oldPos < producer
			isBefore := newPosition[oldPos] < newPosition[producer]
			if wasBefore != isBefore {
				unsafe[oldPos] = true
				unsafe[producer] = true
			}
		}
	}
	return unsafe
}

// describeOrder renders a pipeline's stage kinds, for error messages and tests.
func describeOrder(pipeline ir.Pipeline) string {
	parts := make([]string, 0, len(pipeline))
	for _, stage := range pipeline {
		parts = append(parts, rankOf(stage).String())
	}
	return strings.Join(parts, " -> ")
}
