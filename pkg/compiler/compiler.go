// Package compiler runs the translation pipeline end to end.
//
// The stages live in the sub-packages — parser, resolver, validator, emitter,
// fidelity — and each is independently usable. What lives here is the one
// sequence that assembles them, so that a CLI, a proxy and a dashboard
// translator all drive the same code rather than three copies of it that can
// drift.
//
// # What this decides and what it leaves alone
//
// Translate reports what a translation cost. It never decides what to do about
// it: no exit code, no HTTP status, no threshold. Those are policy, they differ
// per caller, and a library that made them would force every caller to agree.
// The Result carries the fidelity report and the caller reads it.
package compiler

import (
	"context"
	"fmt"
	"strings"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/fidelity"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/compiler/resolver"
	"github.com/polyql/polyql/pkg/compiler/validator"
	"github.com/polyql/polyql/pkg/registry"
)

// Request is one query to translate.
type Request struct {
	// SourceDSL and TargetDSL name the languages, case-insensitively.
	SourceDSL string
	TargetDSL string
	// Query is the source text.
	Query string
	// Registry supplies both languages' definitions. It is required.
	Registry *registry.Registry
}

// Result is one finished translation.
type Result struct {
	SourceDSL string `json:"source_dsl"`
	TargetDSL string `json:"target_dsl"`
	Input     string `json:"input"`
	// Output is the translated query alone.
	Output string `json:"output"`
	// Notes are the comment lines the emitter wrote above the query, lifted out
	// so a machine reading this does not have to parse them back out of the
	// text.
	Notes []string `json:"notes"`
	// Text is what the emitter actually produced: the notes as comment lines
	// followed by the query. It is what a person copies, and what the target's
	// own parser must accept.
	Text string `json:"-"`
	// Report is the fidelity verdict.
	Report *fidelity.Report `json:"fidelity"`
	// Query is the validated IR tree, for a caller that wants to inspect the
	// annotations rather than read the report.
	Query *ir.Query `json:"-"`
}

// Lossless reports whether every construct translated exactly.
//
// A signal mismatch is deliberately not part of this. Whether the constructs
// translated and whether the result can run against the target backend are
// different questions, and the report keeps them apart; a caller that cares
// about the second reads Report.SignalMismatch.
func (r *Result) Lossless() bool {
	return r != nil && r.Report != nil &&
		r.Report.UnsupportedCount == 0 && r.Report.PartialCount == 0
}

// Complete reports whether the target could express every construct, allowing
// approximations. It is the weaker of the two, and the one a fail-closed policy
// usually wants: an approximation was written and explained, while an
// unsupported construct was not written at all.
func (r *Result) Complete() bool {
	return r != nil && r.Report != nil && r.Report.UnsupportedCount == 0
}

// Translate runs a query through parse, resolve, validate and emit, and reports
// what the translation cost.
//
// The error is for a translation that could not be performed — a query that will
// not parse, a language with no parser or emitter, a missing registry. A
// construct the target merely cannot express is not an error: it is what the
// Result's report is for.
func Translate(ctx context.Context, req Request) (*Result, error) {
	ctx, done := startSpan(ctx, req)

	result, err := translate(ctx, req)
	done(result, err)
	return result, err
}

func translate(ctx context.Context, req Request) (*Result, error) {
	// Translation is fast, but a caller that has already given up — a proxy
	// whose client disconnected — should not be made to wait for it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Registry == nil {
		return nil, fmt.Errorf("compiler: a registry is required")
	}
	source := strings.ToLower(strings.TrimSpace(req.SourceDSL))
	target := strings.ToLower(strings.TrimSpace(req.TargetDSL))
	if source == "" || target == "" {
		return nil, fmt.Errorf("compiler: both a source and a target language are required")
	}

	if err := CheckLanguages(source, target, req.Registry); err != nil {
		return nil, err
	}

	p, err := parser.Get(source)
	if err != nil {
		return nil, fmt.Errorf("compiler: %w", err)
	}
	node, err := p.Parse(req.Query)
	if err != nil {
		return nil, err
	}

	resolved, err := resolver.Resolve(node, source, req.Registry)
	if err != nil {
		return nil, err
	}

	// Validation annotates the tree in place; the issue list holds every
	// verdict, including ones a later, harsher finding would otherwise mask.
	_, issues, mismatch := validator.Validate(resolved, target, req.Registry)

	e, err := emitter.Get(target)
	if err != nil {
		return nil, fmt.Errorf("compiler: %w", err)
	}
	text, err := e.Emit(resolved, req.Registry)
	if err != nil {
		return nil, err
	}

	findings := make([]fidelity.Finding, 0, len(issues))
	for _, issue := range issues {
		findings = append(findings, fidelity.Finding{
			Path: issue.Path, Flag: issue.Flag, Reason: issue.Reason,
		})
	}

	notes, query := SplitNotes(text)
	return &Result{
		SourceDSL: source,
		TargetDSL: target,
		Input:     req.Query,
		Output:    query,
		Notes:     notes,
		Text:      text,
		Report:    fidelity.GenerateWithIssues(resolved, findings, source, target, mismatch),
		Query:     resolved,
	}, nil
}

// CheckLanguages verifies that both languages have a definition, a parser and an
// emitter, naming what is available when one does not.
//
// It is separate from Translate so a caller translating many queries can fail on
// a typo in the target once, up front, rather than once per query.
func CheckLanguages(sourceDSL, targetDSL string, reg *registry.Registry) error {
	if reg == nil {
		return fmt.Errorf("compiler: a registry is required")
	}
	if _, err := reg.Get(sourceDSL); err != nil {
		return fmt.Errorf("unknown source language %q (available: %s)",
			sourceDSL, strings.Join(reg.List(), ", "))
	}
	if _, err := reg.Get(targetDSL); err != nil {
		return fmt.Errorf("unknown target language %q (available: %s)",
			targetDSL, strings.Join(reg.List(), ", "))
	}
	if _, err := parser.Get(sourceDSL); err != nil {
		return fmt.Errorf("no parser for %q (this binary can read: %s)",
			sourceDSL, strings.Join(parser.List(), ", "))
	}
	if _, err := emitter.Get(targetDSL); err != nil {
		return fmt.Errorf("no emitter for %q (this binary can write: %s)",
			targetDSL, strings.Join(emitter.List(), ", "))
	}
	return nil
}

// SplitNotes separates the emitter's leading comment lines from the query.
//
// Emitters write what they could not express as comment lines above the result,
// which keeps the output parseable in the target language while still saying
// what went missing. This recovers the two halves.
func SplitNotes(text string) (notes []string, query string) {
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
