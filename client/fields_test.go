package client_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/adf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeBody unmarshals a rendered body into a generic map so tests assert
// structure rather than coupling to literal whitespace or key ordering.
func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoErrorf(t, json.Unmarshal(b, &got), "decode body: %s", string(b))
	return got
}

// fields extracts the "fields" sub-object from a decoded body, or nil.
func fieldsOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	v, ok := body["fields"]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	require.True(t, ok, `"fields" must be a JSON object, got %T`, v)
	return m
}

// updates extracts the "update" sub-object from a decoded body, or nil.
func updatesOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	v, ok := body["update"]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	require.True(t, ok, `"update" must be a JSON object, got %T`, v)
	return m
}

// ---------------------------------------------------------------------------
// RenderCreateBody — required-field seeding + the full option surface
// ---------------------------------------------------------------------------

// TestRenderCreateBody_SeedsProjectAndIssueType locks in the contract that
// the two required positional parameters (project key + issuetype name)
// are always present in the rendered body, regardless of options.
func TestRenderCreateBody_SeedsProjectAndIssueType(t *testing.T) {
	t.Parallel()
	body, err := client.RenderCreateBody("PROJ", "Task")
	require.NoError(t, err)

	f := fieldsOf(t, decodeBody(t, body))
	require.NotNil(t, f, `"fields" must be populated`)
	assert.Equal(t, map[string]any{"key": "PROJ"}, f["project"], "project")
	assert.Equal(t, map[string]any{"name": "Task"}, f["issuetype"], "issuetype")
}

// TestRenderCreateBody_OptionMatrix walks every supplied option and asserts
// the field it contributes to the fields map. The "kitchen-sink" pattern
// also documents how the options compose into a single body.
func TestRenderCreateBody_OptionMatrix(t *testing.T) {
	t.Parallel()

	descADF := json.RawMessage(`{"version":1,"type":"doc","content":[{"type":"paragraph"}]}`)

	body, err := client.RenderCreateBody("PROJ", "Task",
		client.WithSummary("S"),
		client.WithAssigneeAccountID("aaa-111"),
		client.WithLabels("urgent", "backend"),
		client.WithParent("PROJ-9"),
		client.WithField("customfield_10010", 42),
		client.WithRawFields(map[string]any{
			"customfield_10011": "x",
			"customfield_10012": map[string]any{"value": "v"},
		}),
		client.WithDescriptionADF(descADF),
	)
	require.NoError(t, err)
	f := fieldsOf(t, decodeBody(t, body))
	require.NotNil(t, f)

	assert.Equal(t, "S", f["summary"], "summary")
	assert.Equal(t, map[string]any{"accountId": "aaa-111"}, f["assignee"], "assignee")
	// JSON numbers decode as float64 in a generic map.
	assert.Equal(t, float64(42), f["customfield_10010"], "WithField passthrough")
	assert.Equal(t, []any{"urgent", "backend"}, f["labels"], "labels")
	assert.Equal(t, map[string]any{"key": "PROJ-9"}, f["parent"], "parent")
	assert.Equal(t, "x", f["customfield_10011"], "WithRawFields key 1")
	assert.Equal(t, map[string]any{"value": "v"}, f["customfield_10012"], "WithRawFields key 2")

	// description was passed as raw ADF; it must round-trip as an object
	// (NOT a quoted string).
	desc, ok := f["description"].(map[string]any)
	require.True(t, ok, "description must be embedded as a JSON object, got %T", f["description"])
	assert.Equal(t, "doc", desc["type"])
}

// TestRenderCreateBody_DescriptionTextBecomesADF confirms WithDescriptionText
// runs the plain text through internal/adf.BuildParagraphDoc, NOT a quoted
// string. The text must round-trip through the existing adf reader.
func TestRenderCreateBody_DescriptionTextBecomesADF(t *testing.T) {
	t.Parallel()
	body, err := client.RenderCreateBody("PROJ", "Task",
		client.WithSummary("S"),
		client.WithDescriptionText("hello world"),
	)
	require.NoError(t, err)
	f := fieldsOf(t, decodeBody(t, body))

	desc, ok := f["description"].(map[string]any)
	require.True(t, ok, "description must be an object, got %T", f["description"])
	assert.Equal(t, "doc", desc["type"], "ADF doc type")

	// Re-marshal the description and feed it back through the reader.
	descBytes, err := json.Marshal(desc)
	require.NoError(t, err)
	md, _, err := adf.RenderMarkdown(descBytes)
	require.NoError(t, err)
	assert.Contains(t, md, "hello world", "ADF must round-trip the original text")
}

