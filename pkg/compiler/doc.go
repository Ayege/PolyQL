// Package compiler is the home of the cross-cutting tests for the translation
// pipeline.
//
// The pipeline itself lives in the sub-packages — parser, resolver, validator,
// emitter, fidelity — each with its own tests. What cannot live in any one of
// them is the property that matters most to a user: that a real query, put in
// one end, comes out the other as something the target language accepts, and
// that the report of what it cost is accurate.
//
// That property is tested here, against the corpus of cases under testdata/.
package compiler
