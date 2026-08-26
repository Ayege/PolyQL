package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/polyql/polyql/pkg/compiler"
)

const tracerName = "github.com/polyql/polyql/pkg/proxy"

// queryParams are the request parameters a backend carries its query in, in the
// order they are tried.
//
// Prometheus and Loki both use "query". Tempo uses "q" on its search endpoint.
// Trying each in turn is what lets one handler serve all three without the
// caller saying which dialect of request it is making.
var queryParams = []string{"query", "q"}

// FidelityHeader carries the fidelity score of a forwarded query.
//
// It is set on every translated response, including the allowed ones, so that a
// caller reading responses can tell an exact translation from an approximation
// without parsing a body it did not ask for.
const FidelityHeader = "X-Polyql-Fidelity-Score"

// NotesHeader carries a count of what the translation could not express. The
// notes themselves can be long and multi-line, which a header cannot hold, so
// this says how many there were and the refusal body carries the text.
const NotesHeader = "X-Polyql-Notes"

// TranslatedHeader carries the query actually sent upstream, so an operator
// debugging a surprising result can see what was asked without re-deriving it.
const TranslatedHeader = "X-Polyql-Translated-Query"

// Server is the HTTP front end.
type Server struct {
	translator *Translator
	upstream   *url.URL
	client     *http.Client
}

// NewServer builds a proxy server from a validated configuration.
func NewServer(cfg Config) (*Server, error) {
	translator, err := NewTranslator(cfg)
	if err != nil {
		return nil, err
	}
	upstream, err := parseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	client := cfg.UpstreamClient
	if client == nil {
		client = &http.Client{Timeout: DefaultUpstreamTimeout}
	}
	return &Server{translator: translator, upstream: upstream, client: client}, nil
}

// Handler returns the routes this proxy serves.
//
// The paths are the backends' own, so a tool already pointed at Prometheus, Loki
// or Tempo needs its address changed and nothing else. A path with no rule falls
// through to the catch-all, which forwards untouched — a request carrying no
// query has nothing to translate, and refusing it would break every /-shaped
// health check and metadata call a real client makes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleHealth)

	// Prometheus.
	mux.HandleFunc("/api/v1/query", s.handleQuery)
	mux.HandleFunc("/api/v1/query_range", s.handleQuery)
	// Loki.
	mux.HandleFunc("/loki/api/v1/query", s.handleQuery)
	mux.HandleFunc("/loki/api/v1/query_range", s.handleQuery)
	// Tempo.
	mux.HandleFunc("/api/search", s.handleQuery)

	mux.HandleFunc("/", s.handlePassthrough)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleQuery translates the query parameter and forwards the request.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	// A trace arriving from the caller continues here rather than starting
	// again, which is what makes a translation one hop in someone else's trace.
	ctx := otel.GetTextMapPropagator().Extract(r.Context(),
		propagation.HeaderCarrier(r.Header))
	ctx, span := otel.Tracer(tracerName).Start(ctx, "proxy."+r.URL.Path,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.route", r.URL.Path),
			attribute.String("polyql.source_dsl", s.translator.SourceDSL()),
			attribute.String("polyql.target_dsl", s.translator.TargetDSL()),
		))
	defer span.End()

	request, err := readRequest(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "the request could not be read", err.Error(), nil)
		return
	}

	param, query, found := request.findQuery()
	if !found {
		// Nothing to translate. Forwarding untouched is right: this is a
		// metadata or label call, not a query.
		s.forward(ctx, w, r, request)
		return
	}

	decision, err := s.translator.Translate(ctx, query)
	if err != nil {
		span.RecordError(err)
		s.writeError(w, http.StatusBadRequest, "the query could not be translated", err.Error(), nil)
		return
	}

	report := decision.Result.Report
	span.SetAttributes(
		attribute.Float64("polyql.fidelity_score", report.FidelityScore),
		attribute.Int("polyql.unsupported_count", report.UnsupportedCount),
		attribute.Bool("polyql.allowed", decision.Allowed),
	)

	setFidelityHeaders(w.Header(), decision)

	if !decision.Allowed {
		span.SetAttributes(attribute.String("polyql.refused_reason", decision.Reason))
		s.writeError(w, http.StatusBadRequest, decision.Reason, "", decision.Result)
		return
	}

	// Only the one parameter that held the query is rewritten; everything else
	// the caller sent — time range, step, limit, direction — passes through as
	// it came. A proxy that rebuilt the request would drop whatever it did not
	// know about.
	request.set(param, decision.Result.Output)
	s.forward(ctx, w, r, request)
}

// handlePassthrough forwards anything this proxy has no rule for.
func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(),
		propagation.HeaderCarrier(r.Header))
	request, err := readRequest(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "the request could not be read", err.Error(), nil)
		return
	}
	s.forward(ctx, w, r, request)
}