// TestRenderCreateBody_ExtensibilityViaWithField documents the seam: a
// fictitious new Jira field "customfield_99999" can be added today with no
// new option and no signature change — only WithField. PRD reviewers
// asserting "ZERO signature churn for new fields" check this property.
func TestRenderCreateBody_ExtensibilityViaWithField(t *testing.T) {
	t.Parallel()
	body, err := client.RenderCreateBody("PROJ", "Task",
		client.WithSummary("S"),
		client.WithField("customfield_99999", map[string]any{
			"value": "newly-supported-field",
		}),
	)
	require.NoError(t, err)
	f := fieldsOf(t, decodeBody(t, body))
	require.NotNil(t, f)
	assert.Equal(t,
		map[string]any{"value": "newly-supported-field"},
		f["customfield_99999"],
		"new field must surface via WithField with no signature change")
}

// ---------------------------------------------------------------------------
// RenderUpdateBody — fields + update map semantics, empty-body shape
// ---------------------------------------------------------------------------

func TestRenderUpdateBody_FieldsOptionsShared(t *testing.T) {
	t.Parallel()

	body, err := client.RenderUpdateBody(
		client.WithSummaryUpdate("new summary"),
		client.WithDescriptionTextUpdate("the new description"),
		client.WithLabelsUpdate("a", "b"),
		client.WithFieldUpdate("customfield_10010", 7),
		client.WithRawFieldsUpdate(map[string]any{"customfield_77": "y"}),
		client.WithAssigneeAccountIDUpdate("acc-1"),
	)
	require.NoError(t, err)

	decoded := decodeBody(t, body)
	f := fieldsOf(t, decoded)
	require.NotNil(t, f, `"fields" must be populated by Update options`)

	assert.Equal(t, "new summary", f["summary"])
	assert.Equal(t, []any{"a", "b"}, f["labels"])
	assert.Equal(t, float64(7), f["customfield_10010"])
	assert.Equal(t, "y", f["customfield_77"])
	assert.Equal(t, map[string]any{"accountId": "acc-1"}, f["assignee"])

	desc, ok := f["description"].(map[string]any)
	require.True(t, ok)
	descBytes, _ := json.Marshal(desc)
	md, _, err := adf.RenderMarkdown(descBytes)
	require.NoError(t, err)
	assert.Contains(t, md, "the new description")

	// No update verbs were used → the "update" key should be absent so
	// Jira does not see an empty {"update":{}} blob.
	_, hasUpdate := decoded["update"]
	assert.False(t, hasUpdate, `"update" key must be omitted when no update verbs used`)
}

func TestRenderUpdateBody_WithUpdateVerb_AddRemoveSet(t *testing.T) {
	t.Parallel()

	body, err := client.RenderUpdateBody(
		client.WithUpdateVerb("labels", "add", "urgent"),
		client.WithUpdateVerb("labels", "remove", "stale"),
		client.WithUpdateVerb("summary", "set", "renamed"),
	)
	require.NoError(t, err)

	u := updatesOf(t, decodeBody(t, body))
	require.NotNil(t, u, `"update" must be populated`)

	labels, ok := u["labels"].([]any)
	require.True(t, ok, "update.labels must be a JSON array, got %T", u["labels"])
	require.Len(t, labels, 2, "update.labels must preserve add+remove order")
	assert.Equal(t, map[string]any{"add": "urgent"}, labels[0])
	assert.Equal(t, map[string]any{"remove": "stale"}, labels[1])

	summaryOps, ok := u["summary"].([]any)
	require.True(t, ok, "update.summary must be a JSON array, got %T", u["summary"])
	require.Len(t, summaryOps, 1)
	assert.Equal(t, map[string]any{"set": "renamed"}, summaryOps[0])
}

// TestRenderUpdateBody_EmptyYieldsEmptyObject pins the no-options behaviour:
// the body is {}, not {"fields":{}}, so Jira treats it as a no-op rather
// than rejecting a malformed payload.
func TestRenderUpdateBody_EmptyYieldsEmptyObject(t *testing.T) {
	t.Parallel()
	body, err := client.RenderUpdateBody()
	require.NoError(t, err)
	assert.Equal(t, "{}", strings.TrimSpace(string(body)),
		"empty options must render as a bare empty object")
}
