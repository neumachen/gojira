// Package gojira_test contains end-to-end tests for the public library facade.
//
// Each test uses httptest.NewServer to fake the Jira API and t.TempDir() for
// output. No live network calls are made.
//
// Tests cover the five PRD acceptance criteria called out in the Phase 4.1
// task block:
//
//   - AC 1  (classify): Classify wraps classify.Classify correctly.
//   - AC 2  (single issue render): FetchAndRender returns expected Markdown.
//   - AC 6  (deduplication): Crawl with A↔B cycle fetches each exactly once.
//   - AC 9  (permission-denied stub): Crawl with 403 writes a stub.
//   - AC 10 (skip-if-exists): Crawl skips issues whose index.md already exists.
package gojira_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
// Fixture helpers
// ---------------------------------------------------------------------------

// minimalIssueJSON returns a minimal valid Jira issue JSON for key with no
// outbound links. The site parameter is used in the "self" URL only.
func minimalIssueJSON(key, site string) []byte {
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

// issueWithLinkJSON returns a Jira issue JSON for key that has an outward
// issue link to linkedKey.
func issueWithLinkJSON(key, linkedKey, site string) []byte {
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

// ---------------------------------------------------------------------------
// httptest server helpers
// ---------------------------------------------------------------------------

// issueServer starts an httptest.Server that serves Jira issue JSON.
// responses maps issue key → raw JSON bytes. A 404 is returned for unknown
// keys. statusOverrides maps issue key → HTTP status code (e.g. 403).
func issueServer(t *testing.T, responses map[string][]byte, statusOverrides map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Expect paths like /rest/api/3/issue/<KEY>
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		// Strip any trailing path segments.
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}

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

// testConfig builds a gojira.Config pointing at the given httptest server URL
// and using t.TempDir() as the output directory.
func testConfig(t *testing.T, siteURL, outputDir string) gojira.Config {
	t.Helper()
	cfg, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":        siteURL,
		"GOJIRA_USER":        "test@example.com",
		"GOJIRA_TOKEN":       "test-token",
		"GOJIRA_OUTPUT_DIR":  outputDir,
		"GOJIRA_CONCURRENCY": "1",
		"GOJIRA_ISSUE_CAP":   "0",
	})
	require.NoError(t, err, "testConfig: LoadConfig")
	return cfg
}

// noSleepOpt returns a client.Option that replaces the sleep function with a
// no-op so retry backoffs do not slow down tests.
func noSleepOpt() client.Option {
	return client.WithRateLimitBackoff(0, 0)
}

// ---------------------------------------------------------------------------
// AC 1 — Classify
// ---------------------------------------------------------------------------

// TestAC01_Classify verifies that the facade Classify function correctly
// wraps classify.Classify and returns the expected Kind for all four input
// shapes defined in PRD AC 1.
func TestAC01_Classify(t *testing.T) {
	const site = "https://mycompany.atlassian.net"

	tests := []struct {
		name     string
		input    string
		wantKind classify.Kind
	}{
		{
			name:     "bare Jira issue key",
			input:    "EXAMPLE-1",
			wantKind: classify.KindJiraKey,
		},
		{
			name:     "Jira issue URL",
			input:    "https://mycompany.atlassian.net/browse/EXAMPLE-1",
			wantKind: classify.KindJiraURL,
		},
		{
			name:     "GitHub pull request URL",
			input:    "https://github.com/org/repo/pull/42",
			wantKind: classify.KindGitHubPR,
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
			assert.Equal(t, tt.wantKind, got.Kind,
				"Classify(%q, %q).Kind", tt.input, site)
		})
	}

	// Verify that the facade result is identical to calling classify.Classify
	// directly (re-export correctness).
	for _, tt := range tests {
		direct := classify.Classify(tt.input, site)
		facade := gojira.Classify(tt.input, site)
		assert.Equal(t, direct, facade,
			"facade result differs from direct classify.Classify for %q", tt.input)
	}
}

// ---------------------------------------------------------------------------
// AC 2 — Single issue render
// ---------------------------------------------------------------------------

