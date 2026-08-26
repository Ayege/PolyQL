// Package docs holds no code. This test guards the C4 diagrams against the one
// failure they are prone to: drifting out of step with the tree they describe.
//
// The drift runs in a predictable direction. Someone builds a component and
// forgets that a diagram still calls it planned, so the diagram understates what
// exists; or a component is described that was never built, so it overstates.
// Both have happened in this repository, which is why the check exists rather
// than a convention.
package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plannedMarker is what a diagram writes on a component that does not exist yet.
const plannedMarker = "[PLANNED]"

// plannedPackages maps a component marked planned to the package that would
// hold it. A directory holding only .gitkeep counts as absent: that is exactly
// what a reserved-but-unbuilt package looks like here.
var plannedPackages = map[string]string{
	"Federation proxy": "../cmd/polyql-proxy",
	"OTel exporter":    "../pkg/telemetry",
}

// TestPlannedComponentsAreActuallyAbsent fails when a diagram calls something
// planned that has since been built. That is the direction the drift actually
// runs, and the direction nobody notices.
func TestPlannedComponentsAreActuallyAbsent(t *testing.T) {
	diagrams := readDiagrams(t)

	for component, pkg := range plannedPackages {
		marked := false
		for name, body := range diagrams {
			for _, line := range strings.Split(body, "\n") {
				if strings.Contains(line, component) && strings.Contains(line, plannedMarker) {
					marked = true
					t.Logf("%s marks %q planned", name, component)
				}
			}
		}
		if !marked {
			continue
		}
		if hasGoSource(t, pkg) {
			t.Errorf("the diagrams still mark %q as %s, but %s now holds Go source; "+
				"the component was built and the diagram was not updated",
				component, plannedMarker, pkg)
		}
	}
}

// builtPackages maps a component the diagrams describe as real to the package
// that implements it. It is the mirror image of plannedPackages: that map
// catches a diagram that understates the tree, this one catches a diagram that
// describes something nobody built.
var builtPackages = map[string]string{
	"Browser playground":   "../cmd/polyql-wasm",
	"Translating proxy":    "../pkg/proxy",
	"Dashboard translator": "../pkg/dashboard",
}

// TestDescribedComponentsHaveSource fails when a diagram names a component as
// built and the package behind it is empty or missing.
func TestDescribedComponentsHaveSource(t *testing.T) {
	diagrams := readDiagrams(t)

	for component, pkg := range builtPackages {
		described := false
		for _, body := range diagrams {
			for _, line := range strings.Split(body, "\n") {
				if strings.Contains(line, component) && !strings.Contains(line, plannedMarker) {
					described = true
				}
			}
		}
		if !described {
			continue
		}
		if !hasGoSource(t, pkg) {
			t.Errorf("the diagrams describe %q as built, but %s holds no Go source",
				component, pkg)
		}
	}
}

// TestNoDiagramClaimsAnUnbuiltDependency covers the other direction for the one
// claim that is cheap to check mechanically: a diagram may not describe an
// OpenTelemetry exporter as real while the module has no such dependency.
func TestNoDiagramClaimsAnUnbuiltDependency(t *testing.T) {
	gomod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	hasOTel := strings.Contains(string(gomod), "opentelemetry")

	for name, body := range readDiagrams(t) {
		for i, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "OTel exporter") {
				continue
			}
			if !hasOTel && !strings.Contains(line, plannedMarker) {
				t.Errorf("%s:%d describes an OTel exporter without marking it %s, "+
					"but go.mod has no OpenTelemetry dependency", name, i+1, plannedMarker)
			}
			if hasOTel && strings.Contains(line, plannedMarker) {
				t.Errorf("%s:%d still marks the OTel exporter %s, "+
					"but go.mod now depends on OpenTelemetry", name, i+1, plannedMarker)
			}
		}
	}
}

// TestDiagramsAreMirrored keeps docs/ and diagrams/ identical. Two copies of the
// same file are a standing invitation to update one of them.
func TestDiagramsAreMirrored(t *testing.T) {
	for name, body := range readDiagrams(t) {
		mirror := filepath.Join("../diagrams", name)
		other, err := os.ReadFile(mirror)
		if err != nil {
			t.Errorf("%s has no counterpart at %s: %v", name, mirror, err)
			continue
		}
		if string(other) != body {
			t.Errorf("docs/%s and diagrams/%s have diverged; copy one over the other", name, name)
		}
	}
}

// readDiagrams returns every .mmd file in this directory, keyed by base name.
func readDiagrams(t *testing.T) map[string]string {
	t.Helper()

	matches, err := filepath.Glob("*.mmd")
	if err != nil {
		t.Fatalf("globbing diagrams: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no diagrams found; this test is checking nothing")
	}

	diagrams := make(map[string]string, len(matches))
	for _, path := range matches {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		diagrams[filepath.Base(path)] = string(body)
	}
	return diagrams
}

// hasGoSource reports whether a directory holds any .go file. A directory that
// exists but contains only a .gitkeep is a reservation, not an implementation.
func hasGoSource(t *testing.T, dir string) bool {
	t.Helper()

	found := false
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			// A missing directory is the expected state for a planned component.
			return nil
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".go") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return found
}
