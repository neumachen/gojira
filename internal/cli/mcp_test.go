// mcp_test.go — exercises `gojira mcp` at the CLI boundary.
//
// The full in-memory MCP client/server round-trip + tool gating is
// covered by internal/mcp/tools_test.go; here we focus on the
// cmd-level wiring: the Phase-A guard still applies, the
// mcp.mode-required check fires before any backend work, an invalid
// mode is rejected with a clear message, a bridge-mode startup with
// no server address is rejected, and the serve path keeps stdout
// pure (no log records leak into the protocol stream).
package cli

import (
	"context"
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
	// internal/mcp.Serve writes the mode-required diagnostic to
	// os.Stderr (the package owns the message now). Capture both the
	// cli ErrWriter (via captureRun) and os.Stderr so the assertion
	// sees the full user-visible surface.
	stopCapture := captureOSStderr(t)
	// Site/User/Token via env satisfies the guard's arm 3; OUTPUT_DIR
	// keeps loadServeConfig's LoadConfig validation happy. NO mcp.mode.
	_, cmdStderr, code := captureRun(context.Background(),
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":       "https://example.atlassian.net",
			"GOJIRA_USER":       "test@example.com",
			"GOJIRA_TOKEN":      "test-token",
			"GOJIRA_OUTPUT_DIR": t.TempDir(),
		})
	stderr := cmdStderr + stopCapture()
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
	// internal/mcp.Serve writes the invalid-mode diagnostic to
	// os.Stderr; capture it alongside the cli ErrWriter.
	stopCapture := captureOSStderr(t)
	_, cmdStderr, code := captureRun(context.Background(),
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":       "https://example.atlassian.net",
			"GOJIRA_USER":       "test@example.com",
			"GOJIRA_TOKEN":      "test-token",
			"GOJIRA_OUTPUT_DIR": t.TempDir(),
			"GOJIRA_MCP_MODE":   "banana",
		})
	stderr := cmdStderr + stopCapture()
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "banana",
		"stderr should name the offending mode value; got %q", stderr)
}

// ---------------------------------------------------------------------------
// (d) Bridge mode startup mentions the dial target on stderr
// ---------------------------------------------------------------------------
//
// cfg.ServerAddress defaults to 127.0.0.1:50051 via the config cascade
// (matching the serve command's default bind), so an unset address is
// NOT a hard error — it is the documented "local loopback gojira serve"
// topology. What we DO require is that the startup diagnostic line tells
// the operator where the bridge will dial, so an unintended default is
// easy to spot.
func TestMCP_BridgeMode_StartupLineNamesAddress(t *testing.T) {
	neutralizeXDG(t)
	// The "gojira mcp starting" line lives inside internal/mcp.Serve
	// and goes to os.Stderr; capture it alongside the cli ErrWriter.
	stopCapture := captureOSStderr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, cmdStderr, _ := captureRun(ctx,
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":           "https://example.atlassian.net",
			"GOJIRA_USER":           "test@example.com",
			"GOJIRA_TOKEN":          "test-token",
			"GOJIRA_OUTPUT_DIR":     t.TempDir(),
			"GOJIRA_MCP_MODE":       "bridge",
			"GOJIRA_SERVER_ADDRESS": "127.0.0.1:55555", // a dial target the operator chose
		})
	stderr := cmdStderr + stopCapture()
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

	// internal/mcp.Serve emits the "gojira mcp starting" line on
	// os.Stderr; capture it so the assertion below can see it
	// alongside the cli ErrWriter.
	stopCapture := captureOSStderr(t)

	// Cancel the context very quickly so runMCP returns shortly after
	// Serve begins. The test's primary assertion is on stdout — we
	// only need Serve to *start*, not to do anything useful.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	stdout, cmdStderr, _ := captureRun(ctx,
		[]string{"gojira", "mcp"},
		map[string]string{
			"GOJIRA_SITE":       srv.URL,
			"GOJIRA_USER":       "test@example.com",
			"GOJIRA_TOKEN":      "test-token",
			"GOJIRA_OUTPUT_DIR": t.TempDir(),
			"GOJIRA_MCP_MODE":   "self",
		})
	stderr := cmdStderr + stopCapture()

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

// ---------------------------------------------------------------------------
// Regression: configToKV must carry mcp.mode / mcp.allow_writes from a
// file-supplied YAML config. Without the fix, the file-layer Config is
// re-flattened by configToKV which dropped these MCP keys, so a
// file-set "mcp.mode: self" came back empty and runMCP wrongly exited 1
// with the "mcp.mode is required" message.
//
// We drive the cmd-layer path end to end via captureRun with --config
// pointed at a temp YAML carrying the full mcp section, then cancel
// the context shortly after so Serve returns and the test does not
// block on stdin. The assertion is that the "mcp.mode is required"
// error does NOT appear in stderr — i.e. mode survived the flatten.
// ---------------------------------------------------------------------------

func TestMCP_ConfigFile_CarriesMCPMode_Regression(t *testing.T) {
	neutralizeXDG(t)
	outDir := t.TempDir()

	// Write a complete schema-valid config with mcp.mode: self.
	cfgPath := filepath.Join(t.TempDir(), "gojira.yaml")
	body := []byte(strings.Join([]string{
		"schema: gojira.config.v1",
		"jira:",
		"  base_url: https://example.atlassian.net",
		"  email: u@example.com",
		"  api_token: tok",
		"output:",
		"  dir: " + outDir,
		"mcp:",
		"  mode: self",
		"  allow_writes: true",
		"",
	}, "\n"))
	require.NoError(t, os.WriteFile(cfgPath, body, 0o600))

	// internal/mcp.Serve writes any mode-related diagnostic to
	// os.Stderr; capture both streams so the negative assertion
	// covers the package's diagnostic surface too.
	stopCapture := captureOSStderr(t)

	// 200ms cancel so Serve returns without blocking on stdin.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, cmdStderr, _ := captureRun(ctx,
		[]string{"gojira", "mcp", "--config", cfgPath},
		map[string]string{}) // no env — file must carry mode through the cascade
	stderr := cmdStderr + stopCapture()

	// The bug surfaced this exact string. After the fix, it must NOT
	// appear: mode was successfully read from the file and survived
	// the file<env<flag re-flatten through configToKV.
	assert.NotContains(t, stderr, "mcp.mode is required",
		"file-supplied mcp.mode must survive configToKV; stderr=%q", stderr)
}
