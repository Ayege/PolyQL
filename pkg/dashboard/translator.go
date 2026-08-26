package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/fidelity"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/compiler/resolver"
	"github.com/polyql/polyql/pkg/compiler/validator"
	"github.com/polyql/polyql/pkg/registry"
)

// PanelReport is what happened to one query in one panel.
type PanelReport struct {
	PanelID     int    `json:"panel_id"`
	PanelTitle  string `json:"panel_title"`
	TargetRefID string `json:"target_ref_id"`
	// PanelPath locates a panel inside a row, so two panels sharing an id are
	// still distinguishable.
	PanelPath      string           `json:"panel_path"`
	OriginalExpr   string           `json:"original_expr"`
	TranslatedExpr string           `json:"translated_expr"`
	Notes          []string         `json:"notes"`
	Fidelity       *fidelity.Report `json:"fidelity,omitempty"`
	// Error is set when the expression could not be translated. The panel's
	// expression is then left exactly as it was, because a half-translated
	// query is worse than an untranslated one.
	Error error `json:"-"`
	// ErrorText carries the same message through JSON, which cannot hold an
	// error value.
	ErrorText string `json:"error,omitempty"`
}

// Failed reports whether the query could not be translated.
func (r PanelReport) Failed() bool { return r.Error != nil }

// Label identifies the query in a combined report.
func (r PanelReport) Label() string {
	name := r.PanelTitle
	if name == "" {
		name = fmt.Sprintf("panel %d", r.PanelID)
	}
	if r.TargetRefID != "" {
		return fmt.Sprintf("%s [%s]", name, r.TargetRefID)
	}
	return name
}

// TranslateResult is a translated dashboard together with the account of what
// the translation cost.
type TranslateResult struct {
	Dashboard      *Dashboard             `json:"-"`
	PanelReports   []PanelReport          `json:"panels"`
	Summary        *fidelity.Report       `json:"summary"`
	SignalMismatch *ir.SignalMismatchInfo `json:"signal_mismatch,omitempty"`
	SourceDSL      string                 `json:"source_dsl"`
	TargetDSL      string                 `json:"target_dsl"`
}

// Failures returns the queries that could not be translated.
func (r *TranslateResult) Failures() []PanelReport {
	var failures []PanelReport
	for _, report := range r.PanelReports {
		if report.Failed() {
			failures = append(failures, report)
		}
	}
	return failures
}

// Translate rewrites every panel expression in a dashboard, and reports on each.
//
// One expression that will not parse does not stop the rest: a dashboard is a
// batch, and abandoning thirty good panels because of one bad one would make the
// tool useless on exactly the dashboards that most need migrating. The panel
// keeps its original expression and the report says why.
func Translate(dash *Dashboard, fromDSL, toDSL string, reg *registry.Registry) (*TranslateResult, error) {
	if dash == nil {
		return nil, fmt.Errorf("dashboard: nothing to translate")
	}
	if reg == nil {
		return nil, fmt.Errorf("dashboard: a registry is required")
	}

	fromDSL = strings.ToLower(strings.TrimSpace(fromDSL))
	toDSL = strings.ToLower(strings.TrimSpace(toDSL))

	// Both languages are checked once, up front: a missing parser is a fault in
	// the request rather than in any panel, and reporting it thirty times would
	// bury it.
	sourceParser, err := parser.Get(fromDSL)
	if err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	targetEmitter, err := emitter.Get(toDSL)
	if err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	if _, err := reg.Get(fromDSL); err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	if _, err := reg.Get(toDSL); err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}

	t := &translator{
		from:    fromDSL,
		to:      toDSL,
		reg:     reg,
		parser:  sourceParser,
		emitter: targetEmitter,
		result:  &TranslateResult{Dashboard: dash, SourceDSL: fromDSL, TargetDSL: toDSL},
	}
	t.walkPanels(dash.Panels, "")

	parts := make([]fidelity.AggregateSource, 0, len(t.result.PanelReports))
	for _, report := range t.result.PanelReports {
		if report.Fidelity == nil {
			continue
		}
		parts = append(parts, fidelity.AggregateSource{Label: report.Label(), Report: report.Fidelity})
		if report.Fidelity.SignalMismatch != nil {
			t.result.SignalMismatch = report.Fidelity.SignalMismatch
		}
	}
	t.result.Summary = fidelity.Aggregate(fromDSL, toDSL, parts)
	if t.result.SignalMismatch != nil {
		t.result.Summary.SignalMismatch = t.result.SignalMismatch
	}

	return t.result, nil
}

type translator struct {
	from    string
	to      string
	reg     *registry.Registry
	parser  parser.Parser
	emitter emitter.Emitter
	result  *TranslateResult
}

