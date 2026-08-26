package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/polyql/polyql/pkg/compiler"
	"github.com/polyql/polyql/pkg/compiler/fidelity"
	"github.com/polyql/polyql/pkg/registry"
)

// Output formats.
const (
	formatText      = "text"
	formatJSON      = "json"
	formatQueryOnly = "query-only"
)

type translateOptions struct {
	*options
	from                 string
	to                   string
	query                string
	file                 string
	output               string
	format               string
	failOnPartial        bool
	failOnSignalMismatch bool
}

// Result is one query's translation, and the shape the JSON format writes.
type Result struct {
	SourceDSL string `json:"source_dsl"`
	TargetDSL string `json:"target_dsl"`
	Input     string `json:"input"`
	Output    string `json:"output"`
	// Notes are the comment lines the emitter wrote above the query, lifted out
	// so a machine reading the JSON does not have to parse them back out of the
	// text.
	Notes    []string         `json:"notes"`
	Fidelity *fidelity.Report `json:"fidelity"`
	// Error is set when the query could not be translated at all, in which case
	// Output and Fidelity are empty. A file of queries reports per-query rather
	// than abandoning the run at the first bad line.
	Error string `json:"error,omitempty"`
}

func newTranslateCommand(opts *options) *cobra.Command {
	t := &translateOptions{options: opts}

	cmd := &cobra.Command{
		Use:   "translate",
		Short: "Translate a query from one language to another",
		Long: "Translate reads a query in one language and writes it in another, together\n" +
			"with a report of what the target could not express.\n\n" +
			"The query comes from --query, from a file of them given with --file, or from\n" +
			"stdin when neither is named and something is piped in.\n\n" +
			"It exits 0 when the translation is exact, 1 when something was lost, and 2\n" +
			"when the command could not run — so a CI job can gate on fidelity without\n" +
			"parsing the output.",
		Example: "  polyql translate --from promql --to logql \\\n" +
			"    --query 'rate(http_requests_total{status=\"500\"}[5m])'\n\n" +
			"  polyql translate --from logql --to promql --file queries.txt --format json\n\n" +
			"  polyql translate --from promql --to logql --query 'up' --format query-only\n\n" +
			"  cat queries.txt | polyql translate --from logql --to promql",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return t.run() },
	}

	cmd.Flags().StringVar(&t.from, "from", "", "source language (required)")
	cmd.Flags().StringVar(&t.to, "to", "", "target language (required)")
	cmd.Flags().StringVar(&t.query, "query", "", "query to translate")
	cmd.Flags().StringVar(&t.file, "file", "", `file of queries, one per line; "-" reads stdin`)
	cmd.Flags().StringVarP(&t.output, "output", "o", "", "write to this file instead of stdout")
	cmd.Flags().StringVar(&t.format, "format", formatText,
		"output format: text, json, or query-only")
	cmd.Flags().BoolVar(&t.failOnPartial, "fail-on-partial", false,
		"exit non-zero on an approximate translation, not only on an impossible one")
	cmd.Flags().BoolVar(&t.failOnSignalMismatch, "fail-on-signal-mismatch", false,
		"exit non-zero when the query reads a different signal class than the target backend")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	cmd.MarkFlagsMutuallyExclusive("query", "file")

	return cmd
}

func (t *translateOptions) run() error {
	if err := t.validateFlags(); err != nil {
		return err
	}

	started := time.Now()
	reg, err := t.registry()
	if err != nil {
		return fatalf("%s", err)
	}
	source := "the compiled-in registry"
	if t.registryDir != "" {
		source = t.registryDir
	}
	t.debugf("loaded %s from %s", strings.Join(reg.List(), ", "), source)

	// Both languages are checked before any query runs, so a typo in --to fails
	// at once rather than after the first translation.
	if err := t.checkLanguages(reg); err != nil {
		return err
	}

	queries, err := t.inputs()
	if err != nil {
		return err
	}

	ctx := context.Background()
	results := make([]Result, 0, len(queries))
	worst := exitOK
	for _, query := range queries {
		result, code := t.translateOne(ctx, query, reg)
		results = append(results, result)
		if code > worst {
			worst = code
		}
	}

	// A single query that would not translate is a failed command, so its
	// message goes to stderr and nothing is written to the output stream. A
	// stream of them — a file, or a pipe — is different: one bad line should not
	// discard the other results, so those failures are reported per query
	// alongside them.
	if t.isSingleQuery() && len(results) == 1 && results[0].Error != "" {
		return fatalf("%s", results[0].Error)
	}
	t.debugf("translated %d quer%s in %s", len(results), plural(len(results)), time.Since(started).Round(time.Microsecond))

	out, closeOut, err := t.writer()
	if err != nil {
		return err
	}
	defer closeOut()

	if err := t.render(out, results); err != nil {
		return fatalf("writing output: %s", err)
	}

	if worst == exitError {
		// Each failure was already reported beside its query.
		return &exitCodeError{code: exitError, err: errors.New("")}
	}
	if worst == exitFidelity {
		return fidelityFailure
	}
	return nil
}