// TestAC02_FetchAndRender verifies that FetchAndRender against an httptest
// server returning a fixture issue produces the expected Markdown content.
// PRD AC 2: indexMD contains # KEY heading; outboundMD matches reference list.
func TestAC02_FetchAndRender(t *testing.T) {
	outputDir := t.TempDir()

	// Build the server first so we know the site URL.
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
		body := minimalIssueJSON("EXAMPLE-1", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, outputDir)

	// Use a custom HTTP client pointing at the test server.
	hc := srv.Client()
	ctx := context.Background()

	indexMD, outboundMD, discoveredKeys, err := gojira.FetchAndRender(
		ctx, cfg, "EXAMPLE-1",
		client.WithHTTPClient(hc),
	)
	require.NoError(t, err, "FetchAndRender")

	// AC 2: indexMD must contain the # KEY — Summary heading.
	assert.Contains(t, indexMD, "# EXAMPLE-1", "indexMD missing '# EXAMPLE-1' heading")
	assert.Contains(t, indexMD, "Summary of EXAMPLE-1", "indexMD missing summary text")

	// Metadata section must be present.
	assert.Contains(t, indexMD, "## Metadata", "indexMD missing '## Metadata' section")

	// Description section must be present.
	assert.Contains(t, indexMD, "## Description", "indexMD missing '## Description' section")

	// This minimal issue has no outbound Jira links.
	assert.Empty(t, discoveredKeys, "discoveredKeys should be empty")

	// outboundMD is empty for an issue with no references.
	assert.Empty(t, outboundMD, "outboundMD should be empty string")

	// FetchAndRender must NOT write to disk.
	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	_, err = os.Stat(indexPath)
	assert.Error(t, err, "FetchAndRender must not write to disk at %s", indexPath)
}

// TestAC02_FetchAndRender_WithLinks verifies that discoveredKeys is populated
// when the issue has outbound Jira links.
func TestAC02_FetchAndRender_WithLinks(t *testing.T) {
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
		body := issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	_, _, discoveredKeys, err := gojira.FetchAndRender(
		ctx, cfg, "EXAMPLE-1",
		client.WithHTTPClient(srv.Client()),
	)
	require.NoError(t, err, "FetchAndRender")

	assert.NotEmpty(t, discoveredKeys, "discoveredKeys must not be empty; expected EXAMPLE-2")
	assert.Contains(t, discoveredKeys, "EXAMPLE-2", "discoveredKeys must contain EXAMPLE-2")
}

// ---------------------------------------------------------------------------
// AC 6 — Deduplication
// ---------------------------------------------------------------------------

// TestAC06_Deduplication verifies that a two-issue cycle (A↔B) results in
// exactly two fetches and no infinite loop. PRD AC 6.
func TestAC06_Deduplication(t *testing.T) {
	outputDir := t.TempDir()

	var fetchCounts [2]int64 // [0]=EXAMPLE-1, [1]=EXAMPLE-2

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
			atomic.AddInt64(&fetchCounts[0], 1)
			body = issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		case "EXAMPLE-2":
			atomic.AddInt64(&fetchCounts[1], 1)
			body = issueWithLinkJSON("EXAMPLE-2", "EXAMPLE-1", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, outputDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 6: each issue fetched exactly once.
	assert.Equal(t, 2, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, int64(1), atomic.LoadInt64(&fetchCounts[0]), "EXAMPLE-1 fetch count")
	assert.Equal(t, int64(1), atomic.LoadInt64(&fetchCounts[1]), "EXAMPLE-2 fetch count")

	// Both index.md files must exist.
	for _, key := range []string{"EXAMPLE-1", "EXAMPLE-2"} {
		p := filepath.Join(outputDir, key, "index.md")
		_, err := os.Stat(p)
		assert.NoError(t, err, "index.md must exist for %s", key)
	}
}

// ---------------------------------------------------------------------------
// AC 9 — Permission-denied stub
// ---------------------------------------------------------------------------

// TestAC09_PermissionDeniedStub verifies that when one issue returns 403,
// a stub index.md is written and Summary.Stubbed == 1. PRD AC 9.
func TestAC09_PermissionDeniedStub(t *testing.T) {
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
			// EXAMPLE-1 links to EXAMPLE-2 which will return 403.
			body := issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "EXAMPLE-2":
			// 403 — permission denied.
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, outputDir)

	ctx := context.Background()
	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl returned unexpected error")

	// AC 9: EXAMPLE-1 fetched, EXAMPLE-2 stubbed.
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, 1, sum.Stubbed, "Summary.Stubbed")

	// Stub file must exist for EXAMPLE-2.
	stubPath := filepath.Join(outputDir, "EXAMPLE-2", "index.md")
	content, err := os.ReadFile(stubPath)
	require.NoError(t, err, "stub index.md must be readable at %s", stubPath)

	// Stub must mention the key and the denial reason.
	assert.Contains(t, string(content), "EXAMPLE-2", "stub content must contain 'EXAMPLE-2'")
	assert.Contains(t, string(content), "Permission denied", "stub content must contain 'Permission denied'")
}

