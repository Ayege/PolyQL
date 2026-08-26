package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/proxy"
	"github.com/polyql/polyql/pkg/registry"

	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/traceql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/traceql"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	return reg
}

// upstreamCall is what the stub backend saw.
type upstreamCall struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
	Header http.Header
}

// newProxy starts a stub upstream and a proxy in front of it.
func newProxy(t *testing.T, cfg proxy.Config) (*httptest.Server, *[]upstreamCall) {
	t.Helper()

	calls := &[]upstreamCall{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*calls = append(*calls, upstreamCall{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(),
			Body: string(body), Header: r.Header.Clone(),
		})
		w.Header().Set("Content-Type", "application/json")
		// A shape only this backend would return, so a test can prove the body
		// came back untouched.
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(upstream.Close)

	cfg.Upstream = upstream.URL
	cfg.Registry = testRegistry(t)
	cfg.UpstreamClient = upstream.Client()

	server, err := proxy.NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	return front, calls
}

// TestTranslatesAndForwards covers the ordinary path.
func TestTranslatesAndForwards(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{SourceDSL: "promql", TargetDSL: "logql"})

	response, err := http.Get(front.URL + "/api/v1/query?query=" +
		url.QueryEscape(`rate(http_requests_total[5m])`) + "&time=123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %s\n%s", response.Status, body)
	}
	if len(*calls) != 1 {
		t.Fatalf("upstream saw %d calls, want 1", len(*calls))
	}
	call := (*calls)[0]

	if got := call.Path; got != "/api/v1/query" {
		t.Errorf("upstream path = %q", got)
	}
	// The query reached the backend translated.
	if got := call.Query.Get("query"); !strings.Contains(got, "rate(") ||
		!strings.Contains(got, "__name__") {
		t.Errorf("upstream query = %q, want a LogQL rate", got)
	}
	// Everything else the caller sent must survive. A proxy that rebuilt the
	// request would drop the time range and silently change the answer.
	if got := call.Query.Get("time"); got != "123" {
		t.Errorf("time = %q, want it passed through", got)
	}

	// The response body is the backend's own.
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), `"resultType":"vector"`) {
		t.Errorf("the upstream body should pass through untouched: %s", body)
	}

	t.Run("the fidelity is reported on the response", func(t *testing.T) {
		if got := response.Header.Get(proxy.FidelityHeader); got == "" {
			t.Error("a translated response should carry its score")
		}
		if got := response.Header.Get(proxy.TranslatedHeader); !strings.Contains(got, "rate(") {
			t.Errorf("%s = %q, want the query that was sent", proxy.TranslatedHeader, got)
		}
	})
}

// TestRefusesAnIncompleteTranslation is the policy decision made concrete: a
// query the target cannot fully express is refused rather than forwarded.
func TestRefusesAnIncompleteTranslation(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{SourceDSL: "promql", TargetDSL: "logql"})

	// histogram_quantile has no LogQL form at all.
	response, err := http.Get(front.URL + "/api/v1/query?query=" +
		url.QueryEscape(`histogram_quantile(0.99, x)`))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", response.Status)
	}
	// The point of refusing is that nothing reaches the backend.
	if len(*calls) != 0 {
		t.Errorf("the upstream was called %d times; a refused query must not be forwarded",
			len(*calls))
	}

	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		PolyQL *struct {
			Notes    []string `json:"notes"`
			Fidelity *struct {
				UnsupportedCount int     `json:"unsupported"`
				FidelityScore    float64 `json:"fidelity_score"`
			} `json:"fidelity"`
		} `json:"polyql"`
	}
	raw, _ := io.ReadAll(response.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the refusal is not valid JSON: %v\n%s", err, raw)
	}

	if body.Status != "error" {
		t.Errorf("status = %q", body.Status)
	}
	if !strings.Contains(body.Error, "--allow-partial") {
		t.Errorf("the refusal should say how to override it: %q", body.Error)
	}
	// A bare "cannot translate" is not actionable. What the caller needs is
	// which construct and why, which is what the report carries.
	if body.PolyQL == nil || body.PolyQL.Fidelity == nil {
		t.Fatalf("the refusal should carry the fidelity report:\n%s", raw)
	}
	if body.PolyQL.Fidelity.UnsupportedCount == 0 {
		t.Error("the report should name what could not be expressed")
	}
	if len(body.PolyQL.Notes) == 0 {
		t.Error("the emitter's notes should be in the refusal")
	}
}

