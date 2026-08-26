// Package telemetry configures OpenTelemetry export for a PolyQL command.
//
// It is deliberately the only package that imports the OTel SDK. The library
// instruments itself against the OTel API, which is a no-op until a provider is
// installed, so a caller embedding PolyQL pays nothing and configures nothing.
// Installing a provider is an application's decision, and this is where an
// application makes it.
//
// Nothing here is required for PolyQL to work. With no endpoint configured,
// Setup installs no provider and returns a shutdown that does nothing, which is
// the path every ordinary CLI run takes.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// EndpointEnv is the standard variable an OTLP endpoint may come from, so that
// PolyQL picks up an endpoint already set for everything else in a container
// without a flag of its own.
const EndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// shutdownTimeout bounds the flush at exit. A CLI that hangs on a dead collector
// is worse than one that drops a span, so this is short on purpose.
const shutdownTimeout = 5 * time.Second

// Config is what a command knows about exporting traces.
type Config struct {
	// Endpoint is an OTLP/HTTP collector address. An empty value disables
	// export entirely, which is the default.
	Endpoint string
	// ServiceName names this process in a trace.
	ServiceName string
	// Version is the build version, recorded on the resource.
	Version string
	// Insecure sends over plain HTTP rather than TLS. A collector on localhost
	// usually has no certificate.
	Insecure bool
	// ErrorLog is where the SDK's own failures are written — an unreachable
	// collector, a rejected batch.
	//
	// It exists because the SDK otherwise writes them through the standard
	// logger, which lands on stderr unprefixed and unattributed. This command
	// keeps stderr clear enough to be read next to a piped stdout, so a stray
	// line from a dependency is worth routing. A nil value discards them: a
	// failure to export a trace is never a reason to interrupt a translation.
	ErrorLog io.Writer
}

// Shutdown flushes and stops the provider. It is safe to call on a Config that
// installed nothing.
type Shutdown func(context.Context) error

// Setup installs a tracer provider when an endpoint is configured, and returns a
// function to flush it.
//
// An endpoint given explicitly wins over the environment. When neither names
// one, nothing is installed and the returned Shutdown does nothing — the OTel
// API's no-op tracer stays in place, which is what every run without a collector
// wants.
func Setup(ctx context.Context, cfg Config) (Shutdown, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv(EndpointEnv))
	}
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	options := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if cfg.Insecure {
		options = append(options, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: connecting to the OTLP collector at %s: %w", endpoint, err)
	}

	// Schemaless on purpose. Merging a resource that pins a schema version
	// against resource.Default, which pins the SDK's own, fails the moment the
	// two differ — and they differ on every SDK upgrade that bumps semconv.
	// These two attributes need no schema to be understood.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", cfg.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: describing this service: %w", err)
	}

	// The SDK reports its own failures through a global handler. Left alone it
	// writes to the standard logger; routed here it stays with everything else
	// this command says, or is dropped when the caller wants silence.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if cfg.ErrorLog == nil || err == nil {
			return
		}
		fmt.Fprintf(cfg.ErrorLog, "polyql: tracing: %s\n", err)
	}))

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	// Context propagation matters for the proxy, where a translation is one hop
	// in someone else's trace rather than the whole of it.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		return provider.Shutdown(ctx)
	}, nil
}
