package parse

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSite = "https://example.atlassian.net"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err, "read fixture %s", name)
	return data
}

// TestParseFullIssue exercises every typed field using the full_issue.json
// fixture, which populates parent, subtasks, issuelinks, remotelinks, and
// several customfield_* keys.
func TestParseFullIssue(t *testing.T) {
	raw := readFixture(t, "full_issue.json")
	issue, err := Parse(raw, testSite)
	require.NoError(t, err)

	assert.Equal(t, "EXAMPLE-1", issue.Key, "Key")
	assert.Equal(t, "Implement the login flow", issue.Summary, "Summary")
	assert.Equal(t, "In Progress", issue.Status, "Status")
	assert.Equal(t, "Story", issue.IssueType, "IssueType")
	assert.Equal(t, "Alice Example", issue.Assignee, "Assignee")
	assert.Equal(t, "Bob Example", issue.Reporter, "Reporter")

	wantCreated := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	assert.True(t, issue.Created.Equal(wantCreated), "Created: got %v, want %v", issue.Created, wantCreated)

	wantUpdated := time.Date(2026, 1, 20, 14, 45, 0, 0, time.UTC)
	assert.True(t, issue.Updated.Equal(wantUpdated), "Updated: got %v, want %v", issue.Updated, wantUpdated)

	assert.Equal(t, "https://example.atlassian.net/browse/EXAMPLE-1", issue.SourceURL, "SourceURL")

	require.NotEmpty(t, issue.Description, "Description: expected non-empty ADF JSON")
	var descCheck any
	assert.NoError(t, json.Unmarshal(issue.Description, &descCheck), "Description must be valid JSON")

	// Parent
	require.NotNil(t, issue.Parent, "Parent")
	assert.Equal(t, "EXAMPLE-0", issue.Parent.Key, "Parent.Key")
	assert.Equal(t, "Authentication epic", issue.Parent.Summary, "Parent.Summary")

	// Subtasks
	require.Len(t, issue.Subtasks, 2, "Subtasks count")
	assert.Equal(t, "EXAMPLE-1a", issue.Subtasks[0].Key, "Subtasks[0].Key")
	assert.Equal(t, "EXAMPLE-1b", issue.Subtasks[1].Key, "Subtasks[1].Key")

	// IssueLinks — fixture has one outward "Blocks" and one inward "Relates"
	require.Len(t, issue.IssueLinks, 2, "IssueLinks count")
	outward := issue.IssueLinks[0]
	assert.Equal(t, "outward", outward.Direction, "IssueLinks[0].Direction")
	assert.Equal(t, "Blocks", outward.Type, "IssueLinks[0].Type")
	assert.Equal(t, "EXAMPLE-2", outward.Key, "IssueLinks[0].Key")
	inward := issue.IssueLinks[1]
	assert.Equal(t, "inward", inward.Direction, "IssueLinks[1].Direction")
	assert.Equal(t, "EXAMPLE-3", inward.Key, "IssueLinks[1].Key")

	// RemoteLinks
	require.Len(t, issue.RemoteLinks, 2, "RemoteLinks count")
	assert.Equal(t, "Design document", issue.RemoteLinks[0].Title, "RemoteLinks[0].Title")
	assert.Equal(t, "https://example.com/design-doc", issue.RemoteLinks[0].URL, "RemoteLinks[0].URL")
	assert.Equal(t, "https://github.com/example-org/example-repo/pull/42", issue.RemoteLinks[1].URL, "RemoteLinks[1].URL")

	// CustomFields — fixture has customfield_10014, customfield_10016, customfield_10020
	assert.Len(t, issue.CustomFields, 3, "CustomFields count")
	for _, k := range []string{"customfield_10014", "customfield_10016", "customfield_10020"} {
		assert.Contains(t, issue.CustomFields, k, "CustomFields missing %q", k)
	}
}