// ---------------------------------------------------------------------------
// AC 10 — Skip-if-exists
// ---------------------------------------------------------------------------

// TestAC10_SkipIfExists verifies that when index.md already exists on disk
// and cfg.Refetch is false, the fetcher is not invoked for that key.
// PRD AC 10.
func TestAC10_SkipIfExists(t *testing.T) {
	outputDir := t.TempDir()

	// Pre-create EXAMPLE-1/index.md.
	issueDir := filepath.Join(outputDir, "EXAMPLE-1")
	require.NoError(t, os.MkdirAll(issueDir, 0755), "MkdirAll")
	existingContent := "# EXAMPLE-1 — pre-existing\n"
	require.NoError(t, os.WriteFile(filepath.Join(issueDir, "index.md"), []byte(existingContent), 0644), "WriteFile")

	// Count how many times the server is called for EXAMPLE-1.
	var fetchCount int64

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
			atomic.AddInt64(&fetchCount, 1)
		}
		// Return a valid response anyway (should not be reached for EXAMPLE-1).
		body := minimalIssueJSON(key, r.Host)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, outputDir)
	// Refetch defaults to false — skip-if-exists is active.

	ctx := context.Background()
	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl")

	// AC 10: EXAMPLE-1 must be skipped, not fetched.
	assert.Equal(t, 1, sum.Skipped, "Summary.Skipped")
	assert.Equal(t, 0, sum.Fetched, "Summary.Fetched")

	// The fetcher must not have been called for EXAMPLE-1.
	assert.Equal(t, int64(0), atomic.LoadInt64(&fetchCount), "server must not be called for EXAMPLE-1")

	// The pre-existing file must not be overwritten.
	content, err := os.ReadFile(filepath.Join(issueDir, "index.md"))
	require.NoError(t, err, "ReadFile")
	assert.Equal(t, existingContent, string(content), "index.md must not be overwritten")
}

// ---------------------------------------------------------------------------
// Additional: LoadConfig error path
// ---------------------------------------------------------------------------

// TestLoadConfig_MissingRequired verifies that LoadConfig returns an error
// when a required key is absent.
func TestLoadConfig_MissingRequired(t *testing.T) {
	_, err := gojira.LoadConfig(map[string]string{
		// GOJIRA_SITE is intentionally missing.
		"GOJIRA_USER":       "me@example.com",
		"GOJIRA_TOKEN":      "tok",
		"GOJIRA_OUTPUT_DIR": "/tmp/out",
	})
	assert.Error(t, err, "LoadConfig: expected error for missing GOJIRA_SITE")
	assert.ErrorIs(t, err, gojira.ErrConfigMissingRequired,
		"LoadConfig must wrap the re-exported ErrConfigMissingRequired sentinel")
}

// ---------------------------------------------------------------------------
// Additional: LoadAppConfig — Phase 5 cascade entry point
// ---------------------------------------------------------------------------

// TestLoadAppConfig_CanonicalEnvEquivalentToLegacy asserts the new
// cascade entry point produces a Config equivalent to the legacy
// LoadConfig path when the inputs match. This pins the contract that
// LoadAppConfig is a drop-in upgrade rather than a behavior change.
func TestLoadAppConfig_CanonicalEnvEquivalentToLegacy(t *testing.T) {
	// Canonical Phase 0 env keys.
	env := map[string]string{
		"GOJIRA_JIRA_BASE_URL":   "https://example.atlassian.net",
		"GOJIRA_JIRA_EMAIL":      "me@example.com",
		"GOJIRA_JIRA_API_TOKEN":  "tok-123",
		"GOJIRA_OUTPUT_DIR":      "/tmp/out",
		"GOJIRA_CRAWL_ISSUE_CAP": "200",
		"GOJIRA_LOG_LEVEL":       "debug",
	}
	got, err := gojira.LoadAppConfig("", env)
	require.NoError(t, err)

	// Equivalent legacy invocation uses the v0.1 flat keys.
	legacy, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":       "https://example.atlassian.net",
		"GOJIRA_USER":       "me@example.com",
		"GOJIRA_TOKEN":      "tok-123",
		"GOJIRA_OUTPUT_DIR": "/tmp/out",
		"GOJIRA_ISSUE_CAP":  "200",
		"GOJIRA_LOG_LEVEL":  "debug",
	})
	require.NoError(t, err)

	// Field-by-field equivalence on the user-facing values.
	assert.Equal(t, legacy.Site, got.Site)
	assert.Equal(t, legacy.User, got.User)
	assert.Equal(t, legacy.Token, got.Token)
	assert.Equal(t, legacy.OutputDir, got.OutputDir)
	assert.Equal(t, legacy.IssueCap, got.IssueCap)
	assert.Equal(t, legacy.LogLevel, got.LogLevel)
}

