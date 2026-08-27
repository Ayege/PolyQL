package emitter

// FunctionApproximation suggests a usable alternative function when the target language
// doesn't support the source function. It trades precision for expressivity.
type FunctionApproximation struct {
	// SourceFunction is the IR function name that has no direct equivalent
	SourceFunction string
	// ApproximationName is the target language function name to use instead
	ApproximationName string
	// Explanation describes the trade-off (why this approximation is used)
	Explanation string
	// Severity indicates whether this is a good approximation (PARTIAL) or
	// a stretch (UNSUPPORTED - try anyway)
	Severity string
}

// FunctionApproximations maps unsupported functions to their best approximations
// across different target languages.
var FunctionApproximations = map[string][]FunctionApproximation{
	// PromQL functions with LogQL approximations
	"logql": {
		{
			SourceFunction:    "histogram_quantile",
			ApproximationName: "max",
			Explanation: "histogram_quantile approximated as max - LogQL has no histogram " +
				"functions, so the maximum value is used as a rough approximation of the quantile",
			Severity: "PARTIAL",
		},
		{
			SourceFunction:    "histogram_fraction",
			ApproximationName: "max",
			Explanation: "histogram_fraction has no LogQL equivalent - max is used as approximation",
			Severity: "PARTIAL",
		},
	},
	// PromQL functions with TraceQL approximations
	"traceql": {
		{
			SourceFunction:    "histogram_quantile",
			ApproximationName: "max",
			Explanation: "histogram_quantile approximated as max - spans are events, not time series",
			Severity: "PARTIAL",
		},
	},
	// LogQL functions with PromQL approximations
	"promql": {
		// LogQL-specific functions that don't exist in PromQL
		// Most log processing functions have no metric equivalents
	},
}

// GetFunctionApproximation finds an alternative function to use when the target
// language doesn't support the source function.
func GetFunctionApproximation(targetDSL string, sourceFunc string) *FunctionApproximation {
	approximations, ok := FunctionApproximations[targetDSL]
	if !ok {
		return nil
	}

	for i := range approximations {
		if approximations[i].SourceFunction == sourceFunc {
			return &approximations[i]
		}
	}
	return nil
}
