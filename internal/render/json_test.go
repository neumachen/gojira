package render_test

import (
	"encoding/json"
	"testing"

	"github.com/neumachen/gojira/classify"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/internal/render"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderIssueJSON(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Example issue for JSON rendering",
		Status:    "In Progress",
		IssueType: "Story",
		Assignee:  "Alice",
		Reporter:  "Bob",
		Created:   mustTime("2024-01-15T10:00:00Z"),
		Updated:   mustTime("2024-06-01T12:30:00Z"),
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		// Description is nil (no ADF).
		CustomFields: map[string]json.RawMessage{},
	}

	refs := []extract.Reference{
		{
			Kind:     classify.KindJiraKey,
			IssueKey: "EXAMPLE-2",
			Source:   extract.SourceRelationship,
			ClassifyResult: classify.Result{
				Kind:     classify.KindJiraKey,
				IssueKey: "EXAMPLE-2",
			},
		},
		{
			Kind:   classify.KindExternal,
			URL:    "https://example.com/doc",
			Text:   "External doc",
			Source: extract.SourceRemoteLink,
			ClassifyResult: classify.Result{
				Kind: classify.KindExternal,
				URL:  "https://example.com/doc",
			},
		},
	}

	got, err := render.RenderIssueJSON(issue, refs)
	require.NoError(t, err)

	// Must be valid JSON.
	assert.True(t, json.Valid([]byte(got)), "RenderIssueJSON output is not valid JSON")

	// Must contain key fields.
	assert.Contains(t, got, "EXAMPLE-1", "output should contain the issue key")
	assert.Contains(t, got, `"references"`, "output should contain the references key")

	// Golden-file comparison.
	checkGolden(t, "issue_json.json", got)
}