// TestAllowPartialForwardsAnyway covers the opt-out.
func TestAllowPartialForwardsAnyway(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{
		SourceDSL: "promql", TargetDSL: "logql", AllowPartial: true,
	})

	response, err := http.Get(front.URL + "/api/v1/query?query=" +
		url.QueryEscape(`histogram_quantile(0.99, x)`))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %s, want the query forwarded", response.Status)
	}
	if len(*calls) != 1 {
		t.Fatalf("upstream saw %d calls, want 1", len(*calls))
	}
	// Forwarding is not the same as hiding: the score still rides on the
	// response, so a caller can tell what it got.
	if got := response.Header.Get(proxy.FidelityHeader); got == "" {
		t.Error("a forwarded partial translation should still report its score")
	}
	if got := response.Header.Get(proxy.NotesHeader); got == "" || got == "0" {
		t.Errorf("%s = %q, want the count of what was lost", proxy.NotesHeader, got)
	}
}

// TestAnUnparseableQueryIsRejected covers the other 400.
func TestAnUnparseableQueryIsRejected(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{SourceDSL: "promql", TargetDSL: "logql"})

	response, err := http.Get(front.URL + "/api/v1/query?query=" + url.QueryEscape("rate(unclosed"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", response.Status)
	}
	if len(*calls) != 0 {
		t.Error("a query that will not parse must not be forwarded")
	}
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), "parse error") {
		t.Errorf("the body should carry the parse error:\n%s", raw)
	}
}

// TestRequestsWithNoQueryPassThrough covers metadata calls. Refusing them would
// break every label and series lookup a real client makes.
func TestRequestsWithNoQueryPassThrough(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{SourceDSL: "promql", TargetDSL: "logql"})

	for _, path := range []string{"/api/v1/query?time=1", "/api/v1/labels", "/loki/api/v1/label"} {
		response, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %s", path, response.Status)
		}
	}
	if len(*calls) != 3 {
		t.Errorf("upstream saw %d calls, want all three forwarded", len(*calls))
	}
}

// TestFormEncodedPostIsTranslated covers how Grafana sends a long query: in a
// form body rather than the URL, because a URL has a length limit and a query
// does not. Reading only the URL would pass those through untranslated.
func TestFormEncodedPostIsTranslated(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{SourceDSL: "promql", TargetDSL: "logql"})

	form := url.Values{}
	form.Set("query", `rate(http_requests_total[5m])`)
	form.Set("step", "15")

	response, err := http.Post(front.URL+"/api/v1/query_range",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %s\n%s", response.Status, body)
	}
	if len(*calls) != 1 {
		t.Fatalf("upstream saw %d calls, want 1", len(*calls))
	}

	sent, err := url.ParseQuery((*calls)[0].Body)
	if err != nil {
		t.Fatalf("the forwarded body is not a form: %v", err)
	}
	if got := sent.Get("query"); !strings.Contains(got, "__name__") {
		t.Errorf("the body query = %q, want it translated", got)
	}
	if got := sent.Get("step"); got != "15" {
		t.Errorf("step = %q, want it passed through", got)
	}
	// A re-encoded form is a different length, and a stale Content-Length would
	// truncate it.
	if got := (*calls)[0].Header.Get("Content-Length"); got != "" {
		if got != strconv.Itoa(len((*calls)[0].Body)) {
			t.Errorf("Content-Length = %s but the body is %d bytes",
				got, len((*calls)[0].Body))
		}
	}
}

// TestUpstreamFailureIsABadGateway covers the backend being down, which is not
// the proxy failing.
func TestUpstreamFailureIsABadGateway(t *testing.T) {
	server, err := proxy.NewServer(proxy.Config{
		SourceDSL: "promql", TargetDSL: "promql",
		// Port 9 is the discard port: nothing listens.
		Upstream: "http://127.0.0.1:9",
		Registry: testRegistry(t),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	front := httptest.NewServer(server.Handler())
	defer front.Close()

	response, err := http.Get(front.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %s, want 502: the proxy worked, the backend did not answer",
			response.Status)
	}
}

// TestHealthEndpoints covers the two a scheduler needs.
func TestHealthEndpoints(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{SourceDSL: "promql", TargetDSL: "logql"})

	for _, path := range []string{"/healthz", "/readyz"} {
		response, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %s", path, response.Status)
		}
	}
	// Health must not depend on the backend, or a proxy that is fine reports
	// itself unhealthy whenever the thing behind it blips.
	if len(*calls) != 0 {
		t.Errorf("health checks reached the upstream %d times", len(*calls))
	}
}

