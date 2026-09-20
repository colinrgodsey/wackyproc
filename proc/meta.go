package proc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// readMeta loads meta.json for the process record in procDir.
//
// A record whose meta.json is missing or unparsable is an error rather than a zero Meta.
// The zero value is dangerous in the signalling paths: Meta.StartTime is what
// CheckLiveness uses to detect a recycled PID, so a blanked Meta silently disables that
// guard and the caller proceeds to signal a process group that may belong to somebody
// else. Enumerating callers (List, Prune, the terminal scan) deliberately tolerate it -
// see their call sites - because refusing to list or prune would make a corrupt record
// impossible to clean up.
func readMeta(procDir string) (Meta, error) {
	var meta Meta
	data, err := os.ReadFile(filepath.Join(procDir, MetaFileName))
	if err != nil {
		return meta, fmt.Errorf("failed to read %s in %s: %w", MetaFileName, procDir, err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("failed to parse %s in %s: %w", MetaFileName, procDir, err)
	}
	return meta, nil
}
