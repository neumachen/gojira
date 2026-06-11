package render_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/internal/render"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// update controls whether golden files are regenerated on this run.
// Run with: go test -run . -update ./internal/render/...
// (We use a package-level var so the flag can be set in TestMain if needed;
// for simplicity we use an env var instead.)
func updateGolden() bool {
	return os.Getenv("UPDATE_GOLDEN") == "1"
}

// checkGolden compares got against the golden file at testdata/<name>.
// When UPDATE_GOLDEN=1 the golden file is written instead of compared.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if updateGolden() {
		require.NoError(t, os.MkdirAll("testdata", 0o755), "mkdir testdata")
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644), "write golden %s", path)
		t.Logf("updated golden: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "read golden %s (run with UPDATE_GOLDEN=1 to create)", path)
	assert.Equal(t, string(want), got, "output mismatch for %s", name)
}

// mustTime parses an RFC3339 timestamp or panics.
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// mustRawMessage encodes v as JSON or panics.
func mustRawMessage(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// adfParagraph returns a minimal ADF document with a single paragraph.
func adfParagraph(text string) json.RawMessage {
	return mustRawMessage(map[string]any{
		"version": 1,
		"type":    "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": text},
				},
			},
		},
	})
}

// adfWithLink returns an ADF document with a paragraph containing a link.
func adfWithLink(text, href string) json.RawMessage {
	return mustRawMessage(map[string]any{
		"version": 1,
		"type":    "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": text,
						"marks": []any{
							map[string]any{
								"type":  "link",
								"attrs": map[string]any{"href": href},
							},
						},
					},
				},
			},
		},
	})
}

// adfWithUnknownNode returns an ADF document with a paragraph followed by an
// unknown node type ("panel") containing inner text.
func adfWithUnknownNode() json.RawMessage {
	return mustRawMessage(map[string]any{
		"version": 1,
		"type":    "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "Before the panel."},
				},
			},
			map[string]any{
				"type":  "panel",
				"attrs": map[string]any{"panelType": "info"},
				"content": []any{
					map[string]any{
						"type": "paragraph",
						"content": []any{
							map[string]any{"type": "text", "text": "Panel inner text."},
						},
					},
				},
			},
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "After the panel."},
				},
			},
		},
	})
}

// ---- RenderIssue tests ----

// TestRenderIssue_Full tests a fully-populated issue with all sections.
func TestRenderIssue_Full(t *testing.T) {
	issue := parse.Issue{
		Key:         "EXAMPLE-1",
		Summary:     "Implement the login flow",
		Status:      "In Progress",
		IssueType:   "Story",
		Assignee:    "Alice Example",
		Reporter:    "Bob Example",
		Created:     mustTime("2026-01-15T10:30:00Z"),
		Updated:     mustTime("2026-01-20T14:45:00Z"),
		SourceURL:   "https://example.atlassian.net/browse/EXAMPLE-1",
		Description: adfParagraph("This story covers the login flow implementation."),
		Parent:      &parse.ParentRef{Key: "EXAMPLE-0", Summary: "Authentication epic"},
		Subtasks: []parse.LinkedIssue{
			{Key: "EXAMPLE-1a", Summary: "Write unit tests for login"},
			{Key: "EXAMPLE-1b", Summary: "Write integration tests for login"},
		},
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Blocks", Key: "EXAMPLE-2", Summary: "Implement the logout flow"},
			{Direction: "inward", Type: "Relates", Key: "EXAMPLE-3", Summary: "Session management"},
		},
		RemoteLinks: []parse.RemoteLink{
			{Title: "Design document", URL: "https://example.com/design-doc"},
		},
		CustomFields: map[string]json.RawMessage{},
	}
	// All keys are in neighbours — relative links.
	neighbours := map[string]bool{
		"EXAMPLE-0":  true,
		"EXAMPLE-1a": true,
		"EXAMPLE-1b": true,
		"EXAMPLE-2":  true,
		"EXAMPLE-3":  true,
	}
	got, err := render.RenderIssue(issue, neighbours, false)
	require.NoError(t, err, "RenderIssue")
	checkGolden(t, "full_issue.md", got)
}

// TestRenderIssue_Minimal tests an issue with only Key, Summary, and Status.
func TestRenderIssue_Minimal(t *testing.T) {
	issue := parse.Issue{
		Key:          "EXAMPLE-99",
		Summary:      "Minimal issue",
		Status:       "Open",
		IssueType:    "Task",
		Reporter:     "Alice Example",
		Created:      mustTime("2026-03-01T08:00:00Z"),
		Updated:      mustTime("2026-03-01T08:00:00Z"),
		SourceURL:    "https://example.atlassian.net/browse/EXAMPLE-99",
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err, "RenderIssue")
	checkGolden(t, "minimal_issue.md", got)
}

// TestRenderIssue_WithGitHubPR tests an issue whose ADF description contains
// a GitHub pull request link. The link appears in the rendered Description
// section as a standard Markdown link.
func TestRenderIssue_WithGitHubPR(t *testing.T) {
	issue := parse.Issue{
		Key:          "EXAMPLE-4",
		Summary:      "Issue with GitHub PR in description",
		Status:       "In Review",
		IssueType:    "Story",
		Assignee:     "Alice Example",
		Reporter:     "Bob Example",
		Created:      mustTime("2026-02-01T09:00:00Z"),
		Updated:      mustTime("2026-02-05T11:00:00Z"),
		SourceURL:    "https://example.atlassian.net/browse/EXAMPLE-4",
		Description:  adfWithLink("org/repo#42", "https://github.com/org/repo/pull/42"),
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err, "RenderIssue")
	checkGolden(t, "with_github_pr.md", got)
}

// TestRenderIssue_UnknownADFNode tests that an unknown ADF node type is
// preserved inline (as a Markdown comment) and also listed in the
// "## Unknown content" footer section.
func TestRenderIssue_UnknownADFNode(t *testing.T) {
	issue := parse.Issue{
		Key:          "EXAMPLE-5",
		Summary:      "Issue with unknown ADF node",
		Status:       "Open",
		IssueType:    "Task",
		Reporter:     "Alice Example",
		Created:      mustTime("2026-02-10T10:00:00Z"),
		Updated:      mustTime("2026-02-10T10:00:00Z"),
		SourceURL:    "https://example.atlassian.net/browse/EXAMPLE-5",
		Description:  adfWithUnknownNode(),
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err, "RenderIssue")
	checkGolden(t, "unknown_adf_node.md", got)
}

