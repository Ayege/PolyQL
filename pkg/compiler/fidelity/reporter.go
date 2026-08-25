// Package fidelity reports how faithfully a query survived translation.
//
// PolyQL's value rests on this being honest. A translator that quietly
// approximates is worse than one that refuses, because the user cannot tell the
// difference until an alert misfires. The reporter therefore states what was
// lost, where, and why, in the terms of the query the user actually wrote.
//
// It reads; it never decides. Every verdict was reached by the validator and
// left on the tree, and the reporter's job is to gather those verdicts, count
// them, and render them. Nothing here re-derives a flag or second-guesses one.
package fidelity

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/ir"
)

// NodeReport is one node the target could not express exactly.
type NodeReport struct {
	// Path locates the node in the query, as the validator names it —
	// "Query.Pipeline[1].FilterStage.Predicate".
	Path string `json:"path"`
	// NodeType is the IR node's type, without its package qualifier.
	NodeType string `json:"node_type"`
	// Flag is the verdict: PARTIAL for an approximation, UNSUPPORTED for a
	// construct with no equivalent at all.
	Flag ir.TranslatabilityFlag `json:"flag"`
	// Reason is the validator's explanation.
	Reason string `json:"reason"`
}

// Report is the fidelity of one translation.
type Report struct {
	SourceDSL string `json:"source_dsl"`
	TargetDSL string `json:"target_dsl"`
	// TotalNodes is every IR node the query contains, whatever its verdict.
	TotalNodes       int `json:"total_nodes"`
	FullCount        int `json:"full"`
	PartialCount     int `json:"partial"`
	UnsupportedCount int `json:"unsupported"`
	// FidelityScore is the share of nodes that survived intact, in [0, 1]. A
	// query with no nodes scores 1: there was nothing to lose.
	//
	// It is deliberately a blunt measure. One unsupported join in a large query
	// scores well while making the translation useless, so the score is a
	// summary to sort by, not a verdict to act on — Nodes is what says whether
	// the result can be trusted.
	FidelityScore float64 `json:"fidelity_score"`
	// WorstFlag is the most severe verdict anywhere in the query.
	WorstFlag ir.TranslatabilityFlag `json:"worst_flag"`
	// WorstReason is that verdict's explanation.
	WorstReason string `json:"worst_reason,omitempty"`
	// SignalMismatch is set when the query reads one class of telemetry and the
	// target reads another. It is nil when they agree.
	//
	// It sits outside the counts and outside FidelityScore on purpose: it is not
	// a translation loss. See ir.SignalMismatchInfo.
	SignalMismatch *ir.SignalMismatchInfo `json:"signal_mismatch,omitempty"`
	// Summary is the one-line form.
	Summary string `json:"summary"`
	// Nodes holds every finding, approximations before refusals. A fully
	// faithful translation leaves it empty.
	//
	// One node can draw more than one verdict — a query that the target cannot
	// hold at all and whose absent-data convention also differs — and a node
	// carries only the worst of them. GenerateWithIssues recovers the rest from
	// the validator's own list, so len(Nodes) may exceed the count of non-FULL
	// nodes. The counts above stay a census of nodes; this stays a list of
	// findings.
	Nodes []NodeReport `json:"nodes"`
}

