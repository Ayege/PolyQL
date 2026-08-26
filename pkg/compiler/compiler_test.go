package compiler_test

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/polyql/polyql/pkg/compiler"
	"github.com/polyql/polyql/pkg/registry"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	return reg
}

// TestTranslate covers the facade every caller now drives, so that the CLI, the
// proxy and the dashboard translator cannot diverge in what a translation means.
func TestTranslate(t *testing.T) {
	reg := testRegistry(t)

	t.Run("a faithful translation reports no loss", func(t *testing.T) {
		result, err := compiler.Translate(context.Background(), compiler.Request{
			SourceDSL: "promql", TargetDSL: "promql",
			Query: `rate(http_requests_total[5m])`, Registry: reg,
		})
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		if got := result.Output; got != `rate(http_requests_total[5m])` {
			t.Errorf("Output = %q", got)
		}
		if !result.Lossless() {
			t.Errorf("into its own language nothing is lost: %+v", result.Report)
		}
		if !result.Complete() {
			t.Error("Complete should hold whenever Lossless does")
		}
	})

	t.Run("a lossy translation is a result, not an error", func(t *testing.T) {
		result, err := compiler.Translate(context.Background(), compiler.Request{
			SourceDSL: "promql", TargetDSL: "logql",
			Query: `histogram_quantile(0.99, x)`, Registry: reg,
		})
		if err != nil {
			t.Fatalf("a construct the target cannot express is not an error: %v", err)
		}
		if result.Complete() {
			t.Error("histogram_quantile has no LogQL form; Complete should be false")
		}
		if len(result.Notes) == 0 {
			t.Error("the emitter's notes should be lifted out")
		}
		// Text is what a person copies; Output is the query alone.
		if !strings.HasPrefix(result.Text, "#") {
			t.Errorf("Text should carry the note comments: %q", result.Text)
		}
		if strings.HasPrefix(result.Output, "#") {
			t.Errorf("Output should be the query alone: %q", result.Output)
		}
	})

	t.Run("an approximation is complete but not lossless", func(t *testing.T) {
		// The two predicates exist to be different: an approximation was
		// written and explained, an unsupported construct was not written.
		result, err := compiler.Translate(context.Background(), compiler.Request{
			SourceDSL: "traceql", TargetDSL: "logql",
			Query: `{span.http.status_code = 500}`, Registry: reg,
		})
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		if !result.Complete() {
			t.Errorf("every construct was written: %s", result.Report.Summary)
		}
		if result.Lossless() {
			t.Error("the scoped attribute was renamed, so this is not lossless")
		}
	})

	t.Run("a query that will not parse is an error", func(t *testing.T) {
		_, err := compiler.Translate(context.Background(), compiler.Request{
			SourceDSL: "promql", TargetDSL: "logql",
			Query: `rate(unclosed`, Registry: reg,
		})
		if err == nil {
			t.Fatal("expected a parse error")
		}
	})

	t.Run("bad inputs are refused", func(t *testing.T) {
		cases := []struct {
			name string
			req  compiler.Request
			want string
		}{
			{"no registry", compiler.Request{SourceDSL: "promql", TargetDSL: "logql", Query: "up"}, "registry"},
			{"no source", compiler.Request{TargetDSL: "logql", Query: "up", Registry: reg}, "language"},
			{"no target", compiler.Request{SourceDSL: "promql", Query: "up", Registry: reg}, "language"},
			{"unknown source", compiler.Request{SourceDSL: "nosuch", TargetDSL: "logql", Query: "up", Registry: reg}, "unknown source"},
			{"unknown target", compiler.Request{SourceDSL: "promql", TargetDSL: "nosuch", Query: "up", Registry: reg}, "unknown target"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				_, err := compiler.Translate(context.Background(), c.req)
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("error %q should mention %q", err, c.want)
				}
			})
		}
	})

	t.Run("language names are case-insensitive", func(t *testing.T) {
		result, err := compiler.Translate(context.Background(), compiler.Request{
			SourceDSL: "  PromQL ", TargetDSL: "LogQL", Query: "up", Registry: reg,
		})
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		if result.SourceDSL != "promql" || result.TargetDSL != "logql" {
			t.Errorf("names should be normalised: %s → %s", result.SourceDSL, result.TargetDSL)
		}
	})
}

