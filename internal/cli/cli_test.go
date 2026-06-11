// Package main tests the gojira CLI binary entry point.
//
// All tests use httptest.Server (no live network), capture stdout/stderr with
// bytes.Buffer, and verify behavior through the run() function. No files
// outside t.TempDir() are written.
package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gojira "github.com/neumachen/gojira"
	gojiralog "github.com/neumachen/gojira/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// minimalIssueJSON returns a minimal valid Jira issue JSON for key.
// site is used in the "self" URL only.
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

// newIssueServer starts an httptest.Server that serves Jira issue JSON.
// responses maps issue key → raw JSON bytes. statusOverrides maps issue key →
// HTTP status code (e.g. 403). Unknown keys return 404.
func newIssueServer(t *testing.T, responses map[string][]byte, statusOverrides map[string]int) *httptest.Server {
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

// baseEnv returns a minimal env map pointing at srvURL and outputDir.
func baseEnv(srvURL, outputDir string) map[string]string {
	return map[string]string{
		"GOJIRA_SITE":        srvURL,
		"GOJIRA_USER":        "test@example.com",
		"GOJIRA_TOKEN":       "test-token",
		"GOJIRA_OUTPUT_DIR":  outputDir,
		"GOJIRA_CONCURRENCY": "1",
		"GOJIRA_ISSUE_CAP":   "0",
	}
}

// captureRun calls run() and returns stdout, stderr strings and the exit code.
func captureRun(ctx context.Context, args []string, env map[string]string) (stdout, stderr string, code int) {
	var outBuf, errBuf bytes.Buffer
	code = run(ctx, args, &outBuf, &errBuf, env)
	return outBuf.String(), errBuf.String(), code
}

// ---------------------------------------------------------------------------
// Test 1: No arguments → non-zero exit, usage on stderr
// ---------------------------------------------------------------------------

func TestRun_NoArgs(t *testing.T) {
	stdout, stderr, code := captureRun(context.Background(), []string{"gojira"}, nil)
	assert.NotEqual(t, 0, code, "expected non-zero exit code")
	combined := strings.ToLower(stdout + stderr)
	if !strings.Contains(combined, "usage") && !strings.Contains(combined, "gojira crawl") {
		t.Errorf("expected usage text in output, got stdout=%q stderr=%q", stdout, stderr)
	}
}

// ---------------------------------------------------------------------------
// Test 2: --help → exit 0, stdout contains "gojira crawl"
// ---------------------------------------------------------------------------

func TestRun_HelpFlag(t *testing.T) {
	stdout, _, code := captureRun(context.Background(), []string{"gojira", "--help"}, nil)
	assert.Equal(t, 0, code, "expected exit 0")
	assert.Contains(t, stdout, "gojira crawl", "expected 'gojira crawl' in stdout")
}

// ---------------------------------------------------------------------------
// Test 3: --version → exit 0, stdout contains "v0.1.0"
// ---------------------------------------------------------------------------

func TestRun_Version(t *testing.T) {
	stdout, _, code := captureRun(context.Background(), []string{"gojira", "--version"}, nil)
	assert.Equal(t, 0, code, "expected exit 0")
	assert.Contains(t, stdout, "v0.1.0", "expected 'v0.1.0' in stdout")
}

// ---------------------------------------------------------------------------
// Test 4: Unknown subcommand → non-zero exit, stderr contains "unknown"
// ---------------------------------------------------------------------------

func TestRun_UnknownSubcommand(t *testing.T) {
	_, stderr, code := captureRun(context.Background(), []string{"gojira", "foo"}, nil)
	assert.NotEqual(t, 0, code, "expected non-zero exit code")
	assert.Contains(t, strings.ToLower(stderr), "unknown", "expected 'unknown' in stderr")
}

// ---------------------------------------------------------------------------
// Test 5: Missing required flag (--site) → exit 1, stderr describes missing key
// ---------------------------------------------------------------------------

func TestRun_MissingRequired(t *testing.T) {
	// Provide user/token/output-dir but NOT site.
	env := map[string]string{
		"GOJIRA_USER":       "test@example.com",
		"GOJIRA_TOKEN":      "test-token",
		"GOJIRA_OUTPUT_DIR": t.TempDir(),
	}
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)
	assert.Equal(t, 1, code, "expected exit 1")
	assert.Contains(t, stderr, "GOJIRA_SITE", "expected 'GOJIRA_SITE' in stderr")
}

