// Command polyql translates observability queries between query languages.
//
// The compiler it drives is a library; this is the thin edge that reads a query,
// runs it through parse, resolve, validate, emit, and prints the result together
// with an honest account of what the translation cost.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/polyql/polyql/pkg/registry"

	// Imported for their registration side effects: which languages this binary
	// supports is decided by which packages it imports.
	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/traceql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/traceql"
)

// Exit codes. They are the CLI's contract with a shell script or a CI job, so
// they distinguish "the translation lost something" from "the command could not
// run at all".
const (
	// exitOK means every construct translated fully.
	exitOK = 0
	// exitFidelity means the translation succeeded but lost something: an
	// unsupported construct, or a partial one under --fail-on-partial.
	exitFidelity = 1
	// exitError means the command could not do its job — a query that would not
	// parse, a registry that would not load, an unknown language.
	exitError = 2
)

// options are the settings shared by every subcommand.
type options struct {
	registryDir string
	verbose     bool
	// stdout and stderr are fields rather than the real streams so tests can
	// drive the command in-process and read what it wrote.
	stdout io.Writer
	stderr io.Writer
	// stdin is where a query is read from when no --query or --file names one.
	//
	// It is nil when nothing is piped in. Deciding that is main's job rather
	// than this package's: a command reading an interactive terminal would hang
	// waiting for input that is never coming, and only the real process knows
	// whether its stdin is a terminal.
	stdin io.Reader
}

// registry loads the language definitions: the set compiled into the binary
// unless a directory was named.
func (o *options) registry() (*registry.Registry, error) {
	if o.registryDir == "" {
		return registry.DefaultRegistry()
	}
	return registry.Open(o.registryDir)
}

// debugf writes a line to stderr when --verbose is set, keeping it clear of
// stdout so that piping the translated query stays possible.
func (o *options) debugf(format string, args ...any) {
	if !o.verbose {
		return
	}
	fmt.Fprintf(o.stderr, "polyql: "+format+"\n", args...)
}

// exitCodeError carries an exit code out of a command's RunE.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

// fidelityFailure is returned when the translation ran but lost something. It
// carries no message of its own: the report already said what happened, and
// repeating it as an error would say it twice.
var fidelityFailure = &exitCodeError{code: exitFidelity, err: errors.New("")}

// fatalf returns an error that exits with the fatal code.
func fatalf(format string, args ...any) error {
	return &exitCodeError{code: exitError, err: fmt.Errorf(format, args...)}
}

// pipedStdin returns os.Stdin when something is piped or redirected into it,
// and nil when it is an interactive terminal.
//
// The distinction is what lets "polyql translate --from promql --to logql" with
// no query read a pipe, while the same command typed at a prompt reports that it
// needs a query instead of hanging on a terminal that will never send one.
func pipedStdin() io.Reader {
	info, err := os.Stdin.Stat()
	if err != nil {
		// Nothing readable can be assumed about a stdin that will not stat.
		return nil
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return os.Stdin
}

// newRootCommand builds the command tree over the given streams. A nil stdin
// means nothing was piped in, and the commands that can read one say so rather
// than blocking.
func newRootCommand(stdin io.Reader, stdout, stderr io.Writer) (*cobra.Command, *options) {
	opts := &options{stdin: stdin, stdout: stdout, stderr: stderr}

	root := &cobra.Command{
		Use:   "polyql",
		Short: "Observability query translator",
		Long: "polyql translates observability queries between query languages through a\n" +
			"shared telemetry IR aligned to the CNCF query language semantic specification.\n\n" +
			"Every translation comes with a fidelity report saying what the target language\n" +
			"could not express, because a translator that hides its losses is worse than one\n" +
			"that refuses the work.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&opts.registryDir, "registry-dir", "",
		"directory of language definitions; the compiled-in set is used when omitted")
	root.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false,
		"log timing and translation detail to stderr")

	root.AddCommand(
		newTranslateCommand(opts),
		newDashboardCommand(opts),
		newRegistryCommand(opts),
		newVersionCommand(opts),
	)
	return root, opts
}

func main() {
	root, opts := newRootCommand(pipedStdin(), os.Stdout, os.Stderr)

	if err := root.Execute(); err != nil {
		code := exitError
		var coded *exitCodeError
		if errors.As(err, &coded) {
			code = coded.code
		}
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(opts.stderr, "polyql: "+msg)
		}
		os.Exit(code)
	}
}
