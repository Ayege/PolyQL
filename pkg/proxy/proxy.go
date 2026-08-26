// Package proxy translates queries in flight and forwards them to a backend.
//
// # What it is
//
// A caller points a tool that speaks one query language at a backend that speaks
// another. The proxy accepts the request on a backend-compatible endpoint,
// translates the query, forwards it upstream, and returns the upstream's answer.
//
// # What it deliberately is not
//
// Responses are passed through untouched. Prometheus and Loki return different
// JSON shapes, so a genuinely transparent proxy would have to translate results
// as well — a second compiler roughly the size of the first, mapping result
// shapes rather than query syntax. Passing responses through is the honest
// behavior: the caller gets the target backend's format and knows it.
//
// # Fidelity is a gate, not a footnote
//
// A query the target cannot fully express is refused by default, with the
// fidelity report as the body. PolyQL exists to refuse to hide translation loss,
// and a proxy that silently forwarded a half-translated query would be the one
// component contradicting that. AllowPartial opts out per deployment.
package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/polyql/polyql/pkg/compiler"
	"github.com/polyql/polyql/pkg/registry"
)

// DefaultUpstreamTimeout bounds a forwarded request.
const DefaultUpstreamTimeout = 60 * time.Second

// Config describes one translating route.
type Config struct {
	// SourceDSL is the language callers write.
	SourceDSL string
	// TargetDSL is the language the upstream speaks.
	TargetDSL string
	// Upstream is the backend to forward to, as a base URL.
	Upstream string
	// Registry supplies both languages' definitions.
	Registry *registry.Registry
	// AllowPartial forwards a query the target could not fully express instead
	// of refusing it.
	//
	// The default is to refuse. A translated query that quietly dropped a
	// construct returns data that looks right and is not, which is the failure
	// this project exists to prevent — so opting into it is explicit and
	// per-deployment.
	AllowPartial bool
	// UpstreamClient issues the forwarded request. A nil value gets one with
	// DefaultUpstreamTimeout.
	UpstreamClient *http.Client
}

// Translator turns a query in the source language into one the upstream accepts,
// and decides whether it may be sent.
//
// It holds no transport, which is what makes the decision testable without a
// server and keeps the policy in one place rather than spread across handlers.
type Translator struct {
	cfg Config
}

// NewTranslator validates a configuration and returns a Translator for it.
func NewTranslator(cfg Config) (*Translator, error) {
	if cfg.Registry == nil {
		return nil, fmt.Errorf("proxy: a registry is required")
	}
	if strings.TrimSpace(cfg.SourceDSL) == "" || strings.TrimSpace(cfg.TargetDSL) == "" {
		return nil, fmt.Errorf("proxy: both a source and a target language are required")
	}
	// Both languages are checked once, at startup, rather than on every request:
	// a typo in a flag should stop the process rather than fail every query.
	if err := compiler.CheckLanguages(cfg.SourceDSL, cfg.TargetDSL, cfg.Registry); err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if _, err := parseUpstream(cfg.Upstream); err != nil {
		return nil, err
	}
	return &Translator{cfg: cfg}, nil
}

// Decision is what the proxy concluded about one query.
type Decision struct {
	// Result is the translation. It is present even when the query is refused,
	// because the report is what explains the refusal.
	Result *compiler.Result
	// Allowed reports whether the query may be forwarded.
	Allowed bool
	// Reason states why a refused query was refused, in terms a caller can act
	// on.
	Reason string
}

// Translate translates one query and decides whether it may be forwarded.
//
// An error means the query could not be translated at all — it would not parse,
// or the languages are unusable. A query that translated but lost something is
// not an error: it is a Decision with Allowed false, which the caller turns into
// whatever its transport calls a refusal.
func (t *Translator) Translate(ctx context.Context, query string) (*Decision, error) {
	result, err := compiler.Translate(ctx, compiler.Request{
		SourceDSL: t.cfg.SourceDSL,
		TargetDSL: t.cfg.TargetDSL,
		Query:     query,
		Registry:  t.cfg.Registry,
	})
	if err != nil {
		return nil, err
	}

	decision := &Decision{Result: result, Allowed: true}

	// Completeness rather than losslessness is the gate. An approximation was
	// written and explained; an unsupported construct was not written at all,
	// so the query upstream asks a different question from the one posed.
	if !result.Complete() && !t.cfg.AllowPartial {
		decision.Allowed = false
		decision.Reason = fmt.Sprintf(
			"%s cannot express %d construct(s) of this %s query, so the translated query "+
				"would ask something different; start the proxy with --allow-partial to "+
				"forward it anyway",
			t.cfg.TargetDSL, result.Report.UnsupportedCount, t.cfg.SourceDSL)
	}

	return decision, nil
}

// SourceDSL returns the language callers write.
func (t *Translator) SourceDSL() string { return t.cfg.SourceDSL }

// TargetDSL returns the language the upstream speaks.
func (t *Translator) TargetDSL() string { return t.cfg.TargetDSL }

// AllowPartial reports whether incomplete translations are forwarded.
func (t *Translator) AllowPartial() bool { return t.cfg.AllowPartial }

// parseUpstream validates a backend base URL.
func parseUpstream(raw string) (*url.URL, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return nil, fmt.Errorf("proxy: an upstream backend URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("proxy: %q is not a valid upstream URL: %w", raw, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("proxy: %q is not a valid upstream URL: "+
			"give a full address such as http://loki:3100", raw)
	}
	return parsed, nil
}