// ---------------------------------------------------------------------------
// Test 6: Env fallback — required values via env map, no flags
// ---------------------------------------------------------------------------

func TestRun_EnvFallback(t *testing.T) {
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

	env := baseEnv(srv.URL, outputDir)
	_, _, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)
	assert.Equal(t, 0, code, "expected exit 0")

	// Verify the output file was written.
	indexPath := filepath.Join(outputDir, "EXAMPLE-1", "index.md")
	_, err := os.Stat(indexPath)
	assert.NoError(t, err, "expected output file at %s", indexPath)
}

// ---------------------------------------------------------------------------
// Test 7: Flag overrides env — --output-dir flag wins over GOJIRA_OUTPUT_DIR env
// ---------------------------------------------------------------------------

func TestRun_FlagOverridesEnv(t *testing.T) {
	envDir := t.TempDir()
	flagDir := t.TempDir()

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

	// env has envDir; flag has flagDir — flag must win.
	env := baseEnv(srv.URL, envDir)
	_, _, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--output-dir", flagDir, "EXAMPLE-1"}, env)
	assert.Equal(t, 0, code, "expected exit 0")

	// Output must be in flagDir, not envDir.
	flagPath := filepath.Join(flagDir, "EXAMPLE-1", "index.md")
	_, err := os.Stat(flagPath)
	assert.NoError(t, err, "expected output in flagDir at %s", flagPath)
	envPath := filepath.Join(envDir, "EXAMPLE-1", "index.md")
	_, err = os.Stat(envPath)
	assert.Error(t, err, "output should not be written to envDir %s; flag should have overridden it", envPath)
}

// ---------------------------------------------------------------------------
// Test 8: Exit code mapping
// ---------------------------------------------------------------------------

// TestRun_ExitCode_AllSuccess verifies exit 0 when all issues succeed.
func TestRun_ExitCode_AllSuccess(t *testing.T) {
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

	env := baseEnv(srv.URL, outputDir)
	_, _, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)
	assert.Equal(t, 0, code, "all-success: expected exit 0")
}

// TestRun_ExitCode_AuthFailure verifies exit 1 when the server returns 401.
func TestRun_ExitCode_AuthFailure(t *testing.T) {
	outputDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	env := baseEnv(srv.URL, outputDir)
	_, _, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)
	assert.Equal(t, 1, code, "auth failure: expected exit 1")
}

// TestRun_ExitCode_PartialSuccess verifies exit 2 when some issues succeed
// and some are cap-limited.
//
// We use a two-issue chain: EXAMPLE-1 links to EXAMPLE-2. We set
// GOJIRA_ISSUE_CAP=1 so EXAMPLE-1 is fetched but EXAMPLE-2 is cap-limited.
// Cap-limited issues degrade the exit code to 2.
func TestRun_ExitCode_PartialSuccess(t *testing.T) {
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
			body := issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "EXAMPLE-2":
			body := minimalIssueJSON("EXAMPLE-2", srv.URL)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// Cap at 1 issue so EXAMPLE-2 is cap-limited.
	env := baseEnv(srv.URL, outputDir)
	env["GOJIRA_ISSUE_CAP"] = "1"
	_, _, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)
	assert.Equal(t, 2, code, "partial success: expected exit 2")
}

// ---------------------------------------------------------------------------
// Test 9: Log level filtering
// ---------------------------------------------------------------------------