// inboundRequest holds a request's parameters, from wherever they arrived.
//
// A query reaches a backend in one of two places, and which one is not the
// caller's choice to make consistently: Grafana puts a short Prometheus query in
// the URL and a long one in a form-encoded POST body, because a URL has a length
// limit and a query does not. Reading both is what stops the proxy silently
// passing the long ones through untranslated.
type inboundRequest struct {
	url  url.Values
	form url.Values
	// body is the original body when it was not a form, kept so it can be
	// forwarded unread.
	body []byte
	// hadBody records that a body was present, so an empty one is not confused
	// with none.
	hadBody bool
}

// maxFormBytes caps a form body read into memory. A query is kilobytes; a body
// this size is not one, and buffering it whole would be the failure.
const maxFormBytes = 8 << 20 // 8 MiB

func readRequest(r *http.Request) (*inboundRequest, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("reading the query string: %w", err)
	}
	request := &inboundRequest{url: values}

	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return request, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxFormBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the request body: %w", err)
	}
	request.body = body
	request.hadBody = len(body) > 0

	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "application/x-www-form-urlencoded" && len(body) > 0 {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("reading the form body: %w", err)
		}
		request.form = form
	}
	return request, nil
}

// findQuery locates the parameter carrying the query, preferring the body: a
// client that sent both meant the body, which is where the longer one goes.
func (q *inboundRequest) findQuery() (param, query string, ok bool) {
	for _, source := range []url.Values{q.form, q.url} {
		if source == nil {
			continue
		}
		for _, name := range queryParams {
			if value := source.Get(name); strings.TrimSpace(value) != "" {
				return name, value, true
			}
		}
	}
	return "", "", false
}

// set replaces a parameter wherever it was found.
func (q *inboundRequest) set(param, value string) {
	if q.form != nil && q.form.Get(param) != "" {
		q.form.Set(param, value)
		return
	}
	q.url.Set(param, value)
}

// rawQuery renders the URL query string.
func (q *inboundRequest) rawQuery() string { return q.url.Encode() }

// bodyReader returns the body to forward, re-encoding a form whose values were
// rewritten.
func (q *inboundRequest) bodyReader() (io.Reader, int64) {
	if q.form != nil {
		encoded := q.form.Encode()
		return strings.NewReader(encoded), int64(len(encoded))
	}
	if !q.hadBody {
		return nil, 0
	}
	return bytes.NewReader(q.body), int64(len(q.body))
}

// forward sends the request upstream and copies the response back verbatim.
//
// Verbatim is the contract: the body is the backend's own, in the backend's own
// shape. Translating results would be a second compiler, and pretending to have
// one by reshaping a few fields would be worse than not having it.
func (s *Server) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, request *inboundRequest) {
	target := *s.upstream
	target.Path = strings.TrimRight(s.upstream.Path, "/") + r.URL.Path
	target.RawQuery = request.rawQuery()

	body, length := request.bodyReader()
	outbound, err := http.NewRequestWithContext(ctx, r.Method, target.String(), body)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "the upstream request could not be built",
			err.Error(), nil)
		return
	}
	copyHeaders(outbound.Header, r.Header)
	if body != nil {
		// A re-encoded form is a different length from the one that arrived, and
		// a stale Content-Length would truncate it.
		outbound.ContentLength = length
		outbound.Header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	// Hop-by-hop headers belong to this connection, not the next one.
	for _, hop := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade"} {
		outbound.Header.Del(hop)
	}
	// The caller's trace continues into the upstream call.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(outbound.Header))

	response, err := s.client.Do(outbound)
	if err != nil {
		// 502 rather than 500: the proxy worked, the backend did not answer.
		s.writeError(w, http.StatusBadGateway,
			fmt.Sprintf("the upstream at %s did not answer", s.upstream.Host), err.Error(), nil)
		return
	}
	defer response.Body.Close()

	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// setFidelityHeaders records what the translation cost on the response.
func setFidelityHeaders(header http.Header, decision *Decision) {
	report := decision.Result.Report
	header.Set(FidelityHeader, fmt.Sprintf("%.4f", report.FidelityScore))
	header.Set(NotesHeader, fmt.Sprintf("%d", len(decision.Result.Notes)))
	if decision.Allowed {
		header.Set(TranslatedHeader, singleLine(decision.Result.Output))
	}
}

// singleLine flattens text for a header value, since a header cannot hold a
// newline and a multi-line query would otherwise truncate the response.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// errorBody is what a refusal returns.
//
// It carries the fidelity report rather than a bare message, because "this could
// not be translated faithfully" is not actionable on its own — what a caller
// needs is which construct, and why.
type errorBody struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	// Detail carries an underlying message, such as a parse error.
	Detail string `json:"detail,omitempty"`
	// PolyQL is the translation report, present when there was one.
	PolyQL *compiler.Result `json:"polyql,omitempty"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, message, detail string, result *compiler.Result) {
	// The envelope matches Prometheus' own error shape, so a client that already
	// parses backend errors can read this one.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	body := errorBody{Status: "error", Error: message, Detail: detail, PolyQL: result}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(body)
}
