// Package parser defines the compiler's front-end contract and the registry
// that makes it extensible.
//
// A parser is stage 1 of the pipeline: it turns query text in one DSL into that
// DSL's own AST (stage 2). It performs no IR mapping and no cross-DSL
// reasoning — the resolver does that, working from the tree a parser returns.
//
// Adding a DSL means writing a Parser and calling Register from the package's
// init, with no change to any code here.
package parser

import "github.com/polyql/polyql/pkg/compiler/ast"

// Parser is the front end for a single query DSL.
//
// Implementations must be safe for concurrent use: the federation proxy shares
// one registered parser instance across requests, so a Parser should hold no
// mutable state between calls.
type Parser interface {
	// Parse turns query text into the DSL's AST. The error should carry the
	// position of the offending token where the DSL can determine it.
	Parse(input string) (ast.Node, error)

	// DSL returns the language's canonical name, lowercase — "promql",
	// "logql", "traceql". It is the key the parser registers under and the
	// basename of the language registry's YAML file.
	DSL() string
}