func (t *translateOptions) validateFlags() error {
	// Nothing named on the command line is fine when a query is being piped in,
	// which is what makes "cat queries.txt | polyql translate ..." work. With no
	// pipe either, there is genuinely nothing to translate.
	if t.query == "" && t.file == "" && t.stdin == nil {
		return fatalf("give a query with --query, a file of queries with --file, " +
			"or pipe queries in on stdin")
	}
	switch t.format {
	case formatText, formatJSON, formatQueryOnly:
	default:
		return fatalf("unknown --format %q: expected %s, %s or %s",
			t.format, formatText, formatJSON, formatQueryOnly)
	}
	return nil
}

// checkLanguages fails on an unusable language pair before any query runs, so a
// typo in --to is reported once rather than once per query.
func (t *translateOptions) checkLanguages(reg *registry.Registry) error {
	if err := compiler.CheckLanguages(t.from, t.to, reg); err != nil {
		return fatalf("%s", err)
	}
	return nil
}

// isSingleQuery reports whether the caller named exactly one query inline.
//
// It is what decides the shape of the output: one inline query renders as a bare
// result, while a stream of them — from a file or a pipe — renders as a list,
// because a caller reading a stream cannot know in advance how many will come
// back. The test is on --query rather than on an empty --file, since an empty
// --file now means "read stdin" rather than "no source given".
func (t *translateOptions) isSingleQuery() bool { return t.query != "" }

// inputs collects the queries to translate.
//
// Three sources feed it, in the order the flags name them: an inline --query, a
// --file, or stdin. Stdin is reached either by "--file -", matching the spelling
// --output already uses for stdout, or by naming no source at all when something
// is piped in.
func (t *translateOptions) inputs() ([]string, error) {
	if t.query != "" {
		return []string{t.query}, nil
	}

	reader, name, closeInput, err := t.inputStream()
	if err != nil {
		return nil, err
	}
	defer closeInput()

	var queries []string
	scanner := bufio.NewScanner(reader)
	// A query can be long, and the default limit would truncate one silently.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Blank lines and comments let a query file carry its own notes.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		queries = append(queries, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fatalf("reading %s: %s", name, err)
	}
	if len(queries) == 0 {
		return nil, fatalf("%s holds no queries", name)
	}
	return queries, nil
}

// inputStream opens whichever source the flags selected, returning it with a
// name to blame in an error and a function to close it.
//
// Stdin is never closed: it belongs to the process rather than to this command,
// and closing it would break a caller that goes on to read more.
func (t *translateOptions) inputStream() (io.Reader, string, func(), error) {
	noop := func() {}

	if t.file == "" || t.file == "-" {
		if t.stdin == nil {
			// Only reachable through "--file -", since validateFlags already
			// rejected naming no source with nothing piped in.
			return nil, "", noop, fatalf("--file - reads stdin, but nothing was piped in")
		}
		return t.stdin, "stdin", noop, nil
	}

	file, err := os.Open(t.file)
	if err != nil {
		return nil, "", noop, fatalf("reading %s: %s", t.file, err)
	}
	return file, t.file, func() { _ = file.Close() }, nil
}

// translateOne runs the pipeline over one query and reports the exit code it
// earned.
//
// The pipeline itself lives in pkg/compiler, so that this command, the proxy and
// the dashboard translator drive one sequence rather than three copies of it.
// What stays here is policy: which outcomes are worth a non-zero exit.
func (t *translateOptions) translateOne(ctx context.Context, query string, reg *registry.Registry) (Result, int) {
	result := Result{
		SourceDSL: strings.ToLower(t.from),
		TargetDSL: strings.ToLower(t.to),
		Input:     query,
		Notes:     []string{},
	}

	translated, err := compiler.Translate(ctx, compiler.Request{
		SourceDSL: t.from,
		TargetDSL: t.to,
		Query:     query,
		Registry:  reg,
	})
	if err != nil {
		result.Error = err.Error()
		return result, exitError
	}

	result.Output = translated.Output
	result.Notes = translated.Notes
	result.Fidelity = translated.Report

	t.debugf("resolved %q to %d IR nodes", query, translated.Report.TotalNodes)
	t.debugf("fidelity %.2f (%d full, %d partial, %d unsupported)",
		result.Fidelity.FidelityScore, result.Fidelity.FullCount,
		result.Fidelity.PartialCount, result.Fidelity.UnsupportedCount)

	return result, t.exitCodeFor(result.Fidelity)
}

// exitCodeFor decides what a report is worth as an exit status.
func (t *translateOptions) exitCodeFor(report *fidelity.Report) int {
	if report == nil {
		return exitOK
	}
	if report.UnsupportedCount > 0 {
		return exitFidelity
	}
	if report.SignalMismatch != nil && t.failOnSignalMismatch {
		return exitFidelity
	}
	if t.failOnPartial && report.PartialCount > 0 {
		return exitFidelity
	}
	return exitOK
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
