package compiler

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName identifies this instrumentation in a trace.
const tracerName = "github.com/polyql/polyql/pkg/compiler"

// This file holds the only OpenTelemetry code in the library, and it imports
// the API rather than the SDK.
//
// That split is the whole reason instrumenting here is safe. Without a provider
// configured, otel.Tracer returns a no-op whose Start allocates nothing and
// whose spans do nothing, so a caller embedding PolyQL as a library pays no
// runtime cost and needs no OTel setup. The SDK — the exporters, the batching,
// the gRPC or HTTP transport — is pulled in only by a command that chooses to,
// through pkg/telemetry.
//
// A translation is the unit worth a span. It is the boundary a user thinks in,
// it is where the fidelity score exists, and it is coarse enough that tracing
// one costs nothing next to running it.

// startSpan opens a span for a translation and returns a function to close it
// with the outcome.
//
// The attributes are set in two passes because the interesting ones — the score,
// what was lost — do not exist until the work is done.
func startSpan(ctx context.Context, req Request) (context.Context, func(*Result, error)) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "compiler.Translate",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("polyql.source_dsl", req.SourceDSL),
			attribute.String("polyql.target_dsl", req.TargetDSL),
		),
	)

	return ctx, func(result *Result, err error) {
		defer span.End()

		if err != nil {
			// A translation that could not be performed at all, as distinct
			// from one that lost something — which is a successful span
			// carrying a low score.
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return
		}
		if result == nil || result.Report == nil {
			return
		}

		report := result.Report
		span.SetAttributes(
			attribute.Int("polyql.ir_nodes", report.TotalNodes),
			attribute.Float64("polyql.fidelity_score", report.FidelityScore),
			attribute.Int("polyql.full_count", report.FullCount),
			attribute.Int("polyql.partial_count", report.PartialCount),
			attribute.Int("polyql.unsupported_count", report.UnsupportedCount),
			// A query the target cannot run at all is the thing an operator
			// most wants to filter a trace on, and it is not derivable from
			// the score — the two answer different questions.
			attribute.Bool("polyql.signal_mismatch", report.SignalMismatch != nil),
		)

		// The span status stays OK: the translation succeeded. Losing a
		// construct is a result, not a failure, and marking it an error would
		// make every honest report look like a broken request.
		span.SetStatus(codes.Ok, "")
	}
}
