// Command polyql-proxy translates observability queries in flight.
//
// Point a tool that speaks one query language at this, give it a backend that
// speaks another, and queries are translated on the way through. The endpoints
// are the backends' own, so a client already configured for Prometheus, Loki or
// Tempo needs its address changed and nothing else.
//
// Responses are passed through untouched: the body a caller receives is the
// upstream's own, in the upstream's own shape. Translating results would be a
// second compiler, and reshaping a few fields to look like one would be worse
// than not having it.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/polyql/polyql/pkg/proxy"
	"github.com/polyql/polyql/pkg/registry"
	"github.com/polyql/polyql/pkg/telemetry"

	// Imported for their registration side effects: which languages this binary
	// can translate between is decided by which packages it imports.
	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/traceql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/traceql"
)

// Build information, set at link time the same way the CLI's is.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

const (
	exitOK    = 0
	exitError = 2
)

// shutdownGrace is how long an in-flight request has to finish once a signal
// arrives. A query already forwarded upstream should be allowed to come back
// rather than be cut off at the moment of a rolling restart.
const shutdownGrace = 15 * time.Second

type options struct {
	listen       string
	sourceDSL    string
	targetDSL    string
	upstream     string
	registryDir  string
	allowPartial bool
	otlpEndpoint string
	otlpInsecure bool
	upstreamWait time.Duration
}

func main() {
	if err := newCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "polyql-proxy: "+err.Error())
		os.Exit(exitError)
	}
	os.Exit(exitOK)
}

func newCommand() *cobra.Command {
	opts := &options{}

	cmd := &cobra.Command{
		Use:   "polyql-proxy",
		Short: "Translate observability queries in flight",
		Long: "polyql-proxy accepts a query in one language, translates it through the\n" +
			"shared telemetry IR, and forwards it to a backend that speaks another.\n\n" +
			"A query the target cannot fully express is refused with the fidelity report\n" +
			"as the body, because a proxy that silently forwarded a half-translated query\n" +
			"would return data that looks right and is not. Pass --allow-partial to\n" +
			"forward it anyway.\n\n" +
			"Responses are passed through untouched: what comes back is the upstream's own\n" +
			"body, in the upstream's own shape.",
		Example: "  # Point a Prometheus client at a Loki backend.\n" +
			"  polyql-proxy --source-dsl promql --to-dsl logql --upstream http://loki:3100\n\n" +
			"  # Allow approximations through, reporting them in a response header.\n" +
			"  polyql-proxy --source-dsl traceql --to-dsl logql \\\n" +
			"    --upstream http://loki:3100 --allow-partial",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return run(cmd.Context(), opts) },
	}

	cmd.Flags().StringVar(&opts.listen, "listen", ":9099", "address to serve on")
	cmd.Flags().StringVar(&opts.sourceDSL, "source-dsl", "", "language callers write (required)")
	cmd.Flags().StringVar(&opts.targetDSL, "to-dsl", "", "language the upstream speaks (required)")
	cmd.Flags().StringVar(&opts.upstream, "upstream", "",
		"backend to forward to, such as http://loki:3100 (required)")
	cmd.Flags().StringVar(&opts.registryDir, "registry-dir", "",
		"directory of language definitions; the compiled-in set is used when omitted")
	cmd.Flags().BoolVar(&opts.allowPartial, "allow-partial", false,
		"forward a query the target cannot fully express, instead of refusing it")
	cmd.Flags().DurationVar(&opts.upstreamWait, "upstream-timeout", proxy.DefaultUpstreamTimeout,
		"how long to wait for the backend to answer")
	cmd.Flags().StringVar(&opts.otlpEndpoint, "otlp-endpoint", "",
		"export a trace span per request to this OTLP/HTTP collector "+
			"(default: the "+telemetry.EndpointEnv+" environment variable, or no export)")
	cmd.Flags().BoolVar(&opts.otlpInsecure, "otlp-insecure", false,
		"send traces over plain HTTP rather than TLS")

	_ = cmd.MarkFlagRequired("source-dsl")
	_ = cmd.MarkFlagRequired("to-dsl")
	_ = cmd.MarkFlagRequired("upstream")

	cmd.AddCommand(newVersionCommand())
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "polyql-proxy %s\n  commit: %s\n  built:  %s\n",
				version, commit, buildDate)
			return nil
		},
	}
}

func run(ctx context.Context, opts *options) error {
	if ctx == nil {
		ctx = context.Background()
	}

	shutdownTracing, err := telemetry.Setup(ctx, telemetry.Config{
		Endpoint:    opts.otlpEndpoint,
		ServiceName: "polyql-proxy",
		Version:     version,
		Insecure:    opts.otlpInsecure,
		ErrorLog:    os.Stderr,
	})
	if err != nil {
		// A misconfigured collector must not stop the proxy serving queries.
		fmt.Fprintln(os.Stderr, "polyql-proxy: tracing disabled: "+err.Error())
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "polyql-proxy: flushing traces: "+err.Error())
		}
	}()

	reg, err := registry.Open(opts.registryDir)
	if err != nil {
		return err
	}

	server, err := proxy.NewServer(proxy.Config{
		SourceDSL:      opts.sourceDSL,
		TargetDSL:      opts.targetDSL,
		Upstream:       opts.upstream,
		Registry:       reg,
		AllowPartial:   opts.allowPartial,
		UpstreamClient: &http.Client{Timeout: opts.upstreamWait},
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:    opts.listen,
		Handler: server.Handler(),
		// A slow client must not hold a connection open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
	}

	policy := "refusing"
	if opts.allowPartial {
		policy = "forwarding"
	}
	fmt.Fprintf(os.Stderr,
		"polyql-proxy: %s → %s, forwarding to %s, listening on %s (%s incomplete translations)\n",
		opts.sourceDSL, opts.targetDSL, opts.upstream, opts.listen, policy)

	return serve(ctx, httpServer)
}

// serve runs the server until a signal arrives, then drains.
func serve(ctx context.Context, server *http.Server) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "polyql-proxy: draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
