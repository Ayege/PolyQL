package compiler_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/polyql/polyql/pkg/compiler"
	"github.com/polyql/polyql/pkg/compiler/fidelity"
	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/compiler/parser"
	"github.com/polyql/polyql/pkg/compiler/resolver"
	"github.com/polyql/polyql/pkg/registry"

	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/traceql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/traceql"
)

// testdataDir holds the corpus, one YAML file per case.
const testdataDir = "../../testdata"

// scoreTolerance is how far a fidelity score may drift before the case fails.
// The score is a ratio of node counts, so any real change moves it further than
// this; the slack is for float formatting rather than for behavior.
const scoreTolerance = 0.05

// ExpectedFlag is one assertion about a node's verdict.
type ExpectedFlag struct {
	Path string `yaml:"path"`
	Flag string `yaml:"flag"`
}

// TestCase is one translation to check, as read from a YAML file.
type TestCase struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	SourceDSL   string `yaml:"source_dsl"`
	TargetDSL   string `yaml:"target_dsl"`
	Input       string `yaml:"input"`
	// ExpectedOutput is the translated query without the emitter's comment
	// lines. An empty value skips the text comparison, for a case that only
	// asserts fidelity.
	ExpectedOutput string `yaml:"expected_output"`
	// ExpectedFidelityScore is checked within scoreTolerance. A nil value skips
	// the check.
	ExpectedFidelityScore *float64       `yaml:"expected_fidelity_score"`
	ExpectedFlags         []ExpectedFlag `yaml:"expected_flags"`
	// ExpectsNoQuery marks a translation where the target can write nothing at
	// all, so the emitter produces notes and no query.
	//
	// It exists because the alternative is worse. A target that cannot express
	// any part of a query could be made to emit something parseable — an empty
	// selector, say — but that would be a silent lie about what the translation
	// achieved. Emitting only the notes is the honest outcome, and this field
	// lets a case say so out loud instead of the corpus quietly avoiding the
	// shape. A case setting it still asserts its flags, so what was lost is
	// pinned as precisely as anywhere else.
	ExpectsNoQuery bool `yaml:"expects_no_query"`
	// Bidirectional additionally translates the output back and compares the two
	// IR trees.
	Bidirectional bool   `yaml:"bidirectional"`
	Notes         string `yaml:"notes"`

	// group and file are filled in by the loader, for reporting.
	group string
	file  string
}

// LoadTestCases reads every YAML case under dir, recursively.
func LoadTestCases(dir string) ([]TestCase, error) {
	var cases []TestCase

	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}

		var testCase TestCase
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		// A case file is a fixture, so a misspelled key must fail loudly rather
		// than quietly weakening the assertion it was meant to make.
		decoder.KnownFields(true)
		if decodeErr := decoder.Decode(&testCase); decodeErr != nil {
			return fmt.Errorf("parsing %s: %w", path, decodeErr)
		}

		if validateErr := testCase.validate(); validateErr != nil {
			return fmt.Errorf("%s: %w", path, validateErr)
		}

		testCase.file = path
		testCase.group = filepath.Base(filepath.Dir(path))
		cases = append(cases, testCase)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(cases, func(i, j int) bool {
		if cases[i].group != cases[j].group {
			return cases[i].group < cases[j].group
		}
		return cases[i].file < cases[j].file
	})
	return cases, nil
}

func (c TestCase) validate() error {
	switch {
	case c.Name == "":
		return fmt.Errorf("a case needs a name")
	case c.SourceDSL == "":
		return fmt.Errorf("%s: a case needs a source_dsl", c.Name)
	case c.TargetDSL == "":
		return fmt.Errorf("%s: a case needs a target_dsl", c.Name)
	case c.Input == "":
		return fmt.Errorf("%s: a case needs an input", c.Name)
	}
	for _, flag := range c.ExpectedFlags {
		if _, err := ir.ParseTranslatabilityFlag(flag.Flag); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		if flag.Path == "" {
			return fmt.Errorf("%s: an expected flag needs a path", c.Name)
		}
	}
	return nil
}

// translation is what one run of the pipeline produced.
type translation struct {
	query  *ir.Query
	text   string
	output string
	report *fidelity.Report
}

