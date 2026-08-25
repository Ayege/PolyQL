// Package ast defines the contract every DSL-specific abstract syntax tree in
// PolyQL satisfies.
//
// The ASTs themselves are deliberately not shared. A PromQL tree and a LogQL
// tree have different shapes because the languages do, and flattening them into
// one grammar would lose exactly the structure the resolver needs in order to
// map each DSL onto the QLS-aligned IR faithfully. What is shared is this
// minimal contract, which is enough for the parser registry to hand a tree back
// to a caller and for a resolver to identify which registry entry governs it.
package ast

// Node is implemented by every AST node a PolyQL parser produces.
type Node interface {
	// String renders the node back into the syntax of its own DSL. Re-parsing
	// the result must yield an equivalent tree, which is what makes round-trip
	// translation testable.
	String() string

	// DSL names the query language this node was parsed from, matching the name
	// its Parser reports and its language registry file.
	DSL() string
}
