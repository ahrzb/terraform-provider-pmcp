package coverage

import (
	"encoding/json"
	"fmt"
	"os"
)

// stagedFile mirrors coverage/staged.json's shape. `$comment` is documentation for a human
// reader of the JSON file, not data the oracle consumes.
type stagedFile struct {
	Staged []string `json:"staged"`
}

// Staged is coverage/staged.json's `staged` list, indexed for lookup. Each entry names an op
// (the whole op is provider-ahead-of-fixture) or `op.field` (only that field is), per §22.5:
// "each entry names an operation or 'operation.field' this provider supports ahead of the hub's
// fixture, with the reason." Validation — both the field-coverage and totality assertions — is
// skipped only for a target listed here; everything else unknown still fails.
type Staged map[string]bool

// LoadStaged reads coverage/staged.json. A missing `staged` key is an empty set, not an error —
// the file's own committed state today.
func LoadStaged(path string) (Staged, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading staged file %s: %w", path, err)
	}
	var f stagedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing staged file %s: %w", path, err)
	}
	out := make(Staged, len(f.Staged))
	for _, entry := range f.Staged {
		out[entry] = true
	}
	return out, nil
}