// TestTraceContextIsPropagated covers the proxy being one hop in someone else's
// trace rather than the start of its own.
func TestTraceContextIsPropagated(t *testing.T) {
	front, calls := newProxy(t, proxy.Config{SourceDSL: "promql", TargetDSL: "promql"})

	request, err := http.NewRequest(http.MethodGet, front.URL+"/api/v1/query?query=up", nil)
	if err != nil {
		t.Fatal(err)
	}
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	request.Header.Set("traceparent", traceparent)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = response.Body.Close()

	if len(*calls) != 1 {
		t.Fatalf("upstream saw %d calls", len(*calls))
	}
	// Without a tracer provider installed there is no span to propagate, so the
	// header may be absent — but it must never be a different trace.
	if got := (*calls)[0].Header.Get("traceparent"); got != "" {
		if !strings.Contains(got, "4bf92f3577b34da6a3ce929d0e0e4736") {
			t.Errorf("traceparent = %q, want the caller's trace continued", got)
		}
	}
}

// TestTranslatorDecidesWithoutTransport covers the policy in isolation, which is
// why it is separated from the server at all.
func TestTranslatorDecidesWithoutTransport(t *testing.T) {
	reg := testRegistry(t)

	strict, err := proxy.NewTranslator(proxy.Config{
		SourceDSL: "promql", TargetDSL: "logql",
		Upstream: "http://example:3100", Registry: reg,
	})
	if err != nil {
		t.Fatalf("NewTranslator: %v", err)
	}

	t.Run("a clean translation is allowed", func(t *testing.T) {
		decision, err := strict.Translate(context.Background(), `rate(x[5m])`)
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		if !decision.Allowed {
			t.Errorf("expected the query to be allowed: %s", decision.Reason)
		}
	})

	t.Run("an incomplete translation is refused", func(t *testing.T) {
		decision, err := strict.Translate(context.Background(), `histogram_quantile(0.99, x)`)
		if err != nil {
			t.Fatalf("a lossy translation is a decision, not an error: %v", err)
		}
		if decision.Allowed {
			t.Error("histogram_quantile has no LogQL form; it should be refused")
		}
		if decision.Reason == "" {
			t.Error("a refusal needs a reason")
		}
		// The result is present even on a refusal, because the report is what
		// explains it.
		if decision.Result == nil || decision.Result.Report == nil {
			t.Error("a refusal should carry the report")
		}
	})

	t.Run("an approximation is allowed", func(t *testing.T) {
		// Completeness rather than losslessness is the gate. A regex crossing
		// dialects is PARTIAL — it was written, with a note that anchoring and
		// escaping conventions differ — so the query upstream still asks the
		// same question and may go.
		decision, err := strict.Translate(context.Background(), `up{job=~"api.*"}`)
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		if !decision.Allowed {
			t.Errorf("an approximation should be forwarded: %s", decision.Reason)
		}
		if decision.Result.Lossless() {
			t.Error("this test needs a query that is approximate but complete")
		}
		if !decision.Result.Complete() {
			t.Error("nothing here is unsupported")
		}
	})
}

// TestNewServerRejectsBadConfig covers the checks that run once at startup, so
// that a typo stops the process rather than failing every request.
func TestNewServerRejectsBadConfig(t *testing.T) {
	reg := testRegistry(t)
	cases := []struct {
		name string
		cfg  proxy.Config
		want string
	}{
		{"no registry", proxy.Config{SourceDSL: "promql", TargetDSL: "logql", Upstream: "http://x"}, "registry"},
		{"no source", proxy.Config{TargetDSL: "logql", Upstream: "http://x", Registry: reg}, "language"},
		{"unknown target", proxy.Config{SourceDSL: "promql", TargetDSL: "nosuch", Upstream: "http://x", Registry: reg}, "unknown target"},
		{"no upstream", proxy.Config{SourceDSL: "promql", TargetDSL: "logql", Registry: reg}, "upstream"},
		{"upstream with no scheme", proxy.Config{SourceDSL: "promql", TargetDSL: "logql", Upstream: "loki:3100", Registry: reg}, "upstream"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := proxy.NewServer(c.cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err, c.want)
			}
		})
	}
}