// TestLoadAppConfig_DeprecatedAliasesWork asserts that supplying the
// v0.1 flat keys to the new cascade still loads a valid Config. This
// is the back-compat path the CLI relies on so existing shell
// configurations (GOJIRA_SITE / GOJIRA_USER / GOJIRA_TOKEN) keep
// working unchanged after the Phase 5 refactor.
func TestLoadAppConfig_DeprecatedAliasesWork(t *testing.T) {
	env := map[string]string{
		"GOJIRA_SITE":       "https://alias.atlassian.net",
		"GOJIRA_USER":       "alias@example.com",
		"GOJIRA_TOKEN":      "alias-tok",
		"GOJIRA_OUTPUT_DIR": "/tmp/aliased",
	}
	cfg, err := gojira.LoadAppConfig("", env)
	require.NoError(t, err)
	assert.Equal(t, "https://alias.atlassian.net", cfg.Site)
	assert.Equal(t, "alias@example.com", cfg.User)
	assert.Equal(t, "alias-tok", cfg.Token)
	assert.Equal(t, "/tmp/aliased", cfg.OutputDir)
}

// TestLoadAppConfig_MissingRequired asserts the cascade returns an
// error wrapping the re-exported ErrConfigMissingRequired sentinel
// when no input layer supplies a required field. This is the
// contract callers depend on for exit-code mapping.
func TestLoadAppConfig_MissingRequired(t *testing.T) {
	_, err := gojira.LoadAppConfig("", map[string]string{
		// No Jira credentials.
		"GOJIRA_OUTPUT_DIR": "/tmp/out",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, gojira.ErrConfigMissingRequired,
		"missing Jira creds must wrap the re-exported ErrConfigMissingRequired")
}

// TestLoadAppConfig_ExplicitButMissingConfigPathIsHardError asserts
// that an explicit configPath pointing at a non-existent file is a
// hard error wrapping the re-exported ErrConfigInvalidValue. This
// distinguishes "the user asked for that specific file and it's
// missing" (loud failure) from "no file was discovered" (fall
// through to env + defaults).
func TestLoadAppConfig_ExplicitButMissingConfigPathIsHardError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, err := gojira.LoadAppConfig(missing, map[string]string{
		"GOJIRA_JIRA_BASE_URL":  "https://x.atlassian.net",
		"GOJIRA_JIRA_EMAIL":     "x@example.com",
		"GOJIRA_JIRA_API_TOKEN": "tok",
		"GOJIRA_OUTPUT_DIR":     "/tmp/x",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, gojira.ErrConfigInvalidValue,
		"explicit-but-missing config path must wrap ErrConfigInvalidValue")
	assert.Contains(t, err.Error(), missing)
}

// ---------------------------------------------------------------------------
// Additional: Crawl with nil sink uses NoopSink (no panic)
// ---------------------------------------------------------------------------

// TestCrawl_NilSink verifies that passing nil as the sink does not panic.
func TestCrawl_NilSink(t *testing.T) {
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
		body := minimalIssueJSON(key, srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	// nil sink must not panic.
	sum, err := gojira.Crawl(ctx, cfg, []string{"EXAMPLE-1"}, nil)
	require.NoError(t, err, "Crawl with nil sink")
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")
}

// ---------------------------------------------------------------------------
// GetIssue — structured data (fetch/render split)
// ---------------------------------------------------------------------------

// TestGetIssue is a table-driven test for the GetIssue facade function.
// It covers: success with no links, success with outbound Jira links, fetch
// failure (non-200 response), and parse failure (malformed JSON).
func TestGetIssue(t *testing.T) {
	outputDir := t.TempDir()

	tests := []struct {
		name string
		// serverFn builds the httptest handler for this case.
		serverFn func(siteURL string) http.HandlerFunc
		// key is the issue key to request.
		key string
		// wantErr is true when GetIssue must return a non-nil error.
		wantErr bool
		// wantKey is the expected issue.Key on success.
		wantKey string
		// wantSummary is a substring expected in issue.Summary on success.
		wantSummary string
		// wantRefKeys are Jira issue keys expected in the returned refs.
		wantRefKeys []string
	}{
		{
			name: "success: minimal issue, no links",
			serverFn: func(siteURL string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					const prefix = "/rest/api/3/issue/"
					if !strings.HasPrefix(r.URL.Path, prefix) {
						http.NotFound(w, r)
						return
					}
					key := strings.TrimPrefix(r.URL.Path, prefix)
					if idx := strings.Index(key, "/"); idx >= 0 {
						key = key[:idx]
					}
					if key != "PROJ-1" {
						http.NotFound(w, r)
						return
					}
					body := minimalIssueJSON("PROJ-1", siteURL)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(body)
				}
			},
			key:         "PROJ-1",
			wantErr:     false,
			wantKey:     "PROJ-1",
			wantSummary: "Summary of PROJ-1",
			wantRefKeys: nil,
		},
		{
			name: "success: issue with outbound Jira link",
			serverFn: func(siteURL string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					const prefix = "/rest/api/3/issue/"
					if !strings.HasPrefix(r.URL.Path, prefix) {
						http.NotFound(w, r)
						return
					}
					key := strings.TrimPrefix(r.URL.Path, prefix)
					if idx := strings.Index(key, "/"); idx >= 0 {
						key = key[:idx]
					}
					if key != "PROJ-2" {
						http.NotFound(w, r)
						return
					}
					body := issueWithLinkJSON("PROJ-2", "PROJ-3", siteURL)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(body)
				}
			},
			key:         "PROJ-2",
			wantErr:     false,
			wantKey:     "PROJ-2",
			wantSummary: "Summary of PROJ-2",
			wantRefKeys: []string{"PROJ-3"},
		},
		{
			name: "fetch failure: server returns 404",
			serverFn: func(siteURL string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					http.NotFound(w, r)
				}
			},
			key:     "PROJ-99",
			wantErr: true,
		},
		{
			name: "parse failure: server returns malformed JSON",
			serverFn: func(siteURL string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					const prefix = "/rest/api/3/issue/"
					if !strings.HasPrefix(r.URL.Path, prefix) {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					// Deliberately malformed JSON.
					_, _ = w.Write([]byte(`{not valid json`))
				}
			},
			key:     "PROJ-BAD",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.serverFn(""))
			// Re-create handler with the real server URL now that srv exists.
			srv.Close()
			srv = httptest.NewServer(tt.serverFn(srv.URL))
			t.Cleanup(srv.Close)

			cfg := testConfig(t, srv.URL, outputDir)
			ctx := context.Background()

			issue, refs, err := gojira.GetIssue(ctx, cfg, tt.key,
				client.WithHTTPClient(srv.Client()),
			)

			if tt.wantErr {
				require.Error(t, err, "GetIssue must return an error for %q", tt.name)
				return
			}

			require.NoError(t, err, "GetIssue must not return an error for %q", tt.name)

			// Verify issue fields.
			assert.Equal(t, tt.wantKey, issue.Key, "issue.Key")
			assert.Contains(t, issue.Summary, tt.wantSummary, "issue.Summary")

			// Verify discovered Jira keys from refs.
			var gotRefKeys []string
			for _, r := range refs {
				if r.IssueKey != "" {
					gotRefKeys = append(gotRefKeys, r.IssueKey)
				}
			}
			if len(tt.wantRefKeys) == 0 {
				assert.Empty(t, gotRefKeys, "refs must be empty")
			} else {
				for _, wk := range tt.wantRefKeys {
					assert.Contains(t, gotRefKeys, wk, "refs must contain %q", wk)
				}
			}
		})
	}
}

