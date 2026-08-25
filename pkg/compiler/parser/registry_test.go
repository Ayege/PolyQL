package parser

import (
	"strings"
	"testing"

	"github.com/polyql/polyql/pkg/compiler/ast"
)

// fakeNode is a minimal ast.Node for exercising the registry without pulling in
// a real DSL front end.
type fakeNode struct{ dsl, text string }

func (n fakeNode) String() string { return n.text }
func (n fakeNode) DSL() string    { return n.dsl }

type fakeParser struct {
	dsl string
	err error
}

func (p fakeParser) DSL() string { return p.dsl }
func (p fakeParser) Parse(input string) (ast.Node, error) {
	if p.err != nil {
		return nil, p.err
	}
	return fakeNode{dsl: p.dsl, text: input}, nil
}

// withCleanRegistry swaps in an empty registry for the duration of a test, so
// that tests neither see each other's registrations nor leak into the real one.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	registry.Lock()
	saved := registry.parsers
	registry.parsers = make(map[string]Parser)
	registry.Unlock()

	t.Cleanup(func() {
		registry.Lock()
		registry.parsers = saved
		registry.Unlock()
	})
}

func TestRegisterAndGet(t *testing.T) {
	withCleanRegistry(t)
	Register(fakeParser{dsl: "testql"})

	got, err := Get("testql")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DSL() != "testql" {
		t.Errorf("DSL() = %q", got.DSL())
	}

	node, err := got.Parse("some query")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if node.String() != "some query" || node.DSL() != "testql" {
		t.Errorf("node = %+v", node)
	}
}

func TestGetNormalizesTheName(t *testing.T) {
	withCleanRegistry(t)
	Register(fakeParser{dsl: "PromQL"})

	// Registration lowercases, so every spelling reaches the same parser.
	for _, name := range []string{"promql", "PromQL", "PROMQL", "  promql  "} {
		if _, err := Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
	if names := List(); len(names) != 1 || names[0] != "promql" {
		t.Errorf("List() = %v, want [promql]", names)
	}
}

func TestGetUnknownDSL(t *testing.T) {
	withCleanRegistry(t)

	// With nothing registered the message should point at the likely cause.
	_, err := Get("promql")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no parser package was imported") {
		t.Errorf("error %q should explain that no DSLs are registered", err)
	}

	// Once something is registered, the message should list what is available.
	Register(fakeParser{dsl: "logql"})
	Register(fakeParser{dsl: "promql"})
	_, err = Get("traceql")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "logql, promql") {
		t.Errorf("error %q should list the registered DSLs", err)
	}
}

func TestListIsSorted(t *testing.T) {
	withCleanRegistry(t)
	for _, dsl := range []string{"traceql", "promql", "logql"} {
		Register(fakeParser{dsl: dsl})
	}
	got := List()
	want := []string{"logql", "promql", "traceql"}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v", got, want)
		}
	}
}

// TestRegisterRejectsProgrammingErrors covers the three cases that can only
// arise from a build-time mistake, where failing loudly at startup beats
// running with the wrong parser.
func TestRegisterRejectsProgrammingErrors(t *testing.T) {
	cases := []struct {
		name   string
		setup  func()
		action func()
	}{
		{
			name:   "nil parser",
			action: func() { Register(nil) },
		},
		{
			name:   "empty DSL name",
			action: func() { Register(fakeParser{dsl: "   "}) },
		},
		{
			name:   "duplicate registration",
			setup:  func() { Register(fakeParser{dsl: "dupe"}) },
			action: func() { Register(fakeParser{dsl: "dupe"}) },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withCleanRegistry(t)
			if c.setup != nil {
				c.setup()
			}
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			c.action()
		})
	}
}

func TestRegistryIsConcurrentSafe(t *testing.T) {
	withCleanRegistry(t)
	Register(fakeParser{dsl: "promql"})

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				if _, err := Get("promql"); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				_ = List()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