// TestRenderIssue_UnknownCustomField tests that unknown custom fields are
// rendered in the "## Custom fields" section and not silently dropped.
func TestRenderIssue_UnknownCustomField(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-7",
		Summary:   "Issue with custom fields",
		Status:    "Done",
		IssueType: "Bug",
		Assignee:  "Alice Example",
		Reporter:  "Bob Example",
		Created:   mustTime("2026-02-10T09:00:00Z"),
		Updated:   mustTime("2026-02-12T17:30:00Z"),
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-7",
		CustomFields: map[string]json.RawMessage{
			"customfield_10014": mustRawMessage("EXAMPLE-0"),
			"customfield_10016": mustRawMessage(8),
			"customfield_10020": mustRawMessage(map[string]any{"name": "Sprint 2", "state": "closed"}),
			"customfield_99001": mustRawMessage("some-string-value"),
			"customfield_99002": mustRawMessage([]string{"tag-a", "tag-b", "tag-c"}),
			"customfield_99003": json.RawMessage("null"),
		},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err, "RenderIssue")
	checkGolden(t, "unknown_custom_field.md", got)
}

// TestRenderIssue_WithHierarchyChildren tests that JQL-discovered hierarchy
// children populate the "### Children" subsection separately from the
// legacy "### Sub-tasks" subsection.
//
// This documents the v4 rename: issue.Subtasks → "### Sub-tasks" and
// issue.Children (a new field populated by internal/crawl after JQL
// search) → "### Children". Both subsections can appear together.
func TestRenderIssue_WithHierarchyChildren(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-100",
		Summary:   "Epic with hierarchy children",
		Status:    "In Progress",
		IssueType: "Epic",
		Reporter:  "Alice Example",
		Created:   mustTime("2026-03-01T08:00:00Z"),
		Updated:   mustTime("2026-03-01T08:00:00Z"),
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-100",
		Subtasks: []parse.LinkedIssue{
			{Key: "EXAMPLE-100a", Summary: "Legacy subtask"},
		},
		Children:     []string{"EXAMPLE-200", "EXAMPLE-201"},
		CustomFields: map[string]json.RawMessage{},
	}
	neighbours := map[string]bool{
		"EXAMPLE-100a": true,
		"EXAMPLE-200":  true,
		"EXAMPLE-201":  true,
	}
	got, err := render.RenderIssue(issue, neighbours, false)
	require.NoError(t, err, "RenderIssue")
	// Both subsections present in the documented order: Sub-tasks before Children.
	assert.Contains(t, got, "### Sub-tasks\n")
	assert.Contains(t, got, "### Children\n")
	subtasksIdx := strings.Index(got, "### Sub-tasks")
	childrenIdx := strings.Index(got, "### Children")
	assert.Less(t, subtasksIdx, childrenIdx, "### Sub-tasks must appear before ### Children")
	// Children are rendered as relative links because they're in neighbours.
	assert.Contains(t, got, "../EXAMPLE-200/index.md")
	assert.Contains(t, got, "../EXAMPLE-201/index.md")
}

// TestRenderIssue_UnresolvedLinks tests that relationship links for keys NOT
// in neighbours fall back to absolute Jira browse URLs.
func TestRenderIssue_UnresolvedLinks(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Issue with unresolved links",
		Status:    "In Progress",
		IssueType: "Story",
		Reporter:  "Bob Example",
		Created:   mustTime("2026-01-15T10:30:00Z"),
		Updated:   mustTime("2026-01-20T14:45:00Z"),
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		Parent:    &parse.ParentRef{Key: "EXAMPLE-0", Summary: "Authentication epic"},
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Blocks", Key: "EXAMPLE-2", Summary: "Logout flow"},
		},
		CustomFields: map[string]json.RawMessage{},
	}
	// Empty neighbours — all links are unresolved.
	got, err := render.RenderIssue(issue, map[string]bool{}, false)
	require.NoError(t, err, "RenderIssue")
	checkGolden(t, "unresolved_links.md", got)
}

// ---------------------------------------------------------------------------
// Custom fields rendering (expand=names labels + JSON pretty-print)
// ---------------------------------------------------------------------------