// TestParseMinimalIssue exercises default/zero behaviour when optional fields
// are null or absent.
func TestParseMinimalIssue(t *testing.T) {
	raw := readFixture(t, "minimal_issue.json")
	issue, err := Parse(raw, testSite)
	require.NoError(t, err)

	assert.Equal(t, "EXAMPLE-99", issue.Key)
	assert.Equal(t, "Minimal issue", issue.Summary)
	assert.Empty(t, issue.Assignee, "Assignee should be empty for null assignee")
	assert.Nil(t, issue.Parent, "Parent should be nil")
	assert.Empty(t, issue.Subtasks, "Subtasks should be empty")
	assert.Empty(t, issue.IssueLinks, "IssueLinks should be empty")
	assert.Empty(t, issue.RemoteLinks, "RemoteLinks should be empty")
	assert.Empty(t, issue.Description, "Description should be empty for null description")
	assert.NotNil(t, issue.CustomFields, "CustomFields should be non-nil even when empty")
	assert.Empty(t, issue.CustomFields, "CustomFields should be empty")
	assert.Equal(t, "https://example.atlassian.net/browse/EXAMPLE-99", issue.SourceURL, "SourceURL")
}

// TestParseCustomFields verifies that all customfield_* keys survive into
// CustomFields verbatim, including null values and array values (AC13).
func TestParseCustomFields(t *testing.T) {
	raw := readFixture(t, "custom_fields_issue.json")
	issue, err := Parse(raw, testSite)
	require.NoError(t, err)

	assert.Equal(t, "EXAMPLE-7", issue.Key)

	wantKeys := []string{
		"customfield_10014",
		"customfield_10016",
		"customfield_10020",
		"customfield_99001",
		"customfield_99002",
		"customfield_99003",
	}
	for _, k := range wantKeys {
		assert.Contains(t, issue.CustomFields, k, "CustomFields missing %q", k)
	}
	assert.Len(t, issue.CustomFields, len(wantKeys), "CustomFields count")

	// customfield_99001 must be a JSON string.
	var s string
	require.NoError(t, json.Unmarshal(issue.CustomFields["customfield_99001"], &s))
	assert.Equal(t, "some-string-value", s, "customfield_99001 value")

	// customfield_99002 must be a JSON array of length 3.
	var arr []string
	require.NoError(t, json.Unmarshal(issue.CustomFields["customfield_99002"], &arr))
	assert.Len(t, arr, 3, "customfield_99002 length")

	// customfield_99003 is null — must be preserved (not dropped).
	assert.Equal(t, "null", string(issue.CustomFields["customfield_99003"]), "customfield_99003 should be the literal null")
}

// TestParseEmptySite verifies that passing an empty site leaves SourceURL empty.
func TestParseEmptySite(t *testing.T) {
	raw := readFixture(t, "minimal_issue.json")
	issue, err := Parse(raw, "")
	require.NoError(t, err)
	assert.Empty(t, issue.SourceURL, "SourceURL should be empty when site is empty")
}

// TestParseInvalidJSON verifies that malformed input returns an error.
func TestParseInvalidJSON(t *testing.T) {
	_, err := Parse([]byte(`not json`), testSite)
	assert.Error(t, err, "Parse should reject malformed JSON")
}

// TestParseMissingKey verifies that an issue JSON without a "key" field returns
// an error.
func TestParseMissingKey(t *testing.T) {
	raw := []byte(`{"fields":{"summary":"no key"}}`)
	_, err := Parse(raw, testSite)
	assert.Error(t, err, "Parse should reject issue JSON without a key")
}

