package adf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/neumachen/gojira/classify"
	"github.com/neumachen/gojira/internal/adf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jiraSite is the placeholder Jira base URL used in the with_links fixture.
const jiraSite = "https://your-site.atlassian.net"

// ---- helpers ---------------------------------------------------------------

// readFixture reads a file from the testdata directory relative to the test.
func readFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, "read fixture %s", name)
	return json.RawMessage(data)
}

// readGolden reads a golden Markdown file from testdata.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, "read golden %s", name)
	return string(data)
}

// assertMarkdown renders doc and compares the output to the golden file.
func assertMarkdown(t *testing.T, doc json.RawMessage, goldenFile string) {
	t.Helper()
	got, _, err := adf.RenderMarkdown(doc)
	require.NoError(t, err, "RenderMarkdown")
	want := readGolden(t, goldenFile)
	assert.Equal(t, want, got, "RenderMarkdown output mismatch for %s", goldenFile)
}

// ---- RenderMarkdown golden tests -------------------------------------------

func TestRenderMarkdown_PlainParagraph(t *testing.T) {
	doc := readFixture(t, "plain_paragraph.json")
	assertMarkdown(t, doc, "plain_paragraph.md")
}

func TestRenderMarkdown_Heading(t *testing.T) {
	doc := readFixture(t, "heading.json")
	assertMarkdown(t, doc, "heading.md")
}

func TestRenderMarkdown_BulletList(t *testing.T) {
	doc := readFixture(t, "bullet_list.json")
	assertMarkdown(t, doc, "bullet_list.md")
}

func TestRenderMarkdown_WithLinks(t *testing.T) {
	doc := readFixture(t, "with_links.json")
	assertMarkdown(t, doc, "with_links.md")
}

func TestRenderMarkdown_UnknownNode(t *testing.T) {
	doc := readFixture(t, "unknown_node.json")
	got, unknown, err := adf.RenderMarkdown(doc)
	require.NoError(t, err, "RenderMarkdown")
	// Golden comparison.
	want := readGolden(t, "unknown_node.md")
	assert.Equal(t, want, got, "RenderMarkdown output mismatch")
	// Must have exactly one unknown node of type "panel".
	require.Len(t, unknown, 1, "expected 1 unknown node")
	assert.Equal(t, "panel", unknown[0].NodeType, "unknown node type")
	assert.NotEmpty(t, unknown[0].Raw, "unknown node Raw")
}

func TestRenderMarkdown_NestedMarks(t *testing.T) {
	doc := readFixture(t, "nested_marks.json")
	assertMarkdown(t, doc, "nested_marks.md")
}

func TestRenderMarkdown_NullDoc(t *testing.T) {
	doc := readFixture(t, "null_doc.json")
	got, unknown, err := adf.RenderMarkdown(doc)
	require.NoError(t, err, "RenderMarkdown")
	assert.Empty(t, got, "expected empty string for null doc")
	assert.Empty(t, unknown, "expected no unknown nodes for null doc")
}

// ---- ExtractLinks tests ----------------------------------------------------

func TestExtractLinks_WithLinks(t *testing.T) {
	doc := readFixture(t, "with_links.json")
	links, err := adf.ExtractLinks(doc, jiraSite)
	require.NoError(t, err, "ExtractLinks")
	require.Len(t, links, 3, "expected 3 links")

	tests := []struct {
		wantURL  string
		wantText string
		wantKind classify.Kind
	}{
		{
			wantURL:  "https://your-site.atlassian.net/browse/EXAMPLE-2",
			wantText: "See EXAMPLE-2",
			wantKind: classify.KindJiraURL,
		},
		{
			wantURL:  "https://github.com/org/repo/pull/42",
			wantText: "org/repo#42",
			wantKind: classify.KindGitHubPR,
		},
		{
			wantURL:  "https://example.com/doc",
			wantText: "external doc",
			wantKind: classify.KindExternal,
		},
	}

	for i, tt := range tests {
		l := links[i]
		assert.Equal(t, tt.wantURL, l.URL, "link[%d].URL", i)
		assert.Equal(t, tt.wantText, l.Text, "link[%d].Text", i)
		assert.Equal(t, tt.wantKind, l.Classification.Kind, "link[%d].Classification.Kind", i)
	}

	// Verify Jira link carries the issue key.
	assert.Equal(t, "EXAMPLE-2", links[0].Classification.IssueKey, "Jira link IssueKey")
	// Verify GitHub PR carries owner/repo/number.
	assert.Equal(t, "org", links[1].Classification.Owner, "GitHub PR Owner")
	assert.Equal(t, "repo", links[1].Classification.Repo, "GitHub PR Repo")
	assert.Equal(t, 42, links[1].Classification.PRNumber, "GitHub PR PRNumber")
}

