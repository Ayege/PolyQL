package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func mustMarshal(t *testing.T, dash *Dashboard) []byte {
	t.Helper()
	data, err := Marshal(dash)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// decodeGeneric turns JSON into plain Go values, so two documents can be
// compared by what they say rather than by how they are written.
func decodeGeneric(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	// Numbers stay as written, so 1 and 1.0 do not silently compare equal.
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return value
}

// stripExprs removes every "expr" field, leaving the parts of a dashboard a
// translation must not touch.
func stripExprs(value any) {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "expr")
		for _, child := range typed {
			stripExprs(child)
		}
	case []any:
		for _, child := range typed {
			stripExprs(child)
		}
	}
}

// describeDiff reports the first places two decoded documents disagree, so a
// failure names the field rather than printing two whole dashboards.
func describeDiff(path string, before, after any) string {
	if reflect.DeepEqual(before, after) {
		return ""
	}

	switch left := before.(type) {
	case map[string]any:
		right, ok := after.(map[string]any)
		if !ok {
			break
		}
		keys := map[string]bool{}
		for key := range left {
			keys[key] = true
		}
		for key := range right {
			keys[key] = true
		}
		names := make([]string, 0, len(keys))
		for key := range keys {
			names = append(names, key)
		}
		sort.Strings(names)

		var diffs []string
		for _, key := range names {
			leftValue, inLeft := left[key]
			rightValue, inRight := right[key]
			switch {
			case !inRight:
				diffs = append(diffs, fmt.Sprintf("  %s.%s: removed", path, key))
			case !inLeft:
				diffs = append(diffs, fmt.Sprintf("  %s.%s: added", path, key))
			default:
				if nested := describeDiff(path+"."+key, leftValue, rightValue); nested != "" {
					diffs = append(diffs, nested)
				}
			}
		}
		return strings.Join(diffs, "\n")

	case []any:
		right, ok := after.([]any)
		if !ok {
			break
		}
		if len(left) != len(right) {
			return fmt.Sprintf("  %s: %d entries became %d", path, len(left), len(right))
		}
		var diffs []string
		for i := range left {
			if nested := describeDiff(fmt.Sprintf("%s[%d]", path, i), left[i], right[i]); nested != "" {
				diffs = append(diffs, nested)
			}
		}
		return strings.Join(diffs, "\n")
	}

	return fmt.Sprintf("  %s: %v became %v", path, before, after)
}

// TranslatedOrOriginal reports the expression a target now carries, which is the
// translated one where a translation succeeded and the original where it did
// not.
func (t Target) TranslatedOrOriginal() string { return t.Expr }
