package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/polyql/polyql/pkg/compiler/emitter"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/registry"
)

func newRegistryCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Inspect and check language definitions",
		Long: "The registry holds one YAML definition per language: its functions and how\n" +
			"each maps onto the IR, its operator spellings, its type coercions and what it\n" +
			"can and cannot express. These commands read it.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		newRegistryListCommand(opts),
		newRegistryValidateCommand(opts),
		newRegistryDiffCommand(opts),
	)
	return cmd
}

func newRegistryListCommand(opts *options) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the languages this binary knows",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			reg, err := opts.registry()
			if err != nil {
				return fatalf("%s", err)
			}

			type entry struct {
				DSL        string   `json:"dsl"`
				Signals    []string `json:"signals"`
				Functions  int      `json:"functions"`
				Operators  int      `json:"operators"`
				CanParse   bool     `json:"can_parse"`
				CanEmit    bool     `json:"can_emit"`
				SourcePath string   `json:"source"`
			}

			entries := make([]entry, 0, len(reg.List()))
			for _, name := range reg.List() {
				def, err := reg.Get(name)
				if err != nil {
					return fatalf("%s", err)
				}
				signals := make([]string, 0, len(def.SupportedSignalTypes))
				for _, signal := range def.SupportedSignalTypes {
					signals = append(signals, strings.ToLower(signal.String()))
				}
				_, parseErr := parser.Get(name)
				_, emitErr := emitter.Get(name)
				entries = append(entries, entry{
					DSL:        def.DSL,
					Signals:    signals,
					Functions:  len(def.Functions),
					Operators:  len(def.Operators),
					CanParse:   parseErr == nil,
					CanEmit:    emitErr == nil,
					SourcePath: def.SourcePath,
				})
			}

			if asJSON {
				encoder := json.NewEncoder(opts.stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(entries)
			}

			w := tabwriter.NewWriter(opts.stdout, 0, 0, 4, ' ', 0)
			fmt.Fprintln(w, "DSL\tSIGNALS\tFUNCTIONS\tOPERATORS\tDIRECTION")
			for _, e := range entries {
				fmt.Fprintf(w, "%s\tsignals: %s\t%d\t%d\t%s\n",
					e.DSL, strings.Join(e.Signals, ", "), e.Functions, e.Operators,
					describeDirection(e.CanParse, e.CanEmit))
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

// describeDirection says whether this binary can read a language, write it, or
// both — a definition can exist without a front or back end for it.
func describeDirection(canParse, canEmit bool) string {
	switch {
	case canParse && canEmit:
		return "read, write"
	case canParse:
		return "read only"
	case canEmit:
		return "write only"
	default:
		return "definition only"
	}
}

func newRegistryValidateCommand(opts *options) *cobra.Command {
	var dir string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check that a directory of language definitions loads",
		Long: "Validate loads every definition in a directory and reports what is wrong with\n" +
			"it — malformed YAML, an unknown IR symbol, a missing required field, an arity\n" +
			"that disagrees with its argument list.\n\n" +
			"It is for contributors: run it against a definition before opening a pull\n" +
			"request, rather than discovering the problem through a mistranslation.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			target := dir
			if target == "" {
				target = opts.registryDir
			}

			var defs map[string]*registry.DSLDefinition
			var err error
			if target == "" {
				defs, err = registry.LoadEmbedded()
				target = "the compiled-in registry"
			} else {
				defs, err = registry.LoadDir(target)
			}
			if err != nil {
				fmt.Fprintf(opts.stdout, "✗ %s\n", err)
				return &exitCodeError{code: exitFidelity, err: fmt.Errorf("")}
			}

			names := make([]string, 0, len(defs))
			for name := range defs {
				names = append(names, name)
			}
			sort.Strings(names)

			fmt.Fprintf(opts.stdout, "✓ %s: %d definition%s load cleanly\n",
				target, len(names), pluralS(len(names)))
			for _, name := range names {
				def := defs[name]
				fmt.Fprintf(opts.stdout, "  %s: %d functions, %d operators, %d type coercions\n",
					def.DSL, len(def.Functions), len(def.Operators), len(def.TypeCoercion))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dir, "dir", "", "directory to check; the compiled-in set when omitted")
	return cmd
}

func newRegistryDiffCommand(opts *options) *cobra.Command {
	var dir string

	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare a directory of definitions against the compiled-in set",
		Long: "Diff shows what a directory of definitions changes relative to the ones built\n" +
			"into this binary: languages added or removed, and functions and operators\n" +
			"added, removed or altered.\n\n" +
			"It is for developing a definition — for seeing at a glance what an edit\n" +
			"actually changed.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if dir == "" {
				return fatalf("give the directory to compare with --dir")
			}
			base, err := registry.LoadEmbedded()
			if err != nil {
				return fatalf("%s", err)
			}
			candidate, err := registry.LoadDir(dir)
			if err != nil {
				return fatalf("%s", err)
			}

			differences := diffRegistries(base, candidate)
			if len(differences) == 0 {
				fmt.Fprintf(opts.stdout, "%s matches the compiled-in registry.\n", dir)
				return nil
			}
			fmt.Fprintf(opts.stdout, "%s differs from the compiled-in registry:\n\n", dir)
			for _, line := range differences {
				fmt.Fprintln(opts.stdout, line)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dir, "dir", "", "directory to compare (required)")
	return cmd
}

// diffRegistries reports how a candidate set differs from the built-in one, as
// lines ready to print.
func diffRegistries(base, candidate map[string]*registry.DSLDefinition) []string {
	var lines []string

	for _, dsl := range sortedKeys(base) {
		if _, ok := candidate[dsl]; !ok {
			lines = append(lines, "- "+dsl+" (language removed)")
		}
	}
	for _, dsl := range sortedKeys(candidate) {
		if _, ok := base[dsl]; !ok {
			lines = append(lines, "+ "+dsl+" (language added)")
		}
	}

	for _, dsl := range sortedKeys(candidate) {
		before, ok := base[dsl]
		if !ok {
			continue
		}
		after := candidate[dsl]

		var changes []string
		changes = append(changes, diffFunctions(before, after)...)
		changes = append(changes, diffOperators(before, after)...)
		if capabilitiesDiffer(before.Capabilities, after.Capabilities) {
			changes = append(changes, "    ~ capabilities changed")
		}
		if len(changes) == 0 {
			continue
		}
		lines = append(lines, "  "+dsl+":")
		lines = append(lines, changes...)
	}
	return lines
}

// capabilitiesDiffer compares two capability blocks. They hold slices, so they
// are not comparable with ==.
func capabilitiesDiffer(before, after registry.Capabilities) bool {
	if before.Joins != after.Joins || before.Subqueries != after.Subqueries ||
		before.PipelineOrdered != after.PipelineOrdered ||
		before.BoolModifier != after.BoolModifier {
		return true
	}
	if len(before.JoinTypes) != len(after.JoinTypes) ||
		len(before.WindowAlignments) != len(after.WindowAlignments) {
		return true
	}
	for i := range before.JoinTypes {
		if before.JoinTypes[i] != after.JoinTypes[i] {
			return true
		}
	}
	for i := range before.WindowAlignments {
		if before.WindowAlignments[i] != after.WindowAlignments[i] {
			return true
		}
	}
	return false
}

func diffFunctions(before, after *registry.DSLDefinition) []string {
	var lines []string
	for _, name := range before.FunctionNames() {
		if _, ok := after.Functions[name]; !ok {
			lines = append(lines, "    - function "+name)
		}
	}
	for _, name := range after.FunctionNames() {
		old, ok := before.Functions[name]
		if !ok {
			lines = append(lines, "    + function "+name)
			continue
		}
		current := after.Functions[name]
		if old.IRName != current.IRName || old.IsAggregation != current.IsAggregation ||
			old.AggOp != current.AggOp || old.AggScope != current.AggScope ||
			old.ReturnType != current.ReturnType || old.Arity != current.Arity {
			lines = append(lines, "    ~ function "+name+" changed")
		}
	}
	return lines
}

func diffOperators(before, after *registry.DSLDefinition) []string {
	var lines []string
	for _, symbol := range sortedOperatorKeys(before) {
		if _, ok := after.Operators[symbol]; !ok {
			lines = append(lines, "    - operator "+symbol)
		}
	}
	for _, symbol := range sortedOperatorKeys(after) {
		old, ok := before.Operators[symbol]
		if !ok {
			lines = append(lines, "    + operator "+symbol)
			continue
		}
		if *old != *after.Operators[symbol] {
			lines = append(lines, "    ~ operator "+symbol+" changed")
		}
	}
	return lines
}

func sortedKeys(defs map[string]*registry.DSLDefinition) []string {
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedOperatorKeys(def *registry.DSLDefinition) []string {
	names := make([]string, 0, len(def.Operators))
	for name := range def.Operators {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// translatableLanguages lists the languages this binary can both read and write.
func translatableLanguages() []string {
	var both []string
	for _, name := range parser.List() {
		if _, err := emitter.Get(name); err == nil {
			both = append(both, name)
		}
	}
	sort.Strings(both)
	return both
}

func joinOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return strings.Join(values, ", ")
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
