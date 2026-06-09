// integtest/graph_test.go — facade-level E2E coverage for the opt-in
// graph export (gojira.Crawl with EmitGraph enabled).
//
// The test runs gojira.Crawl against an httptest fake Jira serving two
// cross-linked issues (EXAMPLE-1 ↔ EXAMPLE-2). It also wires a GitHub
// PR URL (via ADF description) and an external URL into the issue
// body so the resulting graph.json / graph.d2 cover all four node
// kinds (issue, github_pr, external) and the expected edge mix.
package integtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gojira "github.com/neumachen/gojira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphTestConfig is testConfig + EmitGraph=true.
func graphTestConfig(t *testing.T, siteURL, outputDir string, emitGraph bool) gojira.Config {
	t.Helper()
	kv := map[string]string{
		"GOJIRA_SITE":        siteURL,
		"GOJIRA_USER":        "test@example.com",
		"GOJIRA_TOKEN":       "test-token",
		"GOJIRA_OUTPUT_DIR":  outputDir,
		"GOJIRA_CONCURRENCY": "1",
		"GOJIRA_ISSUE_CAP":   "0",
	}
	if emitGraph {
		kv["GOJIRA_EMIT_GRAPH"] = "true"
	}
	cfg, err := gojira.LoadConfig(kv)
	require.NoError(t, err, "LoadConfig")
	return cfg
}

// issueWithLinkAndPRJSON serves an issue that:
//   - has an outbound issue link to linkedKey,
//   - embeds a GitHub PR URL inside its ADF description, and
//   - embeds an external URL inside the same description.
//
// Both the link and the PR/external refs are exercised by the graph
// collector so a single fixture covers structured + ref-based paths.
func issueWithLinkAndPRJSON(key, linkedKey, site string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": %q,
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": {
      "version": 1, "type": "doc",
      "content": [
        {"type": "paragraph", "content": [
          {"type": "text", "text": "See ",
           "marks": [{"type": "link", "attrs": {"href": "https://github.com/acme/widget/pull/42"}}]},
          {"type": "text", "text": " and "},
          {"type": "text", "text": "external",
           "marks": [{"type": "link", "attrs": {"href": "https://docs.example.com/foo"}}]}
        ]}
      ]
    },
    "parent": null,
    "subtasks": [],
    "issuelinks": [
      {
        "type": {"name": "Relates", "inward": "relates to", "outward": "relates to"},
        "outwardIssue": {"key": %q, "fields": {"summary": "Summary of linked"}}
      }
    ],
    "remotelinks": []
  }
}`, key, site+"/rest/api/3/issue/"+key, key, linkedKey))
}

func startGraphServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}
		switch key {
		case "EXAMPLE-1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(issueWithLinkAndPRJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL))
		case "EXAMPLE-2":
			// Minimal issue, no extra refs — keeps the second fetch
			// quick and proves issue↔issue dedup at the graph layer.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(minimalIssueJSON("EXAMPLE-2", srv.URL))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// EmitGraph=true → both files written, with expected node/edge content
// ---------------------------------------------------------------------------

func TestGraphExport_Enabled_WritesBothFiles(t *testing.T) {
	outputDir := t.TempDir()
	srv := startGraphServer(t)
	cfg := graphTestConfig(t, srv.URL, outputDir, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")
	require.Equal(t, 2, sum.Fetched, "both issues should be fetched")

	jsonPath := filepath.Join(outputDir, "graph.json")
	d2Path := filepath.Join(outputDir, "graph.d2")

	jsonBytes, err := os.ReadFile(jsonPath)
	require.NoError(t, err, "graph.json must exist")

	d2Bytes, err := os.ReadFile(d2Path)
	require.NoError(t, err, "graph.d2 must exist")
	assert.NotEmpty(t, d2Bytes, "graph.d2 must be non-empty")

	// --- graph.json shape ---
	var env struct {
		Version int `json:"version"`
		Nodes   []struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Fetched bool   `json:"fetched"`
		} `json:"nodes"`
		Edges []struct {
			From string `json:"from"`
			To   string `json:"to"`
			Kind string `json:"kind"`
		} `json:"edges"`
	}
	require.NoError(t, json.Unmarshal(jsonBytes, &env))
	assert.Equal(t, 1, env.Version, "envelope version")

	// Node IDs and kinds.
	byID := map[string]string{}
	fetchedByID := map[string]bool{}
	for _, n := range env.Nodes {
		byID[n.ID] = n.Kind
		fetchedByID[n.ID] = n.Fetched
	}
	assert.Equal(t, "issue", byID["EXAMPLE-1"], "EXAMPLE-1 is an issue node")
	assert.Equal(t, "issue", byID["EXAMPLE-2"], "EXAMPLE-2 is an issue node")
	assert.Equal(t, "github_pr", byID["acme/widget#42"], "PR node present")
	assert.Equal(t, "external", byID["https://docs.example.com/foo"], "external node present")
	assert.True(t, fetchedByID["EXAMPLE-1"], "EXAMPLE-1 was fetched")
	assert.True(t, fetchedByID["EXAMPLE-2"], "EXAMPLE-2 was fetched")

	// Expected edges.
	type edgeKey struct{ from, to, kind string }
	edges := map[edgeKey]bool{}
	for _, e := range env.Edges {
		edges[edgeKey{e.From, e.To, e.Kind}] = true
	}
	assert.True(t, edges[edgeKey{"EXAMPLE-1", "EXAMPLE-2", "link"}],
		"issue-link edge EXAMPLE-1 -> EXAMPLE-2 must exist")
	assert.True(t, edges[edgeKey{"EXAMPLE-1", "acme/widget#42", "pull_request"}],
		"PR edge must exist")
	assert.True(t, edges[edgeKey{"EXAMPLE-1", "https://docs.example.com/foo", "external"}],
		"external edge must exist")

	// --- graph.d2 shape ---
	d2 := string(d2Bytes)
	assert.True(t, strings.HasPrefix(d2, "# gojira issue graph"),
		"d2 source must start with the header comment, got: %.80q", d2)
	assert.Contains(t, d2, "d2 graph.d2 graph.svg", "d2 source must document the render command")
	assert.Contains(t, d2, "->", "d2 source must contain at least one connection")
	assert.Contains(t, d2, `"EXAMPLE-1"`)
	assert.Contains(t, d2, `"EXAMPLE-2"`)
	assert.Contains(t, d2, `"acme/widget#42"`)
	assert.Contains(t, d2, "shape: hexagon", "PR node should be a hexagon")
	assert.Contains(t, d2, "shape: page", "external node should be a page")
}

// ---------------------------------------------------------------------------
// EmitGraph=false (default) → no graph files written
// ---------------------------------------------------------------------------

func TestGraphExport_Disabled_WritesNoFiles(t *testing.T) {
	outputDir := t.TempDir()
	srv := startGraphServer(t)
	cfg := graphTestConfig(t, srv.URL, outputDir, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	_, err = os.Stat(filepath.Join(outputDir, "graph.json"))
	assert.True(t, os.IsNotExist(err), "graph.json must NOT exist when EmitGraph=false, got: %v", err)

	_, err = os.Stat(filepath.Join(outputDir, "graph.d2"))
	assert.True(t, os.IsNotExist(err), "graph.d2 must NOT exist when EmitGraph=false, got: %v", err)
}
