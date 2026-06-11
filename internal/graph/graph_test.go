// Package graph_test verifies the pure graph collector and renderers.
//
// These are package-local unit tests (white-box) for the in-memory model;
// they exercise the Collector deduplication semantics and the two renderers
// (JSON, D2). No I/O — the package itself is pure and the tests stay pure.
package graph

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper: build a Reference for a Jira key (description source)
func descJiraRef(key string) extract.Reference {
	return extract.Reference{
		Kind:     classify.KindJiraKey,
		IssueKey: key,
		Source:   extract.SourceDescription,
		ClassifyResult: classify.Result{
			Kind:     classify.KindJiraKey,
			IssueKey: key,
		},
	}
}

func ghPRRef(url, owner, repo string, pr int) extract.Reference {
	return extract.Reference{
		Kind:   classify.KindGitHubPR,
		URL:    url,
		Source: extract.SourceDescription,
		ClassifyResult: classify.Result{
			Kind: classify.KindGitHubPR, Owner: owner, Repo: repo, PRNumber: pr, URL: url,
		},
	}
}

func extRef(url string) extract.Reference {
	return extract.Reference{
		Kind:           classify.KindExternal,
		URL:            url,
		Text:           "External thing",
		Source:         extract.SourceDescription,
		ClassifyResult: classify.Result{Kind: classify.KindExternal, URL: url},
	}
}

// ---------------------------------------------------------------------------
// Collector: A↔B cycle dedupes to two nodes + each edge once.
// ---------------------------------------------------------------------------

func TestCollector_Cycle_DedupesNodesAndEdges(t *testing.T) {
	t.Parallel()

	a := parse.Issue{
		Key: "PROJ-1", Summary: "first", Status: "Open", IssueType: "Task",
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Blocks", Key: "PROJ-2", Summary: "second"},
		},
	}
	b := parse.Issue{
		Key: "PROJ-2", Summary: "second", Status: "Open", IssueType: "Task",
		IssueLinks: []parse.IssueLink{
			{Direction: "inward", Type: "Blocks", Key: "PROJ-1", Summary: "first"},
		},
	}

	c := NewCollector()
	c.Add(a, nil)
	c.Add(b, nil)

	m := c.Model()
	assert.Len(t, m.Nodes, 2)
	// Both nodes are issue + fetched.
	for _, n := range m.Nodes {
		assert.Equal(t, NodeIssue, n.Kind)
		assert.True(t, n.Fetched, "both issues were Add'd → fetched")
	}
	// Two edges (A->B and B->A), both Kind=link.
	require.Len(t, m.Edges, 2)
	for _, e := range m.Edges {
		assert.Equal(t, EdgeLink, e.Kind)
	}
	// Sorted determinism: re-Model() yields identical edges.
	m2 := c.Model()
	assert.Equal(t, m, m2)
}

// ---------------------------------------------------------------------------
// Collector: structured relationships produce the expected edge kinds.
// ---------------------------------------------------------------------------

