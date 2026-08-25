package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// writer returns where output goes, and a function to close it. Writing to a
// file rather than stdout keeps a report out of a pipe carrying the query.
func (t *translateOptions) writer() (io.Writer, func(), error) {
	if t.output == "" || t.output == "-" {
		return t.stdout, func() {}, nil
	}
	file, err := os.Create(t.output)
	if err != nil {
		return nil, nil, fatalf("creating %s: %s", t.output, err)
	}
	return file, func() { _ = file.Close() }, nil
}

func (t *translateOptions) render(out io.Writer, results []Result) error {
	switch t.format {
	case formatJSON:
		return t.renderJSON(out, results)
	case formatQueryOnly:
		return t.renderQueryOnly(out, results)
	default:
		return t.renderText(out, results)
	}
}

// renderJSON writes one object for a single query and an array for a file, so a
// caller translating one query is not made to unwrap a list of one.
func (t *translateOptions) renderJSON(out io.Writer, results []Result) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if t.file == "" && len(results) == 1 {
		return encoder.Encode(results[0])
	}
	return encoder.Encode(results)
}

// renderQueryOnly writes just the translated text, for piping into another tool.
//
// Notes and the report are dropped, and an untranslatable query writes nothing
// on its line rather than something that looks like a query — a caller piping
// this has no way to tell a comment from a result, so it gets neither.
func (t *translateOptions) renderQueryOnly(out io.Writer, results []Result) error {
	for _, result := range results {
		if result.Error != "" {
			// The failure still has to be visible somewhere.
			fmt.Fprintf(t.stderr, "polyql: %s: %s\n", result.Input, result.Error)
			fmt.Fprintln(out)
			continue
		}
		fmt.Fprintln(out, result.Output)
	}
	return nil
}

func (t *translateOptions) renderText(out io.Writer, results []Result) error {
	single := len(results) == 1 && t.file == ""

	for i, result := range results {
		if !single {
			if i > 0 {
				fmt.Fprintln(out, "\n"+strings.Repeat("-", 60))
			}
			fmt.Fprintf(out, "# %s\n", result.Input)
		}

		if result.Error != "" {
			fmt.Fprintf(out, "error: %s\n", result.Error)
			continue
		}

		// The emitter's notes stay with the query as comments, so the printed
		// text remains valid in the target language and can be copied out whole.
		for _, note := range result.Notes {
			fmt.Fprintf(out, "# %s\n", note)
		}
		fmt.Fprintln(out, result.Output)

		if result.Fidelity != nil {
			fmt.Fprintln(out)
			fmt.Fprint(out, result.Fidelity.ToText())
		}
	}

	if !single {
		t.renderSummary(out, results)
	}
	return nil
}

// renderSummary closes a multi-query run with the totals, so a long file ends
// with the answer rather than with its last query.
func (t *translateOptions) renderSummary(out io.Writer, results []Result) {
	var full, partial, unsupported, failed int
	for _, result := range results {
		switch {
		case result.Error != "":
			failed++
		case result.Fidelity == nil:
		case result.Fidelity.UnsupportedCount > 0:
			unsupported++
		case result.Fidelity.PartialCount > 0:
			partial++
		default:
			full++
		}
	}

	fmt.Fprintln(out, "\n"+strings.Repeat("=", 60))
	fmt.Fprintf(out, "%d quer%s: %d exact, %d approximate, %d incomplete",
		len(results), plural(len(results)), full, partial, unsupported)
	if failed > 0 {
		fmt.Fprintf(out, ", %d failed to parse", failed)
	}
	fmt.Fprintln(out)
}