// TestSplitNotes covers the split between what the emitter could not express and
// the query itself.
func TestSplitNotes(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantNotes int
		wantQuery string
	}{
		{"no notes", "up", 0, "up"},
		{"one note", "# lost something\nup", 1, "up"},
		{"several", "# a\n# b\nup", 2, "up"},
		{"notes only", "# a\n# b\n", 2, ""},
		{"a multi-line query keeps its lines", "# a\nfoo\nbar", 1, "foo\nbar"},
		{"empty", "", 0, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notes, query := compiler.SplitNotes(c.text)
			if len(notes) != c.wantNotes {
				t.Errorf("got %d notes, want %d: %v", len(notes), c.wantNotes, notes)
			}
			if query != c.wantQuery {
				t.Errorf("query = %q, want %q", query, c.wantQuery)
			}
		})
	}
}

// TestTranslateEmitsASpan covers the instrumentation, using the SDK's own
// in-memory recorder rather than a collector.
//
// The attributes are the point: a score with no source and target on it cannot
// be grouped, and a trace nobody can group is a trace nobody reads.
func TestTranslateEmitsASpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	_, err := compiler.Translate(context.Background(), compiler.Request{
		SourceDSL: "promql", TargetDSL: "logql",
		Query: `histogram_quantile(0.99, x)`, Registry: testRegistry(t),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want one per translation", len(spans))
	}
	span := spans[0]
	if got := span.Name(); got != "compiler.Translate" {
		t.Errorf("span name = %q", got)
	}

	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range span.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	for _, want := range []attribute.Key{
		"polyql.source_dsl", "polyql.target_dsl", "polyql.ir_nodes",
		"polyql.fidelity_score", "polyql.full_count", "polyql.partial_count",
		"polyql.unsupported_count", "polyql.signal_mismatch",
	} {
		if _, ok := attrs[want]; !ok {
			t.Errorf("the span carries no %q", want)
		}
	}
	if got := attrs["polyql.source_dsl"].AsString(); got != "promql" {
		t.Errorf("source_dsl = %q", got)
	}
	if attrs["polyql.unsupported_count"].AsInt64() == 0 {
		t.Error("histogram_quantile is unsupported in LogQL; the count should say so")
	}

	// A lossy translation is a successful span. Marking it an error would make
	// every honest report look like a broken request.
	if got := span.Status().Code.String(); got != "Ok" {
		t.Errorf("status = %s, want Ok: losing a construct is a result, not a failure", got)
	}
}

// TestTranslateRecordsAnErrorOnTheSpan covers the other direction: a translation
// that could not run at all.
func TestTranslateRecordsAnErrorOnTheSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	_, err := compiler.Translate(context.Background(), compiler.Request{
		SourceDSL: "promql", TargetDSL: "logql",
		Query: `rate(unclosed`, Registry: testRegistry(t),
	})
	if err == nil {
		t.Fatal("expected a parse error")
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want one", len(spans))
	}
	if got := spans[0].Status().Code.String(); got != "Error" {
		t.Errorf("status = %s, want Error", got)
	}
	if len(spans[0].Events()) == 0 {
		t.Error("the error should be recorded on the span")
	}
}

// TestTranslateWithoutAProviderIsANoOp covers what an embedder gets: the OTel
// API with no SDK behind it, which must neither fail nor cost anything.
func TestTranslateWithoutAProviderIsANoOp(t *testing.T) {
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	result, err := compiler.Translate(context.Background(), compiler.Request{
		SourceDSL: "promql", TargetDSL: "promql", Query: "up", Registry: testRegistry(t),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if result.Output != "up" {
		t.Errorf("Output = %q", result.Output)
	}
}
