// guard_test.go — tests for the require-config pre-flight guard.
//
// The guard's 4-arm predicate is exercised against the real urfave/cli/v3
// machinery through run()/captureRun(); arm 4 (XDG/cwd discovery) is
// controlled hermetically via t.Setenv + t.Chdir so the resolver's
// process-level reads cannot leak across tests.
//
// No live network — every pass case is backed by the shared writeServer
// httptest fake from write_cmds_test.go.
package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// neutralizeXDG forces all real-environment paths the XDGResolver
// consults to point at empty tempdirs, so arm 4 of the guard can
// never accidentally resolve a stray config from the host. Tests
// that explicitly want arm 4 to fire override the relevant var
// AFTER calling this helper.
func neutralizeXDG(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	// GOJIRA_CONFIG_FILE could otherwise be inherited from the
	// developer's shell.
	t.Setenv("GOJIRA_CONFIG_FILE", "")
	t.Chdir(t.TempDir())
}

// writeMinimalConfigYAML writes a schema-valid gojira config YAML at
// path and returns the path. The contents only need to clear
// gojira.LoadFileConfig + LoadConfig validation — site/user/token
// non-empty, schema header, output dir.
func writeMinimalConfigYAML(t *testing.T, path, site string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := []byte(
		"schema: gojira.config.v1\n" +
			"jira:\n" +
			"  base_url: " + site + "\n" +
			"  email: test@example.com\n" +
			"  api_token: test-token\n" +
			"output:\n" +
			"  dir: " + t.TempDir() + "\n",
	)
	require.NoError(t, os.WriteFile(path, body, 0o600))
	return path
}

// guardFailureMessage is a substring every guard-failure stderr message
// must contain so it is recognisable to scripts and humans alike.
const guardFailureMessage = "gojira init"

// ---------------------------------------------------------------------------
// phase-1-guard-5: pass-arms
// ---------------------------------------------------------------------------

func TestGuard_Arm1_ConfigFlag_Allows(t *testing.T) {
	neutralizeXDG(t)
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{
		{"id": "11", "name": "Start", "to": map[string]any{"name": "In Progress"}},
	}

	// arm 1: --config <tempYAML>
	cfgPath := writeMinimalConfigYAML(t,
		filepath.Join(t.TempDir(), "gojira.yaml"), srv.URL)
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions", "--config", cfgPath, "PROJ-1"},
		map[string]string{})
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.NotContains(t, stderr, guardFailureMessage,
		"guard should NOT have fired with --config set")
	assert.GreaterOrEqual(t, len(srv.records()), 1,
		"a guard-pass should let the request reach the server")
}

func TestGuard_Arm2_SiteUserTokenFlags_Allow(t *testing.T) {
	neutralizeXDG(t)
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{}

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions",
			"--site", srv.URL,
			"--user", "test@example.com",
			"--token", "test-token",
			"PROJ-1",
		},
		// Provide OUTPUT_DIR so loadWriteConfig's LoadConfig passes;
		// no Jira env keys.
		map[string]string{"GOJIRA_OUTPUT_DIR": t.TempDir()})
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.NotContains(t, stderr, guardFailureMessage)
	assert.GreaterOrEqual(t, len(srv.records()), 1)
}

func TestGuard_Arm3_JiraEnvTrio_Allows(t *testing.T) {
	neutralizeXDG(t)
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{}

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions", "PROJ-1"},
		map[string]string{
			"GOJIRA_SITE":       srv.URL,
			"GOJIRA_USER":       "test@example.com",
			"GOJIRA_TOKEN":      "test-token",
			"GOJIRA_OUTPUT_DIR": t.TempDir(),
		})
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.NotContains(t, stderr, guardFailureMessage)
	assert.GreaterOrEqual(t, len(srv.records()), 1)
}

func TestGuard_Arm3_ConfigFileEnv_Allows(t *testing.T) {
	neutralizeXDG(t)
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{}

	cfgPath := writeMinimalConfigYAML(t,
		filepath.Join(t.TempDir(), "config.yaml"), srv.URL)
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions", "PROJ-1"},
		map[string]string{"GOJIRA_CONFIG_FILE": cfgPath})
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.NotContains(t, stderr, guardFailureMessage)
	assert.GreaterOrEqual(t, len(srv.records()), 1)
}

