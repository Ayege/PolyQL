//go:build js && wasm

// Command polyql-wasm is the translator compiled for the browser.
//
// It is the same pipeline the CLI and the proxy drive — pkg/compiler's
// Translate, over the embedded registry — with a JavaScript calling convention
// instead of a command line. Nothing is reimplemented here, which is the point:
// a playground that answered differently from the binary would be worse than no
// playground, because it would be believed.
//
// There is no network in this program. A query typed into the page is parsed,
// translated and reported on inside the tab, and the result never leaves it.
package main

import (
	"context"
	"syscall/js"

	"github.com/polyql/polyql/pkg/compiler"
	"github.com/polyql/polyql/pkg/compiler/fidelity"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/registry"

	// Imported for their registration side effects, exactly as the CLI does:
	// which languages the playground offers is decided by what it imports.
	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/traceql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/traceql"
)

// Build information, set at link time the way the other binaries' is.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	reg, err := registry.DefaultRegistry()
	if err != nil {
		// Without a registry there is no translation to offer. Report it to the
		// page rather than exiting, so it can say so instead of hanging on a
		// runtime that never became ready.
		js.Global().Set("polyqlLoadError", js.ValueOf(err.Error()))
		select {}
	}

	js.Global().Set("polyqlTranslate", js.FuncOf(translateFunc(reg)))
	js.Global().Set("polyqlLanguages", js.FuncOf(languagesFunc(reg)))
	js.Global().Set("polyqlDetect", js.FuncOf(detectFunc()))
	js.Global().Set("polyqlSurvey", js.FuncOf(surveyFunc(reg)))
	js.Global().Set("polyqlVersion", js.ValueOf(map[string]any{
		"version": version,
		"commit":  commit,
	}))

	// Signal readiness last, so the page never calls a function that is not yet
	// installed.
	if ready := js.Global().Get("polyqlReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}

	// Block forever: exiting would tear down the exported functions.
	select {}
}

// translateFunc returns the JavaScript entry point, closing over the registry so
// it is loaded once rather than per keystroke.
//
// It never panics into JavaScript. A syscall/js panic unwinds through the
// runtime and kills the exported functions with it, so every failure comes back
// as a value the page can render.
func translateFunc(reg *registry.Registry) func(js.Value, []js.Value) any {
	return func(_ js.Value, args []js.Value) (result any) {
		defer func() {
			if r := recover(); r != nil {
				result = failure("internal error: translation panicked")
			}
		}()

		if len(args) < 3 {
			return failure("translate expects (source, target, query)")
		}
		source, target, query := args[0].String(), args[1].String(), args[2].String()

		res, err := compiler.Translate(context.Background(), compiler.Request{
			SourceDSL: source,
			TargetDSL: target,
			Query:     query,
			Registry:  reg,
		})
		if err != nil {
			return failure(err.Error())
		}

		return map[string]any{
			"ok":     true,
			"output": res.Output,
			"text":   res.Text,
			"notes":  strings(res.Notes),
			"report": reportValue(res.Report),
		}
	}
}

// languagesFunc reports what this build can translate, so the page's language
// pickers are populated from the binary rather than hard-coded in HTML that
// could fall out of step with it.
func languagesFunc(reg *registry.Registry) func(js.Value, []js.Value) any {
	return func(js.Value, []js.Value) any {
		return strings(reg.List())
	}
}

// failure is the shape the page renders when a query could not be translated at
// all — a parse error, an unknown language. It is deliberately the same shape as
// a success with ok:false, so the caller has one branch and not two.
func failure(message string) map[string]any {
	return map[string]any{"ok": false, "error": message}
}

// reportValue flattens the fidelity report into values syscall/js can carry.
// js.ValueOf handles only a fixed set of Go types, so every field is converted
// explicitly rather than handed over as a struct.
func reportValue(r *fidelity.Report) any {
	if r == nil {
		return nil
	}

	nodes := make([]any, 0, len(r.Nodes))
	for _, n := range r.Nodes {
		nodes = append(nodes, map[string]any{
			"path":   n.Path,
			"type":   n.NodeType,
			"flag":   n.Flag.String(),
			"reason": n.Reason,
		})
	}

	value := map[string]any{
		"score":       r.FidelityScore,
		"total":       r.TotalNodes,
		"full":        r.FullCount,
		"partial":     r.PartialCount,
		"unsupported": r.UnsupportedCount,
		"worstFlag":   r.WorstFlag.String(),
		"worstReason": r.WorstReason,
		"summary":     r.Summary,
		"nodes":       nodes,
	}
	if r.SignalMismatch != nil {
		value["signalMismatch"] = r.SignalMismatch.Message
	}
	return value
}

// detectFunc reports which languages will parse the given text.
//
// This is not language identification and does not guess: it asks each
// registered parser whether it accepts the input, and returns those that do. A
// query can legitimately come back with more than one — "{app=\"x\"}" is a valid
// stream selector and a valid spanset — and the page presents the answer as
// "these parsers accept it", never as "this is LogQL".
//
// It exists so that changing the source language can say something useful when
// the text stops parsing, instead of showing a parse error for a language the
// user never claimed the query was written in.
func detectFunc() func(js.Value, []js.Value) any {
	return func(_ js.Value, args []js.Value) (result any) {
		defer func() {
			if r := recover(); r != nil {
				result = []any{}
			}
		}()

		if len(args) < 1 {
			return []any{}
		}
		query := args[0].String()
		if query == "" {
			return []any{}
		}

		var accepted []string
		for _, dsl := range parser.List() {
			front, err := parser.Get(dsl)
			if err != nil {
				continue
			}
			if _, err := front.Parse(query); err == nil {
				accepted = append(accepted, dsl)
			}
		}
		return strings(accepted)
	}
}

// surveyFunc translates one query into every registered language and reports
// what each would cost, so the target picker can show the price of a choice
// before it is made rather than after.
//
// Every entry is a real translation run through the real pipeline. Nothing here
// estimates: a target listed as losing something lost it, and the same call with
// that target selected produces exactly the output the page then shows.
func surveyFunc(reg *registry.Registry) func(js.Value, []js.Value) any {
	return func(_ js.Value, args []js.Value) (result any) {
		defer func() {
			if r := recover(); r != nil {
				result = []any{}
			}
		}()

		if len(args) < 2 {
			return []any{}
		}
		source, query := args[0].String(), args[1].String()

		targets := reg.List()
		out := make([]any, 0, len(targets))
		for _, target := range targets {
			entry := map[string]any{"target": target}

			res, err := compiler.Translate(context.Background(), compiler.Request{
				SourceDSL: source,
				TargetDSL: target,
				Query:     query,
				Registry:  reg,
			})
			if err != nil {
				// A parse failure is the source language's problem and is the
				// same for every target, so it is reported per entry rather than
				// silently dropping the target from the list.
				entry["ok"] = false
				entry["error"] = err.Error()
				out = append(out, entry)
				continue
			}

			entry["ok"] = true
			entry["score"] = res.Report.FidelityScore
			entry["partial"] = res.Report.PartialCount
			entry["unsupported"] = res.Report.UnsupportedCount
			entry["worstFlag"] = res.Report.WorstFlag.String()
			entry["signalMismatch"] = res.Report.SignalMismatch != nil
			// Whether anything was written at all is the distinction a picker
			// most needs: a target that produced no query is a different kind of
			// answer from one that produced an approximate query.
			entry["empty"] = res.Output == ""
			out = append(out, entry)
		}
		return out
	}
}

// strings converts a Go slice for js.ValueOf, which accepts []any and not
// []string.
func strings(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