// TestClassifyCustomField exercises the four-kind classifier used by
// the Custom fields renderer. One table row per kind plus a few edge
// cases (whitespace-only, single-line object, invalid JSON).
func TestClassifyCustomField(t *testing.T) {
	tests := []struct {
		name       string
		raw        json.RawMessage
		wantKind   string
		wantPretty string
		wantMulti  bool
	}{
		{
			name:       "null literal",
			raw:        json.RawMessage(`null`),
			wantKind:   "null",
			wantPretty: "null",
			wantMulti:  false,
		},
		{
			name:       "empty bytes treated as null",
			raw:        json.RawMessage{},
			wantKind:   "null",
			wantPretty: "null",
			wantMulti:  false,
		},
		{
			name:       "primitive string",
			raw:        json.RawMessage(`"hello"`),
			wantKind:   "primitive",
			wantPretty: `"hello"`,
			wantMulti:  false,
		},
		{
			name:       "primitive number",
			raw:        json.RawMessage(`3.0`),
			wantKind:   "primitive",
			wantPretty: "3.0",
			wantMulti:  false,
		},
		{
			name:       "primitive bool",
			raw:        json.RawMessage(`true`),
			wantKind:   "primitive",
			wantPretty: "true",
			wantMulti:  false,
		},
		{
			name:      "structured array multi-line",
			raw:       json.RawMessage(`[{"id":1,"name":"Sprint 1"}]`),
			wantKind:  "structured",
			wantMulti: true,
		},
		{
			name:      "structured object multi-line",
			raw:       json.RawMessage(`{"name":"Sprint","state":"closed"}`),
			wantKind:  "structured",
			wantMulti: true,
		},
		{
			name:       "empty array structured but single-line",
			raw:        json.RawMessage(`[]`),
			wantKind:   "structured",
			wantPretty: "[]",
			wantMulti:  false,
		},
		{
			name:       "empty object structured but single-line",
			raw:        json.RawMessage(`{}`),
			wantKind:   "structured",
			wantPretty: "{}",
			wantMulti:  false,
		},
		{
			name:      "invalid JSON (atlassian customfield_10000 notation)",
			raw:       json.RawMessage(`{pullrequest={overall={count=2, lastUpdated=2026-05-08T13:44:52.000+0000}, byInstanceType={GitHub={count=2}}}}`),
			wantKind:  "invalid",
			wantMulti: false,
		},

		// --- JSON-string second-pass cases (PROJ-1578 Dev Status
		//     summary field rendering, AC 33). ---

		{
			// String of a JSON object that pretty-prints onto a
			// single line: inner `{}` after json.Indent stays "{}".
			name:       "string of JSON object (empty)",
			raw:        mustRawMessage("{}"),
			wantKind:   "structured",
			wantPretty: "{}",
			wantMulti:  false,
		},
		{
			// String of a JSON object whose pretty form spans
			// multiple lines: the inner JSON drives kind+layout.
			name:      "string of JSON object (multi-line)",
			raw:       mustRawMessage(`{"a":1,"b":2}`),
			wantKind:  "structured",
			wantMulti: true,
		},
		{
			// String of a JSON array that pretty-prints to a
			// single line (`[]` stays `[]`).
			name:       "string of JSON array (empty)",
			raw:        mustRawMessage("[]"),
			wantKind:   "structured",
			wantPretty: "[]",
			wantMulti:  false,
		},
		{
			// String of a JSON array whose pretty form spans
			// multiple lines.
			name:      "string of JSON array (multi-element)",
			raw:       mustRawMessage(`[1,2,3]`),
			wantKind:  "structured",
			wantMulti: true,
		},
		{
			// String of a valid JSON primitive (a string of a
			// string). A string of a string is just a string;
			// preserve the outer JSON-string quotes.
			name:       "string of valid JSON primitive (string-in-string)",
			raw:        mustRawMessage(`"hello"`),
			wantKind:   "primitive",
			wantPretty: `"\"hello\""`,
			wantMulti:  false,
		},
		{
			// String of Atlassian {key=value, json={...}} notation
			// — the canonical Dev Status summary blob. Not valid
			// JSON, looksStructured returns true (the string
			// contains `{`, `=`, and a newline). The outer JSON-
			// string quotes are stripped: the inner content is
			// the only legible representation.
			name: "string of Atlassian dev-status notation",
			raw: mustRawMessage(
				"{repository={count=1, dataType=repository},\n" +
					"json={\"cachedValue\":{\"errors\":[]," +
					"\"summary\":{\"repository\":{\"overall\":" +
					"{\"count\":1,\"dataType\":\"repository\"}}}}," +
					"\"isStale\":true}}"),
			wantKind: "string-structured",
			wantPretty: "{repository={count=1, dataType=repository},\n" +
				"json={\"cachedValue\":{\"errors\":[]," +
				"\"summary\":{\"repository\":{\"overall\":" +
				"{\"count\":1,\"dataType\":\"repository\"}}}}," +
				"\"isStale\":true}}",
			wantMulti: true,
		},
		{
			// Plain short string with no structural characters
			// (no `{`, `[`, `=`, or '\n'). Renders inline as a
			// primitive with the outer JSON-string quotes
			// preserved — wrapping it in a fenced block would be
			// visual noise.
			name:       "string of plain short text",
			raw:        json.RawMessage(`"hello world"`),
			wantKind:   "primitive",
			wantPretty: `"hello world"`,
			wantMulti:  false,
		},
		{
			// Newline-only string. '\n' is in the structured-
			// character set: once a string has a newline, fenced-
			// block rendering is the legible choice even though
			// the content is otherwise empty.
			name:       "string of single newline",
			raw:        json.RawMessage(`"\n"`),
			wantKind:   "string-structured",
			wantPretty: "\n",
			wantMulti:  true,
		},
		{
			// `"{}"` — the inner content IS valid JSON (an empty
			// object). The structured-single-line branch handles
			// it, so the kind is "structured", not "string-
			// structured".
			name:       "string of brackets only",
			raw:        json.RawMessage(`"{}"`),
			wantKind:   "structured",
			wantPretty: "{}",
			wantMulti:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, pretty, multi := render.ClassifyCustomFieldForTest(tt.raw)
			assert.Equal(t, tt.wantKind, kind, "kind")
			if tt.wantPretty != "" {
				assert.Equal(t, tt.wantPretty, pretty, "pretty")
			}
			assert.Equal(t, tt.wantMulti, multi, "indented (multi-line)")
		})
	}
}

// TestRenderIssue_CustomFieldsWithNames verifies that the Custom
// fields section uses **Bold** labels for fields present in
// issue.Names and `backticked-id` labels for fields not present.
func TestRenderIssue_CustomFieldsWithNames(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Custom fields with labels",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		CustomFields: map[string]json.RawMessage{
			"customfield_10115": json.RawMessage(`"sprint-string"`),
			"customfield_10224": json.RawMessage(`3.0`),
			"customfield_99999": json.RawMessage(`"unnamed"`),
		},
		Names: map[string]string{
			"customfield_10115": "Sprint",
			"customfield_10224": "Story Points",
			// customfield_99999 deliberately absent → fallback.
		},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)

	assert.Contains(t, got, "## Custom fields\n", "section header present")
	assert.Contains(t, got, "- **Sprint**: \"sprint-string\"\n",
		"named field renders with bold label")
	assert.Contains(t, got, "- **Story Points**: 3.0\n",
		"second named field renders with bold label")
	assert.Contains(t, got, "- `customfield_99999`: \"unnamed\"\n",
		"unnamed field falls back to backticked id")
	assert.NotContains(t, got, "- `customfield_10115`",
		"named field must NOT also render with backticked id")
}