func TestGuard_Arm4_DiscoveredConfig_Allows(t *testing.T) {
	// neutralizeXDG sets XDG_CONFIG_HOME to a fresh tempdir; we then
	// drop config.yaml under it so DiscoverConfigFile resolves it.
	neutralizeXDG(t)
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{}

	xdg := os.Getenv("XDG_CONFIG_HOME")
	require.NotEmpty(t, xdg, "neutralizeXDG should have set XDG_CONFIG_HOME")
	writeMinimalConfigYAML(t,
		filepath.Join(xdg, "gojira", "config.yaml"), srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions", "PROJ-1"},
		map[string]string{})
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.NotContains(t, stderr, guardFailureMessage)
	assert.GreaterOrEqual(t, len(srv.records()), 1)
}

// ---------------------------------------------------------------------------
// phase-1-guard-6: fail case — no flags, empty env, no discoverable config
// ---------------------------------------------------------------------------

func TestGuard_NoConfig_FailsFast_BeforeHTTP(t *testing.T) {
	neutralizeXDG(t)
	srv := newWriteServer(t)
	// Wrap srv so we can detect ANY request; a guard-failure must NOT
	// reach the server. The writeServer.records() length stays 0.

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions", "PROJ-1"},
		map[string]string{}) // empty injected env
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, guardFailureMessage,
		"stderr must mention `gojira init`")
	for _, alt := range []string{"--config", "--site", "--user", "--token",
		"GOJIRA_CONFIG_FILE", "GOJIRA_SITE", "GOJIRA_USER", "GOJIRA_TOKEN"} {
		assert.Contains(t, stderr, alt,
			"stderr must list alternative %q", alt)
	}
	// Feature C — the message must name BOTH config locations and
	// BOTH `init` variants so users discover the project-local
	// option without re-reading --help.
	for _, needle := range []string{
		"gojira init --local",
		"project-local ./gojira.yaml",
		"~/.config/gojira/config.yaml",
	} {
		assert.Contains(t, stderr, needle,
			"stderr must surface the local-config option (%q); got %q",
			needle, stderr)
	}
	assert.Equal(t, 0, len(srv.records()),
		"guard must fail before any HTTP call; got %d recorded requests",
		len(srv.records()))
}

// Apply the no-config guard to every guarded command so a future
// wiring regression on any single Action is caught loudly.
func TestGuard_NoConfig_FailsForEveryGuardedCommand(t *testing.T) {
	commands := [][]string{
		{"gojira", "crawl", "PROJ-1"},
		{"gojira", "serve"},
		{"gojira", "create", "--project", "PROJ", "--summary", "x"},
		{"gojira", "update", "PROJ-1", "--summary", "x"},
		{"gojira", "comment", "PROJ-1", "--text", "hi"},
		{"gojira", "transitions", "PROJ-1"},
		{"gojira", "transition", "PROJ-1", "--id", "11"},
	}
	for _, args := range commands {
		t.Run(args[1], func(t *testing.T) {
			neutralizeXDG(t)
			_, stderr, code := captureRun(context.Background(),
				args, map[string]string{})
			assert.Equal(t, 1, code, "expected exit 1; stderr=%q", stderr)
			assert.Contains(t, stderr, guardFailureMessage,
				"command %q should produce the guard message", args[1])
		})
	}
}

// ---------------------------------------------------------------------------
// phase-1-guard-7: exempt — --help and --version succeed without config
// ---------------------------------------------------------------------------

func TestGuard_Exempt_HelpAndVersion(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"help", []string{"gojira", "--help"}},
		{"version", []string{"gojira", "--version"}},
		// Phase 2: init must be exempt from the guard so it is always
		// the way out of a no-config state. --help variant keeps the
		// command from actually trying to write anything.
		{"init_help", []string{"gojira", "init", "--help"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			neutralizeXDG(t)
			_, stderr, code := captureRun(context.Background(),
				tc.args, map[string]string{})
			assert.Equal(t, 0, code, "expected exit 0; stderr=%q", stderr)
			assert.NotContains(t, stderr, guardFailureMessage,
				"exempt command must not trigger the guard")
		})
	}
}

// ---------------------------------------------------------------------------
// Quiet the unused-import warning when signalled atomic is otherwise needed
// only via captureRun.
// ---------------------------------------------------------------------------

var _ atomic.Bool

// Quiet "imported and not used" if strings ever ends up only used in
// expectations above.
var _ = strings.Contains