// TestRun_LogLevelFiltering verifies that --log-level error suppresses
// KindIssueFetched events but still prints KindIssueFailed events.
//
// EXAMPLE-1 is fetched successfully (→ KindIssueFetched, INFO).
// EXAMPLE-2 returns 200 with invalid JSON (→ parse error → KindIssueFailed,
// ERROR).
//
// After the slog wiring (commit T) the CLI logs in slog's text format —
// records carry "level=INFO" or "level=ERROR" rather than the legacy
// "[FETCHED]"/"[FAILED]" bracket prefixes. The filter intent is unchanged:
// at log-level=error, INFO-level records (fetched/queued/skipped/summary)
// must be dropped and ERROR-level records (failed) must remain.
func TestRun_LogLevelFiltering(t *testing.T) {
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
			body := issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", srv.URL)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "EXAMPLE-2":
			// Return 200 with invalid JSON → parse error → KindIssueFailed.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{invalid json`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	env := baseEnv(srv.URL, outputDir)
	_, stderr, _ := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--log-level", "error", "EXAMPLE-1"}, env)

	// Pull out the slog event lines (everything that is not part of the
	// plain-text summary block). The summary header is "=== gojira crawl
	// summary ===" followed by key/value lines and a closing "===" line;
	// slog text records always carry a "level=" token.
	var eventLines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "level=") {
			eventLines = append(eventLines, line)
		}
	}

	// At log-level=error, INFO-level records (issue.fetched, issue.queued,
	// issue.skipped, crawl.summary) must be suppressed by slog.
	assert.NotContains(t, stderr, "level=INFO", "log-level=error: unexpected INFO records in stderr:\n%s", stderr)

	// ERROR-level records (issue.failed) must still appear.
	assert.Contains(t, stderr, "level=ERROR", "log-level=error: expected an ERROR-level record in stderr:\n%s", stderr)

	// Belt-and-braces: there must be at least one event line, and every
	// surviving event line must be ERROR-level.
	require.NotEmpty(t, eventLines, "expected at least one slog event line in stderr")
	for _, line := range eventLines {
		assert.Contains(t, line, "level=ERROR", "non-ERROR event leaked past filter: %q", line)
	}
}

// TestRun_LogFormatJSON verifies that --log-format=json switches the slog
// handler to JSON output: each event record is a single line of JSON with at
// minimum a "msg" and a "level" string field. The summary block is plain
// text and is intentionally ignored.
func TestRun_LogFormatJSON(t *testing.T) {
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

	env := baseEnv(srv.URL, outputDir)
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--log-format", "json", "EXAMPLE-1"}, env)
	require.Equal(t, 0, code, "expected exit 0; stderr:\n%s", stderr)

	// Scan stderr line by line; any line that begins with '{' is a
	// candidate slog JSON record. The summary block is plain text and
	// will not start with '{'.
	var jsonRecords []map[string]interface{}
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
			t.Errorf("expected JSON-shaped line but failed to decode: line=%q err=%v", trimmed, err)
			continue
		}
		jsonRecords = append(jsonRecords, rec)
	}

	require.NotEmpty(t, jsonRecords, "expected at least one JSON event line in stderr:\n%s", stderr)

	// At least one decoded record must have non-empty msg and level
	// string fields — that is the slog JSON contract.
	var seen bool
	for _, rec := range jsonRecords {
		msg, msgOK := rec["msg"].(string)
		lvl, lvlOK := rec["level"].(string)
		if msgOK && lvlOK && msg != "" && lvl != "" {
			seen = true
			break
		}
	}
	assert.True(t, seen, "expected at least one JSON record with non-empty msg and level fields; records=%v", jsonRecords)
}

// ---------------------------------------------------------------------------
// Test 10: Context cancellation → exit 2
// ---------------------------------------------------------------------------

// TestRun_ContextCancellation verifies that when the context is cancelled
// before the crawl completes, the CLI exits with code 2.
func TestRun_ContextCancellation(t *testing.T) {
	outputDir := t.TempDir()

	// Track how many requests have been received.
	var requestCount atomic.Int64

	// Slow server: each request takes 200ms.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}
		// Simulate a slow response.
		select {
		case <-r.Context().Done():
			// Client cancelled; return without writing.
			return
		case <-time.After(200 * time.Millisecond):
		}
		body := minimalIssueJSON(key, r.Host)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	// Cancel the context after 50ms — before the first response arrives.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	env := baseEnv(srv.URL, outputDir)
	_, stderr, code := captureRun(ctx,
		[]string{"gojira", "crawl", "EXAMPLE-1"}, env)

	assert.Equal(t, 2, code, "context cancellation: expected exit 2")
	// The summary must still be printed.
	assert.Contains(t, stderr, "gojira crawl summary", "expected summary in stderr")
}

// ---------------------------------------------------------------------------
// Verify JSON round-trip for issue fixture (sanity check)
// ---------------------------------------------------------------------------

