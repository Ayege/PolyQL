package emitter

import (
	"fmt"
	"strings"
)

// ErrorContext provides helpful explanations for translation failures
type ErrorContext struct {
	// The unsupported construct
	Construct string
	// The source language
	SourceDSL string
	// The target language
	TargetDSL string
	// Why it can't be translated
	Reason string
	// What to try instead (if applicable)
	Suggestion string
	// The category of incompatibility
	Category string // "SEMANTIC", "SYNTAX", "STRUCTURAL", "LANGUAGE_SPECIFIC"
}

// ErrorMessages provides detailed explanations for common translation failures
var ErrorMessages = map[string]ErrorContext{
	// Signal mismatches (semantic gaps)
	"metric_to_span": {
		Construct:  "Metric aggregation",
		Reason:     "Metrics are named series of values; Spans are event instances in traces",
		Suggestion: "Use a different approach: spans don't have named metrics, only attributes",
		Category:   "SEMANTIC",
	},
	"log_to_metric": {
		Construct:  "Log line parsing",
		Reason:     "Log lines are unstructured text; Metrics are structured numeric values",
		Suggestion: "Pre-process logs to extract structured data before query translation",
		Category:   "SEMANTIC",
	},
	"span_to_metric": {
		Construct:  "Span attributes",
		Reason:     "Spans contain trace context that metrics don't capture",
		Suggestion: "Use metrics that approximate the same information (e.g., request count)",
		Category:   "SEMANTIC",
	},

	// Temporal aggregations (axis mismatch)
	"temporal_to_span": {
		Construct:  "Temporal aggregation (rate, avg over time)",
		Reason:     "PromQL aggregates over TIME; TraceQL aggregates over SPAN COUNT",
		Suggestion: "These are fundamentally different axes - use span count operations instead",
		Category:   "SEMANTIC",
	},

	// Structural features
	"joins_unsupported": {
		Construct:  "Join operation",
		Reason:     "Joins require matching logic; LogQL and TraceQL use different span/stream models",
		Suggestion: "Consider using separate queries and correlating results externally",
		Category:   "STRUCTURAL",
	},
	"subquery_unsupported": {
		Construct:  "Subquery",
		Reason:     "TraceQL has no subquery syntax for nested expressions",
		Suggestion: "Flatten the query or run as separate queries",
		Category:   "STRUCTURAL",
	},

	// Syntax differences
	"offset_without_range": {
		Construct:  "Offset without range",
		Reason:     "PromQL offset only works with range queries, not instant vectors",
		Suggestion: "Add a minimal range like [1m] to the selector to enable the offset",
		Category:   "SYNTAX",
	},
	"without_clause": {
		Construct:  "without() grouping clause",
		Reason:     "TraceQL only supports by() grouping, not without()",
		Suggestion: "Rewrite to list the attributes to group BY instead of those to exclude",
		Category:   "SYNTAX",
	},

	// Language-specific limitations
	"comparison_in_selector": {
		Construct:  "Comparison operators (>, <, >=, <=) in label selector",
		Reason:     "PromQL selectors only support equality (=, !=) and regex (=~, !~)",
		Suggestion: "Use equality matches or move comparisons to filter operations",
		Category:   "LANGUAGE_SPECIFIC",
	},
	"line_filter_after_parser": {
		Construct:  "Line filter after parser",
		Reason:     "LogQL can't reorder parsers and line filters in the pipeline",
		Suggestion: "Restructure query to apply line filters before or inline with parsers",
		Category:   "LANGUAGE_SPECIFIC",
	},
}

// MakeDetailedError creates a helpful error message for translation failures
func MakeDetailedError(construct, sourceDSL, targetDSL string) string {
	key := strings.ToLower(strings.ReplaceAll(construct, " ", "_"))

	// Look for specific error context
	if ctx, ok := ErrorMessages[key]; ok {
		return fmt.Sprintf(
			"Cannot translate %s from %s to %s\n"+
				"  Reason: %s\n"+
				"  Suggestion: %s\n"+
				"  Category: %s (incompatibility type)",
			construct, strings.ToUpper(sourceDSL), strings.ToUpper(targetDSL),
			ctx.Reason, ctx.Suggestion, ctx.Category)
	}

	// Generic fallback for unknown constructs
	return fmt.Sprintf(
		"Cannot translate %q from %s to %s (reason not documented)",
		construct, strings.ToUpper(sourceDSL), strings.ToUpper(targetDSL))
}

// SignalMismatchExplanation explains why signal types don't match
func SignalMismatchExplanation(sourceSignal, targetSignal string) string {
	return fmt.Sprintf(
		"SIGNAL MISMATCH: This query operates on %s data but your backend expects %s data. "+
			"The translated query is syntactically valid but semantically incompatible.\n"+
			"Query will execute but may return unexpected or empty results.",
		sourceSignal, targetSignal)
}
