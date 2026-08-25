package main

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/fidelity"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/compiler/resolver"
	"github.com/polyql/polyql/pkg/compiler/validator"
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
			"It exits 0 when the translation is exact, 1 when something was lost, and 2\n" +
			"when the command could not run — so a CI job can gate on fidelity without\n" +
			"parsing the output.",
		Example: "  polyql translate --from promql --to logql \\\n" +
			"    --query 'rate(http_requests_total{status=\"500\"}[5m])'\n\n" +
			"  polyql translate --from logql --to promql --file queries.txt --format json\n\n" +
			"  polyql translate --from promql --to logql --query 'up' --format query-only",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return t.run() },
	}

	cmd.Flags().StringVar(&t.from, "from", "", "source language (required)")
	cmd.Flags().StringVar(&t.to, "to", "", "target language (required)")
	cmd.Flags().StringVar(&t.query, "query", "", "query to translate")
	cmd.Flags().StringVar(&t.file, "file", "", "file of queries, one per line")
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

	results := make([]Result, 0, len(queries))
	worst := exitOK
	for _, query := range queries {
		result, code := t.translateOne(query, reg)
		results = append(results, result)
		if code > worst {
			worst = code
		}
	}

	// A single query that would not translate is a failed command, so its
	// message goes to stderr and nothing is written to the output stream. A
	// file is different: one bad line should not discard the other results, so
	// those failures are reported per query alongside them.
	if t.file == "" && len(results) == 1 && results[0].Error != "" {
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
	if t.query == "" && t.file == "" {
		return fatalf("give a query with --query or a file of queries with --file")
	}
	switch t.format {
	case formatText, formatJSON, formatQueryOnly:
	default:
		return fatalf("unknown --format %q: expected %s, %s or %s",
			t.format, formatText, formatJSON, formatQueryOnly)
	}
	return nil
}

// checkLanguages verifies that both languages have a definition, a parser and an
// emitter, naming what is available when one does not.
func (t *translateOptions) checkLanguages(reg *registry.Registry) error {
	if _, err := reg.Get(t.from); err != nil {
		return fatalf("unknown source language %q (available: %s)", t.from, strings.Join(reg.List(), ", "))
	}
	if _, err := reg.Get(t.to); err != nil {
		return fatalf("unknown target language %q (available: %s)", t.to, strings.Join(reg.List(), ", "))
	}
	if _, err := parser.Get(t.from); err != nil {
		return fatalf("no parser for %q (this binary can read: %s)",
			t.from, strings.Join(parser.List(), ", "))
	}
	if _, err := emitter.Get(t.to); err != nil {
		return fatalf("no emitter for %q (this binary can write: %s)",
			t.to, strings.Join(emitter.List(), ", "))
	}
	return nil
}

// inputs collects the queries to translate.
func (t *translateOptions) inputs() ([]string, error) {
	if t.query != "" {
		return []string{t.query}, nil
	}

	file, err := os.Open(t.file)
	if err != nil {
		return nil, fatalf("reading %s: %s", t.file, err)
	}
	defer file.Close()

	var queries []string
	scanner := bufio.NewScanner(file)
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
		return nil, fatalf("reading %s: %s", t.file, err)
	}
	if len(queries) == 0 {
		return nil, fatalf("%s holds no queries", t.file)
	}
	return queries, nil
}

// translateOne runs the pipeline over one query and reports the exit code it
// earned.
func (t *translateOptions) translateOne(query string, reg *registry.Registry) (Result, int) {
	result := Result{
		SourceDSL: strings.ToLower(t.from),
		TargetDSL: strings.ToLower(t.to),
		Input:     query,
		Notes:     []string{},
	}

	p, _ := parser.Get(t.from)
	node, err := p.Parse(query)
	if err != nil {
		result.Error = err.Error()
		return result, exitError
	}

	resolved, err := resolver.Resolve(node, t.from, reg)
	if err != nil {
		result.Error = err.Error()
		return result, exitError
	}
	t.debugf("resolved %q to %d IR nodes", query, countNodes(resolved))

	_, issues, mismatch := validator.Validate(resolved, t.to, reg)

	e, _ := emitter.Get(t.to)
	text, err := e.Emit(resolved, reg)
	if err != nil {
		result.Error = err.Error()
		return result, exitError
	}

	// The emitter writes its notes as comment lines above the query. Splitting
	// them out gives the JSON format structured notes and the query-only format
	// a bare query.
	result.Notes, result.Output = splitNotes(text)

	// A node can draw more than one verdict and keeps only the worst, so the
	// validator's list is handed over to recover the rest.
	findings := make([]fidelity.Finding, 0, len(issues))
	for _, issue := range issues {
		findings = append(findings, fidelity.Finding{
			Path: issue.Path, Flag: issue.Flag, Reason: issue.Reason,
		})
	}
	result.Fidelity = fidelity.GenerateWithIssues(resolved, findings, t.from, t.to, mismatch)
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

// splitNotes separates the emitter's leading comment lines from the query.
func splitNotes(text string) (notes []string, query string) {
	notes = []string{}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			notes = append(notes, strings.TrimSpace(strings.TrimPrefix(line, "#")))
			continue
		}
		return notes, strings.Join(lines[i:], "\n")
	}
	return notes, ""
}

func countNodes(query *ir.Query) int {
	count := 0
	ir.Inspect(query, func(ir.Node) bool {
		count++
		return true
	})
	return count
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