// TestParseNamesPresent verifies that a response carrying a top-level
// "names" object alongside "fields" populates Issue.Names with the
// human-readable field labels keyed by field ID.
func TestParseNamesPresent(t *testing.T) {
	raw := []byte(`{
		"key": "EXAMPLE-1",
		"names": {
			"summary": "Summary",
			"customfield_10115": "Sprint",
			"customfield_10116": "Rank"
		},
		"fields": {
			"summary": "Test issue",
			"status": {"name": "Open"},
			"issuetype": {"name": "Task"},
			"reporter": {"displayName": "Alice"},
			"created": "2026-01-15T10:30:00Z",
			"updated": "2026-01-15T10:30:00Z",
			"customfield_10115": [],
			"customfield_10116": "0|i07gzp:"
		}
	}`)
	issue, err := Parse(raw, "")
	require.NoError(t, err)

	require.NotNil(t, issue.Names, "Names must be populated when response carries expand=names")
	assert.Equal(t, "Summary", issue.Names["summary"], "Names[summary]")
	assert.Equal(t, "Sprint", issue.Names["customfield_10115"], "Names[customfield_10115]")
	assert.Equal(t, "Rank", issue.Names["customfield_10116"], "Names[customfield_10116]")
}

// TestParseNamesAbsent verifies that a response without a top-level
// "names" object leaves Issue.Names nil; the renderer falls back to
// the raw customfield_NNNNN id in that case.
func TestParseNamesAbsent(t *testing.T) {
	raw := []byte(`{
		"key": "EXAMPLE-1",
		"fields": {
			"summary": "Test issue",
			"status": {"name": "Open"},
			"issuetype": {"name": "Task"},
			"reporter": {"displayName": "Alice"},
			"created": "2026-01-15T10:30:00Z",
			"updated": "2026-01-15T10:30:00Z"
		}
	}`)
	issue, err := Parse(raw, "")
	require.NoError(t, err)
	assert.Nil(t, issue.Names, "Names must be nil when response omits the names object")
}

// TestParseNamesSkipsNonStringEntries verifies that the names parser
// is tolerant: an entry whose value is not a JSON string is skipped
// individually, the well-formed entries around it are preserved, and
// Parse does not return an error. This matters because expand=names
// is best-effort enrichment and a single malformed entry from
// Atlassian must not erase every label.
func TestParseNamesSkipsNonStringEntries(t *testing.T) {
	raw := []byte(`{
		"key": "EXAMPLE-1",
		"names": {
			"summary": "Summary",
			"customfield_10115": 42,
			"customfield_10116": "Rank",
			"customfield_10117": null
		},
		"fields": {
			"summary": "Test issue",
			"status": {"name": "Open"},
			"issuetype": {"name": "Task"},
			"reporter": {"displayName": "Alice"},
			"created": "2026-01-15T10:30:00Z",
			"updated": "2026-01-15T10:30:00Z"
		}
	}`)
	issue, err := Parse(raw, "")
	require.NoError(t, err, "non-string entry must not fail the parse")

	require.NotNil(t, issue.Names, "well-formed entries must survive")
	assert.Equal(t, "Summary", issue.Names["summary"])
	assert.Equal(t, "Rank", issue.Names["customfield_10116"])
	assert.NotContains(t, issue.Names, "customfield_10115",
		"non-string entry (int) must be skipped")
	assert.NotContains(t, issue.Names, "customfield_10117",
		"non-string entry (null) must be skipped")
}

// TestParseTimeFormats verifies that both Jira time formats (with and without
// milliseconds) are parsed correctly.
func TestParseTimeFormats(t *testing.T) {
	tests := []struct {
		name    string
		created string
		want    time.Time
	}{
		{
			name:    "with milliseconds",
			created: "2026-01-15T10:30:00.000+0000",
			want:    time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			name:    "without milliseconds",
			created: "2026-01-15T10:30:00+0000",
			want:    time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			name:    "RFC3339 with Z",
			created: "2026-01-15T10:30:00Z",
			want:    time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{
				"key": "EXAMPLE-T",
				"fields": {
					"summary": "time test",
					"status": {"name": "Open"},
					"issuetype": {"name": "Task"},
					"reporter": {"displayName": "Alice Example"},
					"created": "` + tt.created + `",
					"updated": "` + tt.created + `"
				}
			}`)
			issue, err := Parse(raw, "")
			require.NoError(t, err)
			assert.True(t, issue.Created.Equal(tt.want), "Created: got %v, want %v", issue.Created, tt.want)
		})
	}
}