func TestCollector_StructuredEdgeKinds(t *testing.T) {
	t.Parallel()

	iss := parse.Issue{
		Key:       "PROJ-10",
		Summary:   "epic",
		Status:    "In Progress",
		IssueType: "Epic",
		Assignee:  "Alice",
		SourceURL: "https://example.atlassian.net/browse/PROJ-10",
		Parent:    &parse.ParentRef{Key: "PROJ-1", Summary: "root"},
		Subtasks: []parse.LinkedIssue{
			{Key: "PROJ-11", Summary: "subtask"},
		},
		Children: []string{"PROJ-12"},
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Blocks", Key: "PROJ-13", Summary: "blocked"},
		},
		RemoteLinks: []parse.RemoteLink{
			{Title: "Wiki entry", URL: "https://wiki.example.com/page"},
		},
		DevStatus: parse.DevStatusData{
			PullRequests: []parse.PullRequest{
				{URL: "https://github.com/acme/widget/pull/42", Title: "Fix the widget", Application: "GitHub"},
			},
		},
	}

	c := NewCollector()
	c.Add(iss, nil)
	m := c.Model()

	// Edge kind census.
	kinds := edgeKindCensus(m.Edges)
	assert.Equal(t, 1, kinds[EdgeParent], "expected 1 parent edge")
	assert.Equal(t, 1, kinds[EdgeSubtask], "expected 1 subtask edge")
	assert.Equal(t, 1, kinds[EdgeChild], "expected 1 child edge")
	assert.Equal(t, 1, kinds[EdgeLink], "expected 1 link edge")
	assert.Equal(t, 1, kinds[EdgeRemote], "expected 1 remote edge")
	assert.Equal(t, 1, kinds[EdgePullRequest], "expected 1 pull_request edge")

	// Node kinds present.
	prID := "acme/widget#42"
	require.NotNil(t, findNode(m, prID), "PR node must be present with owner/repo#N id")
	pr := findNode(m, prID)
	assert.Equal(t, NodeGitHubPR, pr.Kind)

	// Parent/sub/child/link issue nodes exist as referenced (Fetched=false).
	for _, key := range []string{"PROJ-1", "PROJ-11", "PROJ-12", "PROJ-13"} {
		n := findNode(m, key)
		require.NotNil(t, n, "expected issue node %s", key)
		assert.Equal(t, NodeIssue, n.Kind)
		assert.False(t, n.Fetched, "%s was only referenced", key)
	}

	// The Add'd issue PROJ-10 is fetched and has metadata populated.
	root := findNode(m, "PROJ-10")
	require.NotNil(t, root)
	assert.True(t, root.Fetched)
	assert.Equal(t, "Epic", root.Type)
	assert.Equal(t, "In Progress", root.Status)
	assert.Equal(t, "Alice", root.Assignee)
	assert.Equal(t, iss.SourceURL, root.URL)

	// IssueLink edge carries the relation label.
	link := findEdge(m, "PROJ-10", "PROJ-13", EdgeLink)
	require.NotNil(t, link)
	assert.Contains(t, strings.ToLower(link.Label), "blocks")
}

// ---------------------------------------------------------------------------
// Collector: refs dedupe against structured edges; external + PR via refs.
// ---------------------------------------------------------------------------

func TestCollector_RefsDedupAgainstStructured(t *testing.T) {
	t.Parallel()

	iss := parse.Issue{
		Key: "PROJ-1", Summary: "x", Status: "Open", IssueType: "Task",
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Blocks", Key: "PROJ-2", Summary: "two"},
		},
	}
	// refs include the same key via description (would yield "description"
	// edge) AND a fresh external + PR.
	refs := []extract.Reference{
		descJiraRef("PROJ-2"),
		ghPRRef("https://github.com/acme/widget/pull/7", "acme", "widget", 7),
		extRef("https://external.example.com/doc"),
	}

	c := NewCollector()
	c.Add(iss, refs)
	m := c.Model()

	// Structured "link" edge wins; no duplicate "description" edge for PROJ-1->PROJ-2.
	linkEdges := edgesBetween(m.Edges, "PROJ-1", "PROJ-2")
	require.Len(t, linkEdges, 1, "structured link edge should win over description duplicate")
	assert.Equal(t, EdgeLink, linkEdges[0].Kind)

	// PR node + edge present.
	require.NotNil(t, findEdge(m, "PROJ-1", "acme/widget#7", EdgePullRequest))
	require.NotNil(t, findNode(m, "acme/widget#7"))

	// External node + edge present.
	require.NotNil(t, findEdge(m, "PROJ-1", "https://external.example.com/doc", EdgeExternal))
	extN := findNode(m, "https://external.example.com/doc")
	require.NotNil(t, extN)
	assert.Equal(t, NodeExternal, extN.Kind)
}

// ---------------------------------------------------------------------------
// MarkFetched flips a previously referenced placeholder.
// ---------------------------------------------------------------------------

func TestCollector_MarkFetched_FlipsPlaceholder(t *testing.T) {
	t.Parallel()

	// First issue references a not-yet-fetched key.
	a := parse.Issue{
		Key: "PROJ-1", Summary: "a", Status: "Open", IssueType: "Task",
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Relates", Key: "PROJ-2", Summary: "b"},
		},
	}
	c := NewCollector()
	c.Add(a, nil)

	// PROJ-2 currently exists as a placeholder.
	m := c.Model()
	pre := findNode(m, "PROJ-2")
	require.NotNil(t, pre)
	assert.False(t, pre.Fetched)

	// Now fetch PROJ-2 — Add'ing it should flip Fetched and populate fields.
	b := parse.Issue{
		Key: "PROJ-2", Summary: "second issue", Status: "Done", IssueType: "Story",
	}
	c.Add(b, nil)
	m = c.Model()
	post := findNode(m, "PROJ-2")
	require.NotNil(t, post)
	assert.True(t, post.Fetched)
	assert.Equal(t, "Done", post.Status)
	assert.Equal(t, "Story", post.Type)
}