// translate runs the whole pipeline over one query, through the same facade the
// CLI and the proxy use. Driving the real entry point is the point: a corpus
// that exercised its own private copy would pass while the shipped path broke.
func translate(reg *registry.Registry, sourceDSL, query, targetDSL string) (*translation, error) {
	result, err := compiler.Translate(context.Background(), compiler.Request{
		SourceDSL: sourceDSL,
		TargetDSL: targetDSL,
		Query:     query,
		Registry:  reg,
	})
	if err != nil {
		return nil, err
	}
	// The resolver decides nothing about fidelity, so a flag it left behind
	// would mean a verdict reached too early. That has to be checked against a
	// freshly resolved tree, since Translate hands back a validated one.
	if err := assertResolverIsNeutral(reg, sourceDSL, query); err != nil {
		return nil, err
	}

	return &translation{
		query:  result.Query,
		text:   result.Text,
		output: result.Output,
		report: result.Report,
	}, nil
}

// assertResolverIsNeutral re-runs parse and resolve alone and checks that the
// tree arrives unflagged.
func assertResolverIsNeutral(reg *registry.Registry, sourceDSL, query string) error {
	p, err := parser.Get(sourceDSL)
	if err != nil {
		return fmt.Errorf("no parser for %s: %w", sourceDSL, err)
	}
	node, err := p.Parse(query)
	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}
	resolved, err := resolver.Resolve(node, sourceDSL, reg)
	if err != nil {
		return fmt.Errorf("resolving: %w", err)
	}
	if worst, _ := ir.WorstTranslatability(resolved); worst != ir.TranslatabilityFull {
		return fmt.Errorf("the resolver produced a tree already flagged %s", worst)
	}
	return nil
}

// normalizeQuery collapses whitespace so a comparison is about the query rather
// than about spacing.
func normalizeQuery(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

// TestRoundTripCorpus runs every case in testdata/.
func TestRoundTripCorpus(t *testing.T) {
	reg, err := registry.DefaultRegistry()
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}

	cases, err := LoadTestCases(testdataDir)
	if err != nil {
		t.Fatalf("loading cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases found; the corpus should not be empty")
	}

	type tally struct{ passed, total int }
	results := map[string]*tally{}

	for _, testCase := range cases {
		counts, ok := results[testCase.group]
		if !ok {
			counts = &tally{}
			results[testCase.group] = counts
		}
		counts.total++

		passed := t.Run(testCase.group+"/"+filepath.Base(testCase.file), func(t *testing.T) {
			testCase.run(t, reg)
		})
		if passed {
			counts.passed++
		}
	}

	groups := make([]string, 0, len(results))
	for group := range results {
		groups = append(groups, group)
	}
	sort.Strings(groups)

	var passed, total int
	var report strings.Builder
	for _, group := range groups {
		counts := results[group]
		fmt.Fprintf(&report, "  %-20s %d/%d passed\n", group+":", counts.passed, counts.total)
		passed += counts.passed
		total += counts.total
	}
	fmt.Fprintf(&report, "  %-20s %d/%d passed\n", "Total:", passed, total)
	t.Logf("round-trip corpus\n%s", report.String())
}

func (c TestCase) run(t *testing.T, reg *registry.Registry) {
	t.Helper()

	result, err := translate(reg, c.SourceDSL, c.Input, c.TargetDSL)
	if err != nil {
		t.Fatalf("%s\n  input: %s\n  %v", c.Name, c.Input, err)
	}

	c.checkOutput(t, result)
	c.checkScore(t, result)
	c.checkFlags(t, result)
	c.checkReparses(t, result)

	if c.Bidirectional {
		c.checkBidirectional(t, reg, result)
	}
}

func (c TestCase) checkOutput(t *testing.T, result *translation) {
	t.Helper()
	if c.ExpectedOutput == "" {
		return
	}
	if got, want := normalizeQuery(result.output), normalizeQuery(c.ExpectedOutput); got != want {
		t.Errorf("%s\n  input:    %s\n  expected: %s\n  actual:   %s",
			c.Name, c.Input, want, got)
	}
}

func (c TestCase) checkScore(t *testing.T, result *translation) {
	t.Helper()
	if c.ExpectedFidelityScore == nil {
		return
	}
	if result.report != nil && result.report.SignalMismatch != nil {
		return
	}
	got, want := result.report.FidelityScore, *c.ExpectedFidelityScore
	if math.Abs(got-want) > scoreTolerance {
		t.Errorf("%s\n  input:          %s\n  expected score: %.4f\n  actual score:   %.4f\n%s",
			c.Name, c.Input, want, got, indent(result.report.ToText()))
	}
}

