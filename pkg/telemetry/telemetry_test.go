package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// TestSetupWithoutAnEndpointInstallsNothing covers the path every ordinary run
// takes. Installing a provider that exports nowhere would cost every CLI
// invocation a batcher and a background goroutine for no benefit.
func TestSetupWithoutAnEndpointInstallsNothing(t *testing.T) {
	t.Setenv(EndpointEnv, "")

	before := otel.GetTracerProvider()
	shutdown, err := Setup(context.Background(), Config{ServiceName: "polyql"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Error("no endpoint was configured, so the provider should be untouched")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutting down a no-op should not fail: %v", err)
	}
}

// TestSetupReadsTheStandardEnvironmentVariable covers picking up an endpoint
// already set for everything else in a container.
func TestSetupReadsTheStandardEnvironmentVariable(t *testing.T) {
	spans := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case spans <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(EndpointEnv, server.URL)

	before := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(before) })

	shutdown, err := Setup(context.Background(), Config{ServiceName: "polyql", Insecure: true})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if otel.GetTracerProvider() == before {
		t.Fatal("an endpoint was configured, so a provider should be installed")
	}

	_, span := otel.Tracer("test").Start(context.Background(), "unit")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	select {
	case <-spans:
	case <-time.After(3 * time.Second):
		t.Error("shutdown should have flushed the span to the collector")
	}
}

// TestExplicitEndpointBeatsTheEnvironment covers precedence.
func TestExplicitEndpointBeatsTheEnvironment(t *testing.T) {
	var mu sync.Mutex
	var reached []string
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			reached = append(reached, name)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}
	fromEnv := httptest.NewServer(handler("env"))
	defer fromEnv.Close()
	fromFlag := httptest.NewServer(handler("flag"))
	defer fromFlag.Close()

	t.Setenv(EndpointEnv, fromEnv.URL)

	before := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(before) })

	shutdown, err := Setup(context.Background(), Config{
		Endpoint: fromFlag.URL, ServiceName: "polyql", Insecure: true,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "unit")
	span.End()
	_ = shutdown(context.Background())

	mu.Lock()
	defer mu.Unlock()
	for _, name := range reached {
		if name == "env" {
			t.Error("the explicit endpoint should win over the environment")
		}
	}
}

// TestErrorsAreRoutedToTheGivenWriter covers keeping the SDK's own failures out
// of the standard logger, so a command's stderr stays readable next to a piped
// stdout.
func TestErrorsAreRoutedToTheGivenWriter(t *testing.T) {
	t.Setenv(EndpointEnv, "")

	var log bytes.Buffer
	before := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(before) })

	// Port 9 is the discard port: nothing listens, so export always fails.
	shutdown, err := Setup(context.Background(), Config{
		Endpoint: "http://127.0.0.1:9", ServiceName: "polyql",
		Insecure: true, ErrorLog: &log,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_, span := otel.Tracer("test").Start(context.Background(), "unit")
	span.End()
	_ = shutdown(context.Background())

	if got := log.String(); !strings.Contains(got, "polyql: tracing:") {
		t.Errorf("the export failure should be routed and attributed, got %q", got)
	}
}

// TestANilErrorLogDiscards covers the embedding case: a failure to export a
// trace is never a reason to write to a stream the caller did not offer.
func TestANilErrorLogDiscards(t *testing.T) {
	t.Setenv(EndpointEnv, "")

	before := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(before) })

	shutdown, err := Setup(context.Background(), Config{
		Endpoint: "http://127.0.0.1:9", ServiceName: "polyql", Insecure: true,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "unit")
	span.End()
	// The assertion is that this neither panics nor blocks.
	_ = shutdown(context.Background())
}
