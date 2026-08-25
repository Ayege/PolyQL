package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/dashboard"
)

type dashboardOptions struct {
	*options
	from          string
	to            string
	input         string
	output        string
	report        string
	reportFormat  string
	skipErrors    bool
	failOnPartial bool
}

func newDashboardCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Translate the queries in a Grafana dashboard",
		Long: "Dashboard commands work on a Grafana dashboard as a whole, translating every\n" +
			"panel expression and leaving the rest of the document exactly as it was.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newDashboardTranslateCommand(opts))
	return cmd
}

func newDashboardTranslateCommand(opts *options) *cobra.Command {
	d := &dashboardOptions{options: opts}

	cmd := &cobra.Command{
		Use:   "translate",
		Short: "Translate every panel expression in a dashboard",
		Long: "Translate reads a Grafana dashboard, rewrites each panel's query into the\n" +
			"target language, and writes a report of what each panel cost.\n\n" +
			"Only the expressions change. Layout, datasources, field configuration,\n" +
			"annotations, templating and anything else the document carries are written\n" +
			"back untouched and in their original order, so the result diffs against the\n" +
			"original in just the places the translation touched.\n\n" +
			"A panel whose expression will not parse keeps it, and is reported. One bad\n" +
			"panel does not abandon the rest of the dashboard.",
		Example: "  polyql dashboard translate --from promql --to logql \\\n" +
			"    --input my-dashboard.json --output translated.json \\\n" +
			"    --report report.md --report-format markdown\n\n" +
			"  # Preview without writing anything.\n" +
			"  polyql dashboard translate --from promql --to logql --input my-dashboard.json",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return d.run() },
	}

	cmd.Flags().StringVar(&d.from, "from", "", "source language (required)")
	cmd.Flags().StringVar(&d.to, "to", "", "target language (required)")
	cmd.Flags().StringVar(&d.input, "input", "", "dashboard JSON to read (required)")
	cmd.Flags().StringVarP(&d.output, "output", "o", "",
		`write the translated dashboard here; "-" or unset means stdout`)
	cmd.Flags().StringVar(&d.report, "report", "",
		`write the fidelity report here; "-" means stdout, unset means stderr`)
	cmd.Flags().StringVar(&d.reportFormat, "report-format", formatText,
		"report format: text, json, or markdown")
	cmd.Flags().BoolVar(&d.skipErrors, "skip-errors", true,
		"carry on past a panel whose expression will not parse")
	cmd.Flags().BoolVar(&d.failOnPartial, "fail-on-partial", false,
		"exit non-zero when any panel is approximate, not only when one is incomplete")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.MarkFlagRequired("input")

	return cmd
}

const formatMarkdown = "markdown"

func (d *dashboardOptions) run() error {
	switch d.reportFormat {
	case formatText, formatJSON, formatMarkdown:
	default:
		return fatalf("unknown --report-format %q: expected %s, %s or %s",
			d.reportFormat, formatText, formatJSON, formatMarkdown)
	}

	reg, err := d.registry()
	if err != nil {
		return fatalf("%s", err)
	}
	d.debugf("loaded %s", strings.Join(reg.List(), ", "))

	dash, err := dashboard.ReadDashboard(d.input)
	if err != nil {
		return fatalf("%s", err)
	}

	result, err := dashboard.Translate(dash, d.from, d.to, reg)
	if err != nil {
		return fatalf("%s", err)
	}
	d.debugf("translated %d of %d quer%s across the dashboard",
		result.TranslatedCount(), len(result.PanelReports), plural(len(result.PanelReports)))

	failures := result.Failures()
	if len(failures) > 0 && !d.skipErrors {
		// --skip-errors=false makes an unparseable panel fatal, for a caller
		// that would rather fix the dashboard than translate around the gap.
		return fatalf("%d panel expression(s) would not parse; the first is %s: %s",
			len(failures), failures[0].Label(), failures[0].Error)
	}

	if err := d.writeDashboard(result); err != nil {
		return err
	}
	if err := d.writeReport(result); err != nil {
		return err
	}

	switch {
	case len(failures) > 0:
		return &exitCodeError{code: exitError, err: fmt.Errorf("")}
	case result.Summary.UnsupportedCount > 0:
		return fidelityFailure
	case result.Summary.PartialCount > 0 && d.failOnPartial:
		return fidelityFailure
	}
	return nil
}

func (d *dashboardOptions) writeDashboard(result *dashboard.TranslateResult) error {
	data, err := dashboard.Marshal(result.Dashboard)
	if err != nil {
		return fatalf("%s", err)
	}
	// "-" means stdout, the usual convention, and avoids asking the caller to
	// name a device file that not every platform lets a process open.
	if d.output == "" || d.output == "-" {
		if _, err := d.stdout.Write(data); err != nil {
			return fatalf("writing the dashboard: %s", err)
		}
		return nil
	}
	if err := os.WriteFile(d.output, data, 0o644); err != nil {
		return fatalf("writing %s: %s", d.output, err)
	}
	d.debugf("wrote the dashboard to %s", d.output)
	return nil
}