func TestMinimalIssueJSON_Valid(t *testing.T) {
	data := minimalIssueJSON("EXAMPLE-1", "https://example.atlassian.net")
	var v interface{}
	require.NoError(t, json.Unmarshal(data, &v), "minimalIssueJSON produced invalid JSON")
}

func TestIssueWithLinkJSON_Valid(t *testing.T) {
	data := issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2", "https://example.atlassian.net")
	var v interface{}
	require.NoError(t, json.Unmarshal(data, &v), "issueWithLinkJSON produced invalid JSON")
}

// ---------------------------------------------------------------------------
// Phase phase-a-levels-2: --log-level trace acceptance + round-trip
// ---------------------------------------------------------------------------

// TestRun_LogLevelTrace_Accepted exercises the full CLI cascade with
// --log-level trace and asserts that the value is accepted by both the
// config validator and the slog wiring — i.e. the run completes
// normally (does not error out with "invalid log level") and the slog
// output is shaped exactly as it is for the existing levels. The
// previous --log-level=verbose path is still an error case; that
// validation is exercised at the config layer in
// internal/config/config_test.go.
func TestRun_LogLevelTrace_Accepted(t *testing.T) {
	outputDir := t.TempDir()
	srv := newIssueServer(t, map[string][]byte{
		"EXAMPLE-1": minimalIssueJSON("EXAMPLE-1", "https://test"),
	}, nil)
	t.Cleanup(srv.Close)

	env := baseEnv(srv.URL, outputDir)
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--log-level", "trace", "EXAMPLE-1"}, env)

	require.Equal(t, 0, code, "trace log-level run must exit 0 (got %d); stderr=%s", code, stderr)
	// stderr must not contain the validator's rejection message.
	require.NotContains(t, stderr, "invalid",
		"trace log-level must NOT be rejected; stderr=%s", stderr)
	require.NotContains(t, stderr, "must be one of",
		"trace log-level must NOT hit the oneof validator; stderr=%s", stderr)
}

// TestParseLevel_Trace_FromConfig confirms the wire-up at the parsing
// boundary: when the loaded Config carries LogLevel="trace",
// log.ParseLevel returns log.LevelTrace. This is the integration the
// runCrawl level-parse switch depends on; an inconsistency here would
// silently downgrade trace runs to a different level without the user
// noticing.
func TestParseLevel_Trace_FromConfig(t *testing.T) {
	cfg, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":       "https://example.atlassian.net",
		"GOJIRA_USER":       "test@example.com",
		"GOJIRA_TOKEN":      "test-token",
		"GOJIRA_OUTPUT_DIR": t.TempDir(),
		"GOJIRA_LOG_LEVEL":  "trace",
	})
	require.NoError(t, err, "LoadConfig must accept trace")
	require.Equal(t, "trace", cfg.LogLevel)

	lv, err := gojiralog.ParseLevel(cfg.LogLevel)
	require.NoError(t, err, "ParseLevel must accept trace from a validated Config")
	require.Equal(t, gojiralog.LevelTrace, lv,
		"trace must round-trip through Config + ParseLevel to log.LevelTrace")
}

// ---------------------------------------------------------------------------
// Phase phase-e-wire-2 + phase-g-verify-2: end-to-end CLI correlation +
// credential-redaction audit at the binary boundary
// ---------------------------------------------------------------------------

