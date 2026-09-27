package reviewer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TriageOutputPath is where the triage agent writes its selection, relative
// to the workspace. It is a separate file from the review's, so that a triage
// run cannot be mistaken for a review that found nothing.
const TriageOutputPath = ScratchDir + "/out/triage.json"

// TriageOutput is the JSON document the triage agent produces: which files of
// a very large change are worth a reviewer's attention.
type TriageOutput struct {
	SchemaVersion int      `json:"schema_version"`
	Paths         []string `json:"paths"`
	// Notes explains the selection in one or two sentences. It is shown to
	// the author, because a review that quietly skipped half the change is
	// worse than no review.
	Notes string `json:"notes,omitempty"`
}

// ParseTriage decodes and checks a triage document.
func ParseTriage(data []byte) (*TriageOutput, error) {
	var out TriageOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("triage output is not valid JSON: %w", err)
	}
	if out.SchemaVersion != OutputSchemaVersion {
		return nil, fmt.Errorf("triage schema_version is %d, want %d", out.SchemaVersion, OutputSchemaVersion)
	}

	paths := make([]string, 0, len(out.Paths))
	for _, p := range out.Paths {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("triage selected no files")
	}
	out.Paths = paths
	return &out, nil
}
