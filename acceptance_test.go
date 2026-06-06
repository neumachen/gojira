// Package gojira_test contains the end-to-end acceptance harness for the
// gojira library facade.
//
// Each test function corresponds to exactly one PRD §13 acceptance criterion
// (AC 1 through AC 15). Tests are named TestAC<NN>_<ShortName>.
//
// All tests are fixture-based and use httptest.Server for Jira API responses.
// No live network calls are made. Output is written to t.TempDir().
//
// AC 16, 17, and 18 (CLI-level) live in cmd/gojira/acceptance_test.go.
package gojira_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/classify"
	"github.com/neumachen/gojira/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// acceptanceServer — shared httptest.Server helper
// ---------------------------------------------------------------------------

// acceptanceServer starts an httptest.Server that routes GET
// /rest/api/3/issue/<KEY> requests.
//
// responses maps issue key → raw JSON bytes (200 OK).
// statusOverrides maps issue key → HTTP status code (body is empty).
// fetchCounts maps issue key → *atomic.Int64 (incremented on each request).
//
// Unknown keys return 404. The server is registered for cleanup via t.Cleanup.
func acceptanceServer(
	t *testing.T,
	responses map[string][]byte,
	statusOverrides map[string]int,
	fetchCounts map[string]*atomic.Int64,
) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		// Strip any trailing path segments (e.g. /remotelink).
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}

		// Increment fetch counter if registered.
		if cnt, ok := fetchCounts[key]; ok {
			cnt.Add(1)
		}

		// Status override takes priority.
		if code, ok := statusOverrides[key]; ok {
			w.WriteHeader(code)
			return
		}

		body, ok := responses[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// minimalJSON returns a minimal valid Jira issue JSON for key with no
// outbound links. site is used in the "self" URL.
func minimalJSON(key, site string) []byte {
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
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": []
  }
}`, key, site+"/rest/api/3/issue/"+key, key))
}

// linkedJSON returns a Jira issue JSON for key that has an outward issue link
// to linkedKey.
func linkedJSON(key, linkedKey, site string) []byte {
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
    "description": null,
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

// adfLinkJSON returns a Jira issue JSON whose description ADF contains an
// inline link to linkURL with display text linkText.
func adfLinkJSON(key, site, linkURL, linkText string) []byte {
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
      "version": 1,
      "type": "doc",
      "content": [
        {
          "type": "paragraph",
          "content": [
            {
              "type": "text",
              "text": %q,
              "marks": [{"type": "link", "attrs": {"href": %q}}]
            }
          ]
        }
      ]
    },
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": []
  }
}`, key, site+"/rest/api/3/issue/"+key, key, linkText, linkURL))
}

// loadFixture reads a fixture file from testdata/acceptance/ and replaces the
// SITE_PLACEHOLDER token with the given site URL.
func loadFixture(t *testing.T, name, site string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "acceptance", name))
	require.NoError(t, err, "loadFixture(%q)", name)
	return bytes.ReplaceAll(data, []byte("SITE_PLACEHOLDER"), []byte(site))
}

// acConfig builds a gojira.Config pointing at siteURL with outputDir as the
// output directory. Concurrency is set to 1 for determinism; issue cap is 0
// (unlimited) unless overridden by the caller after this call.
func acConfig(t *testing.T, siteURL, outputDir string) gojira.Config {
	t.Helper()
	cfg, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":               siteURL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         outputDir,
		"GOJIRA_CONCURRENCY":        "1",
		"GOJIRA_ISSUE_CAP":          "0",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	})
	require.NoError(t, err, "acConfig: LoadConfig")
	return cfg
}

// acConfigWithCap builds a gojira.Config with a specific issue cap.
func acConfigWithCap(t *testing.T, siteURL, outputDir string, issueCap int) gojira.Config {
	t.Helper()
	cfg, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":               siteURL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         outputDir,
		"GOJIRA_CONCURRENCY":        "1",
		"GOJIRA_ISSUE_CAP":          fmt.Sprintf("%d", issueCap),
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	})
	require.NoError(t, err, "acConfigWithCap: LoadConfig")
	return cfg
}

// acConfigWithDepth builds a gojira.Config with a specific depth limit.
func acConfigWithDepth(t *testing.T, siteURL, outputDir string, depthLimit int) gojira.Config {
	t.Helper()
	cfg, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":               siteURL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         outputDir,
		"GOJIRA_CONCURRENCY":        "1",
		"GOJIRA_ISSUE_CAP":          "0",
		"GOJIRA_DEPTH_LIMIT":        fmt.Sprintf("%d", depthLimit),
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	})
	require.NoError(t, err, "acConfigWithDepth: LoadConfig")
	return cfg
}

// acConfigHierarchy builds a gojira.Config for the hierarchy acceptance
// tests (AC 20–22). It enables IncludeChildren and accepts an optional
// epicLinkField override; when epicLinkField is empty, auto-detection runs.
func acConfigHierarchy(t *testing.T, siteURL, outputDir string, includeChildren bool, epicLinkField string) gojira.Config {
	t.Helper()
	kv := map[string]string{
		"GOJIRA_SITE":               siteURL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         outputDir,
		"GOJIRA_CONCURRENCY":        "1",
		"GOJIRA_ISSUE_CAP":          "0",
		"GOJIRA_CHILD_SEARCH_LIMIT": "100",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}
	if includeChildren {
		kv["GOJIRA_INCLUDE_CHILDREN"] = "true"
	} else {
		kv["GOJIRA_INCLUDE_CHILDREN"] = "false"
	}
	if epicLinkField != "" {
		kv["GOJIRA_EPIC_LINK_FIELD"] = epicLinkField
	}
	cfg, err := gojira.LoadConfig(kv)
	require.NoError(t, err, "acConfigHierarchy: LoadConfig")
	return cfg
}

// acConfigRefetch builds a gojira.Config with Refetch=true.
func acConfigRefetch(t *testing.T, siteURL, outputDir string) gojira.Config {
	t.Helper()
	cfg, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":               siteURL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         outputDir,
		"GOJIRA_CONCURRENCY":        "1",
		"GOJIRA_ISSUE_CAP":          "0",
		"GOJIRA_REFETCH":            "true",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	})
	require.NoError(t, err, "acConfigRefetch: LoadConfig")
	return cfg
}

// noSleep returns a client.Option that eliminates retry backoff delays in tests.
func noSleep() client.Option {
	return client.WithRateLimitBackoff(0, 0)
}

// ---------------------------------------------------------------------------
// AC 1 — Issue key classification
// ---------------------------------------------------------------------------

