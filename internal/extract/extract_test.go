package extract_test

import (
	"encoding/json"
	"testing"

	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSite = "https://example.atlassian.net"

// adfDoc builds a minimal ADF document JSON containing a single paragraph
// with the given link marks. Each entry in links is {href, text}.
func adfDoc(links []struct{ href, text string }) json.RawMessage {
	type markAttrs struct {
		Href string `json:"href"`
	}
	type markNode struct {
		Type  string    `json:"type"`
		Attrs markAttrs `json:"attrs"`
	}
	type textNode struct {
		Type  string     `json:"type"`
		Text  string     `json:"text"`
		Marks []markNode `json:"marks"`
	}
	type paraNode struct {
		Type    string     `json:"type"`
		Content []textNode `json:"content"`
	}
	type docNode struct {
		Version int        `json:"version"`
		Type    string     `json:"type"`
		Content []paraNode `json:"content"`
	}

	var texts []textNode
	for _, l := range links {
		texts = append(texts, textNode{
			Type: "text",
			Text: l.text,
			Marks: []markNode{
				{Type: "link", Attrs: markAttrs{Href: l.href}},
			},
		})
	}
	doc := docNode{
		Version: 1,
		Type:    "doc",
		Content: []paraNode{
			{Type: "paragraph", Content: texts},
		},
	}
	b, _ := json.Marshal(doc)
	return b
}

// emptyIssue returns a minimal parse.Issue with no links of any kind.
func emptyIssue() parse.Issue {
	return parse.Issue{
		Key:          "EXAMPLE-0",
		CustomFields: make(map[string]json.RawMessage),
	}
}

// ---- Test: ADF description links -------------------------------------------

// TestDescriptionLinks verifies that three links in the ADF description
// (Jira URL, GitHub PR URL, external URL) produce three SourceDescription
// references with the correct Kinds.
func TestDescriptionLinks(t *testing.T) {
	jiraURL := testSite + "/browse/EXAMPLE-1"
	ghPRURL := "https://github.com/org/repo/pull/42"
	extURL := "https://example.com/docs"

	issue := emptyIssue()
	issue.Description = adfDoc([]struct{ href, text string }{
		{jiraURL, "EXAMPLE-1"},
		{ghPRURL, "PR #42"},
		{extURL, "Docs"},
	})

	refs, err := extract.Extract(issue, testSite)
	require.NoError(t, err)
	require.Len(t, refs, 3, "want 3 references")

	// All must be SourceDescription.
	for i, r := range refs {
		assert.Equal(t, extract.SourceDescription, r.Source, "refs[%d].Source", i)
	}

	// Check Kinds in order.
	wantKinds := []classify.Kind{classify.KindJiraURL, classify.KindGitHubPR, classify.KindExternal}
	for i, want := range wantKinds {
		assert.Equal(t, want, refs[i].Kind, "refs[%d].Kind", i)
	}

	// Jira URL reference should carry the issue key.
	assert.Equal(t, "EXAMPLE-1", refs[0].IssueKey, "refs[0].IssueKey")

	// GitHub PR reference should carry Owner/Repo/PRNumber via ClassifyResult.
	assert.Equal(t, "org", refs[1].ClassifyResult.Owner, "refs[1].ClassifyResult.Owner")
	assert.Equal(t, "repo", refs[1].ClassifyResult.Repo, "refs[1].ClassifyResult.Repo")
	assert.Equal(t, 42, refs[1].ClassifyResult.PRNumber, "refs[1].ClassifyResult.PRNumber")

	// External reference URL should be preserved.
	assert.Equal(t, extURL, refs[2].URL, "refs[2].URL")
}

// ---- Test: relationship fields ---------------------------------------------

// TestRelationshipFields verifies that Parent, Subtasks, and IssueLinks each
// produce SourceRelationship references with KindJiraKey and populated IssueKey.
func TestRelationshipFields(t *testing.T) {
	issue := emptyIssue()
	issue.Parent = &parse.ParentRef{Key: "EXAMPLE-10", Summary: "Parent summary"}
	issue.Subtasks = []parse.LinkedIssue{
		{Key: "EXAMPLE-11", Summary: "Subtask one"},
		{Key: "EXAMPLE-12", Summary: "Subtask two"},
	}
	issue.IssueLinks = []parse.IssueLink{
		{Direction: "outward", Type: "Blocks", Key: "EXAMPLE-13", Summary: "Blocked issue"},
	}

	refs, err := extract.Extract(issue, testSite)
	require.NoError(t, err)
	require.Len(t, refs, 4, "want 4 references")

	for i, r := range refs {
		assert.Equal(t, extract.SourceRelationship, r.Source, "refs[%d].Source", i)
		assert.Equal(t, classify.KindJiraKey, r.Kind, "refs[%d].Kind", i)
		assert.NotEmpty(t, r.IssueKey, "refs[%d].IssueKey", i)
	}

	// Verify specific keys and texts.
	assert.Equal(t, "EXAMPLE-10", refs[0].IssueKey, "refs[0].IssueKey")
	assert.Equal(t, "Parent summary", refs[0].Text, "refs[0].Text")
	assert.Equal(t, "EXAMPLE-11", refs[1].IssueKey, "refs[1].IssueKey")
	assert.Equal(t, "EXAMPLE-12", refs[2].IssueKey, "refs[2].IssueKey")

	// IssueLink should carry Relation.
	assert.Equal(t, "EXAMPLE-13", refs[3].IssueKey, "refs[3].IssueKey")
	assert.Equal(t, "outward Blocks", refs[3].Relation, "refs[3].Relation")

	// Parent and subtasks must have empty Relation.
	for i := 0; i < 3; i++ {
		assert.Empty(t, refs[i].Relation, "refs[%d].Relation", i)
	}
}

// ---- Test: remote links ----------------------------------------------------

// TestRemoteLinks verifies that remote links are classified correctly and
// tagged SourceRemoteLink, with Text set from the remote link's Title.
func TestRemoteLinks(t *testing.T) {
	issue := emptyIssue()
	issue.RemoteLinks = []parse.RemoteLink{
		{
			Title: "Related Jira issue",
			URL:   testSite + "/browse/EXAMPLE-20",
		},
		{
			Title: "Fix PR",
			URL:   "https://github.com/org/repo/pull/99",
		},
	}

	refs, err := extract.Extract(issue, testSite)
	require.NoError(t, err)
	require.Len(t, refs, 2, "want 2 references")

	// Both must be SourceRemoteLink.
	for i, r := range refs {
		assert.Equal(t, extract.SourceRemoteLink, r.Source, "refs[%d].Source", i)
	}

	// First: Jira URL.
	assert.Equal(t, classify.KindJiraURL, refs[0].Kind, "refs[0].Kind")
	assert.Equal(t, "EXAMPLE-20", refs[0].IssueKey, "refs[0].IssueKey")
	assert.Equal(t, "Related Jira issue", refs[0].Text, "refs[0].Text")

	// Second: GitHub PR.
	assert.Equal(t, classify.KindGitHubPR, refs[1].Kind, "refs[1].Kind")
	assert.Equal(t, "Fix PR", refs[1].Text, "refs[1].Text")
	assert.Equal(t, 99, refs[1].ClassifyResult.PRNumber, "refs[1].ClassifyResult.PRNumber")
}

// ---- Test: no links --------------------------------------------------------

// TestNoLinks verifies that an issue with no references of any kind returns a
// non-nil empty slice and no error.
func TestNoLinks(t *testing.T) {
	issue := emptyIssue()

	refs, err := extract.Extract(issue, testSite)
	require.NoError(t, err)
	require.NotNil(t, refs, "want non-nil empty slice")
	assert.Empty(t, refs, "want 0 references")
}

// ---- Test: malformed ADF description ---------------------------------------

// TestMalformedADF verifies that a description containing invalid JSON causes
// Extract to return an error. No references are emitted.
func TestMalformedADF(t *testing.T) {
	issue := emptyIssue()
	issue.Description = json.RawMessage(`not json`)

	refs, err := extract.Extract(issue, testSite)
	require.Error(t, err, "want error for malformed ADF")
	assert.Nil(t, refs, "want nil refs on error")
}

// ---- Test: ordering contract -----------------------------------------------

// TestOrdering verifies the documented return order:
// description links → parent → subtasks → issue links → remote links.
func TestOrdering(t *testing.T) {
	issue := emptyIssue()

	// Description: one external link.
	issue.Description = adfDoc([]struct{ href, text string }{
		{"https://example.com/desc", "desc link"},
	})

	// Parent.
	issue.Parent = &parse.ParentRef{Key: "EXAMPLE-30", Summary: "Parent"}

	// Two subtasks.
	issue.Subtasks = []parse.LinkedIssue{
		{Key: "EXAMPLE-31", Summary: "Sub one"},
		{Key: "EXAMPLE-32", Summary: "Sub two"},
	}

	// One issue link.
	issue.IssueLinks = []parse.IssueLink{
		{Direction: "inward", Type: "Relates", Key: "EXAMPLE-33", Summary: "Related"},
	}

	// One remote link.
	issue.RemoteLinks = []parse.RemoteLink{
		{Title: "Remote", URL: "https://example.com/remote"},
	}

	refs, err := extract.Extract(issue, testSite)
	require.NoError(t, err)

	// Total: 1 desc + 1 parent + 2 subtasks + 1 issuelink + 1 remotelink = 6.
	require.Len(t, refs, 6, "want 6 references")

	wantSources := []extract.Source{
		extract.SourceDescription,  // desc link
		extract.SourceRelationship, // parent
		extract.SourceRelationship, // subtask 1
		extract.SourceRelationship, // subtask 2
		extract.SourceRelationship, // issue link
		extract.SourceRemoteLink,   // remote link
	}
	for i, want := range wantSources {
		assert.Equal(t, want, refs[i].Source, "refs[%d].Source", i)
	}

	// Spot-check keys/URLs to confirm identity within each group.
	assert.Equal(t, "https://example.com/desc", refs[0].URL, "refs[0].URL")
	assert.Equal(t, "EXAMPLE-30", refs[1].IssueKey, "refs[1].IssueKey")
	assert.Equal(t, "EXAMPLE-31", refs[2].IssueKey, "refs[2].IssueKey")
	assert.Equal(t, "EXAMPLE-32", refs[3].IssueKey, "refs[3].IssueKey")
	assert.Equal(t, "EXAMPLE-33", refs[4].IssueKey, "refs[4].IssueKey")
	assert.Equal(t, "https://example.com/remote", refs[5].URL, "refs[5].URL")
}