// TestRenderIssue_CustomFieldsSkipsNullByDefault verifies that the
// default config (renderNullCustomFields=false) drops every null-
// valued custom field, and that a section containing only nulls is
// elided entirely.
func TestRenderIssue_CustomFieldsSkipsNullByDefault(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "All-null custom fields",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		CustomFields: map[string]json.RawMessage{
			"customfield_10220": json.RawMessage(`null`),
			"customfield_10221": json.RawMessage(`null`),
			"customfield_10299": json.RawMessage(`null`),
		},
		Names: map[string]string{
			"customfield_10220": "Story Pointes",
			"customfield_10221": "Squad",
		},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)
	assert.NotContains(t, got, "## Custom fields",
		"section must be elided when every entry is null and the flag is off")
	assert.NotContains(t, got, "customfield_10220")
	assert.NotContains(t, got, "Story Pointes")
}

// TestRenderIssue_CustomFieldsIncludesNullWhenConfigured verifies
// that renderNullCustomFields=true surfaces each null-valued field as
// "- <label>: null".
func TestRenderIssue_CustomFieldsIncludesNullWhenConfigured(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Audit null custom fields",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		CustomFields: map[string]json.RawMessage{
			"customfield_10220": json.RawMessage(`null`),
			"customfield_99001": json.RawMessage(`null`),
		},
		Names: map[string]string{
			"customfield_10220": "Squad",
		},
	}
	got, err := render.RenderIssue(issue, nil, true)
	require.NoError(t, err)
	assert.Contains(t, got, "## Custom fields\n", "section present in audit mode")
	assert.Contains(t, got, "- **Squad**: null\n", "named null entry preserved")
	assert.Contains(t, got, "- `customfield_99001`: null\n", "unnamed null entry preserved")
}

// TestRenderIssue_CustomFieldsPrimitivesInline verifies that string,
// number, and boolean values render inline after the label rather
// than inside a code fence.
func TestRenderIssue_CustomFieldsPrimitivesInline(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Primitives",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		CustomFields: map[string]json.RawMessage{
			"customfield_aaa": json.RawMessage(`"text"`),
			"customfield_bbb": json.RawMessage(`42`),
			"customfield_ccc": json.RawMessage(`true`),
		},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)
	assert.Contains(t, got, "- `customfield_aaa`: \"text\"\n")
	assert.Contains(t, got, "- `customfield_bbb`: 42\n")
	assert.Contains(t, got, "- `customfield_ccc`: true\n")
	// No code fences for primitives.
	assert.NotContains(t, got, "```json")
}

// TestRenderIssue_CustomFieldsStructuredAsJSONBlock verifies that
// JSON arrays and objects render in fenced ```json blocks with
// pretty-printed multi-line content, properly indented by two spaces
// under the bullet so the fence does not terminate the list.
func TestRenderIssue_CustomFieldsStructuredAsJSONBlock(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Structured",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		CustomFields: map[string]json.RawMessage{
			"customfield_10115": json.RawMessage(
				`[{"id":4815,"name":"PROJ Sprint 57","state":"closed"}]`),
		},
		Names: map[string]string{
			"customfield_10115": "Sprint",
		},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)
	// Bullet, label, colon, newline, then 2-space-indented fence.
	assert.Contains(t, got, "- **Sprint**:\n  ```json\n", "fence opens under bullet")
	assert.Contains(t, got, "\n  ```\n", "fence closes with 2-space indent")
	// The pretty body lines are 2-space-indented too.
	assert.Contains(t, got, "  [\n", "pretty array opens on its own line")
	assert.Contains(t, got, "  ]\n", "pretty array closes on its own line")
	assert.Contains(t, got, `"name": "PROJ Sprint 57"`, "pretty content preserved")
}

// TestRenderIssue_CustomFieldsInvalidJSONAsPlainFence verifies that
// a value that is not valid JSON (e.g. Atlassian's customfield_10000
// {key=value} mixed notation) renders in a plain ``` fenced block
// with NO `json` language tag.
func TestRenderIssue_CustomFieldsInvalidJSONAsPlainFence(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Invalid",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		CustomFields: map[string]json.RawMessage{
			"customfield_10000": json.RawMessage(
				`{pullrequest={overall={count=2}}, json={}}`),
		},
		Names: map[string]string{
			"customfield_10000": "Development",
		},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)
	assert.Contains(t, got, "- **Development**:\n  ```\n",
		"plain fence (no json tag) opens under bullet")
	assert.NotContains(t, got, "- **Development**:\n  ```json\n",
		"invalid JSON must NOT be tagged as json")
	assert.Contains(t, got, "pullrequest={overall=", "raw content preserved")
}