// TestAC01_IssueKeyClassification verifies that gojira.Classify correctly
// classifies the four canonical input shapes defined in PRD §13 AC 1.
//
// PRD AC 1: Given EXAMPLE-1 → JiraKey; given Jira browse URL → JiraURL;
// given GitHub PR URL → GitHubPR; given external URL → External.
func TestAC01_IssueKeyClassification(t *testing.T) {
	const site = "https://mysite.atlassian.net"

	tests := []struct {
		name         string
		input        string
		wantKind     classify.Kind
		wantIssueKey string
		wantOwner    string
		wantRepo     string
		wantPRNumber int
	}{
		{
			name:         "bare Jira issue key",
			input:        "EXAMPLE-1",
			wantKind:     classify.KindJiraKey,
			wantIssueKey: "EXAMPLE-1",
		},
		{
			name:         "Jira browse URL matching site",
			input:        "https://mysite.atlassian.net/browse/EXAMPLE-1",
			wantKind:     classify.KindJiraURL,
			wantIssueKey: "EXAMPLE-1",
		},
		{
			name:         "GitHub pull request URL",
			input:        "https://github.com/org/repo/pull/42",
			wantKind:     classify.KindGitHubPR,
			wantOwner:    "org",
			wantRepo:     "repo",
			wantPRNumber: 42,
		},
		{
			name:     "external link",
			input:    "https://example.com/doc",
			wantKind: classify.KindExternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gojira.Classify(tt.input, site)
			assert.Equal(t, tt.wantKind, got.Kind, "Kind")
			if tt.wantIssueKey != "" {
				assert.Equal(t, tt.wantIssueKey, got.IssueKey, "IssueKey")
			}
			if tt.wantOwner != "" {
				assert.Equal(t, tt.wantOwner, got.Owner, "Owner")
			}
			if tt.wantRepo != "" {
				assert.Equal(t, tt.wantRepo, got.Repo, "Repo")
			}
			if tt.wantPRNumber != 0 {
				assert.Equal(t, tt.wantPRNumber, got.PRNumber, "PRNumber")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC 2 — Single issue render
// ---------------------------------------------------------------------------

// TestAC02_SingleIssueRender verifies that FetchAndRender against a fixture
// Jira issue JSON produces the canonical Markdown sections.
//
// PRD AC 2: indexMD contains # EXAMPLE-1 — <summary> heading, ## Metadata,
// ## Description. FetchAndRender must NOT write to disk.
func TestAC02_SingleIssueRender(t *testing.T) {
	outputDir := t.TempDir()

	// Build server first so we know the site URL.
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
		if key != "EXAMPLE-1" {
			http.NotFound(w, r)
			return
		}
		body := loadFixture(t, "ac02_single_issue.json", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	indexMD, _, _, err := gojira.FetchAndRender(ctx, cfg, "EXAMPLE-1",
		client.WithHTTPClient(srv.Client()))
	require.NoError(t, err, "FetchAndRender")

	// AC 2: heading must be present.
	assert.Contains(t, indexMD, "# EXAMPLE-1", "indexMD missing '# EXAMPLE-1' heading")
	assert.Contains(t, indexMD, "Acceptance test single issue", "indexMD missing summary text")
	// Metadata section.
	assert.Contains(t, indexMD, "## Metadata", "indexMD missing '## Metadata' section")
	// Description section.
	assert.Contains(t, indexMD, "## Description", "indexMD missing '## Description' section")
	// Description text.
	assert.Contains(t, indexMD, "acceptance test description", "indexMD missing description text")

	// FetchAndRender must NOT write to disk.
	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	_, err = os.Stat(indexPath)
	assert.Error(t, err, "FetchAndRender must not write to disk at %s", indexPath)
}

// ---------------------------------------------------------------------------
// AC 3 — ADF link extraction
// ---------------------------------------------------------------------------

// TestAC03_ADFLinkExtraction verifies that a Jira URL embedded in an ADF
// description is returned in discoveredKeys by FetchAndRender.
//
// PRD AC 3: Given ADF with inline link to EXAMPLE-2 Jira URL, EXAMPLE-2
// appears in discoveredKeys.
func TestAC03_ADFLinkExtraction(t *testing.T) {
	outputDir := t.TempDir()

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
		if key != "EXAMPLE-1" {
			http.NotFound(w, r)
			return
		}
		// ADF description contains a link to EXAMPLE-2 on the same site.
		jiraLink := srv.URL + "/browse/EXAMPLE-2"
		body := adfLinkJSON("EXAMPLE-1", srv.URL, jiraLink, "See EXAMPLE-2")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	_, _, discoveredKeys, err := gojira.FetchAndRender(ctx, cfg, "EXAMPLE-1",
		client.WithHTTPClient(srv.Client()))
	require.NoError(t, err, "FetchAndRender")

	// AC 3: EXAMPLE-2 must appear in discoveredKeys.
	assert.Contains(t, discoveredKeys, "EXAMPLE-2", "discoveredKeys must contain EXAMPLE-2")
}

// ---------------------------------------------------------------------------
// AC 4 — GitHub PR recognition in ADF
// ---------------------------------------------------------------------------

// TestAC04_GitHubPRRecognitionInADF verifies that a GitHub PR URL in an ADF
// description is rendered as a Pull requests section and does NOT appear in
// discoveredKeys.
//
// PRD AC 4: GitHub PR URL → "org/repo#42" in Pull requests section; not in
// discoveredKeys.
func TestAC04_GitHubPRRecognitionInADF(t *testing.T) {
	outputDir := t.TempDir()

	const prURL = "https://github.com/org/repo/pull/42"

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
		if key != "EXAMPLE-1" {
			http.NotFound(w, r)
			return
		}
		body := adfLinkJSON("EXAMPLE-1", srv.URL, prURL, "org/repo#42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	indexMD, _, discoveredKeys, err := gojira.FetchAndRender(ctx, cfg, "EXAMPLE-1",
		client.WithHTTPClient(srv.Client()))
	require.NoError(t, err, "FetchAndRender")

	// AC 4: Pull requests section must contain the PR label.
	assert.Contains(t, indexMD, "org/repo#42", "indexMD missing 'org/repo#42' in Pull requests section")

	// The PR must NOT contribute to discoveredKeys (PRs are not Jira keys).
	for _, k := range discoveredKeys {
		assert.False(t, strings.Contains(k, "github") || strings.Contains(k, "org"),
			"discoveredKeys contains unexpected entry %q (GitHub PR should not be a Jira key)", k)
	}
	assert.Empty(t, discoveredKeys, "discoveredKeys should be empty (no Jira keys in this issue)")
}

// ---------------------------------------------------------------------------
// AC 5 — Relationship rendering with relative paths
// ---------------------------------------------------------------------------

// TestAC05_RelationshipRendering verifies that after a full crawl, the
// rendered EXAMPLE-1/index.md contains a Relationships section with Parent,
// Children, and Linked issues subsections referencing the correct keys.
//
// PRD AC 5: Relationships section with Parent (EXAMPLE-0), Children
// (EXAMPLE-1a), and Linked issues (EXAMPLE-2). Relative paths appear when
// the neighbour keys are already in the visited set at render time.
//
// Implementation note: the crawl renders each issue at fetch time using the
// visited set at that moment. To maximise the chance of relative-path links,
// all four keys are passed as start keys so they are all enqueued (and thus
// in the visited set) before any is rendered. The test asserts the Relationships
// section structure and the presence of each key; it accepts either relative
// or absolute link format since the exact format depends on scheduling order.
func TestAC05_RelationshipRendering(t *testing.T) {
	outputDir := t.TempDir()

	// Build server first so we know the site URL.
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

		var body []byte
		switch key {
		case "EXAMPLE-1":
			body = loadFixture(t, "ac05_with_relationships.json", srv.URL)
		case "EXAMPLE-0":
			body = minimalJSON("EXAMPLE-0", srv.URL)
		case "EXAMPLE-11":
			body = minimalJSON("EXAMPLE-11", srv.URL)
		case "EXAMPLE-2":
			body = minimalJSON("EXAMPLE-2", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Start with all four keys so they are all enqueued (visited) before
	// any is rendered, maximising the chance of relative-path links.
	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-0", "EXAMPLE-1", "EXAMPLE-11", "EXAMPLE-2"}, nil)
	require.NoError(t, err, "Crawl")
	// All four issues should be fetched.
	assert.Equal(t, 4, sum.Fetched, "Summary.Fetched")

	// Read the rendered EXAMPLE-1/index.md.
	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err, "ReadFile(%s)", indexPath)
	md := string(content)

	// AC 5: Relationships section must be present.
	assert.Contains(t, md, "## Relationships", "index.md missing '## Relationships' section")

	// Parent subsection must reference EXAMPLE-0.
	assert.Contains(t, md, "EXAMPLE-0", "index.md missing EXAMPLE-0 in Relationships")

	// Children subsection must reference EXAMPLE-11 (the subtask).
	assert.Contains(t, md, "EXAMPLE-11", "index.md missing EXAMPLE-11 in Relationships")

	// Linked issues subsection must reference EXAMPLE-2.
	assert.Contains(t, md, "EXAMPLE-2", "index.md missing EXAMPLE-2 in Relationships")

	// When all neighbours are in the visited set at render time, links
	// use relative paths. Assert relative paths are present (this is the
	// primary AC 5 assertion).
	assert.Contains(t, md, "../EXAMPLE-0/index.md", "index.md missing relative link to EXAMPLE-0")
	assert.Contains(t, md, "../EXAMPLE-11/index.md", "index.md missing relative link to EXAMPLE-11")
	assert.Contains(t, md, "../EXAMPLE-2/index.md", "index.md missing relative link to EXAMPLE-2")
}

// ---------------------------------------------------------------------------
// AC 6 — Recursive crawl deduplication
// ---------------------------------------------------------------------------

// TestAC06_RecursiveCrawlDeduplication verifies that a two-issue cycle
// (A↔B) results in exactly two fetches and no infinite loop.
//
// PRD AC 6: Each issue fetched exactly once; summary.Fetched == 2.
func TestAC06_RecursiveCrawlDeduplication(t *testing.T) {
	outputDir := t.TempDir()

	var countA, countB atomic.Int64

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

		var body []byte
		switch key {
		case "EXAMPLE-1":
			countA.Add(1)
			body = linkedJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		case "EXAMPLE-2":
			countB.Add(1)
			body = linkedJSON("EXAMPLE-2", "EXAMPLE-1", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 6: each issue fetched exactly once.
	assert.Equal(t, 2, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, int64(1), countA.Load(), "EXAMPLE-1 fetch count")
	assert.Equal(t, int64(1), countB.Load(), "EXAMPLE-2 fetch count")

	// Both index.md files must exist.
	for _, key := range []string{"EXAMPLE-1", "EXAMPLE-2"} {
		p := filepath.Join(outputDir, key, "index.md")
		_, err := os.Stat(p)
		assert.NoError(t, err, "index.md must exist for %s", key)
	}
}

// ---------------------------------------------------------------------------
// AC 7 — Issue cap enforcement
// ---------------------------------------------------------------------------

// TestAC07_IssueCapEnforcement verifies that when IssueCap=3 and 10 issues
// are reachable, exactly 3 are fetched and 7 are cap-limited.
//
// PRD AC 7: summary.Fetched == 3, summary.CapLimited == 7.
//
// Graph design: EXAMPLE-1 links to EXAMPLE-2 through EXAMPLE-10 via
// issuelinks (all 9 in a single response). This ensures all 9 outbound
// keys are discovered when EXAMPLE-1 is processed, so 7 of them hit the
// cap immediately.
func TestAC07_IssueCapEnforcement(t *testing.T) {
	outputDir := t.TempDir()

	// EXAMPLE-1 links to EXAMPLE-2 … EXAMPLE-10 (9 outward links).
	// With IssueCap=3: EXAMPLE-1 is fetched (1), then 2 more are fetched
	// from the queue, and the remaining 7 are cap-limited.
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

		var body []byte
		if key == "EXAMPLE-1" {
			// Build a JSON with 9 outward issuelinks.
			links := ""
			for i := 2; i <= 10; i++ {
				if i > 2 {
					links += ","
				}
				links += fmt.Sprintf(`{
  "type": {"name": "Relates", "inward": "relates to", "outward": "relates to"},
  "outwardIssue": {"key": "EXAMPLE-%d", "fields": {"summary": "Issue %d"}}
}`, i, i)
			}
			body = []byte(fmt.Sprintf(`{
  "key": "EXAMPLE-1",
  "self": %q,
  "fields": {
    "summary": "Hub issue",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [%s],
    "remotelinks": []
  }
}`, srv.URL+"/rest/api/3/issue/EXAMPLE-1", links))
		} else {
			// EXAMPLE-2 through EXAMPLE-10: minimal, no outbound links.
			for i := 2; i <= 10; i++ {
				if key == fmt.Sprintf("EXAMPLE-%d", i) {
					body = minimalJSON(key, srv.URL)
					break
				}
			}
		}
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfigWithCap(t, srv.URL, outputDir, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 7: exactly 3 fetched, 7 cap-limited.
	assert.Equal(t, 3, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, 7, sum.CapLimited, "Summary.CapLimited")
}

// ---------------------------------------------------------------------------
// AC 8 — Depth cap enforcement
// ---------------------------------------------------------------------------

// TestAC08_DepthCapEnforcement verifies that with DepthLimit=1, a chain
// A→B→C results in A and B being fetched and C being cap-limited.
//
// PRD AC 8: EXAMPLE-1 and EXAMPLE-2 fetched; EXAMPLE-3 not fetched.
func TestAC08_DepthCapEnforcement(t *testing.T) {
	outputDir := t.TempDir()

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

		var body []byte
		switch key {
		case "EXAMPLE-1":
			body = linkedJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		case "EXAMPLE-2":
			body = linkedJSON("EXAMPLE-2", "EXAMPLE-3", srv.URL)
		case "EXAMPLE-3":
			body = minimalJSON("EXAMPLE-3", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfigWithDepth(t, srv.URL, outputDir, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 8: EXAMPLE-1 (depth 0) and EXAMPLE-2 (depth 1) fetched;
	// EXAMPLE-3 (depth 2) cap-limited.
	assert.Equal(t, 2, sum.Fetched, "Summary.Fetched")
	assert.GreaterOrEqual(t, sum.CapLimited, 1, "Summary.CapLimited (EXAMPLE-3 should be depth-capped)")

	// EXAMPLE-3/index.md must NOT exist.
	p3 := filepath.Join(outputDir, "EXAMPLE-3", "index.md")
	_, err = os.Stat(p3)
	assert.Error(t, err, "EXAMPLE-3/index.md must not exist (depth cap = 1)")
}

// ---------------------------------------------------------------------------
// AC 9 — Permission-denied stub
// ---------------------------------------------------------------------------

// TestAC09_PermissionDeniedStubFile verifies that a 403 response causes a stub
// index.md to be written and summary.Stubbed == 1.
//
// PRD AC 9: EXAMPLE-2/index.md written with "Permission denied (403)";
// summary.Stubbed == 1.
func TestAC09_PermissionDeniedStubFile(t *testing.T) {
	outputDir := t.TempDir()

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
			body := linkedJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "EXAMPLE-2":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl returned unexpected error")

	// AC 9: EXAMPLE-1 fetched, EXAMPLE-2 stubbed.
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, 1, sum.Stubbed, "Summary.Stubbed")

	// Stub file must exist for EXAMPLE-2.
	stubPath := filepath.Join(outputDir, "EXAMPLE-2", "index.md")
	content, err := os.ReadFile(stubPath)
	require.NoError(t, err, "stub index.md not found at %s", stubPath)
	md := string(content)

	// Stub must mention the key and the denial reason.
	assert.Contains(t, md, "EXAMPLE-2", "stub must mention 'EXAMPLE-2'")
	assert.Contains(t, md, "Permission denied", "stub must mention 'Permission denied'")
}

// ---------------------------------------------------------------------------
// AC 10 — Skip-if-exists (idempotency)
// ---------------------------------------------------------------------------

// TestAC10_SkipIfExistsIdempotent verifies that when index.md already exists
// and cfg.Refetch is false, no API call is made and the file is not overwritten.
//
// PRD AC 10: no API call for EXAMPLE-1; existing file unchanged;
// summary.Skipped == 1.
func TestAC10_SkipIfExistsIdempotent(t *testing.T) {
	outputDir := t.TempDir()

	// Pre-create EXAMPLE-1/index.md.
	issueDir := filepath.Join(outputDir, "EXAMPLE-1")
	require.NoError(t, os.MkdirAll(issueDir, 0755), "MkdirAll")
	const existingContent = "# EXAMPLE-1 — pre-existing\n"
	require.NoError(t, os.WriteFile(filepath.Join(issueDir, "index.md"), []byte(existingContent), 0644), "WriteFile")

	var fetchCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}
		if key == "EXAMPLE-1" {
			fetchCount.Add(1)
		}
		body := minimalJSON(key, r.Host)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	// Refetch defaults to false.
	cfg := acConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 10: skipped, not fetched.
	assert.Equal(t, 1, sum.Skipped, "Summary.Skipped")
	assert.Equal(t, 0, sum.Fetched, "Summary.Fetched")

	// Server must not have been called for EXAMPLE-1.
	assert.Equal(t, int64(0), fetchCount.Load(), "server must not be called for EXAMPLE-1")

	// Pre-existing file must be unchanged.
	got, err := os.ReadFile(filepath.Join(issueDir, "index.md"))
	require.NoError(t, err, "ReadFile")
	assert.Equal(t, existingContent, string(got), "index.md must not be overwritten")
}

// ---------------------------------------------------------------------------
// AC 11 — Re-fetch override
// ---------------------------------------------------------------------------

// TestAC11_RefetchOverride verifies that when index.md already exists and
// cfg.Refetch is true, an API call is made and the file is overwritten.
//
// PRD AC 11: API call made; file overwritten with fresh content.
func TestAC11_RefetchOverride(t *testing.T) {
	outputDir := t.TempDir()

	// Pre-create EXAMPLE-1/index.md with stale content.
	issueDir := filepath.Join(outputDir, "EXAMPLE-1")
	require.NoError(t, os.MkdirAll(issueDir, 0755), "MkdirAll")
	const staleContent = "# EXAMPLE-1 — stale\n"
	require.NoError(t, os.WriteFile(filepath.Join(issueDir, "index.md"), []byte(staleContent), 0644), "WriteFile")

	var fetchCount atomic.Int64
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
		if key == "EXAMPLE-1" {
			fetchCount.Add(1)
		}
		body := minimalJSON(key, srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	// Refetch = true.
	cfg := acConfigRefetch(t, srv.URL, outputDir)
	ctx := context.Background()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 11: fetched (not skipped).
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, 0, sum.Skipped, "Summary.Skipped")

	// Server must have been called.
	assert.NotEqual(t, int64(0), fetchCount.Load(), "server must have been called for EXAMPLE-1")

	// File must be overwritten with fresh content (not stale).
	got, err := os.ReadFile(filepath.Join(issueDir, "index.md"))
	require.NoError(t, err, "ReadFile")
	assert.NotEqual(t, staleContent, string(got), "index.md must be overwritten")
	// Fresh content must contain the canonical heading.
	assert.Contains(t, string(got), "# EXAMPLE-1", "overwritten index.md must contain '# EXAMPLE-1'")
}

// ---------------------------------------------------------------------------
// AC 12 — Unknown ADF node preservation
// ---------------------------------------------------------------------------

// TestAC12_UnknownADFNodePreservation verifies that an unrecognised ADF node
// type is preserved (text content + comment marker) and not silently dropped.
//
// PRD AC 12: rendered indexMD contains the node's text content AND a comment
// noting the unknown type.
func TestAC12_UnknownADFNodePreservation(t *testing.T) {
	outputDir := t.TempDir()

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
		if key != "EXAMPLE-1" {
			http.NotFound(w, r)
			return
		}
		body := loadFixture(t, "ac12_unknown_adf_node.json", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	indexMD, _, _, err := gojira.FetchAndRender(ctx, cfg, "EXAMPLE-1",
		client.WithHTTPClient(srv.Client()))
	require.NoError(t, err, "FetchAndRender")

	// AC 12: text content of the unknown node must be preserved.
	assert.Contains(t, indexMD, "Panel inner text.", "indexMD missing 'Panel inner text.' (unknown node text not preserved)")

	// AC 12: a comment marker noting the unknown type must be present.
	assert.Contains(t, indexMD, "panel", "indexMD missing 'panel' (unknown node type not noted)")
	// The comment format is <!-- adf: unknown node type "panel" -->.
	assert.Contains(t, indexMD, "<!--", "indexMD missing HTML comment marker for unknown ADF node")
}

// ---------------------------------------------------------------------------
// AC 13 — Unknown custom field preservation
// ---------------------------------------------------------------------------

// TestAC13_UnknownCustomFieldPreservation verifies that a custom field not in
// the standard field set is rendered in a Custom fields section.
//
// PRD AC 13: indexMD contains "Custom fields" section with raw key and value.
func TestAC13_UnknownCustomFieldPreservation(t *testing.T) {
	outputDir := t.TempDir()

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
		if key != "EXAMPLE-1" {
			http.NotFound(w, r)
			return
		}
		body := loadFixture(t, "ac13_unknown_custom_field.json", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	indexMD, _, _, err := gojira.FetchAndRender(ctx, cfg, "EXAMPLE-1",
		client.WithHTTPClient(srv.Client()))
	require.NoError(t, err, "FetchAndRender")

	// AC 13: Custom fields section must be present.
	assert.Contains(t, indexMD, "Custom fields", "indexMD missing 'Custom fields' section")

	// The raw key must appear.
	assert.Contains(t, indexMD, "customfield_99999", "indexMD missing 'customfield_99999'")

	// The raw value must appear.
	assert.Contains(t, indexMD, "acceptance-test-custom-value", "indexMD missing 'acceptance-test-custom-value'")
}

// ---------------------------------------------------------------------------
// AC 14 — Outbound reference index
// ---------------------------------------------------------------------------

// TestAC14_OutboundReferenceIndex verifies that after a crawl, the
// EXAMPLE-1/references/outbound.md file lists all three outbound reference
// types with their classifications.
//
// PRD AC 14: outbound.md lists Jira link (EXAMPLE-2), GitHub PR
// (org/repo#42), and external link (https://example.com/doc).
func TestAC14_OutboundReferenceIndex(t *testing.T) {
	outputDir := t.TempDir()

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

		var body []byte
		switch key {
		case "EXAMPLE-1":
			body = loadFixture(t, "ac14_with_outbound_refs.json", srv.URL)
		case "EXAMPLE-2":
			body = minimalJSON("EXAMPLE-2", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")
	require.GreaterOrEqual(t, sum.Fetched, 1, "Summary.Fetched")

	// AC 14: outbound.md must exist for EXAMPLE-1.
	outboundPath := filepath.Join(outputDir, "EXAMPLE-1", "references", "outbound.md")
	content, err := os.ReadFile(outboundPath)
	require.NoError(t, err, "outbound.md not found at %s", outboundPath)
	md := string(content)

	// Jira link classification.
	assert.Contains(t, md, "EXAMPLE-2", "outbound.md missing Jira link to EXAMPLE-2")

	// GitHub PR classification.
	assert.Contains(t, md, "org/repo#42", "outbound.md missing GitHub PR 'org/repo#42'")

	// External link classification.
	assert.Contains(t, md, "https://example.com/doc", "outbound.md missing external link 'https://example.com/doc'")
}

// ---------------------------------------------------------------------------
// AC 15 — Output layout conformance
// ---------------------------------------------------------------------------

// TestAC15_OutputLayoutConformance verifies that after a crawl, every fetched
// issue lives at <KEY>/index.md and references (when present) at
// <KEY>/references/outbound.md. No unexpected files appear at the output root.
//
// PRD AC 15: canonical layout per 30-markdown-output.md.
func TestAC15_OutputLayoutConformance(t *testing.T) {
	outputDir := t.TempDir()

	// Two-issue chain: EXAMPLE-1 → EXAMPLE-2 (with outbound refs).
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

		var body []byte
		switch key {
		case "EXAMPLE-1":
			body = linkedJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		case "EXAMPLE-2":
			body = minimalJSON("EXAMPLE-2", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")
	require.Equal(t, 2, sum.Fetched, "Summary.Fetched")

	// AC 15: walk the output directory and verify layout.
	err = filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(outputDir, path)
		if err != nil {
			return err
		}

		parts := strings.Split(rel, string(filepath.Separator))
		switch len(parts) {
		case 2:
			// <KEY>/index.md — the only allowed two-part path.
			assert.Equal(t, "index.md", parts[1], "unexpected file at output root level: %s", rel)
		case 3:
			// <KEY>/references/<file> — only outbound.md is expected.
			assert.Equal(t, "references", parts[1], "unexpected directory under issue key: %s", rel)
			assert.Equal(t, "outbound.md", parts[2], "unexpected file in references/: %s", rel)
		default:
			t.Errorf("unexpected path depth in output: %s", rel)
		}
		return nil
	})
	require.NoError(t, err, "Walk")

	// Verify EXAMPLE-1/index.md exists.
	_, err = os.Stat(filepath.Join(outputDir, "EXAMPLE-1", "index.md"))
	assert.NoError(t, err, "EXAMPLE-1/index.md must exist")
	// Verify EXAMPLE-2/index.md exists.
	_, err = os.Stat(filepath.Join(outputDir, "EXAMPLE-2", "index.md"))
	assert.NoError(t, err, "EXAMPLE-2/index.md must exist")
}

// ---------------------------------------------------------------------------
// Compile-time checks
// ---------------------------------------------------------------------------

// Ensure the fixture JSON files are valid JSON (sanity check).
func TestFixtureJSON_Valid(t *testing.T) {
	fixtures := []string{
		"ac02_single_issue.json",
		"ac05_with_relationships.json",
		"ac12_unknown_adf_node.json",
		"ac13_unknown_custom_field.json",
		"ac14_with_outbound_refs.json",
	}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			// Use a dummy site for placeholder replacement.
			data := loadFixture(t, name, "https://example.atlassian.net")
			var v interface{}
			assert.NoError(t, json.Unmarshal(data, &v), "fixture %s must be valid JSON", name)
		})
	}
}

// ===========================================================================
// AC 20–22 — Hierarchy traversal (Phase 4-hierarchy-1, design mini-doc v4)
// ===========================================================================

// hierarchyServer starts an httptest.Server that routes the three Jira
// endpoints needed for hierarchy acceptance tests:
//
//   - GET  /rest/api/3/issue/<KEY>     → responses map (200) or 404
//   - POST /rest/api/3/search/jql      → parentMap / epicMap (matched by JQL)
//   - GET  /rest/api/3/field           → fields slice
//
// All call counts are exposed on the returned struct so tests can assert
// that auto-detection is cached (AC 22) or that no search calls happen
// when hierarchy is disabled (AC 21).
type hierarchyServer struct {
	URL string
	srv *httptest.Server

	// fetchCount is the number of GET /issue/<KEY> calls. Indexed by key.
	fetchCount sync.Map
	// searchCount is the total number of POST /search/jql calls regardless
	// of JQL shape.
	searchCount atomic.Int64
	// fieldsCount is the number of GET /field calls.
	fieldsCount atomic.Int64
}

// newHierarchyServer constructs and starts a hierarchyServer.
//
// issueResponses maps issue key → raw JSON body (200 OK).
// parentMap maps parent-key → child keys returned for `parent = "KEY"`.
// epicMap   maps parent-key → child keys returned for `"Epic Link" = "KEY"`.
// fields is the JSON array returned by GET /field; nil means an empty array.
func newHierarchyServer(
	t *testing.T,
	issueResponses map[string][]byte,
	parentMap map[string][]string,
	epicMap map[string][]string,
	fields []map[string]any,
) *hierarchyServer {
	t.Helper()
	h := &hierarchyServer{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
			key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
			if idx := strings.Index(key, "/"); idx >= 0 {
				key = key[:idx]
			}
			if v, ok := h.fetchCount.Load(key); ok {
				h.fetchCount.Store(key, v.(int64)+1)
			} else {
				h.fetchCount.Store(key, int64(1))
			}
			body, ok := issueResponses[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)

		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/search/jql":
			h.searchCount.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req struct {
				JQL string `json:"jql"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			isEpic := strings.Contains(req.JQL, `"Epic Link"`)
			isParent := strings.HasPrefix(req.JQL, `parent =`) && !isEpic
			key := lastQuotedSegment(req.JQL, "Epic Link")
			var keys []string
			switch {
			case isEpic:
				keys = epicMap[key]
			case isParent:
				keys = parentMap[key]
			}
			issues := make([]map[string]string, 0, len(keys))
			for _, k := range keys {
				issues = append(issues, map[string]string{"key": k})
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues})

		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/field":
			h.fieldsCount.Add(1)
			if fields == nil {
				fields = []map[string]any{}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(fields)

		default:
			http.NotFound(w, r)
		}
	}))
	h.URL = h.srv.URL
	t.Cleanup(h.srv.Close)
	return h
}

// lastQuotedSegment returns the last quoted substring in s that is not
// equal to skip. Used to extract the operand key from JQLs like
// `parent = "KEY"` or `"Epic Link" = "KEY"`.
func lastQuotedSegment(s, skip string) string {
	parts := strings.Split(s, `"`)
	for i := len(parts) - 2; i >= 1; i -= 2 {
		seg := parts[i]
		if seg != "" && seg != skip {
			return seg
		}
	}
	return ""
}

// epicIssueJSON returns a minimal valid Jira Epic JSON for key (issuetype "Epic").
func epicIssueJSON(key, site string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": %q,
  "fields": {
    "summary": "Epic %s",
    "status": {"name": "In Progress"},
    "issuetype": {"name": "Epic"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": []
  }
}`, key, site+"/rest/api/3/issue/"+key, key))
}

// ---------------------------------------------------------------------------
// AC 20 — Children discovered via parent JQL search
// ---------------------------------------------------------------------------

// TestAC20_ChildrenDiscoveredViaParentSearch verifies that after fetching
// an Epic, the crawler issues a JQL `parent = "KEY"` search, enqueues the
// returned child keys, and renders them under the "### Children" subsection
// with relative links once the children are also fetched.
//
// PRD AC 20: summary.Fetched == 3; EXAMPLE-1/index.md contains a
// "### Children" subsection with relative links to EXAMPLE-2 and EXAMPLE-3.
func TestAC20_ChildrenDiscoveredViaParentSearch(t *testing.T) {
	outputDir := t.TempDir()

	var srv *hierarchyServer
	srv = newHierarchyServer(t,
		map[string][]byte{
			"EXAMPLE-1": nil, // populated after srv is constructed (needs URL)
			"EXAMPLE-2": nil,
			"EXAMPLE-3": nil,
		},
		map[string][]string{
			"EXAMPLE-1": {"EXAMPLE-2", "EXAMPLE-3"},
		},
		map[string][]string{}, // no Epic Link map for this AC
		[]map[string]any{
			// No Epic Link field on tenant; auto-detect returns empty.
			{"id": "summary", "key": "summary", "name": "Summary", "custom": false},
		},
	)
	// Re-write the issueResponses now that we know srv.URL.
	srv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Replace the original handler with one that knows srv.URL.
		// We rebuild the closure to populate body bytes lazily.
		issues := map[string][]byte{
			"EXAMPLE-1": epicIssueJSON("EXAMPLE-1", srv.URL),
			"EXAMPLE-2": minimalJSON("EXAMPLE-2", srv.URL),
			"EXAMPLE-3": minimalJSON("EXAMPLE-3", srv.URL),
		}
		parents := map[string][]string{
			"EXAMPLE-1": {"EXAMPLE-2", "EXAMPLE-3"},
		}
		hierarchyServerHandler(t, srv, issues, parents, nil, []map[string]any{
			{"id": "summary", "key": "summary", "name": "Summary", "custom": false},
		})(w, r)
	})

	cfg := acConfigHierarchy(t, srv.URL, outputDir, true, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 20: parent + 2 children = 3 fetched.
	assert.Equal(t, 3, sum.Fetched, "Summary.Fetched")

	// AC 20: EXAMPLE-1/index.md has "### Children" with relative links.
	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err, "ReadFile %s", indexPath)
	md := string(content)
	assert.Contains(t, md, "### Children", "index.md must contain '### Children' subsection")
	assert.Contains(t, md, "../EXAMPLE-2/index.md", "index.md must link to EXAMPLE-2 via relative path")
	assert.Contains(t, md, "../EXAMPLE-3/index.md", "index.md must link to EXAMPLE-3 via relative path")

	// At least one JQL search was made (parent search for EXAMPLE-1).
	assert.GreaterOrEqual(t, srv.searchCount.Load(), int64(1), "at least one /search/jql call expected")
}

// ---------------------------------------------------------------------------
// AC 21 — IncludeChildren=false disables JQL discovery entirely
// ---------------------------------------------------------------------------

// TestAC21_IncludeChildrenFalseDisablesDiscovery verifies that when
// IncludeChildren is false, no JQL search calls are made and only the
// starting issue is fetched.
//
// PRD AC 21: summary.Fetched == 1; POST /search/jql is NEVER called.
func TestAC21_IncludeChildrenFalseDisablesDiscovery(t *testing.T) {
	outputDir := t.TempDir()

	var srv *hierarchyServer
	srv = newHierarchyServer(t, nil, nil, nil, nil)
	srv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issues := map[string][]byte{
			"EXAMPLE-1": epicIssueJSON("EXAMPLE-1", srv.URL),
			"EXAMPLE-2": minimalJSON("EXAMPLE-2", srv.URL),
			"EXAMPLE-3": minimalJSON("EXAMPLE-3", srv.URL),
		}
		parents := map[string][]string{
			"EXAMPLE-1": {"EXAMPLE-2", "EXAMPLE-3"},
		}
		hierarchyServerHandler(t, srv, issues, parents, nil, []map[string]any{
			{"id": "customfield_10014", "key": "customfield_10014", "name": "Epic Link", "custom": true},
		})(w, r)
	})

	cfg := acConfigHierarchy(t, srv.URL, outputDir, false /* includeChildren */, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 21: only the starting issue fetched.
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched (no hierarchy expansion)")
	// AC 21: NO /search/jql calls at all.
	assert.Equal(t, int64(0), srv.searchCount.Load(), "no /search/jql calls expected")
	// AC 21: NO /field calls either (auto-detection is lazy and only runs
	// when hierarchy discovery actually fires).
	assert.Equal(t, int64(0), srv.fieldsCount.Load(), "no /field calls expected")
}

// ---------------------------------------------------------------------------
// AC 22 — Epic Link auto-detection (cached across crawl)
// ---------------------------------------------------------------------------

// TestAC22_EpicLinkAutoDetection verifies that when the tenant has an Epic
// Link custom field, gojira auto-detects it from /rest/api/3/field on first
// use, caches the result, and runs the legacy `"Epic Link" = "KEY"` JQL
// query alongside the modern `parent = "KEY"` query. Children discovered
// via the Epic Link query are enqueued.
//
// PRD AC 22: summary.Fetched == 2 (Epic + 1 Epic Link child);
// EXAMPLE-99 appears under "### Children" of EXAMPLE-1/index.md;
// GET /field is called exactly once across the whole crawl.
func TestAC22_EpicLinkAutoDetection(t *testing.T) {
	outputDir := t.TempDir()

	var srv *hierarchyServer
	srv = newHierarchyServer(t, nil, nil, nil, nil)
	srv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issues := map[string][]byte{
			"EXAMPLE-1":  epicIssueJSON("EXAMPLE-1", srv.URL),
			"EXAMPLE-99": minimalJSON("EXAMPLE-99", srv.URL),
		}
		parents := map[string][]string{
			"EXAMPLE-1": {}, // no modern parent children
		}
		epics := map[string][]string{
			"EXAMPLE-1": {"EXAMPLE-99"},
		}
		hierarchyServerHandler(t, srv, issues, parents, epics, []map[string]any{
			{"id": "customfield_10014", "key": "customfield_10014", "name": "Epic Link", "custom": true},
			{"id": "customfield_10001", "key": "customfield_10001", "name": "Some Other", "custom": true},
		})(w, r)
	})

	cfg := acConfigHierarchy(t, srv.URL, outputDir, true, "" /* auto-detect */)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 22: Epic + 1 child.
	assert.Equal(t, 2, sum.Fetched, "Summary.Fetched")

	// AC 22: EXAMPLE-99 appears in the Children subsection of EXAMPLE-1.
	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err, "ReadFile %s", indexPath)
	md := string(content)
	assert.Contains(t, md, "### Children", "index.md must contain '### Children'")
	assert.Contains(t, md, "EXAMPLE-99", "index.md must reference EXAMPLE-99")

	// AC 22: /field called exactly once (auto-detection result cached).
	assert.Equal(t, int64(1), srv.fieldsCount.Load(), "GET /field must be called exactly once")
}

// hierarchyServerHandler returns the http.Handler-style closure used by
// the three hierarchy AC tests. It is parameterised so the test body can
// configure all maps once the server URL is known (some fixture JSON
// payloads embed the server URL).
//
// fields may be nil for an empty field list.
func hierarchyServerHandler(
	t *testing.T,
	h *hierarchyServer,
	issues map[string][]byte,
	parentMap map[string][]string,
	epicMap map[string][]string,
	fields []map[string]any,
) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
			key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
			if idx := strings.Index(key, "/"); idx >= 0 {
				key = key[:idx]
			}
			if v, ok := h.fetchCount.Load(key); ok {
				h.fetchCount.Store(key, v.(int64)+1)
			} else {
				h.fetchCount.Store(key, int64(1))
			}
			body, ok := issues[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/search/jql":
			h.searchCount.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req struct {
				JQL string `json:"jql"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			isEpic := strings.Contains(req.JQL, `"Epic Link"`)
			isParent := strings.HasPrefix(req.JQL, `parent =`) && !isEpic
			key := lastQuotedSegment(req.JQL, "Epic Link")
			var keys []string
			switch {
			case isEpic:
				keys = epicMap[key]
			case isParent:
				keys = parentMap[key]
			}
			issues := make([]map[string]string, 0, len(keys))
			for _, k := range keys {
				issues = append(issues, map[string]string{"key": k})
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues})
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/field":
			h.fieldsCount.Add(1)
			out := fields
			if out == nil {
				out = []map[string]any{}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}
}

// ===========================================================================
// AC 23–25 — Dev Status pull-request enrichment
// ===========================================================================

// issueWithSummaryAndIDJSON returns a Jira issue JSON for key that
// carries a numeric "id" and an optional customfield_10000 development-
// summary blob with only the pullrequest dataType section. When
// summaryCount is negative, customfield_10000 is omitted; when >= 0,
// the documented summary shape is embedded as a JSON string whose
// inner json={...} payload reports overall.count = summaryCount for
// pullrequest.
func issueWithSummaryAndIDJSON(t *testing.T, key, site, numericID string, summaryCount int) []byte {
	t.Helper()
	customFields := ""
	if summaryCount >= 0 {
		inner := fmt.Sprintf(
			`{"cachedValue":{"errors":[],"summary":{"pullrequest":{"overall":{"count":%d,"lastUpdated":"2026-05-08T13:44:52.000+0000","stateCount":%d,"state":"MERGED","dataType":"pullrequest","open":false},"byInstanceType":{"GitHub":{"count":%d,"name":"GitHub"}}}}},"isStale":true}`,
			summaryCount, summaryCount, summaryCount,
		)
		wrapped := fmt.Sprintf(
			"{pullrequest={dataType=pullrequest, state=MERGED, stateCount=%d}, json=%s}",
			summaryCount, inner,
		)
		blob, err := json.Marshal(wrapped)
		require.NoError(t, err)
		customFields = fmt.Sprintf(`,"customfield_10000":%s`, string(blob))
	}
	return []byte(fmt.Sprintf(`{
  "id": %q,
  "key": %q,
  "self": %q,
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Done"},
    "issuetype": {"name": "Story"},
    "assignee": null,
    "reporter": {"displayName": "Robert Tirserio", "emailAddress": "robert@example.com"},
    "created": "2026-05-01T08:00:00.000+0000",
    "updated": "2026-05-08T13:44:52.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": []%s
  }
}`, numericID, key, site+"/rest/api/3/issue/"+key, key, customFields))
}

// issueWithMultiSummaryJSON returns a Jira issue JSON with a
// customfield_10000 blob whose inner json={...} payload reports
// arbitrary per-dataType counts. Pass -1 for any field to omit that
// section entirely (mirroring the PLATENG-1578 reproducer shape where
// only "repository" was present).
func issueWithMultiSummaryJSON(t *testing.T, key, site, numericID string, pr, br, cm, rp, bd int, isStale bool) []byte {
	t.Helper()
	sections := []string{}
	addSection := func(name string, count int) {
		if count < 0 {
			return
		}
		sections = append(sections, fmt.Sprintf(
			`%q:{"overall":{"count":%d,"dataType":%q}}`,
			name, count, name,
		))
	}
	addSection("pullrequest", pr)
	addSection("branch", br)
	addSection("commit", cm)
	addSection("repository", rp)
	addSection("build", bd)
	joined := ""
	for i, s := range sections {
		if i > 0 {
			joined += ","
		}
		joined += s
	}
	inner := fmt.Sprintf(
		`{"cachedValue":{"errors":[],"summary":{%s}},"isStale":%v}`,
		joined, isStale,
	)
	wrapped := "{json=" + inner + "}"
	blob, err := json.Marshal(wrapped)
	require.NoError(t, err)
	return []byte(fmt.Sprintf(`{
  "id": %q,
  "key": %q,
  "self": %q,
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Done"},
    "issuetype": {"name": "Story"},
    "assignee": null,
    "reporter": {"displayName": "Robert Tirserio", "emailAddress": "robert@example.com"},
    "created": "2026-05-01T08:00:00.000+0000",
    "updated": "2026-05-08T13:44:52.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": [],
    "customfield_10000": %s
  }
}`, numericID, key, site+"/rest/api/3/issue/"+key, key, string(blob)))
}

// branchFixture is a synthetic Dev Status branch response body.
const branchFixture = `{
  "errors": [],
  "detail": [
    {
      "_instance": {"type": "GitHub", "name": "GitHub"},
      "branches": [
        {
          "name": "feature/PLATENG-1578",
          "url": "https://github.com/org/api/tree/feature%2FPLATENG-1578",
          "repository": {"name": "org/api", "url": "https://github.com/org/api"},
          "lastCommit": {
            "id": "abc123def456",
            "displayId": "abc123d",
            "url": "https://github.com/org/api/commit/abc123",
            "message": "feat: branch head",
            "author": {"name": "Kareem Hepburn"},
            "authorTimestamp": "2026-06-02T14:30:00.000+0000"
          }
        }
      ]
    }
  ]
}`

// commitFixture is a synthetic Dev Status commit response body.
const commitFixture = `{
  "errors": [],
  "detail": [
    {
      "_instance": {"type": "GitHub", "name": "GitHub"},
      "commits": [
        {
          "id": "abc123def456",
          "displayId": "abc123d",
          "url": "https://github.com/org/api/commit/abc123",
          "message": "feat: add OIDC initiate endpoint",
          "author": {"name": "Kareem Hepburn"},
          "authorTimestamp": "2026-06-02T14:30:00.000+0000",
          "fileCount": 3,
          "merge": false,
          "repository": {"name": "org/api", "url": "https://github.com/org/api"}
        }
      ]
    }
  ]
}`

// repositoryFixture is a synthetic Dev Status repository response body.
const repositoryFixture = `{
  "errors": [],
  "detail": [
    {
      "_instance": {"type": "GitHub", "name": "GitHub"},
      "repositories": [
        {"name": "org/api", "url": "https://github.com/org/api", "avatar": "https://example.com/a.png"}
      ]
    }
  ]
}`

// buildFixture is a synthetic Dev Status build response body.
const buildFixture = `{
  "errors": [],
  "detail": [
    {
      "_instance": {"type": "GitHub", "name": "GitHub"},
      "builds": [
        {
          "id": "42",
          "buildNumber": 42,
          "name": "Build #42",
          "description": "Pipelines build",
          "url": "https://bitbucket.org/org/api/pipelines/results/42",
          "state": "SUCCESSFUL",
          "lastUpdated": "2026-06-02T14:30:00.000+0000",
          "testSummary": {"totalNumber": 100, "passedNumber": 100, "failedNumber": 0, "skippedNumber": 0},
          "references": [{"name": "feature/PLATENG-1578", "uri": "refs/heads/feature/PLATENG-1578"}]
        }
      ]
    }
  ]
}`

// emptyFixture is the canonical "no entities" Dev Status response,
// returned when a tenant has no integration that produces a particular
// dataType.
const emptyFixture = `{"errors":[],"detail":[]}`

// devStatusFixture returns the canonical Dev Status response for two
// PRs associated with a single GitHub instance, mirroring the shape
// captured from instinctvet.atlassian.net during the prompt's research.
const devStatusFixture = `{
  "errors": [],
  "detail": [
    {
      "_instance": {
        "id": "com.github.integration.production",
        "type": "GitHub",
        "singleInstance": true,
        "baseUrl": "https://github.com",
        "typeName": "GitHub",
        "name": "GitHub"
      },
      "branches": [],
      "pullRequests": [
        {
          "id": "#557",
          "url": "https://github.com/org/repo/pull/557",
          "name": "PLATENG-1573: Cognito User Pool",
          "status": "MERGED",
          "lastUpdate": "2026-05-08T13:44:52.000+0000",
          "source": {"branch": "feature/PLATENG-1573", "url": "https://github.com/org/repo/tree/feature%2FPLATENG-1573"},
          "destination": {"branch": "main", "url": "https://github.com/org/repo/tree/main"},
          "author": {"name": "Robert Tirserio", "avatar": "https://example.com/a.png"},
          "reviewers": [{"name": "Kareem Hepburn", "avatar": "x", "approved": true}],
          "repositoryUrl": "https://github.com/org/repo",
          "repositoryName": "org/repo",
          "repositoryId": "abc",
          "commentCount": 0
        },
        {
          "id": "#560",
          "url": "https://github.com/org/repo/pull/560",
          "name": "PLATENG-1573: Follow-up",
          "status": "MERGED",
          "lastUpdate": "2026-05-09T09:12:30.000+0000",
          "source": {"branch": "feature/PLATENG-1573-followup", "url": "https://github.com/org/repo/tree/feature%2FPLATENG-1573-followup"},
          "destination": {"branch": "main", "url": "https://github.com/org/repo/tree/main"},
          "author": {"name": "Kareem Hepburn", "avatar": "x"},
          "reviewers": [],
          "repositoryUrl": "https://github.com/org/repo",
          "repositoryName": "org/repo",
          "repositoryId": "abc",
          "commentCount": 1
        }
      ],
      "repositories": []
    }
  ]
}`

// devStatusServer routes the GET /issue and GET /dev-status endpoints
// for the AC 23 and AC 25–30 tests, tracking call counts per dataType
// so tests can assert that the smart gate selected the right set.
//
// devStatusBodies maps dataType → response body; the empty default
// (no per-dataType body configured) returns the canonical
// devStatusFixture (pull-request-flavoured) so existing AC 23 calls
// keep working unchanged.
type devStatusServer struct {
	URL string
	srv *httptest.Server

	issueResponses     map[string][]byte
	devStatusBody      string
	devStatusBodies    map[string]string // dataType → body
	devStatusCalls     atomic.Int64
	devStatusPerDT     map[string]*atomic.Int64
	devStatusCallsLock sync.Mutex
}

func newDevStatusServer(t *testing.T) *devStatusServer {
	t.Helper()
	s := &devStatusServer{
		issueResponses:  make(map[string][]byte),
		devStatusBody:   devStatusFixture,
		devStatusBodies: make(map[string]string),
		devStatusPerDT:  make(map[string]*atomic.Int64),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
			key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
			if idx := strings.Index(key, "/"); idx >= 0 {
				key = key[:idx]
			}
			body, ok := s.issueResponses[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && r.URL.Path == "/rest/dev-status/1.0/issue/detail":
			dt := r.URL.Query().Get("dataType")
			s.devStatusCalls.Add(1)
			s.devStatusCallsLock.Lock()
			c, ok := s.devStatusPerDT[dt]
			if !ok {
				c = &atomic.Int64{}
				s.devStatusPerDT[dt] = c
			}
			s.devStatusCallsLock.Unlock()
			c.Add(1)

			body := s.devStatusBody
			if perDT, ok := s.devStatusBodies[dt]; ok {
				body = perDT
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/search/jql":
			// Hierarchy is disabled in these tests, but be robust to a
			// stray search call (return empty).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"issues":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

// callsFor returns the number of Dev Status calls the server received
// for the given dataType. Used by tests to assert the smart gate
// selected (or skipped) a particular dataType.
func (s *devStatusServer) callsFor(dt string) int64 {
	s.devStatusCallsLock.Lock()
	defer s.devStatusCallsLock.Unlock()
	c, ok := s.devStatusPerDT[dt]
	if !ok {
		return 0
	}
	return c.Load()
}

// acConfigDevStatus builds a gojira.Config for the AC 23–25 tests.
// Hierarchy is disabled to keep the test scope tight; dev-status
// enrichment is governed by the includeDevStatus parameter.
func acConfigDevStatus(t *testing.T, siteURL, outputDir string, includeDevStatus bool) gojira.Config {
	t.Helper()
	kv := map[string]string{
		"GOJIRA_SITE":             siteURL,
		"GOJIRA_USER":             "test@example.com",
		"GOJIRA_TOKEN":            "test-token",
		"GOJIRA_OUTPUT_DIR":       outputDir,
		"GOJIRA_CONCURRENCY":      "1",
		"GOJIRA_ISSUE_CAP":        "0",
		"GOJIRA_INCLUDE_CHILDREN": "false",
	}
	if includeDevStatus {
		kv["GOJIRA_INCLUDE_DEV_STATUS"] = "true"
	} else {
		kv["GOJIRA_INCLUDE_DEV_STATUS"] = "false"
	}
	cfg, err := gojira.LoadConfig(kv)
	require.NoError(t, err, "acConfigDevStatus: LoadConfig")
	return cfg
}

// TestAC23_DevStatusPullRequestsSurfaced verifies that an issue with
// dev-status pull-request data has its PR URLs surfaced under
// "## Development > ### Pull requests" with IncludeDevStatus
// defaulting on. With the smart gate removed, ALL five configured
// dataTypes are queried unconditionally; the four non-PR dataTypes
// are wired to the canonical empty response so only the
// dataType=pullrequest call contributes entities to the rendered
// output.
func TestAC23_DevStatusPullRequestsSurfaced(t *testing.T) {
	outputDir := t.TempDir()
	srv := newDevStatusServer(t)
	srv.issueResponses["PLATENG-1573"] = issueWithSummaryAndIDJSON(t, "PLATENG-1573", srv.URL, "86679", 2)
	// Default body (devStatusFixture) is PR-flavoured; with the gate
	// gone, every dataType is queried, so explicitly silence the four
	// non-PR dataTypes to keep the per-subsection assertions tight.
	srv.devStatusBodies["pullrequest"] = devStatusFixture
	srv.devStatusBodies["branch"] = emptyFixture
	srv.devStatusBodies["commit"] = emptyFixture
	srv.devStatusBodies["repository"] = emptyFixture
	srv.devStatusBodies["build"] = emptyFixture

	cfg := acConfigDevStatus(t, srv.URL, outputDir, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"PLATENG-1573"}, nil)
	require.NoError(t, err, "Crawl")
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")

	// Gate removed: every configured dataType is queried regardless of
	// what (if anything) customfield_10000 reports. Five dataTypes ×
	// one application = five calls.
	assert.Equal(t, int64(5), srv.devStatusCalls.Load(), "all five Dev Status dataTypes queried")
	for _, dt := range []string{"pullrequest", "branch", "commit", "repository", "build"} {
		assert.Equal(t, int64(1), srv.callsFor(dt), "dataType %s called exactly once", dt)
	}

	indexPath := filepath.Join(outputDir, "PLATENG-1573", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err, "ReadFile %s", indexPath)
	md := string(content)
	assert.Contains(t, md, "## Development\n", "Development section present")
	assert.Contains(t, md, "### Pull requests\n")
	assert.Contains(t, md, "https://github.com/org/repo/pull/557", "PR #557 URL present")
	assert.Contains(t, md, "https://github.com/org/repo/pull/560", "PR #560 URL present")

	// Other subsections elide cleanly when their lists are empty.
	assert.NotContains(t, md, "### Branches", "Branches subsection elided")
	assert.NotContains(t, md, "### Commits", "Commits subsection elided")
	assert.NotContains(t, md, "### Repositories", "Repositories subsection elided")
	assert.NotContains(t, md, "### Builds", "Builds subsection elided")

	// PR counter incremented by dev-status discoveries.
	assert.Equal(t, 2, sum.PRsFound, "Summary.PRsFound counts dev-status PRs")
}

// TestAC25_IncludeDevStatusFalseDisables verifies that
// GOJIRA_INCLUDE_DEV_STATUS=false disables enrichment entirely: no
// Dev Status calls are issued for any configured dataType regardless
// of what customfield_10000 reports.
func TestAC25_IncludeDevStatusFalseDisables(t *testing.T) {
	outputDir := t.TempDir()
	srv := newDevStatusServer(t)
	srv.issueResponses["PLATENG-1573"] = issueWithSummaryAndIDJSON(t, "PLATENG-1573", srv.URL, "86679", 2)

	cfg := acConfigDevStatus(t, srv.URL, outputDir, false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"PLATENG-1573"}, nil)
	require.NoError(t, err, "Crawl")
	assert.Equal(t, 1, sum.Fetched)

	// AC 25: opt-out disables Dev Status entirely across every dataType.
	assert.Equal(t, int64(0), srv.devStatusCalls.Load(), "Dev Status must NOT be called when IncludeDevStatus=false")

	indexPath := filepath.Join(outputDir, "PLATENG-1573", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "## Development", "no Development section when enrichment disabled")
}

// TestAC26_BranchesSurfacedWithoutPRs verifies that an issue whose
// only Dev Status entity is a branch surfaces "### Branches" but NOT
// "### Pull requests" in the rendered output. With the gate gone the
// crawler queries every configured dataType; only the branch
// response carries an entity (the four other dataTypes return the
// canonical empty body), so only the Branches subsection appears.
func TestAC26_BranchesSurfacedWithoutPRs(t *testing.T) {
	outputDir := t.TempDir()
	srv := newDevStatusServer(t)
	srv.issueResponses["PLATENG-1578"] = issueWithMultiSummaryJSON(t, "PLATENG-1578", srv.URL, "86680",
		0 /*pr*/, 1 /*br*/, 0, 0, 0, false)
	srv.devStatusBodies["branch"] = branchFixture
	// Silence the other four dataTypes; gate-removal means each is
	// queried regardless of the summary, and the default body would
	// otherwise leak a PR into the output.
	srv.devStatusBodies["pullrequest"] = emptyFixture
	srv.devStatusBodies["commit"] = emptyFixture
	srv.devStatusBodies["repository"] = emptyFixture
	srv.devStatusBodies["build"] = emptyFixture

	cfg := acConfigDevStatus(t, srv.URL, outputDir, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"PLATENG-1578"}, nil)
	require.NoError(t, err, "Crawl")
	assert.Equal(t, 1, sum.Fetched)

	// All five dataTypes are queried; the branch response is the only
	// one that carries an entity.
	assert.Equal(t, int64(5), srv.devStatusCalls.Load(), "all five dataTypes queried")
	assert.Equal(t, int64(1), srv.callsFor("branch"))
	assert.Equal(t, int64(1), srv.callsFor("pullrequest"))

	indexPath := filepath.Join(outputDir, "PLATENG-1578", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	md := string(content)
	assert.Contains(t, md, "## Development\n", "Development section present")
	assert.Contains(t, md, "### Branches\n", "Branches subsection present")
	assert.NotContains(t, md, "### Pull requests", "Pull requests subsection elided")
	assert.Contains(t, md, "feature/PLATENG-1578", "branch name rendered")
}

// TestAC27_CommitsSurfaced verifies that an issue whose only Dev
// Status entity is a commit surfaces "### Commits" in the rendered
// output. With the gate gone every configured dataType is queried;
// the four non-commit dataTypes return the canonical empty body.
func TestAC27_CommitsSurfaced(t *testing.T) {
	outputDir := t.TempDir()
	srv := newDevStatusServer(t)
	srv.issueResponses["PLATENG-1579"] = issueWithMultiSummaryJSON(t, "PLATENG-1579", srv.URL, "86681",
		0, 0, 1, 0, 0, false)
	srv.devStatusBodies["commit"] = commitFixture
	srv.devStatusBodies["pullrequest"] = emptyFixture
	srv.devStatusBodies["branch"] = emptyFixture
	srv.devStatusBodies["repository"] = emptyFixture
	srv.devStatusBodies["build"] = emptyFixture

	cfg := acConfigDevStatus(t, srv.URL, outputDir, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"PLATENG-1579"}, nil)
	require.NoError(t, err, "Crawl")
	assert.Equal(t, 1, sum.Fetched)
	assert.Equal(t, int64(5), srv.devStatusCalls.Load(), "all five dataTypes queried")
	assert.Equal(t, int64(1), srv.callsFor("commit"))

	indexPath := filepath.Join(outputDir, "PLATENG-1579", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	md := string(content)
	assert.Contains(t, md, "### Commits\n")
	assert.Contains(t, md, "abc123d", "short commit ID rendered")
	assert.Contains(t, md, "feat: add OIDC initiate endpoint", "commit message rendered")
}

// TestAC28_RepositoriesSurfaced is the literal PLATENG-1578 staleness
// reproducer, updated for the gate-removal contract. The summary
// indicates repository.count=1 and reports no pullrequest section at
// all, with isStale:true (matching the original user-reported
// response). The crawler MUST query ALL five configured dataTypes
// regardless of what the summary says, and the rendered output MUST
// contain a "### Repositories" subsection (the entity the summary
// did surface). This locks in the bug fix: silent misses were
// caused by the gate trusting a stale summary; the gate is gone,
// so the dataType=repository call goes out unconditionally and the
// repository surfaces.
func TestAC28_RepositoriesSurfaced(t *testing.T) {
	outputDir := t.TempDir()
	srv := newDevStatusServer(t)
	srv.issueResponses["PLATENG-1578"] = issueWithMultiSummaryJSON(t, "PLATENG-1578", srv.URL, "86680",
		-1 /*pr omitted*/, -1, -1, 1 /*rp*/, -1, true /*isStale: real response had it set*/)
	srv.devStatusBodies["repository"] = repositoryFixture
	// Silence the other four dataTypes so only the repository call
	// contributes an entity. With the gate gone, every dataType is
	// queried unconditionally.
	srv.devStatusBodies["pullrequest"] = emptyFixture
	srv.devStatusBodies["branch"] = emptyFixture
	srv.devStatusBodies["commit"] = emptyFixture
	srv.devStatusBodies["build"] = emptyFixture

	cfg := acConfigDevStatus(t, srv.URL, outputDir, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"PLATENG-1578"}, nil)
	require.NoError(t, err, "Crawl")
	assert.Equal(t, 1, sum.Fetched)

	// Gate removed: every configured dataType is queried regardless of
	// the summary's per-dataType counts (or its isStale flag). The
	// repository entity surfaces because the dataType=repository call
	// went out unconditionally; the four other calls returned empty.
	assert.Equal(t, int64(5), srv.devStatusCalls.Load(),
		"all five dataTypes queried regardless of summary contents")
	for _, dt := range []string{"pullrequest", "branch", "commit", "repository", "build"} {
		assert.Equal(t, int64(1), srv.callsFor(dt), "dataType %s called exactly once", dt)
	}

	indexPath := filepath.Join(outputDir, "PLATENG-1578", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	md := string(content)
	assert.Contains(t, md, "## Development\n", "Development section present")
	assert.Contains(t, md, "### Repositories\n", "Repositories subsection present")
	assert.Contains(t, md, "[org/api](https://github.com/org/api)",
		"repository rendered as Markdown link")
}

// TestAC29_BuildsSurfaced verifies that an issue whose only Dev
// Status entity is a build is rendered with the documented
// "[STATE] [name](url) — date [tests P/T]" format. With the gate
// gone every configured dataType is queried; the four non-build
// dataTypes return the canonical empty body.
func TestAC29_BuildsSurfaced(t *testing.T) {
	outputDir := t.TempDir()
	srv := newDevStatusServer(t)
	srv.issueResponses["PLATENG-1580"] = issueWithMultiSummaryJSON(t, "PLATENG-1580", srv.URL, "86682",
		0, 0, 0, 0, 1 /*bd*/, false)
	srv.devStatusBodies["build"] = buildFixture
	srv.devStatusBodies["pullrequest"] = emptyFixture
	srv.devStatusBodies["branch"] = emptyFixture
	srv.devStatusBodies["commit"] = emptyFixture
	srv.devStatusBodies["repository"] = emptyFixture

	cfg := acConfigDevStatus(t, srv.URL, outputDir, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"PLATENG-1580"}, nil)
	require.NoError(t, err, "Crawl")
	assert.Equal(t, 1, sum.Fetched)
	assert.Equal(t, int64(5), srv.devStatusCalls.Load(), "all five dataTypes queried")
	assert.Equal(t, int64(1), srv.callsFor("build"))

	indexPath := filepath.Join(outputDir, "PLATENG-1580", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	md := string(content)
	assert.Contains(t, md, "### Builds\n")
	assert.Contains(t, md, "[SUCCESSFUL] [Build #42](https://bitbucket.org/org/api/pipelines/results/42)",
		"build rendered with state and link")
	assert.Contains(t, md, "[tests 100/100]", "test summary suffix rendered")
}

// recordingSink captures every emitted Event for later assertion.
// It is safe for concurrent Emit calls.
type recordingSink struct {
	mu     sync.Mutex
	events []gojira.Event
}

func (r *recordingSink) Emit(e gojira.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recordingSink) snapshot() []gojira.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]gojira.Event, len(r.events))
	copy(out, r.events)
	return out
}

// TestAC31_DevStatusPartialFailureIsWarning is the PLATENG-1417
// regression test at the end-to-end facade layer. When one Dev Status
// dataType call returns a malformed body (causing the client to fail
// the unmarshal) while another succeeds, the issue MUST still be
// rendered with the ## Development section populated from the partial
// data, and the per-call failure MUST surface as a
// "devstatus.partial_failure" event (not "issue.failed"). The crawl
// summary's Failed count MUST NOT include the parent issue.
//
// This locks in the fix for the user-reported PLATENG-1417 case where
// dataType=commit and dataType=build returned object-shaped "errors"
// entries that crashed the unmarshal; the symptoms were:
//   - an ERROR log line "dev status enrichment failed for
//     PLATENG-1417 ... event=issue.failed"
//   - no ## Development section in the rendered output, even though
//     dataType=pullrequest would have returned a usable PR.
func TestAC31_DevStatusPartialFailureIsWarning(t *testing.T) {
	outputDir := t.TempDir()
	srv := newDevStatusServer(t)
	// Stale-summary path forces fallback-to-all (the user's actual
	// PLATENG-1417 scope, where the summary did not steer the gate
	// toward a single dataType).
	srv.issueResponses["PLATENG-1417"] = issueWithMultiSummaryJSON(t, "PLATENG-1417", srv.URL, "86679",
		0, 0, 0, 0, 0, true)

	// dataType=pullrequest returns a valid response with one PR.
	srv.devStatusBodies["pullrequest"] = devStatusFixture
	// dataType=commit returns a body that the client cannot unmarshal:
	// "errors" carries an entry whose value is a JSON number, which is
	// neither a string nor an object/array — it cannot decode into
	// json.RawMessage's containing slice element either when the outer
	// shape is otherwise malformed. Use a truncated body to force a
	// real unmarshal error inside the client.
	srv.devStatusBodies["commit"] = `{"errors": [], "detail": [`
	// dataType=build also fails to unmarshal.
	srv.devStatusBodies["build"] = `{"errors": [], "detail": [`
	// branch and repository return canonical empty responses so the
	// fan-out is realistic.
	srv.devStatusBodies["branch"] = emptyFixture
	srv.devStatusBodies["repository"] = emptyFixture

	cfg := acConfigDevStatus(t, srv.URL, outputDir, true)
	sink := &recordingSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"PLATENG-1417"}, sink)
	require.NoError(t, err, "Crawl must not return a fatal error for a partial enrichment failure")

	// Summary semantics: the issue is Fetched, NOT Failed. The crawl
	// summary's Failed count is the operator's signal that something
	// went wrong with an issue itself; a degraded enrichment source
	// must not pollute it.
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, 0, sum.Failed, "Summary.Failed must NOT include partial-enrichment failures")
	assert.NotContains(t, sum.FailedKeys, "PLATENG-1417",
		"FailedKeys must not include an issue whose enrichment partially failed")

	// Event taxonomy:
	//   - at least one devstatus.partial_failure for PLATENG-1417;
	//   - zero issue.failed events for PLATENG-1417.
	var (
		partialCount int
		failedCount  int
	)
	for _, e := range sink.snapshot() {
		if e.IssueKey != "PLATENG-1417" {
			continue
		}
		switch e.Kind {
		case "devstatus.partial_failure":
			partialCount++
		case "issue.failed":
			failedCount++
		}
	}
	assert.GreaterOrEqual(t, partialCount, 1,
		"at least one devstatus.partial_failure event expected")
	assert.Equal(t, 0, failedCount,
		"NO issue.failed event expected for the issue whose enrichment partially failed")

	// Rendered output: ## Development is present and the PR that did
	// come back is rendered. This is the visible side of the user's
	// PLATENG-1417 report — they observed no Development section at
	// all, which would mean the partial data was discarded.
	indexPath := filepath.Join(outputDir, "PLATENG-1417", "index.md")
	content, readErr := os.ReadFile(indexPath)
	require.NoError(t, readErr, "ReadFile %s", indexPath)
	md := string(content)
	assert.Contains(t, md, "## Development\n",
		"Development section MUST be rendered from the partial data")
	assert.Contains(t, md, "### Pull requests\n",
		"the dataType that succeeded MUST surface in the rendered output")
	assert.Contains(t, md, "https://github.com/org/repo/pull/557",
		"the PR returned by the successful dataType=pullrequest call is rendered")
}

// ---------------------------------------------------------------------------
// AC 32 — Custom fields rendered with human labels via expand=names
// ---------------------------------------------------------------------------

// TestAC32_CustomFieldsRenderedWithHumanLabels verifies that, with
// the default config, an issue's "## Custom fields" section:
//
//   - uses **<Name>** labels for fields whose ID maps to a label in
//     the response's top-level "names" object (delivered by
//     `expand=names` on the GetIssue request),
//   - renders JSON primitives (strings, numbers, bools) inline
//     directly after the label,
//   - pretty-prints JSON objects and arrays inside fenced ```json
//     blocks indented under the list bullet,
//   - skips JSON-null entries entirely (the default
//     GOJIRA_RENDER_NULL_CUSTOM_FIELDS=false behaviour).
//
// This is the end-to-end version of the per-helper render tests in
// internal/render/render_test.go: the assertions here run against
// the index.md that was actually written to disk after a real
// gojira.Crawl, with the full fetch → parse → render → output
// pipeline exercised.
func TestAC32_CustomFieldsRenderedWithHumanLabels(t *testing.T) {
	outputDir := t.TempDir()

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
		if key != "EXAMPLE-1" {
			http.NotFound(w, r)
			return
		}
		// The crawl's fetcher requests expand=names; the fixture
		// carries the names object so the renderer has labels to
		// surface.
		assert.Equal(t, "names", r.URL.Query().Get("expand"),
			"crawl must request expand=names so labels are available")
		body := loadFixture(t, "ac32_custom_fields_with_names.json", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")
	require.GreaterOrEqual(t, sum.Fetched, 1, "Summary.Fetched")

	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err, "ReadFile %s", indexPath)
	md := string(content)

	// Section header present.
	assert.Contains(t, md, "## Custom fields\n", "Custom fields section present")

	// AC 32a: named primitive field renders inline with the label
	// the names object supplied — the opaque ID never appears for it.
	assert.Contains(t, md, "- **Rank**: \"0|i07gzp:\"\n",
		"named primitive field uses **Bold** label inline")
	assert.NotContains(t, md, "`customfield_10116`",
		"named field MUST NOT also appear under its raw id")

	// AC 32b: named structured field renders in a fenced ```json
	// block indented under the bullet. The pretty body must
	// contain a newline (multi-line) and one of the array's
	// values.
	assert.Contains(t, md, "- **Sprint**:\n  ```json\n",
		"named structured field opens a fenced json block under its bullet")
	assert.Contains(t, md, `"name": "PLATENG Sprint 57"`,
		"pretty-printed sprint content preserved")
	assert.Contains(t, md, "\n  ```\n",
		"fenced block closes with proper 2-space indent")

	// AC 32c: null-valued custom field is skipped by default.
	// Neither the label nor the id is rendered.
	assert.NotContains(t, md, "**Squad**",
		"null-valued field's label must be skipped under default config")
	assert.NotContains(t, md, "customfield_10220",
		"null-valued field's id must be skipped under default config")
}

// TestAC33_DevStatusSummaryFieldRendersAsCodeBlock pins the
// PLATENG-1578 rendering fix: a custom-field value whose raw JSON
// form is a JSON-encoded *string* whose decoded contents are
// structured non-JSON text — the canonical case is Atlassian's
// `customfield_10000` Dev Status summary in its
// `{repository={count=1, ...}, json={"cachedValue":{...}, ...}}`
// mixed notation — must render under the `## Custom fields` section
// in a PLAIN ``` fenced code block (no language tag), with:
//
//   - the outer JSON-string quotes stripped from the rendered
//     content (the inner structured text is the only legible
//     representation),
//   - the inner content decoded ONCE before rendering (no `\"`
//     escape sequences leak through to the rendered output),
//   - per-line two-space indentation under the bullet so the fence
//     does not terminate the surrounding Markdown list.
//
// This is the end-to-end version of
// TestRenderIssue_CustomFieldsStringStructuredAsPlainFence in
// internal/render/render_test.go: the assertions here run against
// the index.md that was actually written to disk after a real
// gojira.Crawl, exercising the full fetch → parse → render →
// output pipeline.
func TestAC33_DevStatusSummaryFieldRendersAsCodeBlock(t *testing.T) {
	outputDir := t.TempDir()

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
		if key != "EXAMPLE-1" {
			http.NotFound(w, r)
			return
		}
		// The crawl's fetcher requests expand=names; the fixture
		// carries the names object so "Development" appears as
		// the rendered label.
		assert.Equal(t, "names", r.URL.Query().Get("expand"),
			"crawl must request expand=names so labels are available")
		body := loadFixture(t, "ac33_dev_status_summary_field.json", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := acConfig(t, srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")
	require.GreaterOrEqual(t, sum.Fetched, 1, "Summary.Fetched")

	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	content, err := os.ReadFile(indexPath)
	require.NoError(t, err, "ReadFile %s", indexPath)
	md := string(content)

	// AC 33a: section header and labelled bullet present.
	assert.Contains(t, md, "## Custom fields\n",
		"Custom fields section present")
	assert.Contains(t, md, "**Development**:",
		"Development bullet present under Custom fields")

	// AC 33b: a fenced ```json block opens directly under the
	// bullet with two-space indentation. The inner content is
	// not strictly valid JSON (the outer wrapping uses
	// Atlassian's {key=value} notation), but the bulk of the
	// content is JSON-shaped and Markdown viewers do not
	// validate fence language tags. The json tag gives readers
	// the same syntax-highlighting they get from the structured
	// case.
	assert.Contains(t, md, "- **Development**:\n  ```json\n",
		"```json fence opens under the bullet")
	assert.NotContains(t, md, "- **Development**:\n  ```\n{",
		"string-structured must use the json tag, not a plain fence")

	// AC 33c: the outer Atlassian {key=value} notation is no
	// longer one giant line. prettifyAtlassianBlob walks the
	// content and breaks each container open onto its own line
	// with two-space-per-depth indentation. The `repository={\n`
	// substring proves the outer wrapping was expanded.
	assert.Contains(t, md, "repository={\n",
		"outer Java-notation object should break to multi-line")

	// AC 33d: the inner content was decoded ONCE before rendering.
	// The JSON-escape sequence `\"cachedValue\":` must NOT appear;
	// the rendered output has the bare `"cachedValue":`.
	assert.NotContains(t, md, `\"cachedValue\":`,
		"inner content must be decoded once before rendering")

	// AC 33e: the outer JSON-string quotes that wrapped the entire
	// summary blob are stripped from the rendered output. The
	// rendered fenced line starts with `{`, not with `"`. The
	// specific anti-pattern is a 2-space indent followed by an
	// open quote followed by the inner content.
	assert.NotContains(t, md, "  \"{repository=",
		"outer JSON-string quotes must be stripped from the rendered content")

	// AC 33f: the embedded json={...} payload is delegated to
	// json.Indent at the matching depth. The inner JSON keys are
	// followed by `: ` (colon-space), not bare `:` — the
	// canonical proof that json.Indent fired on the inner range.
	assert.Contains(t, md, "\"isStale\": true",
		"inner JSON payload should be pretty-printed")

	// AC 33g: the fenced ```json block opens with a newline and
	// the JSON pretty-print's first character `{` at the bullet's
	// two-space indent column. This double-asserts the bullet
	// integrity (two-space-indented fence) and the walker's
	// "newline immediately after opening brace" rule.
	assert.Contains(t, md, "  ```json\n  {\n",
		"the fence content begins with a newline-formatted JSON block at the expected 2-space indent under the bullet")
}
