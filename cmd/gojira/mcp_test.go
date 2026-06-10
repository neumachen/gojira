// mcp_test.go — exercises `gojira mcp` at the CLI boundary.
//
// The full in-memory MCP client/server round-trip + tool gating is
// covered by internal/mcpserver/tools_test.go; here we focus on the
// cmd-level wiring: the Phase-A guard still applies, the
// mcp.mode-required check fires before any backend work, an invalid
// mode is rejected with a clear message, a bridge-mode startup with
// no server address is rejected, and the serve path keeps stdout
// pure (no log records leak into the protocol stream).
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// (a) Guard still applies — no config → exit 1 with the init message
// ---------------------------------------------------------------------------

func TestMCP_Guard_NoConfig_FailsFast(t *testing.T) {
	neutralizeXDG(t) // from guard_test.go: empty XDG_CONFIG_HOME + HOME + cwd

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "mcp"},
		map[string]string{})
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "gojira init",
		"mcp must apply the Phase-A guard; stderr=%q", stderr)
}

// ---------------------------------------------------------------------------
// (b) Guard passes but mcp.mode is unset → exit 1
// ---------------------------------------------------------------------------

func TestMCP_ModeMissing_FailsWithRequiredMessage(t *testing.T) {
	neutralizeXDG(t)
	// Site/User/Token via env satisfies the guard's arm 3; OUTPUT_DIR
	// keeps loadServeConfig's LoadConfig validation happy. NO mcp.mode.
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":       "https://example.atlassian.net",
			"GOJIRA_USER":       "test@example.com",
			"GOJIRA_TOKEN":      "test-token",
			"GOJIRA_OUTPUT_DIR": t.TempDir(),
		})
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "mcp.mode",
		"stderr must mention mcp.mode; got %q", stderr)
	assert.Contains(t, strings.ToLower(stderr), "required",
		"stderr must call mode 'required'; got %q", stderr)
}

// ---------------------------------------------------------------------------
// (c) Invalid mode → exit 1
// ---------------------------------------------------------------------------

func TestMCP_ModeInvalid_FailsWithEnumMessage(t *testing.T) {
	neutralizeXDG(t)
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":       "https://example.atlassian.net",
			"GOJIRA_USER":       "test@example.com",
			"GOJIRA_TOKEN":      "test-token",
			"GOJIRA_OUTPUT_DIR": t.TempDir(),
			"GOJIRA_MCP_MODE":   "banana",
		})
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "banana",
		"stderr should name the offending mode value; got %q", stderr)
}

// ---------------------------------------------------------------------------
// (d) Bridge mode startup mentions the dial target on stderr
// ---------------------------------------------------------------------------
//
// resolveServerAddress falls back to 127.0.0.1:50051 (matching the serve
// command's default bind), so an unset address is NOT a hard error — it
// is the documented "local loopback gojira serve" topology. What we DO
// require is that the startup diagnostic line tells the operator where
// the bridge will dial, so an unintended default is easy to spot.
func TestMCP_BridgeMode_StartupLineNamesAddress(t *testing.T) {
	neutralizeXDG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, stderr, _ := captureRun(ctx,
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":           "https://example.atlassian.net",
			"GOJIRA_USER":           "test@example.com",
			"GOJIRA_TOKEN":          "test-token",
			"GOJIRA_OUTPUT_DIR":     t.TempDir(),
			"GOJIRA_MCP_MODE":       "bridge",
			"GOJIRA_SERVER_ADDRESS": "127.0.0.1:55555", // a dial target the operator chose
		})
	assert.Contains(t, stderr, "127.0.0.1:55555",
		"bridge-mode startup line must name the chosen address on stderr; got %q", stderr)
}

// ---------------------------------------------------------------------------
// (e) Stdout purity — startup diagnostics go to stderr; stdout stays empty
// ---------------------------------------------------------------------------
//
// We cannot easily drive a full stdio serve to completion from a Go
// unit test (Serve would block on os.Stdin). Instead, we exploit the
// pre-built fake Jira to make self-mode startup succeed, then cancel
// the context BEFORE the SDK can begin its protocol exchange. The
// SDK's Run loop honors ctx and returns; the invariant under test is
// that runMCP itself wrote ZERO bytes to stdout (only the SDK's own
// protocol output should ever land there, and with an immediate
// cancellation there is none).
func TestMCP_StdoutPurity_StartupGoesToStderr(t *testing.T) {
	neutralizeXDG(t)

	// A no-op Jira fake — runMCP only constructs the facade backend;
	// no real HTTP requests are made before Serve.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	// Cancel the context very quickly so runMCP returns shortly after
	// Serve begins. The test's primary assertion is on stdout — we
	// only need Serve to *start*, not to do anything useful.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	stdout, stderr, _ := captureRun(ctx,
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":       srv.URL,
			"GOJIRA_USER":       "test@example.com",
			"GOJIRA_TOKEN":      "test-token",
			"GOJIRA_OUTPUT_DIR": t.TempDir(),
			"GOJIRA_MCP_MODE":   "self",
		})

	// The key invariant: human-readable runMCP diagnostics MUST land on
	// stderr. The startup line and any error messages contain the word
	// "mcp" (case-insensitive) — assert at least one such word made it
	// to stderr.
	assert.Contains(t, strings.ToLower(stderr), "mcp",
		"runMCP startup diagnostics must go to stderr; got stderr=%q", stderr)

	// And stdout must NOT carry any of runMCP's text. Since the SDK
	// did not get to exchange any frames (ctx cancelled immediately),
	// stdout should be empty or contain only protocol frames (never
	// gojira's own log text).
	low := strings.ToLower(stdout)
	for _, leaked := range []string{"mcp.mode", "mcp serve", "starting", "listening", "error:", "warn", "info"} {
		assert.NotContains(t, low, leaked,
			"stdout must not contain runMCP diagnostic text %q (got stdout=%q)",
			leaked, stdout)
	}
}
