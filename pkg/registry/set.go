package registry

import (
	"fmt"
	"sort"
	"strings"
)

// Registry is a set of language definitions.
//
// It is the non-global form of the package's lookup API: the compiler passes one
// explicitly through the pipeline, so a caller translating against a private
// vendor definition does not have to disturb the process-wide set that Load
// installs. The package-level Get and List read the installed Registry.
type Registry struct {
	defs map[string]*DSLDefinition
}

// New returns a Registry over the given definitions, keyed by DSL name. The map
// is copied, so later edits to it do not change the Registry.
func New(defs map[string]*DSLDefinition) *Registry {
	copied := make(map[string]*DSLDefinition, len(defs))
	for name, def := range defs {
		copied[normalizeDSL(name)] = def
	}
	return &Registry{defs: copied}
}

// Open loads the definitions in dir and returns them as a Registry, without
// installing them process-wide. An empty dir selects the definitions compiled
// into the binary.
func Open(dir string) (*Registry, error) {
	var defs map[string]*DSLDefinition
	var err error
	if strings.TrimSpace(dir) == "" {
		defs, err = LoadEmbedded()
	} else {
		defs, err = LoadDir(dir)
	}
	if err != nil {
		return nil, err
	}
	return New(defs), nil
}

// DefaultRegistry returns the language definitions compiled into the binary.
//
// It is what a caller uses when no registry directory was given: polyql ships as
// a single executable, and the definitions travel inside it. Open with a
// directory overrides that set entirely.
func DefaultRegistry() (*Registry, error) {
	defs, err := LoadEmbedded()
	if err != nil {
		return nil, err
	}
	return New(defs), nil
}

// Get returns a definition by DSL name, case-insensitively.
func (r *Registry) Get(dsl string) (*DSLDefinition, error) {
	if r == nil || len(r.defs) == 0 {
		return nil, fmt.Errorf("registry: no definition for DSL %q: the registry is empty", dsl)
	}
	if def, ok := r.defs[normalizeDSL(dsl)]; ok {
		return def, nil
	}
	return nil, fmt.Errorf("registry: no definition for DSL %q (loaded: %s)",
		dsl, strings.Join(r.List(), ", "))
}

// List returns the DSL names in sorted order.
func (r *Registry) List() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.defs))
	for name := range r.defs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Definitions returns a copy of the underlying map.
func (r *Registry) Definitions() map[string]*DSLDefinition {
	if r == nil {
		return nil
	}
	copied := make(map[string]*DSLDefinition, len(r.defs))
	for name, def := range r.defs {
		copied[name] = def
	}
	return copied
}

// Default returns the Registry that Load installed, which is what the
// package-level Get and List read.
func Default() *Registry {
	loaded.RLock()
	defer loaded.RUnlock()
	copied := make(map[string]*DSLDefinition, len(loaded.defs))
	for name, def := range loaded.defs {
		copied[name] = def
	}
	return &Registry{defs: copied}
}