// TestPrettifyAtlassianBlob exercises the in-place pretty-printer
// for Atlassian's mixed Map.toString()+JSON notation that the
// kindStringStructured rendering branch delegates to. The walker
// is tested directly through the PrettifyAtlassianBlobForTest
// export hook so the rendering tests can stay focused on
// rendering rather than on the bytes of the walker's output.
//
// Cases cover the happy path (empty/balanced containers, simple
// Java notation, nested Java notation, embedded JSON objects and
// arrays, the PROJ-1573 dogfood blob), the JSON-string
// edge case (string literals containing braces must not confuse
// the depth tracker), and the malformed-input paths (unbalanced
// braces, `json=` not followed by a JSON value, `json=` followed
// by invalid JSON) — each malformed case returns the input
// unchanged with ok=false so the caller can fall back to verbatim
// rendering. Partial-mangled output is never returned.
func TestPrettifyAtlassianBlob(t *testing.T) {
	const platEng1573Blob = `{repository={count=1, dataType=repository}, json={"cachedValue":{"errors":[],"summary":{"repository":{"overall":{"count":1,"lastUpdated":"2026-05-13T11:36:08.000-0400","dataType":"repository"},"byInstanceType":{"GitHub":{"count":1,"name":"GitHub"}}}}},"isStale":true}}`

	tests := []struct {
		name        string
		input       string
		wantOK      bool
		wantOutput  string // checked when checkOutput is true
		checkOutput bool   // pin the exact bytes when true
		wantSubstr  []string
		notSubstr   []string
		wantInputAs bool // when true, output must equal the input verbatim
	}{
		{
			// Empty input is structurally valid: depth never moves,
			// so the walker returns "" with ok=true.
			name:        "empty input",
			input:       "",
			wantOK:      true,
			wantOutput:  "",
			checkOutput: true,
		},
		{
			// `{}` is the single-token degenerate case. The walker
			// collapses immediate-close pairs so trivial containers
			// do not balloon into three lines (the open brace's
			// newline+indent followed by the close brace's
			// newline+dedent).
			name:        "balanced empty object",
			input:       "{}",
			wantOK:      true,
			wantOutput:  "{}",
			checkOutput: true,
		},
		{
			// Same collapse rule for the array form.
			name:        "balanced empty array",
			input:       "[]",
			wantOK:      true,
			wantOutput:  "[]",
			checkOutput: true,
		},
		{
			// Single-level Java Map.toString() form. Each ", "
			// becomes a newline + indent at the current depth; the
			// `=` separator is preserved verbatim because the
			// walker treats every non-structural byte as content.
			name:       "atlassian simple",
			input:      "{a=1, b=2}",
			wantOK:     true,
			wantOutput: "{\n  a=1,\n  b=2\n}",
		},
		{
			// Nested Java notation. Depth tracking is the only way
			// to get the inner container indented one level deeper
			// than the outer one; a naive line-break-on-comma rule
			// would emit `b=1,\nc=2` at the outer depth, which
			// looks identical to a top-level key/value pair.
			name:       "atlassian nested",
			input:      "{a={b=1, c=2}, d=3}",
			wantOK:     true,
			wantOutput: "{\n  a={\n    b=1,\n    c=2\n  },\n  d=3\n}",
		},
		{
			// First mixed case: the `json=` marker delegates the
			// embedded `{"k":1}` to json.Indent with prefix matching
			// the current outer indent. The colon-space inside the
			// pretty JSON is the canonical proof that json.Indent
			// fired on the inner range.
			name:  "atlassian with embedded json object",
			input: `{repository={count=1}, json={"k":1}}`,
			wantSubstr: []string{
				"repository={\n",
				"json={\n",
				"\"k\": 1",
			},
			wantOK: true,
		},
		{
			// JSON arrays inside the embedded payload go through
			// the same json.Indent path; each array element gets
			// its own indented line.
			name:  "atlassian with embedded json array",
			input: `{repos={count=2}, json={"items":[1,2,3]}}`,
			wantSubstr: []string{
				"repos={\n",
				"json={\n",
				"\"items\": [",
				"1,\n",
				"2,\n",
				"3\n",
			},
			wantOK: true,
		},
		{
			// The PROJ-1573 dogfood blob in full. We do not pin
			// the exact bytes because the JSON pretty-printer's
			// output is governed by encoding/json (its own contract)
			// and re-pinning every byte would couple this test to
			// the stdlib's formatting rather than to the walker's
			// behaviour. Instead we assert the structural shape:
			// the outer wrapping breaks open, the inner JSON keys
			// each get `: ` (colon-space) and their own line, the
			// `"errors": []` collapsed array is visible, and the
			// `"lastUpdated":` key sits on its own line.
			name:  "PROJ-1573 realistic blob",
			input: platEng1573Blob,
			wantSubstr: []string{
				"repository={\n",
				"json={\n",
				"\"errors\": []",
				"\"lastUpdated\":",
				"\"isStale\": true",
				"\"GitHub\": {",
			},
			notSubstr: []string{
				`\"cachedValue\":`,
			},
			wantOK: true,
		},
		{
			// Unbalanced opening brace: depth never returns to 0,
			// so the walker yields ok=false and the input is
			// returned unchanged for verbatim fallback.
			name:        "malformed unbalanced opening brace",
			input:       "{a=1",
			wantOK:      false,
			wantInputAs: true,
		},
		{
			// Unbalanced closing brace: depth goes negative on the
			// `}`. Same ok=false contract.
			name:        "malformed unbalanced closing brace",
			input:       "a=1}",
			wantOK:      false,
			wantInputAs: true,
		},
		{
			// `json=` followed by a non-JSON value. The walker
			// only treats `json=` as a delegation marker when the
			// next byte is `{` or `[`; otherwise it is plain text.
			// Here `abc` is plain text, so the walker continues
			// past `json=abc` byte-by-byte and reaches the outer
			// closing `}` cleanly — the input is structurally valid
			// Java notation.
			name:       "json= not followed by JSON value renders as plain text",
			input:      "{json=abc}",
			wantOK:     true,
			wantOutput: "{\n  json=abc\n}",
		},
		{
			// `json=` followed by content that looks like a JSON
			// value (`{` opens it) but fails json.Indent: the
			// walker returns the input unchanged with ok=false so
			// the caller falls back to verbatim. The specific
			// failure here is the bare identifier `not` inside the
			// braces, which is not a valid JSON token.
			name:        "json= followed by invalid JSON",
			input:       "{json={not valid}}",
			wantOK:      false,
			wantInputAs: true,
		},
		{
			// JSON string literals inside the embedded payload may
			// contain `{` and `}` bytes; the depth tracker MUST
			// ignore braces inside JSON strings or the walker
			// would lose its place. The canonical test: a quoted
			// value containing both `{` and `}` must pretty-print
			// without the walker treating those bytes as structure.
			name:  "embedded JSON with string containing braces",
			input: `{json={"key": "value{with}braces"}}`,
			wantSubstr: []string{
				"json={\n",
				`"key": "value{with}braces"`,
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := render.PrettifyAtlassianBlobForTest(tt.input)
			assert.Equal(t, tt.wantOK, ok, "ok")

			if tt.wantInputAs {
				assert.Equal(t, tt.input, got,
					"malformed input must be returned unchanged for verbatim fallback")
				return
			}
			if tt.checkOutput {
				assert.Equal(t, tt.wantOutput, got, "pretty output bytes")
			}
			for _, sub := range tt.wantSubstr {
				assert.Contains(t, got, sub,
					"pretty output should contain %q", sub)
			}
			for _, sub := range tt.notSubstr {
				assert.NotContains(t, got, sub,
					"pretty output should NOT contain %q", sub)
			}
		})
	}
}