// TestGetIssue_FetchAndRenderConsistency verifies that GetIssue and
// FetchAndRender agree on the parsed issue key and discovered Jira keys
// when given the same input. This pins the contract that FetchAndRender
// is a pure convenience wrapper over GetIssue.
func TestGetIssue_FetchAndRenderConsistency(t *testing.T) {
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
		if key != "CONSIST-1" {
			http.NotFound(w, r)
			return
		}
		body := issueWithLinkJSON("CONSIST-1", "CONSIST-2", srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, outputDir)
	ctx := context.Background()
	hc := client.WithHTTPClient(srv.Client())

	// Call GetIssue.
	issue, refs, err := gojira.GetIssue(ctx, cfg, "CONSIST-1", hc)
	require.NoError(t, err, "GetIssue")

	// Call FetchAndRender with the same inputs.
	_, _, discoveredKeys, err := gojira.FetchAndRender(ctx, cfg, "CONSIST-1", hc)
	require.NoError(t, err, "FetchAndRender")

	// issue.Key must match what FetchAndRender would have parsed.
	assert.Equal(t, "CONSIST-1", issue.Key)

	// Discovered keys from GetIssue refs must match FetchAndRender's discoveredKeys.
	var refKeys []string
	for _, r := range refs {
		if r.IssueKey != "" {
			refKeys = append(refKeys, r.IssueKey)
		}
	}
	assert.Equal(t, discoveredKeys, refKeys,
		"GetIssue refs and FetchAndRender discoveredKeys must agree")
}