// checkFlags asserts each expected verdict against the tree, which is where the
// validator left it. Reading the tree rather than the report is what lets a case
// assert FULL, since a report lists only what was lost.
func (c TestCase) checkFlags(t *testing.T, result *translation) {
	t.Helper()
	if len(c.ExpectedFlags) == 0 {
		return
	}

	actual := map[string]ir.TranslatabilityFlag{}
	ir.InspectPath(result.query, "Query", func(path string, node ir.Node) bool {
		flag, _ := node.Base().Translatability()
		actual[path] = flag
		return true
	})

	var diffs []string
	for _, expected := range c.ExpectedFlags {
		want, err := ir.ParseTranslatabilityFlag(expected.Flag)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if result.report != nil && result.report.SignalMismatch != nil &&
			expected.Path == "Query" && want == ir.TranslatabilityUnsupported {
			continue
		}
		got, ok := actual[expected.Path]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("    %s: no such node in the tree", expected.Path))
			continue
		}
		if got != want {
			diffs = append(diffs, fmt.Sprintf("    %s: expected %s, got %s", expected.Path, want, got))
		}
	}
	if len(diffs) == 0 {
		return
	}

	paths := make([]string, 0, len(actual))
	for path := range actual {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var tree strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&tree, "    %-64s %s\n", path, actual[path])
	}

	t.Errorf("%s\n  input: %s\n  flag differences:\n%s\n  the tree as validated:\n%s",
		c.Name, c.Input, strings.Join(diffs, "\n"), tree.String())
}

// checkReparses is the property the whole suite exists for: whatever an emitter
// wrote, the target's own parser must accept. A translation that does not parse
// is not a translation.
func (c TestCase) checkReparses(t *testing.T, result *translation) {
	t.Helper()

	if c.ExpectsNoQuery {
		// The case asserts that the target can write nothing. What has to hold
		// is that nothing is what was written, and that the emitter said why —
		// a silent empty result would be the failure, not the empty result.
		if result.output != "" {
			t.Errorf("%s\n  input: %s\n  expected no query, got: %s",
				c.Name, c.Input, result.output)
		}
		if !strings.Contains(result.text, "UNSUPPORTED") {
			t.Errorf("%s\n  input: %s\n  a translation that produced no query must say why:\n%s",
				c.Name, c.Input, indent(result.text))
		}
		return
	}

	p, err := parser.Get(c.TargetDSL)
	if err != nil {
		t.Fatalf("%s: no parser for %s: %v", c.Name, c.TargetDSL, err)
	}
	// The whole emitted text is re-parsed, comments included, since that is what
	// a user copies out.
	if _, err := p.Parse(result.text); err != nil {
		t.Errorf("%s\n  input:  %s\n  output does not parse as %s: %v\n%s",
			c.Name, c.Input, c.TargetDSL, err, indent(result.text))
	}
}

// checkBidirectional translates the output back and compares the two trees.
//
// The comparison is of shape rather than of bytes. A data source is represented
// differently in each language — PromQL names a metric, LogQL matches a
// __name__ label — so demanding an identical tree would fail on a difference
// that is the point of the translation rather than a fault in it. What must
// survive is the operations and their windows.
func (c TestCase) checkBidirectional(t *testing.T, reg *registry.Registry, forward *translation) {
	t.Helper()

	if forward.output == "" {
		t.Fatalf("%s: nothing was emitted to translate back", c.Name)
	}

	back, err := translate(reg, c.TargetDSL, forward.output, c.SourceDSL)
	if err != nil {
		t.Fatalf("%s\n  translating back from %s failed\n  intermediate: %s\n  %v",
			c.Name, c.TargetDSL, forward.output, err)
	}

	if got, want := pipelineShape(back.query), pipelineShape(forward.query); got != want {
		t.Errorf("%s\n  the round trip changed the operations\n"+
			"  input:        %s\n  intermediate: %s\n  back again:   %s\n"+
			"  going out: %s\n  coming back: %s",
			c.Name, c.Input, forward.output, back.output, want, got)
	}
}