// TestRenderIssue_CustomFieldsStringStructuredAsPlainFence verifies
// the PROJ-1578 case: a custom-field value whose raw JSON form
// is a JSON-encoded *string* whose decoded contents are structured
// non-JSON text (Atlassian's Dev Status summary `{key=value,
// json={...}}` notation) must render in a PLAIN ``` fenced block
// (no language tag), with:
//
//   - the outer JSON-string quotes stripped from the rendered
//     content (the inner structured text is the only legible
//     representation),
//   - per-line two-space indentation under the bullet so the fence
//     does not terminate the surrounding Markdown list.
//
// This complements TestRenderIssue_CustomFieldsInvalidJSONAsPlainFence,
// which exercises the case where the raw bytes are NOT valid JSON at
// the outer layer (a rarer historical shape).
func TestRenderIssue_CustomFieldsStringStructuredAsJSONFence(t *testing.T) {
	// Atlassian's customfield_10000 Dev Status summary, delivered
	// JSON-string-encoded by the tenant. The inner content has a
	// '\n' so the renderer treats it as multi-line.
	//
	// Even though the inner content is not strictly valid JSON
	// (the outer wrapping uses Atlassian's {key=value} notation),
	// the bulk of the content is JSON-shaped (quoted strings,
	// numbers, colon/comma punctuation), so the renderer tags the
	// fence as `json` for syntax-highlighting consistency with
	// the structured case. Markdown viewers do not validate fence
	// language tags; they use them only as highlighting hints.
	innerBlob := "{repository={count=1, dataType=repository},\n" +
		"json={\"cachedValue\":{\"errors\":[]," +
		"\"summary\":{\"repository\":{\"overall\":" +
		"{\"count\":1,\"dataType\":\"repository\"}}}}," +
		"\"isStale\":true}}"
	encoded := mustRawMessage(innerBlob)

	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "String-structured",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		CustomFields: map[string]json.RawMessage{
			"customfield_10000": encoded,
		},
		Names: map[string]string{
			"customfield_10000": "Development",
		},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)

	// Fenced ```json block opens under the bullet, same as the
	// structured case. Consistent visual story for "field with
	// structured content."
	assert.Contains(t, got, "- **Development**:\n  ```json\n",
		"```json fence opens under bullet")
	assert.NotContains(t, got, "- **Development**:\n  ```\n{",
		"string-structured must use the json tag, not a plain fence")

	// The outer Java-notation object now breaks to multi-line
	// after each opening brace. prettifyAtlassianBlob walks the
	// bytes, tracks brace depth, and inserts a newline+indent
	// after every "{" or "[". The `repository={\n` substring
	// proves the walker fired on the outer wrapping; without it
	// we would still be looking at a single-line blob.
	assert.Contains(t, got, "repository={\n",
		"outer Java-notation object should break to multi-line after the opening brace")

	// The embedded json={...} payload is delegated to json.Indent
	// at the matching depth, so JSON-shape conventions apply to
	// the inner content: keys are followed by `: ` (colon-space)
	// rather than `:` alone. The `"isStale": true` substring is
	// the most stable proof — it lives at the top of the inner
	// JSON object and is unaffected by surrounding indentation
	// changes.
	assert.Contains(t, got, "\"isStale\": true",
		"inner JSON payload should be pretty-printed with space after colon")

	// The outer JSON-string quotes that wrapped the entire blob
	// must NOT appear in the rendered output: the rendered line
	// starts with `{`, not with `"`. Specifically, the line
	// `  "{repository=` (a 2-space indent followed by an open
	// quote) must be absent.
	assert.NotContains(t, got, "  \"{repository=",
		"outer JSON-string quote must be stripped from the rendered content")

	// The JSON-escaped form of the inner content's embedded
	// quotes must not survive — they were decoded once before
	// rendering, so the rendered output has bare `"cachedValue":`
	// rather than `\"cachedValue\":`.
	assert.NotContains(t, got, `\"cachedValue\":`,
		"inner content must be decoded once before rendering (no \\\" escapes)")
}

// TestRenderIssue_CustomFieldsEmptySection verifies that an issue
// with no custom fields, or whose every custom field would be
// elided, emits no `## Custom fields` header at all.
func TestRenderIssue_CustomFieldsEmptySection(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]json.RawMessage
	}{
		{"empty map", map[string]json.RawMessage{}},
		{"all null fields", map[string]json.RawMessage{
			"customfield_1": json.RawMessage(`null`),
			"customfield_2": json.RawMessage(`null`),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := parse.Issue{
				Key:          "EXAMPLE-1",
				Summary:      "X",
				Status:       "Open",
				IssueType:    "Task",
				Reporter:     "Alice",
				SourceURL:    "https://example.atlassian.net/browse/EXAMPLE-1",
				CustomFields: tt.fields,
			}
			got, err := render.RenderIssue(issue, nil, false)
			require.NoError(t, err)
			assert.NotContains(t, got, "## Custom fields",
				"section header must be elided")
		})
	}
}

// ---- RenderStub tests ----

// TestRenderStub_403 tests the stub output for a permission-denied issue.
func TestRenderStub_403(t *testing.T) {
	got, err := render.RenderStub("EXAMPLE-2", "Permission denied (403)", "https://example.atlassian.net/browse/EXAMPLE-2")
	require.NoError(t, err, "RenderStub")
	checkGolden(t, "stub_403.md", got)
}

// TestRenderStub_404 tests the stub output for a not-found issue.
func TestRenderStub_404(t *testing.T) {
	got, err := render.RenderStub("EXAMPLE-3", "Not found (404)", "https://example.atlassian.net/browse/EXAMPLE-3")
	require.NoError(t, err, "RenderStub")
	checkGolden(t, "stub_404.md", got)
}

// ---- RenderOutbound tests ----

