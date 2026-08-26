package playground_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polyql/polyql/internal/playground"
	"github.com/polyql/polyql/pkg/registry"

	_ "github.com/polyql/polyql/pkg/compiler/emitter/logql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/promql"
	_ "github.com/polyql/polyql/pkg/compiler/emitter/traceql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/traceql"
)

// generatedPath is the file the browser playground loads its examples from.
var generatedPath = filepath.Join("..", "..", "web", "examples.js")

func loadRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.DefaultRegistry()
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}
	return reg
}

// TestGeneratedExamplesAreCurrent is what makes the playground's first paint
// honest.
//
// The page shows each example's seeded translation before the WebAssembly module
// has finished downloading, and replaces it with a live one after. If the
// generated file were allowed to go stale, a visitor would see one answer become
// a different answer in front of them — and the first one, the one shown while
// the page looks finished, would be the wrong one.
func TestGeneratedExamplesAreCurrent(t *testing.T) {
	onDisk, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("reading %s: %v", generatedPath, err)
	}

	fresh, err := playground.Generate(loadRegistry(t))
	if err != nil {
		t.Fatalf("generating examples: %v", err)
	}

	if string(onDisk) != string(fresh) {
		t.Errorf("%s is stale: run `make generate` and commit the result", generatedPath)
	}
}

// TestExamplesShowTheWholeRange guards the example list against becoming a
// showcase of only what works.
//
// The reports are half of what this project does, and a playground that opened
// on eight clean translations would misrepresent it. Requiring at least one
// unsupported construct and one approximation keeps the honest cases in view.
func TestExamplesShowTheWholeRange(t *testing.T) {
	results := decodeGenerated(t)

	var unsupported, partial int
	for _, r := range results {
		if r.Report.UnsupportedCount > 0 {
			unsupported++
		}
		if r.Report.PartialCount > 0 {
			partial++
		}
	}

	if unsupported == 0 {
		t.Error("no example shows an unsupported construct; the playground would only show successes")
	}
	if partial == 0 {
		t.Error("no example shows an approximation")
	}
}

// TestEveryExampleTranslates checks that no example is broken on arrival. A
// query that will not parse is a bug in the list, not a demonstration of a
// limit: an untranslatable construct still parses, and shows up as a report.
func TestEveryExampleTranslates(t *testing.T) {
	for _, r := range decodeGenerated(t) {
		if !r.OK {
			t.Errorf("example %q failed to translate: %s", r.title, r.Error)
		}
	}
}

// generatedResult mirrors the shape the page consumes, so this test reads the
// file the same way the browser does rather than trusting the generator twice.
type generatedResult struct {
	title  string
	OK     bool     `json:"ok"`
	Error  string   `json:"error"`
	Output string   `json:"output"`
	Notes  []string `json:"notes"`
	Report struct {
		Score            float64 `json:"score"`
		PartialCount     int     `json:"partial"`
		UnsupportedCount int     `json:"unsupported"`
	} `json:"report"`
}

func decodeGenerated(t *testing.T) []generatedResult {
	t.Helper()

	content, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("reading %s: %v", generatedPath, err)
	}

	// The file is a JavaScript assignment so the page can load it with a plain
	// script tag; the value itself is JSON. Slice it out rather than adding a
	// JavaScript parser to the test suite.
	text := string(content)
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end < start {
		t.Fatalf("%s does not contain a JSON array", generatedPath)
	}

	var examples []struct {
		Title  string          `json:"title"`
		Result generatedResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &examples); err != nil {
		t.Fatalf("decoding %s: %v", generatedPath, err)
	}

	out := make([]generatedResult, 0, len(examples))
	for _, ex := range examples {
		result := ex.Result
		result.title = ex.Title
		out = append(out, result)
	}
	return out
}