// walkPanels descends into rows, which hold their own panels.
func (t *translator) walkPanels(panels []Panel, prefix string) {
	for i := range panels {
		panel := &panels[i]

		path := fmt.Sprintf("panels[%d]", i)
		if prefix != "" {
			path = prefix + "." + path
		}

		for j := range panel.Targets {
			t.translateTarget(panel, &panel.Targets[j], path)
		}
		// A row's panels are panels in their own right.
		t.walkPanels(panel.Panels, path)
	}
}

func (t *translator) translateTarget(panel *Panel, target *Target, path string) {
	if strings.TrimSpace(target.Expr) == "" {
		// A target with no expression is not a query — a panel may carry one
		// for its own reasons.
		return
	}

	report := PanelReport{
		PanelID:      panel.ID,
		PanelTitle:   panel.Title,
		TargetRefID:  target.RefID,
		PanelPath:    path,
		OriginalExpr: target.Expr,
		Notes:        []string{},
	}

	node, err := t.parser.Parse(target.Expr)
	if err != nil {
		report.Error = fmt.Errorf("parsing: %w", err)
		report.ErrorText = report.Error.Error()
		t.result.PanelReports = append(t.result.PanelReports, report)
		return
	}

	resolved, err := resolver.Resolve(node, t.from, t.reg)
	if err != nil {
		report.Error = fmt.Errorf("resolving: %w", err)
		report.ErrorText = report.Error.Error()
		t.result.PanelReports = append(t.result.PanelReports, report)
		return
	}

	_, issues, mismatch := validator.Validate(resolved, t.to, t.reg)

	text, err := t.emitter.Emit(resolved, t.reg)
	if err != nil {
		report.Error = fmt.Errorf("emitting: %w", err)
		report.ErrorText = report.Error.Error()
		t.result.PanelReports = append(t.result.PanelReports, report)
		return
	}

	notes, query := splitNotes(text)
	report.Notes = notes
	report.TranslatedExpr = query

	findings := make([]fidelity.Finding, 0, len(issues))
	for _, issue := range issues {
		findings = append(findings, fidelity.Finding{
			Path: issue.Path, Flag: issue.Flag, Reason: issue.Reason,
		})
	}
	report.Fidelity = fidelity.GenerateWithIssues(resolved, findings, t.from, t.to, mismatch)

	// The panel carries the query alone. The notes belong in the report, not in
	// a dashboard field Grafana would try to execute.
	target.Expr = query

	t.result.PanelReports = append(t.result.PanelReports, report)
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

// ReadDashboard reads a dashboard from a file.
func ReadDashboard(path string) (*Dashboard, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dashboard: reading %s: %w", path, err)
	}
	return ParseDashboard(data, path)
}

// ParseDashboard decodes dashboard JSON that has already been read.
//
// source names where the bytes came from — a path, a URL — and is used only to
// say which document failed to parse. It exists so that a dashboard fetched over
// HTTP and one read from disk decode through exactly the same code, rather than
// through two copies that can drift.
func ParseDashboard(data []byte, source string) (*Dashboard, error) {
	var dash Dashboard
	if err := json.Unmarshal(data, &dash); err != nil {
		return nil, fmt.Errorf("dashboard: parsing %s: %w", source, err)
	}
	return &dash, nil
}

// WriteDashboard writes a dashboard to a file, indented the way Grafana exports
// one so the result diffs against the original.
func WriteDashboard(dash *Dashboard, path string) error {
	data, err := Marshal(dash)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("dashboard: writing %s: %w", path, err)
	}
	return nil
}

// Marshal renders a dashboard as indented JSON with a trailing newline.
func Marshal(dash *Dashboard) ([]byte, error) {
	if dash == nil {
		return nil, fmt.Errorf("dashboard: nothing to write")
	}
	// Marshaling then re-indenting keeps the key order the custom marshallers
	// preserved, which json.MarshalIndent alone would not. marshalRaw is used
	// rather than json.Marshal so that an ampersand in a panel title survives
	// as an ampersand.
	compact, err := marshalRaw(dash)
	if err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, compact, "", "  "); err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	indented.WriteByte('\n')
	return indented.Bytes(), nil
}

// TranslatedCount reports how many expressions were rewritten.
func (r *TranslateResult) TranslatedCount() int {
	count := 0
	for _, report := range r.PanelReports {
		if !report.Failed() {
			count++
		}
	}
	return count
}

// WorstFlag is the most severe verdict anywhere in the dashboard.
func (r *TranslateResult) WorstFlag() ir.TranslatabilityFlag {
	if r.Summary == nil {
		return ir.TranslatabilityFull
	}
	return r.Summary.WorstFlag
}
