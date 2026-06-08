package render

import (
	"encoding/json"

	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
)

// IssueWithRefs combines a parsed Jira issue and its discovered outbound
// references into a single value suitable for JSON serialisation.
type IssueWithRefs struct {
	Issue      parse.Issue         `json:"issue"`
	References []extract.Reference `json:"references"`
}

// RenderIssueJSON serialises issue and refs to indented JSON.
//
// It is a pure function: no I/O, no side effects. The caller is
// responsible for writing the returned string to disk.
//
// The output is produced by json.MarshalIndent with prefix "" and
// indent "  " (two spaces), consistent with the project's JSON
// formatting convention.
func RenderIssueJSON(issue parse.Issue, refs []extract.Reference) (string, error) {
	v := IssueWithRefs{Issue: issue, References: refs}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
