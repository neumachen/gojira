// Tests for the gojira get subcommand.
//
// The get command fetches a single Jira issue (no crawl) and prints
// either Markdown or JSON to stdout. These tests exercise the happy
// and error paths, proving non-crawl behavior and format selection.
package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test 1: Markdown output to stdout
// ---------------------------------------------------------------------------

// TestGet_Markdown_PrintsIssueToStdout verifies that `gojira get EXAMPLE-1`
// fetches the issue and prints Markdown to stdout (exit 0).
func TestGet_Markdown_PrintsIssueToStdout(t *testing.T) {
	srv := newIssueServer(t, map[string][]byte{
		"EXAMPLE-1": minimalIssueJSON("EXAMPLE-1", "https://test"),
	}, nil)

	// No GOJIRA_OUTPUT_DIR; also disable hierarchy/devstatus to avoid
	// extra API calls (those endpoints aren't mocked).
	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "EXAMPLE-1"}, env)

	assert.Equal(t, 0, code, "expected exit 0; stderr=%s", stderr)
	assert.Contains(t, stdout, "EXAMPLE-1", "stdout must contain the issue key")
	assert.Contains(t, stdout, "Summary of EXAMPLE-1", "stdout must contain the summary")
	assert.Contains(t, stdout, "# ", "stdout must look like Markdown (contains heading)")
}

// ---------------------------------------------------------------------------
// Test 2: Non-crawl behavior (linked issue is NOT fetched)
// ---------------------------------------------------------------------------

// TestGet_DoesNotFetchLinkedIssue verifies that `gojira get` fetches only
// the requested issue — linked issues are discovered but NOT fetched.
func TestGet_DoesNotFetchLinkedIssue(t *testing.T) {
	// Track which keys were requested from the server.
	var requested sync.Map

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
		requested.Store(key, true)

		switch key {
		case "EXAMPLE-1":
			body := issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", r.Host)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "EXAMPLE-2":
			// If EXAMPLE-2 is requested, the test FAILS (get must NOT crawl)
			body := minimalIssueJSON("EXAMPLE-2", r.Host)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "EXAMPLE-1"}, env)

	assert.Equal(t, 0, code, "expected exit 0; stderr=%s", stderr)
	assert.Contains(t, stdout, "EXAMPLE-1", "stdout must contain the requested issue")

	// EXAMPLE-1 must have been requested.
	_, ok := requested.Load("EXAMPLE-1")
	require.True(t, ok, "EXAMPLE-1 must have been requested")

	// EXAMPLE-2 must NOT have been requested (non-crawl behavior).
	_, fetched := requested.Load("EXAMPLE-2")
	assert.False(t, fetched, "EXAMPLE-2 must NOT be fetched by get (non-crawl)")
}

// ---------------------------------------------------------------------------
// Test 3: JSON format output
// ---------------------------------------------------------------------------

// TestGet_JSONFormat_EmitsIssueAndReferences verifies that --format json
// emits valid JSON with "issue" and "references" keys.
func TestGet_JSONFormat_EmitsIssueAndReferences(t *testing.T) {
	srv := newIssueServer(t, map[string][]byte{
		"EXAMPLE-1": issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", "https://test"),
		"EXAMPLE-2": minimalIssueJSON("EXAMPLE-2", "https://test"),
	}, nil)

	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "--format", "json", "EXAMPLE-1"}, env)

	assert.Equal(t, 0, code, "expected exit 0; stderr=%s", stderr)
	assert.True(t, json.Valid([]byte(stdout)), "stdout must be valid JSON; got:\n%s", stdout)
	assert.Contains(t, stdout, `"issue"`, "JSON must contain issue key")
	assert.Contains(t, stdout, `"references"`, "JSON must contain references key")
	assert.Contains(t, stdout, "EXAMPLE-1", "JSON must contain the issue key")
}

// ---------------------------------------------------------------------------
// Test 4: Invalid format → exit 1
// ---------------------------------------------------------------------------

// TestGet_InvalidFormat_Exit1 verifies that an invalid --format value
// results in exit code 1 and an error message on stderr.
func TestGet_InvalidFormat_Exit1(t *testing.T) {
	// Minimal server; not actually called because the format error is
	// detected before the fetch.
	srv := newIssueServer(t, map[string][]byte{
		"EXAMPLE-1": minimalIssueJSON("EXAMPLE-1", "https://test"),
	}, nil)

	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "--format", "xml", "EXAMPLE-1"}, env)

	assert.Equal(t, 1, code, "invalid format: expected exit 1")
	assert.Contains(t, strings.ToLower(stderr), "format", "stderr must mention the bad format")
}

// ---------------------------------------------------------------------------
// Test 5: No output dir required
// ---------------------------------------------------------------------------

// TestGet_NoOutputDirRequired verifies that get works without
// GOJIRA_OUTPUT_DIR (it prints to stdout, not disk).
func TestGet_NoOutputDirRequired(t *testing.T) {
	srv := newIssueServer(t, map[string][]byte{
		"EXAMPLE-1": minimalIssueJSON("EXAMPLE-1", "https://test"),
	}, nil)

	// No GOJIRA_OUTPUT_DIR in the env.
	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "EXAMPLE-1"}, env)

	assert.Equal(t, 0, code, "expected exit 0 without output-dir; stderr=%s", stderr)
	assert.Contains(t, stdout, "EXAMPLE-1", "stdout must contain the issue key")
}

// ---------------------------------------------------------------------------
// Test 6: get writes no files
// ---------------------------------------------------------------------------

// TestGet_WritesNoFiles verifies that get never writes files to disk,
// even when GOJIRA_OUTPUT_DIR is set.
func TestGet_WritesNoFiles(t *testing.T) {
	outputDir := t.TempDir()

	srv := newIssueServer(t, map[string][]byte{
		"EXAMPLE-1": minimalIssueJSON("EXAMPLE-1", "https://test"),
	}, nil)

	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         outputDir,
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "EXAMPLE-1"}, env)

	require.Equal(t, 0, code, "expected exit 0; stderr=%s", stderr)
	require.Contains(t, stdout, "EXAMPLE-1", "stdout must contain the issue key")

	// Walk the output directory and assert no index.md exists.
	var found []string
	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "index.md" {
			found = append(found, path)
		}
		return nil
	})
	require.NoError(t, err, "walking output dir")
	assert.Empty(t, found, "get must not write any index.md files; found: %v", found)
}

// ---------------------------------------------------------------------------
// Test 7: structured format rejected
// ---------------------------------------------------------------------------

// TestGet_StructuredFormat_Rejected verifies that --format structured is
// rejected (only markdown/json make sense to print to stdout).
func TestGet_StructuredFormat_Rejected(t *testing.T) {
	srv := newIssueServer(t, map[string][]byte{
		"EXAMPLE-1": minimalIssueJSON("EXAMPLE-1", "https://test"),
	}, nil)

	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "--format", "structured", "EXAMPLE-1"}, env)

	assert.Equal(t, 1, code, "structured format: expected exit 1")
	assert.Contains(t, strings.ToLower(stderr), "markdown", "stderr must suggest markdown")
	assert.Contains(t, strings.ToLower(stderr), "json", "stderr must suggest json")
}

// ---------------------------------------------------------------------------
// Test 8: Fetch error → exit 1
// ---------------------------------------------------------------------------

// TestGet_FetchError_Exit1 verifies that a fetch error (e.g. 404) results
// in exit 1 and an error on stderr.
func TestGet_FetchError_Exit1(t *testing.T) {
	// Server returns 404 for EXAMPLE-1.
	srv := newIssueServer(t, map[string][]byte{}, map[string]int{
		"EXAMPLE-1": http.StatusNotFound,
	})

	env := map[string]string{
		"GOJIRA_SITE":               srv.URL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "get", "EXAMPLE-1"}, env)

	assert.Equal(t, 1, code, "fetch error: expected exit 1")
	assert.Contains(t, strings.ToLower(stderr), "error", "stderr must contain error message")
}