// writeReport sends the report to its own destination, defaulting to stderr so
// that piping the dashboard on stdout stays possible.
func (d *dashboardOptions) writeReport(result *dashboard.TranslateResult) error {
	out := d.stderr
	switch d.report {
	case "":
		// The report goes to stderr by default, so that piping the dashboard
		// on stdout stays possible without redirecting anything.
	case "-":
		out = d.stdout
	default:
		file, err := os.Create(d.report)
		if err != nil {
			return fatalf("creating %s: %s", d.report, err)
		}
		defer file.Close()
		out = file
	}

	switch d.reportFormat {
	case formatJSON:
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return fatalf("writing the report: %s", err)
		}
	case formatMarkdown:
		writeDashboardMarkdown(out, result)
	default:
		writeDashboardText(out, result)
	}
	return nil
}

func writeDashboardText(out io.Writer, result *dashboard.TranslateResult) {
	fmt.Fprintf(out, "PolyQL dashboard report: %s → %s\n", result.SourceDSL, result.TargetDSL)
	fmt.Fprintln(out, strings.Repeat("─", 60))

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "PANEL\tQUERY\tSCORE\tOUTCOME")
	for _, report := range result.PanelReports {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			truncate(report.PanelTitle, 28), report.TargetRefID,
			panelScore(report), panelOutcome(report))
	}
	_ = w.Flush()

	for _, report := range result.PanelReports {
		if report.Failed() {
			fmt.Fprintf(out, "\n✗ %s\n    %s\n    expression left unchanged: %s\n",
				report.Label(), report.Error, report.OriginalExpr)
			continue
		}
		if report.Fidelity == nil || report.Fidelity.IsFullyTranslated() {
			continue
		}
		fmt.Fprintf(out, "\n%s\n    %s\n → %s\n", report.Label(), report.OriginalExpr, report.TranslatedExpr)
		for _, node := range report.Fidelity.Nodes {
			fmt.Fprintf(out, "    %s %s: %s\n", marker(node.Flag), node.Path, node.Reason)
		}
	}

	fmt.Fprintln(out, "\n"+strings.Repeat("=", 60))
	fmt.Fprintf(out, "%d of %d quer%s translated. %s\n",
		result.TranslatedCount(), len(result.PanelReports),
		plural(len(result.PanelReports)), result.Summary.Summary)
}

func writeDashboardMarkdown(out io.Writer, result *dashboard.TranslateResult) {
	fmt.Fprintf(out, "# PolyQL dashboard report: %s → %s\n\n", result.SourceDSL, result.TargetDSL)
	fmt.Fprintf(out, "%d of %d queries translated. %s\n\n",
		result.TranslatedCount(), len(result.PanelReports), result.Summary.Summary)

	fmt.Fprintln(out, "| Panel | Query | Score | Outcome |")
	fmt.Fprintln(out, "|-------|-------|-------|---------|")
	for _, report := range result.PanelReports {
		fmt.Fprintf(out, "| %s | %s | %s | %s |\n",
			escapePipes(report.PanelTitle), report.TargetRefID,
			panelScore(report), panelOutcome(report))
	}

	for _, report := range result.PanelReports {
		if report.Failed() {
			fmt.Fprintf(out, "\n### %s\n\nThe expression could not be parsed and was left unchanged.\n\n"+
				"```\n%s\n```\n\n%s\n", report.Label(), report.OriginalExpr, report.Error)
			continue
		}
		if report.Fidelity == nil || report.Fidelity.IsFullyTranslated() {
			continue
		}
		fmt.Fprintf(out, "\n### %s\n\n```\n%s\n```\n\nbecomes\n\n```\n%s\n```\n\n",
			report.Label(), report.OriginalExpr, report.TranslatedExpr)
		for _, node := range report.Fidelity.Nodes {
			fmt.Fprintf(out, "- **%s** `%s`: %s\n", node.Flag, node.Path, node.Reason)
		}
	}
}

func panelScore(report dashboard.PanelReport) string {
	if report.Fidelity == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", report.Fidelity.FidelityScore)
}

func panelOutcome(report dashboard.PanelReport) string {
	switch {
	case report.Failed():
		return "not translated"
	case report.Fidelity == nil:
		return "—"
	case report.Fidelity.UnsupportedCount > 0:
		return "incomplete"
	case report.Fidelity.PartialCount > 0:
		return "approximate"
	default:
		return "exact"
	}
}

// marker is the symbol a verdict is written with, matching the fidelity
// reporter's own text rendering.
func marker(flag ir.TranslatabilityFlag) string {
	if flag == ir.TranslatabilityUnsupported {
		return "✗"
	}
	return "⚠"
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}

func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