// ---------------------------------------------------------------------------
// CrawlWithLogger — observability facade
// ---------------------------------------------------------------------------

// decodeJSONLogRecords parses each line of buf as a JSON object. Non-JSON
// lines (or empty lines) are skipped so the test is tolerant of mixed
// output streams.
func decodeJSONLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			// Skip any non-JSON noise rather than failing the test.
			continue
		}
		out = append(out, rec)
	}
	return out
}

// findFirstRecord returns the first record whose msg field equals msg, or
// nil if none is found.
func findFirstRecord(recs []map[string]any, msg string) map[string]any {
	for _, r := range recs {
		if got, _ := r["msg"].(string); got == msg {
			return r
		}
	}
	return nil
}

// findAllRecords returns every record whose msg field equals msg.
func findAllRecords(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if got, _ := r["msg"].(string); got == msg {
			out = append(out, r)
		}
	}
	return out
}

// TestCrawlWithLogger_EmitsBothStreamAndResponseTraces is the end-to-end
// observability test for the facade. It proves that a single CrawlWithLogger
// invocation wires the supplied *slog.Logger through BOTH the crawl
// orchestrator (trace_stream=stream lines) AND the underlying HTTP client
// (trace_stream=response lines via the httplog round-tripper), and that
// both streams share the same run_id correlation attribute.
func TestCrawlWithLogger_EmitsBothStreamAndResponseTraces(t *testing.T) {
	outputDir := t.TempDir()

	// Stand up the fake Jira API using the existing helper.
	responses := map[string][]byte{
		"EX-1": minimalIssueJSON("EX-1", "https://example.atlassian.net"),
	}
	srv := issueServer(t, responses, nil)

	// Capture log output via a JSON handler at slog.LevelInfo. INFO is the
	// floor that emits crawl.start / crawl.end / crawl.measurement (stream)
	// and http.response (response).
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	lg := slog.New(handler)

	cfg := testConfig(t, srv.URL, outputDir)
	ctx := context.Background()

	sum, err := gojira.CrawlWithLogger(ctx, cfg, []string{"EX-1"}, nil, lg)
	require.NoError(t, err, "CrawlWithLogger")
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")

	recs := decodeJSONLogRecords(t, &buf)
	require.NotEmpty(t, recs, "expected captured log records, got 0")

	// Stream side: crawl.start emitted by the orchestrator.
	crawlStart := findFirstRecord(recs, "crawl.start")
	require.NotNil(t, crawlStart, "missing crawl.start record")
	assert.Equal(t, "stream", crawlStart["trace_stream"],
		"crawl.start.trace_stream")

	// Response side: at least one http.response emitted by the round-tripper.
	httpResp := findFirstRecord(recs, "http.response")
	require.NotNil(t, httpResp, "missing http.response record")
	assert.Equal(t, "response", httpResp["trace_stream"],
		"http.response.trace_stream")

	// Correlation: both streams must share the same run_id from the same
	// invocation.
	streamRunID, _ := crawlStart["run_id"].(string)
	responseRunID, _ := httpResp["run_id"].(string)
	require.NotEmpty(t, streamRunID, "crawl.start.run_id must be set")
	require.NotEmpty(t, responseRunID, "http.response.run_id must be set")
	assert.Equal(t, streamRunID, responseRunID,
		"stream and response runs must share run_id")

	// Measurement summary: one crawl.measurement INFO line at end of run
	// carrying call_counts and total_api_time_ms.
	measurements := findAllRecords(recs, "crawl.measurement")
	require.Len(t, measurements, 1,
		"expected exactly one crawl.measurement record")
	m := measurements[0]
	assert.Contains(t, m, "call_counts",
		"crawl.measurement must carry call_counts")
	assert.Contains(t, m, "total_api_time_ms",
		"crawl.measurement must carry total_api_time_ms")
}