// pipelineShape summarizes what a query does, leaving out how each language
// spells its data source.
func pipelineShape(query *ir.Query) string {
	parts := make([]string, 0, len(query.Pipeline)+1)
	for _, stage := range query.Pipeline {
		switch node := stage.(type) {
		case *ir.AggregationStage:
			part := node.Op.String() + "/" + node.Scope.String()
			if len(node.GroupBy) > 0 {
				part += " by(" + strings.Join(node.GroupBy, ",") + ")"
			}
			if len(node.Without) > 0 {
				part += " without(" + strings.Join(node.Without, ",") + ")"
			}
			parts = append(parts, part)
		case *ir.BinaryOpStage:
			part := "binary:" + node.Op.String()
			if node.Left != nil {
				part += "[" + pipelineShape(node.Left) + "]"
			}
			if node.Right != nil {
				part += "[" + pipelineShape(node.Right) + "]"
			}
			parts = append(parts, part)
		case *ir.FunctionStage:
			parts = append(parts, "fn:"+node.Name)
		case *ir.FilterStage:
			parts = append(parts, "filter")
		case *ir.JoinStage:
			parts = append(parts, "join:"+node.JoinType.String())
		}
	}

	shape := strings.Join(parts, " | ")
	if query.Output != nil && query.Output.Window != nil {
		if step := query.Output.Window.Step; !step.IsZero() {
			shape += " step=" + step.Duration().String()
		}
		if offset := query.Output.Window.Offset; !offset.IsZero() {
			shape += " offset=" + offset.Duration().String()
		}
	}
	// The matcher operators are part of what must survive, even though the
	// source's name is not.
	if query.Source != nil {
		var matchers []string
		for _, selector := range query.Source.Selectors {
			for _, matcher := range selector.Matchers {
				if matcher.Key == "__name__" {
					continue
				}
				matchers = append(matchers, matcher.Key+matcher.Op.String())
			}
		}
		sort.Strings(matchers)
		if len(matchers) > 0 {
			shape += " {" + strings.Join(matchers, ",") + "}"
		}
	}
	return shape
}

func indent(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// TestCorpusCoverage keeps the corpus honest about its own breadth: a suite that
// silently shrank would still pass every case it kept.
func TestCorpusCoverage(t *testing.T) {
	cases, err := LoadTestCases(testdataDir)
	if err != nil {
		t.Fatalf("loading cases: %v", err)
	}

	counts := map[string]int{}
	for _, testCase := range cases {
		counts[testCase.group]++
	}

	minimums := map[string]int{
		"promql_to_logql": 20,
		"logql_to_promql": 15,
		"bidirectional":   5,
		// TraceQL's groups are smaller on purpose. Its round-trip group carries
		// the language's own constructs, while the cross-language groups exist
		// mainly to pin what is lost — there are only so many distinct ways a
		// span query fails to be a metric query, and repeating them would pad
		// the corpus without covering anything new.
		"traceql_roundtrip": 10,
		"traceql_to_logql":  5,
		"traceql_to_promql": 4,
		"promql_to_traceql": 4,
		"logql_to_traceql":  3,
	}
	for group, want := range minimums {
		if counts[group] < want {
			t.Errorf("%s holds %d cases, want at least %d", group, counts[group], want)
		}
	}

	t.Run("every case is uniquely named", func(t *testing.T) {
		seen := map[string]string{}
		for _, testCase := range cases {
			key := testCase.group + "/" + testCase.Name
			if first, ok := seen[key]; ok {
				t.Errorf("%q appears in both %s and %s", testCase.Name, first, testCase.file)
			}
			seen[key] = testCase.file
		}
	})

	t.Run("every case explains itself", func(t *testing.T) {
		// A corpus is documentation as much as it is a test. A case with no
		// note leaves the next reader guessing why it is here.
		for _, testCase := range cases {
			if testCase.Description == "" || testCase.Notes == "" {
				t.Errorf("%s has no description or notes", testCase.file)
			}
		}
	})

	t.Run("every pairing is covered", func(t *testing.T) {
		directions := map[string]bool{}
		for _, testCase := range cases {
			directions[testCase.SourceDSL+"->"+testCase.TargetDSL] = true
		}
		for _, want := range []string{
			"promql->logql", "logql->promql",
			"traceql->logql", "traceql->promql",
			"promql->traceql", "logql->traceql",
			// TraceQL into itself is the control for the others: it isolates
			// what the parser and emitter do from any cross-language loss.
			"traceql->traceql",
		} {
			if !directions[want] {
				t.Errorf("no case covers %s", want)
			}
		}
	})

	// TestCorpusCoverage deliberately does not require a bidirectional TraceQL
	// case. Round-tripping a query through TraceQL and back is not expected to
	// preserve it: a span set and a metric series are different things, so the
	// return trip has nothing to rebuild the lost half from. The traceql->traceql
	// group covers what round-tripping can honestly mean here, which is that the
	// language's own constructs survive their own parser and emitter.
	t.Run("no traceql case claims to be bidirectional", func(t *testing.T) {
		for _, testCase := range cases {
			if !testCase.Bidirectional {
				continue
			}
			if testCase.SourceDSL == "traceql" || testCase.TargetDSL == "traceql" {
				t.Errorf("%s marks a TraceQL translation bidirectional; the semantic gap "+
					"between spans and metrics or logs means the return trip cannot "+
					"reconstruct what the first leg dropped", testCase.file)
			}
		}
	})
}