// TestRun_LogLevelTrace_EmitsCorrelatedTraces drives the full CLI cascade
// with --log-level trace --log-format json and verifies three guarantees of
// the observability instrument at the binary boundary:
//
//  1. Both trace streams are emitted: the HTTP client layer emits records
//     with trace_stream="response" (via the httplog round-tripper installed
//     by client.WithLogger), and the crawl orchestrator emits records with
//     trace_stream="stream" (per-issue / per-phase spans).
//  2. The two streams share the same run_id, proving end-to-end correlation
//     from the CLI flag through gojira.CrawlWithLogger down to both the
//     fetch and orchestration layers.
//  3. The end-of-run crawl.measurement INFO summary is emitted.
//
// The test also performs the binary-level credential-redaction audit
// required by AGENTS.md: even at trace level, the Authorization header
// value (base64 of "alice:secret-token-xyz") and the raw token must NEVER
// appear in stderr, and the "REDACTED" placeholder must appear at least
// once where the Authorization header would otherwise have been logged.
func TestRun_LogLevelTrace_EmitsCorrelatedTraces(t *testing.T) {
	outputDir := t.TempDir()

	// httptest server: returns a minimal valid Jira issue JSON for any
	// GET under /rest/api/3/issue/. The fixture is intentionally tiny —
	// the test asserts on log records, not on parsed Jira content.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"EX-1","self":"` + r.Host + `/rest/api/3/issue/EX-1","fields":{"summary":"S","status":{"name":"Open"},"issuetype":{"name":"Task"},"assignee":null,"reporter":{"displayName":"A","emailAddress":"a@e.com"},"created":"2026-01-01T00:00:00.000+0000","updated":"2026-01-01T00:00:00.000+0000","description":null,"parent":null,"subtasks":[],"issuelinks":[],"remotelinks":[]}}`))
	}))
	t.Cleanup(srv.Close)

	// Deliberate fixture credentials. "alice" + "secret-token-xyz" appear
	// nowhere else in the codebase, so any leak is unambiguous.
	env := map[string]string{
		"GOJIRA_SITE":        srv.URL,
		"GOJIRA_USER":        "alice",
		"GOJIRA_TOKEN":       "secret-token-xyz",
		"GOJIRA_OUTPUT_DIR":  outputDir,
		"GOJIRA_CONCURRENCY": "1",
		"GOJIRA_ISSUE_CAP":   "0",
	}

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--log-level", "trace", "--log-format", "json", "EX-1"}, env)
	require.Equal(t, 0, code, "trace+json run must exit 0; stderr:\n%s", stderr)

	// Decode every JSON-shaped line on stderr. Non-JSON noise (the
	// plain-text summary block, the "=== gojira crawl summary ===" banner,
	// etc.) is skipped.
	var records []map[string]interface{}
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
			// Tolerate the rare not-actually-JSON line that
			// happens to start with '{'; the assertions below
			// will catch genuine missing records.
			continue
		}
		records = append(records, rec)
	}
	require.NotEmpty(t, records, "expected JSON-shaped slog records on stderr:\n%s", stderr)

	// Find one record per required (msg, trace_stream) combination and
	// capture its run_id. The first match wins; subsequent occurrences
	// (there will be many) are not needed for correlation.
	var (
		runIDResponse  string
		runIDStream    string
		sawMeasurement bool
	)
	for _, rec := range records {
		msg, _ := rec["msg"].(string)
		stream, _ := rec["trace_stream"].(string)
		switch {
		case msg == "http.response" && stream == "response" && runIDResponse == "":
			runIDResponse, _ = rec["run_id"].(string)
		case msg == "issue.process.start" && stream == "stream" && runIDStream == "":
			runIDStream, _ = rec["run_id"].(string)
		case msg == "crawl.measurement":
			sawMeasurement = true
		}
	}

	require.NotEmpty(t, runIDResponse,
		"expected an http.response record with trace_stream=\"response\" and a non-empty run_id; records=%d, stderr:\n%s",
		len(records), stderr)
	require.NotEmpty(t, runIDStream,
		"expected an issue.process.start record with trace_stream=\"stream\" and a non-empty run_id; records=%d, stderr:\n%s",
		len(records), stderr)
	assert.True(t, sawMeasurement,
		"expected a crawl.measurement INFO record at end of run; stderr:\n%s", stderr)
	assert.Equal(t, runIDResponse, runIDStream,
		"response-stream and crawl-stream runs must share the same run_id (end-to-end correlation)")

	// Redaction audit. The Authorization header on outbound requests is
	// "Basic " + base64("alice:secret-token-xyz"). Even at --log-level
	// trace, NEITHER the base64-encoded credential NOR the raw token may
	// appear anywhere in the captured stderr — and the "REDACTED"
	// placeholder MUST appear at least once where the header value
	// would otherwise have been logged.
	expectedB64 := base64.StdEncoding.EncodeToString([]byte("alice:secret-token-xyz"))
	assert.NotContains(t, stderr, expectedB64,
		"redaction audit: base64-encoded Authorization credential leaked in stderr")
	assert.NotContains(t, stderr, "secret-token-xyz",
		"redaction audit: raw token leaked in stderr")
	assert.Contains(t, stderr, "REDACTED",
		"redaction audit: expected REDACTED placeholder somewhere in stderr (Authorization header should be replaced)")
}