// Generate walks a validated query and reports what the target could not
// express.
//
// The query must already have been through the validator: this reads the flags
// it left, and an unvalidated tree is all FULL and will report a perfect score
// it has not earned.
//
// Prefer GenerateWithIssues wherever the validator's issue list is to hand. A
// node keeps only its worst verdict, so a query judged twice — rejected for one
// reason and warned about for another — loses the milder finding here. This
// function is for the simple case where a node can only have been judged once.
func Generate(query *ir.Query, sourceDSL, targetDSL string, mismatch ...*ir.SignalMismatchInfo) *Report {
	var signalMismatch *ir.SignalMismatchInfo
	if len(mismatch) > 0 {
		signalMismatch = mismatch[0]
	}
	report := &Report{
		SourceDSL:      strings.ToLower(strings.TrimSpace(sourceDSL)),
		TargetDSL:      strings.ToLower(strings.TrimSpace(targetDSL)),
		FidelityScore:  1,
		SignalMismatch: signalMismatch,
		Nodes:          []NodeReport{},
	}
	if query == nil {
		report.Summary = report.summarize()
		return report
	}

	// The traversal is ir.InspectPath, the same one the validator used to place
	// its verdicts. Sharing it is what guarantees the report names every node
	// that was judged, and judges none that were not.
	ir.InspectPath(query, "Query", func(path string, node ir.Node) bool {
		report.TotalNodes++

		flag, reason := node.Base().Translatability()
		switch flag {
		case ir.TranslatabilityFull:
			report.FullCount++
			return true
		case ir.TranslatabilityPartial:
			report.PartialCount++
		default:
			report.UnsupportedCount++
		}

		report.Nodes = append(report.Nodes, NodeReport{
			Path:     path,
			NodeType: ir.NodeTypeName(node),
			Flag:     flag,
			Reason:   reason,
		})
		return true
	})

	if report.TotalNodes > 0 {
		report.FidelityScore = float64(report.FullCount) / float64(report.TotalNodes)
	}
	// The worst verdict comes from the IR's own rollup rather than being
	// recomputed here, so the two can never disagree.
	report.WorstFlag, report.WorstReason = ir.WorstTranslatability(query)

	// Approximations come before refusals: the reader wants to know what
	// changed shape before what vanished, and every rendering follows the same
	// order.
	sort.SliceStable(report.Nodes, func(i, j int) bool {
		return report.Nodes[i].Flag < report.Nodes[j].Flag
	})

	report.Summary = report.summarize()
	return report
}

// Finding is one verdict the validator reached, in the form the reporter merges.
// It mirrors a validator.ValidationIssue without this package having to depend
// on that one.
type Finding struct {
	Path   string
	Flag   ir.TranslatabilityFlag
	Reason string
}

// GenerateWithIssues reports on a query and folds in every verdict the validator
// reached, not only the one each node ended up carrying.
//
// A node holds a single flag and reason — its worst — so a query rejected for
// its signal type and separately warned about its absent-data convention keeps
// only the rejection. Reading flags alone would drop the warning, and a fidelity
// report that quietly drops findings is the one thing this package must not do.
//
// Pass the issues the validator returned. Findings already visible from the
// flags are not repeated.
func GenerateWithIssues(query *ir.Query, findings []Finding, sourceDSL, targetDSL string,
	mismatch ...*ir.SignalMismatchInfo) *Report {

	report := Generate(query, sourceDSL, targetDSL, mismatch...)

	// Index what the flags already said, so a verdict is not listed twice.
	seen := make(map[string]bool, len(report.Nodes))
	nodeTypes := make(map[string]string, len(report.Nodes))
	for _, node := range report.Nodes {
		seen[node.Path+"\x00"+node.Reason] = true
	}
	if query != nil {
		ir.InspectPath(query, "Query", func(path string, node ir.Node) bool {
			nodeTypes[path] = ir.NodeTypeName(node)
			return true
		})
	}

	for _, finding := range findings {
		if finding.Flag == ir.TranslatabilityFull {
			continue
		}
		if seen[finding.Path+"\x00"+finding.Reason] {
			continue
		}
		seen[finding.Path+"\x00"+finding.Reason] = true

		nodeType := nodeTypes[finding.Path]
		if nodeType == "" {
			nodeType = "Query"
		}
		report.Nodes = append(report.Nodes, NodeReport{
			Path:     finding.Path,
			NodeType: nodeType,
			Flag:     finding.Flag,
			Reason:   finding.Reason,
		})
	}

	sort.SliceStable(report.Nodes, func(i, j int) bool {
		return report.Nodes[i].Flag < report.Nodes[j].Flag
	})
	report.Summary = report.summarize()
	return report
}

// IsFullyTranslated reports whether every construct survived intact.
func (r *Report) IsFullyTranslated() bool { return len(r.Nodes) == 0 }