// ---------------------------------------------------------------------------
// RenderJSON envelope + stable bytes.
// ---------------------------------------------------------------------------

func TestRenderJSON_StableAndEnveloped(t *testing.T) {
	t.Parallel()

	iss := parse.Issue{
		Key: "PROJ-1", Summary: "S", Status: "Open", IssueType: "Task",
	}
	c := NewCollector()
	c.Add(iss, nil)
	m := c.Model()

	out1, err := RenderJSON(m)
	require.NoError(t, err)
	out2, err := RenderJSON(m)
	require.NoError(t, err)
	assert.Equal(t, out1, out2, "RenderJSON must be deterministic")

	// Envelope shape.
	var env struct {
		Version int               `json:"version"`
		Nodes   []json.RawMessage `json:"nodes"`
		Edges   []json.RawMessage `json:"edges"`
	}
	require.NoError(t, json.Unmarshal(out1, &env))
	assert.Equal(t, 1, env.Version)
	assert.Len(t, env.Nodes, 1)
	assert.Empty(t, env.Edges)
}

// ---------------------------------------------------------------------------
// RenderD2 produces valid-looking source with header and shape lines.
// ---------------------------------------------------------------------------

func TestRenderD2_OutputShape(t *testing.T) {
	t.Parallel()

	iss := parse.Issue{
		Key: "PROJ-1", Summary: `weird "quoted" summary`, Status: "Open", IssueType: "Task",
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Blocks", Key: "PROJ-2", Summary: "b"},
		},
		DevStatus: parse.DevStatusData{
			PullRequests: []parse.PullRequest{
				{URL: "https://github.com/acme/widget/pull/7"},
			},
		},
	}
	c := NewCollector()
	c.Add(iss, []extract.Reference{extRef("https://external.example.com/x")})
	m := c.Model()

	src, err := RenderD2(m)
	require.NoError(t, err)

	// Header comment.
	assert.True(t, strings.HasPrefix(src, "# gojira issue graph"),
		"D2 source should start with a header comment, got: %.80q", src)
	assert.Contains(t, src, "d2 graph.d2 graph.svg")

	// At least one connection (->) and a node label.
	assert.Contains(t, src, "->")
	assert.Contains(t, src, `"PROJ-1"`, "node keys must be quoted")
	assert.Contains(t, src, `"PROJ-2"`)
	assert.Contains(t, src, `"acme/widget#7"`)
	assert.Contains(t, src, `"https://external.example.com/x"`)

	// Shape lines for non-issue node kinds.
	assert.Contains(t, src, "shape: hexagon", "PR node should be hexagon")
	assert.Contains(t, src, "shape: page", "external node should be page")

	// Embedded double-quote in the label is escaped (backslash-quote).
	assert.Contains(t, src, `\"quoted\"`, "embedded double-quote in label must be escaped")
}

// ---------------------------------------------------------------------------
// helpers: lookups + censuses
// ---------------------------------------------------------------------------

func findNode(m Model, id string) *Node {
	for i := range m.Nodes {
		if m.Nodes[i].ID == id {
			return &m.Nodes[i]
		}
	}
	return nil
}

func findEdge(m Model, from, to string, k EdgeKind) *Edge {
	for i := range m.Edges {
		e := &m.Edges[i]
		if e.From == from && e.To == to && e.Kind == k {
			return e
		}
	}
	return nil
}

func edgesBetween(es []Edge, from, to string) []Edge {
	out := []Edge{}
	for _, e := range es {
		if e.From == from && e.To == to {
			out = append(out, e)
		}
	}
	return out
}

func edgeKindCensus(es []Edge) map[EdgeKind]int {
	out := map[EdgeKind]int{}
	for _, e := range es {
		out[e.Kind]++
	}
	return out
}
