package parser

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// registry holds the parsers registered by DSL name. It is package-level state
// because registration happens from init functions in the per-DSL packages,
// which is what lets a binary choose its supported DSLs purely by which
// packages it imports.
var registry = struct {
	sync.RWMutex
	parsers map[string]Parser
}{parsers: make(map[string]Parser)}

// normalizeDSL puts a DSL name into the canonical form used as a registry key,
// so that a caller passing "PromQL" reaches the parser registered as "promql".
func normalizeDSL(dsl string) string {
	return strings.ToLower(strings.TrimSpace(dsl))
}

// Register adds a parser under the name it reports from DSL. It is intended to
// be called from a package's init function.
//
// Register panics on a nil parser, an empty DSL name, or a duplicate
// registration. Each is a programming error that can only be introduced at
// build time — by importing two packages that claim the same DSL, say — so
// failing at startup is better than letting a binary run with a parser that
// silently is not the one the caller expects.
func Register(parser Parser) {
	if parser == nil {
		panic("parser: Register called with a nil Parser")
	}
	dsl := normalizeDSL(parser.DSL())
	if dsl == "" {
		panic("parser: Register called with a Parser reporting an empty DSL name")
	}

	registry.Lock()
	defer registry.Unlock()
	if existing, ok := registry.parsers[dsl]; ok {
		panic(fmt.Sprintf("parser: DSL %q is already registered by %T", dsl, existing))
	}
	registry.parsers[dsl] = parser
}

// Get returns the parser registered for a DSL. The error names the DSLs that
// are available, since the usual cause of a miss is a missing import of the
// package whose init would have registered it.
func Get(dsl string) (Parser, error) {
	name := normalizeDSL(dsl)

	registry.RLock()
	defer registry.RUnlock()
	if p, ok := registry.parsers[name]; ok {
		return p, nil
	}
	if len(registry.parsers) == 0 {
		return nil, fmt.Errorf("parser: no parser registered for DSL %q: no DSLs are registered at all, "+
			"which usually means no parser package was imported", dsl)
	}
	return nil, fmt.Errorf("parser: no parser registered for DSL %q (registered: %s)",
		dsl, strings.Join(listLocked(), ", "))
}

// List returns the registered DSL names in sorted order, which gives the CLI a
// stable list to show and keeps test output deterministic.
func List() []string {
	registry.RLock()
	defer registry.RUnlock()
	return listLocked()
}

// listLocked returns the sorted DSL names. The caller must hold at least a read
// lock.
func listLocked() []string {
	names := make([]string, 0, len(registry.parsers))
	for name := range registry.parsers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