func TestExtractLinks_PlainParagraph(t *testing.T) {
	// A document with no links should return an empty (nil) slice.
	doc := readFixture(t, "plain_paragraph.json")
	links, err := adf.ExtractLinks(doc, jiraSite)
	require.NoError(t, err, "ExtractLinks")
	assert.Empty(t, links, "expected 0 links")
}

func TestExtractLinks_NullDoc(t *testing.T) {
	doc := readFixture(t, "null_doc.json")
	links, err := adf.ExtractLinks(doc, jiraSite)
	require.NoError(t, err, "ExtractLinks")
	assert.Empty(t, links, "expected 0 links for null doc")
}

func TestExtractLinks_UnknownNode(t *testing.T) {
	// The unknown_node fixture has no link marks; ExtractLinks should return empty.
	doc := readFixture(t, "unknown_node.json")
	links, err := adf.ExtractLinks(doc, jiraSite)
	require.NoError(t, err, "ExtractLinks")
	assert.Empty(t, links, "expected 0 links")
}

// ---- Walk tests ------------------------------------------------------------

func TestWalk_NullDoc(t *testing.T) {
	// Walk on null should call the visitor zero times and return nil.
	called := 0
	err := adf.Walk(json.RawMessage("null"), adf.VisitorFunc(func(_ *adf.Node, _ json.RawMessage) error {
		called++
		return nil
	}))
	require.NoError(t, err, "Walk")
	assert.Equal(t, 0, called, "expected 0 visitor calls for null doc")
}

func TestWalk_PlainParagraph(t *testing.T) {
	// Walk should visit: doc, paragraph, text — 3 nodes total.
	doc := readFixture(t, "plain_paragraph.json")
	var types []string
	err := adf.Walk(doc, adf.VisitorFunc(func(n *adf.Node, _ json.RawMessage) error {
		types = append(types, n.Type)
		return nil
	}))
	require.NoError(t, err, "Walk")
	want := []string{"doc", "paragraph", "text"}
	require.Len(t, types, len(want), "visited node types: got %v, want %v", types, want)
	for i, typ := range want {
		assert.Equal(t, typ, types[i], "node[%d]", i)
	}
}

// ---- RenderMarkdown edge cases ---------------------------------------------

func TestRenderMarkdown_EmptyRawMessage(t *testing.T) {
	got, unknown, err := adf.RenderMarkdown(json.RawMessage(nil))
	require.NoError(t, err, "RenderMarkdown(nil)")
	assert.Empty(t, got, "expected empty string")
	assert.Empty(t, unknown, "expected no unknown nodes")
}

func TestRenderMarkdown_UnknownNodeComment(t *testing.T) {
	// The unknown_node fixture must produce a Markdown comment for the panel.
	doc := readFixture(t, "unknown_node.json")
	got, _, err := adf.RenderMarkdown(doc)
	require.NoError(t, err, "RenderMarkdown")
	const wantComment = `<!-- adf: unknown node type "panel" -->`
	assert.Contains(t, got, wantComment, "expected Markdown comment in output")
	// Inner text must also be preserved.
	const wantText = "Panel inner text."
	assert.Contains(t, got, wantText, "expected inner text preserved in output")
}