// Partials returns the approximated nodes.
func (r *Report) Partials() []NodeReport { return r.byFlag(ir.TranslatabilityPartial) }

// Unsupported returns the nodes with no equivalent in the target.
func (r *Report) Unsupported() []NodeReport { return r.byFlag(ir.TranslatabilityUnsupported) }

func (r *Report) byFlag(flag ir.TranslatabilityFlag) []NodeReport {
	var matches []NodeReport
	for _, node := range r.Nodes {
		if node.Flag == flag {
			matches = append(matches, node)
		}
	}
	return matches
}

func (r *Report) summarize() string {
	summary := fmt.Sprintf("%d nodes: %d full, %d partial, %d unsupported (score: %s)",
		r.TotalNodes, r.FullCount, r.PartialCount, r.UnsupportedCount, formatScore(r.FidelityScore))
	if r.IsFullyTranslated() {
		summary += " — all constructs translated fully"
	}
	return summary
}

// formatScore renders the score to two decimal places. The field keeps its full
// precision; only what a person reads is rounded.
func formatScore(score float64) string { return fmt.Sprintf("%.2f", score) }

// percent renders a count as a share of the total.
func percent(count, total int) string {
	if total == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(count)/float64(total))
}

// ToJSON renders the report as indented JSON, for a machine to consume or a
// pipeline to archive.
func (r *Report) ToJSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// ToText renders the report for a terminal.
func (r *Report) ToText() string {
	var b strings.Builder

	fmt.Fprintf(&b, "PolyQL fidelity report: %s → %s\n", r.SourceDSL, r.TargetDSL)
	b.WriteString(strings.Repeat("─", 40) + "\n")

	// The signal mismatch comes first and stands apart: it says the result
	// cannot run, which is a different thing from saying the translation lost
	// something, and reading the score without it would mislead.
	if r.SignalMismatch != nil {
		fmt.Fprintf(&b, "ⓘ Signal type: %s\n", r.SignalMismatch.Message)
		b.WriteString("  This query cannot execute on the target backend.\n")
		b.WriteString("  Construct-level translation below is still valid.\n\n")
	}

	fmt.Fprintf(&b, "Total nodes:    %d\n", r.TotalNodes)
	fmt.Fprintf(&b, "Full:           %d (%s)\n", r.FullCount, percent(r.FullCount, r.TotalNodes))
	// The counts that are zero are left out: a clean report should read as
	// clean, not as a list of absences.
	if r.PartialCount > 0 {
		fmt.Fprintf(&b, "Partial:        %d (%s)\n", r.PartialCount, percent(r.PartialCount, r.TotalNodes))
	}
	if r.UnsupportedCount > 0 {
		fmt.Fprintf(&b, "Unsupported:    %d (%s)\n", r.UnsupportedCount,
			percent(r.UnsupportedCount, r.TotalNodes))
	}
	fmt.Fprintf(&b, "Fidelity score: %s\n", formatScore(r.FidelityScore))

	if r.IsFullyTranslated() {
		b.WriteString("\n✓ All constructs translated fully.\n")
		return b.String()
	}

	writeSection(&b, "⚠ Partial translations:", r.Partials())
	writeSection(&b, "✗ Unsupported translations:", r.Unsupported())
	return b.String()
}

func writeSection(b *strings.Builder, heading string, nodes []NodeReport) {
	if len(nodes) == 0 {
		return
	}
	b.WriteString("\n" + heading + "\n")
	for _, node := range nodes {
		fmt.Fprintf(b, "  %s (%s)\n", node.Path, node.NodeType)
		if node.Reason != "" {
			fmt.Fprintf(b, "    %s\n", node.Reason)
		}
	}
}

