package registry

// The language definitions live at the repository root, under registry/, where
// contributors find and edit them. go:embed cannot reach outside its own
// package directory, so the copies under data/ are what a built binary carries.
//
// Run "go generate ./pkg/registry" (or "make generate") after editing a
// definition. TestEmbeddedMatchesDisk fails when the two diverge, so a forgotten
// copy is caught by the test suite rather than by a user running a stale binary.

//go:generate cp ../../registry/promql.yaml data/promql.yaml
//go:generate cp ../../registry/logql.yaml data/logql.yaml
//go:generate cp ../../registry/traceql.yaml data/traceql.yaml