// TestRenderOutbound_Mixed tests outbound.md with all three reference kinds.
func TestRenderOutbound_Mixed(t *testing.T) {
	refs := []render.OutboundRef{
		{Kind: "jira", IssueKey: "EXAMPLE-2"},
		{Kind: "jira", IssueKey: "EXAMPLE-3"},
		{Kind: "github-pr", Owner: "org", Repo: "repo", PRNumber: 42, URL: "https://github.com/org/repo/pull/42"},
		{Kind: "external", Text: "External doc", URL: "https://example.com/doc"},
	}
	got, err := render.RenderOutbound(refs)
	require.NoError(t, err, "RenderOutbound")
	checkGolden(t, "outbound_mixed.md", got)
}

// TestRenderOutbound_JiraOnly tests outbound.md with only Jira references.
func TestRenderOutbound_JiraOnly(t *testing.T) {
	refs := []render.OutboundRef{
		{Kind: "jira", IssueKey: "EXAMPLE-2"},
	}
	got, err := render.RenderOutbound(refs)
	require.NoError(t, err, "RenderOutbound")
	checkGolden(t, "outbound_jira_only.md", got)
}

// TestRenderOutbound_Empty tests that an empty refs slice returns "".
func TestRenderOutbound_Empty(t *testing.T) {
	got, err := render.RenderOutbound(nil)
	require.NoError(t, err, "RenderOutbound")
	assert.Empty(t, got, "expected empty string for empty refs")
}

// ---------------------------------------------------------------------------
// Development section (Dev Status pull requests)
// ---------------------------------------------------------------------------

// TestRenderIssue_WithPullRequests verifies that PullRequests populates a
// "## Development" section with a "### Pull requests" subsection placed
// between Description and Relationships, and that each PR renders as a
// single bullet line in the documented format.
func TestRenderIssue_WithPullRequests(t *testing.T) {
	issue := parse.Issue{
		Key:       "PROJ-1573",
		Summary:   "Cognito User Pool module",
		Status:    "Done",
		IssueType: "Story",
		Reporter:  "Robert Tirserio",
		Created:   mustTime("2026-05-01T08:00:00Z"),
		Updated:   mustTime("2026-05-08T13:44:52Z"),
		SourceURL: "https://example.atlassian.net/browse/PROJ-1573",
		// Make sure Relationships is also populated to validate ordering.
		Parent: &parse.ParentRef{Key: "PROJ-1566", Summary: "Auth Epic"},
		DevStatus: parse.DevStatusData{
			PullRequests: []parse.PullRequest{
				{
					ID:           "#557",
					URL:          "https://github.com/org/repo/pull/557",
					Title:        "PROJ-1573: Cognito User Pool",
					Status:       "MERGED",
					Application:  "GitHub",
					Repository:   "org/repo",
					SourceBranch: "feature/PROJ-1573",
					DestBranch:   "main",
					Author:       "Robert Tirserio",
				},
				{
					ID:          "#560",
					URL:         "https://github.com/org/repo/pull/560",
					Title:       "PROJ-1573: Follow-up",
					Status:      "MERGED",
					Application: "GitHub",
					Repository:  "org/repo",
					Author:      "Kareem Hepburn",
				},
			},
		},
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)

	// Section header present.
	assert.Contains(t, got, "## Development\n")
	assert.Contains(t, got, "### Pull requests\n")

	// Each PR rendered as documented.
	assert.Contains(t, got,
		"- [MERGED] [PROJ-1573: Cognito User Pool](https://github.com/org/repo/pull/557) — `org/repo` · Robert Tirserio\n")
	assert.Contains(t, got,
		"- [MERGED] [PROJ-1573: Follow-up](https://github.com/org/repo/pull/560) — `org/repo` · Kareem Hepburn\n")

	// Subsections not populated must be elided.
	assert.NotContains(t, got, "### Branches", "Branches subsection elided when empty")
	assert.NotContains(t, got, "### Commits", "Commits subsection elided when empty")
	assert.NotContains(t, got, "### Repositories", "Repositories subsection elided when empty")
	assert.NotContains(t, got, "### Builds", "Builds subsection elided when empty")

	// Order: Description < Development < Relationships.
	descIdx := strings.Index(got, "## Description")
	devIdx := strings.Index(got, "## Development")
	relIdx := strings.Index(got, "## Relationships")
	require.GreaterOrEqual(t, descIdx, 0)
	require.GreaterOrEqual(t, devIdx, 0)
	require.GreaterOrEqual(t, relIdx, 0)
	assert.Less(t, descIdx, devIdx, "Description must precede Development")
	assert.Less(t, devIdx, relIdx, "Development must precede Relationships")
}

// TestRenderIssue_NoPullRequests verifies the Development section is
// omitted entirely when PullRequests is nil/empty.
func TestRenderIssue_NoPullRequests(t *testing.T) {
	issue := parse.Issue{
		Key:          "EXAMPLE-99",
		Summary:      "No PRs",
		Status:       "Open",
		IssueType:    "Task",
		Reporter:     "Alice Example",
		Created:      mustTime("2026-03-01T08:00:00Z"),
		Updated:      mustTime("2026-03-01T08:00:00Z"),
		SourceURL:    "https://example.atlassian.net/browse/EXAMPLE-99",
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)
	assert.NotContains(t, got, "## Development", "Development section must be elided when PullRequests is empty")
}

// TestRenderIssue_PullRequestMissingRepoAndAuthor verifies that PRs with
// empty Repository or Author elide the corresponding segment cleanly.
func TestRenderIssue_PullRequestMissingRepoAndAuthor(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "S",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		DevStatus: parse.DevStatusData{
			PullRequests: []parse.PullRequest{
				{
					ID:     "#1",
					URL:    "https://github.com/o/r/pull/1",
					Title:  "Bare PR",
					Status: "OPEN",
					// No Repository, no Author.
				},
			},
		},
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)
	assert.Contains(t, got, "- [OPEN] [Bare PR](https://github.com/o/r/pull/1)\n",
		"missing repo/author segments must be elided cleanly")
}

// ---------------------------------------------------------------------------
// Development section: all subsections + elision
// ---------------------------------------------------------------------------

