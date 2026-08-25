package registry

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// embeddedData holds the language definitions compiled into the binary.
//
// Shipping them this way makes polyql a single self-contained executable: the
// CLI, the proxy and the dashboard translator all work with no files alongside
// them. The definitions remain overridable at runtime through LoadDir, so a
// contributor iterating on a DSL — or an operator carrying a private vendor
// definition — does not have to rebuild.
//
//go:embed data/*.yaml
var embeddedData embed.FS

// embeddedDir is the directory inside embeddedData holding the definitions.
const embeddedDir = "data"

// EmbeddedSourcePrefix marks a DSLDefinition.SourcePath as coming from the
// compiled-in set rather than from disk, so an error message can say which.
const EmbeddedSourcePrefix = "embedded:"

// LoadEmbedded reads the definitions compiled into the binary, without
// installing them. It is the fallback Load uses when given no directory.
func LoadEmbedded() (map[string]*DSLDefinition, error) {
	entries, err := embeddedData.ReadDir(embeddedDir)
	if err != nil {
		return nil, fmt.Errorf("registry: cannot read the embedded definitions: %w", err)
	}

	var files []sourceFile
	for _, entry := range entries {
		if entry.IsDir() || !isDefinitionFile(entry.Name()) {
			continue
		}
		name := path.Join(embeddedDir, entry.Name())
		data, err := fs.ReadFile(embeddedData, name)
		if err != nil {
			return nil, fmt.Errorf("registry: cannot read the embedded %s: %w", entry.Name(), err)
		}
		files = append(files, sourceFile{
			path: EmbeddedSourcePrefix + entry.Name(),
			data: data,
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	defs, err := loadFiles(files)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		// Unreachable unless the embed directive stops matching, which would be
		// a build-time mistake worth failing loudly on.
		return nil, fmt.Errorf("registry: the embedded definition set is empty")
	}
	return defs, nil
}

// IsEmbedded reports whether a definition came from the compiled-in set.
func (d *DSLDefinition) IsEmbedded() bool {
	return strings.HasPrefix(d.SourcePath, EmbeddedSourcePrefix)
}
