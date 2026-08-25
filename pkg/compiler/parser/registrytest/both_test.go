// Package registrytest verifies that the DSL front ends coexist. Each parser
// package registers itself from init, so this is the only place that sees the
// registry as a real binary would: with more than one language present.
package registrytest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/parser"

	// Imported for their registration side effects, the way a CLI or the
	// federation proxy selects which DSLs it supports.
	_ "github.com/polyql/polyql/pkg/compiler/parser/logql"
	_ "github.com/polyql/polyql/pkg/compiler/parser/promql"
)

func TestBothParsersRegister(t *testing.T) {
	got := parser.List()
	want := []string{"logql", "promql"}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v", got, want)
		}
	}
}

// TestEachParserProducesItsOwnTree covers the point of keeping the ASTs
// separate: the same conceptual query reaches the resolver in each language's
// own shape, tagged with the DSL it came from.
func TestEachParserProducesItsOwnTree(t *testing.T) {
	cases := []struct{ dsl, query string }{
		{"promql", `rate(http_requests_total{status="500"}[5m])`},
		{"logql", `rate({app="frontend"} |= "error" [5m])`},
	}

	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			p, err := parser.Get(c.dsl)
			if err != nil {
				t.Fatalf("Get(%q): %v", c.dsl, err)
			}
			node, err := p.Parse(c.query)
			if err != nil {
				t.Fatalf("Parse(%s): %v", c.query, err)
			}
			if node.DSL() != c.dsl {
				t.Errorf("node.DSL() = %q, want %q", node.DSL(), c.dsl)
			}
			if node.String() != c.query {
				t.Errorf("String() = %q, want %q", node.String(), c.query)
			}
			// The node type must come from the matching package.
			if want := "*" + c.dsl + "."; !strings.HasPrefix(typeName(node), want) {
				t.Errorf("node type %s should come from package %s", typeName(node), c.dsl)
			}
		})
	}
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }
