// Package main — CLI-level acceptance tests for PRD §13 AC 16, 17, and 18.
//
// These tests exercise the run() entry point directly (package main) so they
// can call unexported helpers. They correspond to the three CLI-level
// acceptance criteria:
//
//   - AC 16: Exit code 1 on total failure (401 response).
//   - AC 17: Exit code 0 on full success.
//   - AC 18: Run summary written to stderr.
//
// All tests use httptest.Server (no live network) and t.TempDir() for output.
package main

import (
	"bytes"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers shared by CLI acceptance tests
// ---------------------------------------------------------------------------

// acMinimalJSON returns a minimal valid Jira issue JSON for key.
// site is used in the "self" URL only.
func acMinimalJSON(key, site string) []byte {
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

// acLinkedJSON returns a Jira issue JSON for key that has an outward issue
// link to linkedKey.
func acLinkedJSON(key, linkedKey, site string) []byte {
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

// acIssueServer starts an httptest.Server that routes GET
// /rest/api/3/issue/<KEY> requests.
// responses maps issue key → raw JSON bytes (200 OK).
// statusOverrides maps issue key → HTTP status code (body is empty).
func acIssueServer(
	t *testing.T,
	responses map[string][]byte,
	statusOverrides map[string]int,
) *httptest.Server {
	t.Helper()
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

// acBaseEnv returns a minimal env map pointing at srvURL and outputDir.
//
// GOJIRA_INCLUDE_CHILDREN and GOJIRA_INCLUDE_DEV_STATUS are explicitly
// disabled so legacy CLI acceptance tests are not affected by the
// hierarchy-discovery or dev-status defaults. Tests that exercise either
// feature override the relevant key directly.
func acBaseEnv(srvURL, outputDir string) map[string]string {
	return map[string]string{
		"GOJIRA_SITE":               srvURL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         outputDir,
		"GOJIRA_CONCURRENCY":        "1",
		"GOJIRA_ISSUE_CAP":          "0",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	}
}

// acCaptureRun calls run() and returns stdout, stderr strings and the exit code.
func acCaptureRun(ctx context.Context, args []string, env map[string]string) (stdout, stderr string, code int) {
	var outBuf, errBuf bytes.Buffer
	code = run(ctx, args, &outBuf, &errBuf, env)
	return outBuf.String(), errBuf.String(), code
}

// ---------------------------------------------------------------------------
// AC 16 — Exit code on total failure
// ---------------------------------------------------------------------------

// TestAC16_ExitCodeOnTotalFailure verifies that when the Jira API returns 401
// for the first request, the CLI exits with code 1 and no index.md files are
// written.
//
// PRD AC 16: invalid API token (401) → exit 1; no index.md files written.
func TestAC16_ExitCodeOnTotalFailure(t *testing.T) {
	outputDir := t.TempDir()

	// Server returns 401 for every request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	env := acBaseEnv(srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, stderr, code := acCaptureRun(ctx,
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)

	// AC 16: exit code must be 1.
	assert.Equal(t, 1, code, "exit code (total failure on 401); stderr:\n%s", stderr)

	// AC 16: no index.md files must be written.
	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(path, "index.md") {
			t.Errorf("unexpected index.md written at %s (should not exist on 401)", path)
		}
		return nil
	})
	require.NoError(t, err, "Walk")
}

// ---------------------------------------------------------------------------
// AC 17 — Exit code on full success
// ---------------------------------------------------------------------------

// TestAC17_ExitCodeOnFullSuccess verifies that when all discovered issues are
// fetched successfully, the CLI exits with code 0.
//
// PRD AC 17: all issues fetched successfully → exit 0.
func TestAC17_ExitCodeOnFullSuccess(t *testing.T) {
	outputDir := t.TempDir()

	// Two-issue chain: EXAMPLE-1 → EXAMPLE-2. Both succeed.
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
			body = acLinkedJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		case "EXAMPLE-2":
			body = acMinimalJSON("EXAMPLE-2", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	env := acBaseEnv(srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, stderr, code := acCaptureRun(ctx,
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)

	// AC 17: exit code must be 0.
	assert.Equal(t, 0, code, "exit code (full success); stderr:\n%s", stderr)

	// Both index.md files must exist.
	for _, key := range []string{"EXAMPLE-1", "EXAMPLE-2"} {
		p := filepath.Join(outputDir, key, "index.md")
		_, err := os.Stat(p)
		assert.NoError(t, err, "index.md must exist for %s", key)
	}
}

// ---------------------------------------------------------------------------
// AC 18 — Run summary on stderr
// ---------------------------------------------------------------------------

// TestAC18_RunSummaryOnStderr verifies that after any crawl, the CLI writes a
// summary block to stderr containing counts of fetched, skipped, failed, and
// cap-limited issues.
//
// PRD AC 18: summary written to stderr with fetched/skipped/failed/cap-limited
// counts. The exact format matches printSummary in main.go.
func TestAC18_RunSummaryOnStderr(t *testing.T) {
	outputDir := t.TempDir()

	// Two-issue chain: EXAMPLE-1 → EXAMPLE-2. Both succeed.
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
			body = acLinkedJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		case "EXAMPLE-2":
			body = acMinimalJSON("EXAMPLE-2", srv.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	env := acBaseEnv(srv.URL, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, stderr, _ := acCaptureRun(ctx,
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)

	// AC 18: summary block must be present on stderr.
	// The format is defined by printSummary in main.go:
	//   === gojira crawl summary ===
	//   fetched:     N
	//   skipped:     N
	//   stubbed:     N
	//   failed:      N
	//   cap-limited: N
	//   pr-refs:     N
	//   duration:    N.NNN s
	//   ============================
	assert.Contains(t, stderr, "gojira crawl summary", "stderr must contain summary header")
	assert.Contains(t, stderr, "fetched:", "stderr must contain 'fetched:'")
	assert.Contains(t, stderr, "skipped:", "stderr must contain 'skipped:'")
	assert.Contains(t, stderr, "failed:", "stderr must contain 'failed:'")
	assert.Contains(t, stderr, "cap-limited:", "stderr must contain 'cap-limited:'")

	// The fetched count must be 2 (EXAMPLE-1 and EXAMPLE-2).
	assert.Contains(t, stderr, "fetched:     2", "stderr must contain 'fetched:     2'")
}

// ---------------------------------------------------------------------------
// AC 19 — --log-format text|json selects the output format
// ---------------------------------------------------------------------------

// ac19SuccessServer starts an httptest.Server that serves a successful crawl
// for EXAMPLE-1 → EXAMPLE-2. Both issues return 200 with valid minimal JSON.
func ac19SuccessServer(t *testing.T) *httptest.Server {
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

		var body []byte
		switch key {
		case "EXAMPLE-1":
			body = acLinkedJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
		case "EXAMPLE-2":
			body = acMinimalJSON("EXAMPLE-2", srv.URL)
		default:
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

// hasTextEventLine returns true if stderr contains at least one slog text
// record. Such records always carry the "level=" token (e.g.
// "level=INFO"); the plain-text summary block does not.
func hasTextEventLine(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "level=") {
			return true
		}
	}
	return false
}

// jsonEventRecords extracts and decodes every line in stderr that looks
// like a slog JSON record (begins with '{'). The plain-text summary block
// is skipped because its lines never start with '{'.
func jsonEventRecords(t *testing.T, stderr string) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
			t.Errorf("expected JSON record but failed to decode: line=%q err=%v", trimmed, err)
			continue
		}
		out = append(out, rec)
	}
	return out
}

// TestAC19_LogFormatSelection covers PRD AC 19: --log-format text|json
// selects the slog output format; "text" is the default.
//
// Subtests:
//   - "text default": no --log-format flag → text records on stderr.
//   - "explicit text": --log-format=text → text records on stderr.
//   - "json": --log-format=json → at least one JSON-decodable record with
//     non-empty msg + level fields on stderr.
//   - "invalid": --log-format=yaml → exit 1 (config validation failure)
//     and stderr mentions GOJIRA_LOG_FORMAT.
func TestAC19_LogFormatSelection(t *testing.T) {
	t.Run("text default", func(t *testing.T) {
		outputDir := t.TempDir()
		srv := ac19SuccessServer(t)

		env := acBaseEnv(srv.URL, outputDir)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		_, stderr, code := acCaptureRun(ctx,
			[]string{"gojira", "crawl", "EXAMPLE-1"}, env)
		require.Equal(t, 0, code, "expected exit 0; stderr:\n%s", stderr)

		assert.True(t, hasTextEventLine(stderr),
			"expected at least one slog text record (containing 'level=') on stderr:\n%s", stderr)
	})

	t.Run("explicit text", func(t *testing.T) {
		outputDir := t.TempDir()
		srv := ac19SuccessServer(t)

		env := acBaseEnv(srv.URL, outputDir)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		_, stderr, code := acCaptureRun(ctx,
			[]string{"gojira", "crawl", "--log-format", "text", "EXAMPLE-1"}, env)
		require.Equal(t, 0, code, "expected exit 0; stderr:\n%s", stderr)

		assert.True(t, hasTextEventLine(stderr),
			"expected at least one slog text record (containing 'level=') on stderr:\n%s", stderr)
	})

	t.Run("json", func(t *testing.T) {
		outputDir := t.TempDir()
		srv := ac19SuccessServer(t)

		env := acBaseEnv(srv.URL, outputDir)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		_, stderr, code := acCaptureRun(ctx,
			[]string{"gojira", "crawl", "--log-format", "json", "EXAMPLE-1"}, env)
		require.Equal(t, 0, code, "expected exit 0; stderr:\n%s", stderr)

		records := jsonEventRecords(t, stderr)
		require.NotEmpty(t, records,
			"expected at least one JSON event line on stderr:\n%s", stderr)

		var seen bool
		for _, rec := range records {
			msg, msgOK := rec["msg"].(string)
			lvl, lvlOK := rec["level"].(string)
			if msgOK && lvlOK && msg != "" && lvl != "" {
				seen = true
				break
			}
		}
		assert.True(t, seen,
			"expected at least one JSON record with non-empty msg and level fields; records=%v", records)
	})

	t.Run("invalid", func(t *testing.T) {
		outputDir := t.TempDir()
		// Server is irrelevant; config validation must fail before any crawl.
		srv := ac19SuccessServer(t)

		env := acBaseEnv(srv.URL, outputDir)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, stderr, code := acCaptureRun(ctx,
			[]string{"gojira", "crawl", "--log-format", "yaml", "EXAMPLE-1"}, env)
		assert.Equal(t, 1, code, "expected exit 1 on invalid log format; stderr:\n%s", stderr)

		// The error message must identify the offending key so the
		// operator can locate the misconfiguration. Either the env-var
		// name or the flag name is acceptable.
		lower := strings.ToLower(stderr)
		matched := strings.Contains(stderr, "GOJIRA_LOG_FORMAT") ||
			strings.Contains(lower, "log-format") ||
			strings.Contains(lower, "log_format")
		assert.True(t, matched,
			"expected stderr to mention GOJIRA_LOG_FORMAT or log-format; stderr:\n%s", stderr)
	})
}