// TestRenderIssue_WithAllDevStatusSubsections verifies that all five
// Development subsections render in the documented canonical order
// (Pull requests, Branches, Commits, Repositories, Builds) when every
// list is non-empty.
func TestRenderIssue_WithAllDevStatusSubsections(t *testing.T) {
	authored := mustTime("2026-06-02T14:30:00Z")
	issue := parse.Issue{
		Key:       "PROJ-1578",
		Summary:   "OIDC flow",
		Status:    "In Progress",
		IssueType: "Story",
		Reporter:  "Robert Tirserio",
		SourceURL: "https://example.atlassian.net/browse/PROJ-1578",
		DevStatus: parse.DevStatusData{
			PullRequests: []parse.PullRequest{
				{
					URL:        "https://github.com/org/api/pull/100",
					Title:      "Initiate endpoint",
					Status:     "OPEN",
					Repository: "org/api",
					Author:     "Kareem Hepburn",
				},
			},
			Branches: []parse.Branch{
				{
					Name:          "feature/PROJ-1578",
					URL:           "https://github.com/org/api/tree/feature/PROJ-1578",
					Repository:    "org/api",
					LastCommitID:  "abc123d",
					LastCommitURL: "https://github.com/org/api/commit/abc123",
				},
			},
			Commits: []parse.Commit{
				{
					ID:         "abc123def456",
					ShortID:    "abc123d",
					URL:        "https://github.com/org/api/commit/abc123",
					Message:    "feat: add OIDC initiate endpoint",
					Author:     "Kareem Hepburn",
					Repository: "org/api",
					AuthoredAt: authored,
				},
			},
			Repositories: []parse.Repository{
				{Name: "org/api", URL: "https://github.com/org/api"},
			},
			Builds: []parse.Build{
				{
					ID:          "42",
					Name:        "Build #42",
					URL:         "https://bitbucket.org/org/api/pipelines/results/42",
					State:       "SUCCESSFUL",
					LastUpdated: authored,
					TestsPassed: 100,
					TestsTotal:  100,
				},
			},
		},
		CustomFields: map[string]json.RawMessage{},
	}

	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)

	// Section header present.
	assert.Contains(t, got, "## Development\n")

	// All five subsections present.
	prIdx := strings.Index(got, "### Pull requests")
	brIdx := strings.Index(got, "### Branches")
	cmIdx := strings.Index(got, "### Commits")
	rpIdx := strings.Index(got, "### Repositories")
	bdIdx := strings.Index(got, "### Builds")
	require.Positive(t, prIdx, "Pull requests subsection present")
	require.Positive(t, brIdx, "Branches subsection present")
	require.Positive(t, cmIdx, "Commits subsection present")
	require.Positive(t, rpIdx, "Repositories subsection present")
	require.Positive(t, bdIdx, "Builds subsection present")

	// Canonical order.
	assert.Less(t, prIdx, brIdx, "Pull requests must precede Branches")
	assert.Less(t, brIdx, cmIdx, "Branches must precede Commits")
	assert.Less(t, cmIdx, rpIdx, "Commits must precede Repositories")
	assert.Less(t, rpIdx, bdIdx, "Repositories must precede Builds")

	// Spot-check the documented per-entity formats.
	assert.Contains(t, got,
		"- [feature/PROJ-1578](https://github.com/org/api/tree/feature/PROJ-1578) — `org/api` · last: [abc123d](https://github.com/org/api/commit/abc123)\n")
	assert.Contains(t, got,
		"- [abc123d](https://github.com/org/api/commit/abc123) — \"feat: add OIDC initiate endpoint\" · Kareem Hepburn · 2026-06-02\n")
	assert.Contains(t, got, "- [org/api](https://github.com/org/api)\n")
	assert.Contains(t, got,
		"- [SUCCESSFUL] [Build #42](https://bitbucket.org/org/api/pipelines/results/42) — 2026-06-02 [tests 100/100]\n")
}

// TestRenderIssue_WithBranchesOnly verifies the Development parent
// section appears with only the Branches subsection when the other
// four lists are empty. This is the per-subsection elision contract.
func TestRenderIssue_WithBranchesOnly(t *testing.T) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Branch only",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		DevStatus: parse.DevStatusData{
			Branches: []parse.Branch{
				{
					Name:       "feature/x",
					URL:        "https://github.com/o/r/tree/feature/x",
					Repository: "o/r",
				},
			},
		},
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)

	assert.Contains(t, got, "## Development\n")
	assert.Contains(t, got, "### Branches\n")
	assert.NotContains(t, got, "### Pull requests", "PRs subsection elided")
	assert.NotContains(t, got, "### Commits", "Commits subsection elided")
	assert.NotContains(t, got, "### Repositories", "Repositories subsection elided")
	assert.NotContains(t, got, "### Builds", "Builds subsection elided")
}

// TestRenderIssue_WithBuildsAndTestSummary verifies the "[tests P/T]"
// suffix appears only when TestsTotal > 0; a build that did not publish
// a test summary renders the line without the suffix.
func TestRenderIssue_WithBuildsAndTestSummary(t *testing.T) {
	updated := mustTime("2026-06-02T14:30:00Z")
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		Summary:   "Builds",
		Status:    "Open",
		IssueType: "Task",
		Reporter:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		DevStatus: parse.DevStatusData{
			Builds: []parse.Build{
				{
					ID:          "1",
					Name:        "Build #1",
					URL:         "https://ci.example.com/1",
					State:       "SUCCESSFUL",
					LastUpdated: updated,
					TestsPassed: 50,
					TestsTotal:  50,
				},
				{
					ID:          "2",
					Name:        "Build #2",
					URL:         "https://ci.example.com/2",
					State:       "FAILED",
					LastUpdated: updated,
					// No test summary.
				},
			},
		},
		CustomFields: map[string]json.RawMessage{},
	}
	got, err := render.RenderIssue(issue, nil, false)
	require.NoError(t, err)

	assert.Contains(t, got,
		"- [SUCCESSFUL] [Build #1](https://ci.example.com/1) — 2026-06-02 [tests 50/50]\n",
		"build with test summary includes the [tests P/T] suffix")
	assert.Contains(t, got,
		"- [FAILED] [Build #2](https://ci.example.com/2) — 2026-06-02\n",
		"build without test summary omits the [tests P/T] suffix")
}