// ToMarkdown renders the report for a pull request comment or a document.
func (r *Report) ToMarkdown() string {
	var b strings.Builder

	fmt.Fprintf(&b, "### PolyQL fidelity report: %s → %s\n\n", r.SourceDSL, r.TargetDSL)

	if r.SignalMismatch != nil {
		fmt.Fprintf(&b, "> ⓘ **Signal type mismatch**: %s.\n", r.SignalMismatch.Message)
		b.WriteString("> This query cannot execute on the target backend, though the\n")
		b.WriteString("> construct-level translation below is still valid.\n\n")
	}

	b.WriteString("| Metric | Count | % |\n")
	b.WriteString("|--------|-------|---|\n")
	fmt.Fprintf(&b, "| Full | %d | %s |\n", r.FullCount, percent(r.FullCount, r.TotalNodes))
	fmt.Fprintf(&b, "| Partial | %d | %s |\n", r.PartialCount, percent(r.PartialCount, r.TotalNodes))
	fmt.Fprintf(&b, "| Unsupported | %d | %s |\n", r.UnsupportedCount,
		percent(r.UnsupportedCount, r.TotalNodes))
	fmt.Fprintf(&b, "| **Total** | **%d** | **Score: %s** |\n",
		r.TotalNodes, formatScore(r.FidelityScore))

	if r.IsFullyTranslated() {
		b.WriteString("\n✓ All constructs translated fully.\n")
		return b.String()
	}

	writeMarkdownSection(&b, "Partial", r.Partials())
	writeMarkdownSection(&b, "Unsupported", r.Unsupported())
	return b.String()
}

func writeMarkdownSection(b *strings.Builder, heading string, nodes []NodeReport) {
	if len(nodes) == 0 {
		return
	}
	fmt.Fprintf(b, "\n**%s:**\n", heading)
	for _, node := range nodes {
		reason := node.Reason
		if reason == "" {
			reason = "no reason recorded"
		}
		// A path can hold characters a table would swallow, so it is written as
		// code.
		fmt.Fprintf(b, "- `%s` (%s): %s\n", node.Path, node.NodeType, reason)
	}
}

// AggregateSource is one report to fold into a combined one.
type AggregateSource struct {
	// Label identifies where the report came from. It prefixes each finding's
	// path, so a combined report over many queries can say which one a finding
	// belongs to.
	Label  string
	Report *Report
}

// Aggregate combines several reports into one.
//
// The counts sum and the score is recomputed from the totals, which weights a
// large query more heavily than a small one — that is the intent: a dashboard
// where one panel of thirty fails is in better shape than one where the only
// panel does.
//
// Reports that are nil, or that came from a query which failed before it could
// be judged, are skipped rather than counted as perfect.
func Aggregate(sourceDSL, targetDSL string, parts []AggregateSource) *Report {
	combined := &Report{
		SourceDSL:     strings.ToLower(strings.TrimSpace(sourceDSL)),
		TargetDSL:     strings.ToLower(strings.TrimSpace(targetDSL)),
		FidelityScore: 1,
		Nodes:         []NodeReport{},
	}

	for _, part := range parts {
		if part.Report == nil {
			continue
		}
		if combined.SignalMismatch == nil && part.Report.SignalMismatch != nil {
			combined.SignalMismatch = part.Report.SignalMismatch
		}
		combined.TotalNodes += part.Report.TotalNodes
		combined.FullCount += part.Report.FullCount
		combined.PartialCount += part.Report.PartialCount
		combined.UnsupportedCount += part.Report.UnsupportedCount

		for _, node := range part.Report.Nodes {
			if part.Label != "" {
				node.Path = part.Label + ": " + node.Path
			}
			combined.Nodes = append(combined.Nodes, node)
		}
		if part.Report.WorstFlag > combined.WorstFlag {
			combined.WorstFlag = part.Report.WorstFlag
			combined.WorstReason = part.Report.WorstReason
		}
	}

	if combined.TotalNodes > 0 {
		combined.FidelityScore = float64(combined.FullCount) / float64(combined.TotalNodes)
	}
	// A signal mismatch belongs to the translation as a whole rather than to any
	// one part of it, so a caller sets it once on the combined report rather
	// than having it summed here.

	sort.SliceStable(combined.Nodes, func(i, j int) bool {
		return combined.Nodes[i].Flag < combined.Nodes[j].Flag
	})
	combined.Summary = combined.summarize()
	return combined
}
