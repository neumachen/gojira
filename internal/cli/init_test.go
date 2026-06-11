// init_test.go — tests for the `gojira init` subcommand.
//
// All tests drive run() / captureRun() with an injected env map and use
// t.Setenv to control XDG_CONFIG_HOME / HOME (the resolver reads the
// PROCESS env, not the injected env map). The interactive token-prompt
// seam is exercised via initStdin (line reader) and readTokenFn (token
// reader) function-fields; tests swap them in a t.Cleanup-restored
// closure so production defaults are never permanently mutated.
package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setXDG redirects the resolver to a per-test XDG_CONFIG_HOME, AND
// neutralizes HOME so the home-based fallback cannot accidentally
// resolve. Returns the chosen XDG dir.
func setXDG(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir()) // exists but empty; cannot leak a config
	return xdg
}

// configYAMLPath returns the resolved config path under the test's
// XDG_CONFIG_HOME.
func configYAMLPath(xdg string) string {
	return filepath.Join(xdg, "gojira", "config.yaml")
}

// withStdin swaps initStdin to r for the duration of the test.
func withStdin(t *testing.T, r io.Reader) {
	t.Helper()
	prev := initStdin
	initStdin = r
	t.Cleanup(func() { initStdin = prev })
}

// withTokenReader swaps readTokenFn for the duration of the test.
func withTokenReader(t *testing.T, fn func(prompt string, errOut io.Writer) (string, error)) {
	t.Helper()
	prev := readTokenFn
	readTokenFn = fn
	t.Cleanup(func() { readTokenFn = prev })
}

// ---------------------------------------------------------------------------
// phase-2-init-9: happy path — writes 0600 schema-valid YAML
// ---------------------------------------------------------------------------

func TestInit_HappyPath_WritesValid0600YAML(t *testing.T) {
	xdg := setXDG(t)
	outputDir := t.TempDir()

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init",
			"--site", "https://x.atlassian.net",
			"--user", "u@example.com",
			"--token", "t-secret",
			"--output-dir", outputDir,
			"--server-address", "127.0.0.1:50051",
		},
		map[string]string{})
	require.Equal(t, 0, code, "stderr=%q", stderr)

	path := configYAMLPath(xdg)
	info, err := os.Stat(path)
	require.NoError(t, err, "config file must exist at %s", path)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "file perms")
	assert.Contains(t, stdout, path, "stdout must announce the written path")

	// Round-trip via the facade loader.
	cfg, err := gojira.LoadFileConfig(path)
	require.NoError(t, err, "LoadFileConfig")
	assert.Equal(t, "https://x.atlassian.net", cfg.Site)
	assert.Equal(t, "u@example.com", cfg.User)
	assert.Equal(t, "t-secret", cfg.Token)
	assert.Equal(t, outputDir, cfg.OutputDir)
}

// ---------------------------------------------------------------------------
// phase-2-init-10: refuses to clobber without --force
// ---------------------------------------------------------------------------

func TestInit_RefusesClobberWithoutForce(t *testing.T) {
	xdg := setXDG(t)
	args := []string{"gojira", "init",
		"--site", "https://x.atlassian.net",
		"--user", "u@example.com",
		"--token", "t-secret",
		"--output-dir", t.TempDir(),
		"--server-address", "127.0.0.1:50051",
	}
	_, stderr, code := captureRun(context.Background(), args, map[string]string{})
	require.Equal(t, 0, code, "first init must succeed; stderr=%q", stderr)

	path := configYAMLPath(xdg)
	pre, err := os.ReadFile(path)
	require.NoError(t, err)

	_, stderr, code = captureRun(context.Background(), args, map[string]string{})
	assert.Equal(t, 1, code, "second init must refuse")
	assert.Contains(t, stderr, "--force",
		"stderr must mention --force; got %q", stderr)

	post, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(pre, post),
		"file must be byte-identical after refused overwrite")
}

// ---------------------------------------------------------------------------
// phase-2-init-11: --force overwrites
// ---------------------------------------------------------------------------

func TestInit_ForceOverwrites(t *testing.T) {
	xdg := setXDG(t)
	outputDir := t.TempDir()

	// Run 1: site=A.
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init",
			"--site", "https://A.atlassian.net",
			"--user", "u@example.com",
			"--token", "t-secret",
			"--output-dir", outputDir,
			"--server-address", "127.0.0.1:50051",
		},
		map[string]string{})
	require.Equal(t, 0, code, "stderr=%q", stderr)

	// Run 2: --force site=B.
	_, stderr, code = captureRun(context.Background(),
		[]string{"gojira", "init", "--force",
			"--site", "https://B.atlassian.net",
			"--user", "u@example.com",
			"--token", "t-secret",
			"--output-dir", outputDir,
			"--server-address", "127.0.0.1:50051",
		},
		map[string]string{})
	require.Equal(t, 0, code, "force run failed; stderr=%q", stderr)

	path := configYAMLPath(xdg)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"perms must remain 0600 after --force")

	cfg, err := gojira.LoadFileConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "https://B.atlassian.net", cfg.Site,
		"file should now carry the second site value")
}

// ---------------------------------------------------------------------------
// phase-2-init-12: interactive prompts via injected stdin (no token echo)
// ---------------------------------------------------------------------------

func TestInit_Interactive_PromptsAndNeverEchoesToken(t *testing.T) {
	xdg := setXDG(t)

	// All four required values come from stdin; final empty line
	// accepts the server-address default.
	withStdin(t, strings.NewReader(
		"https://x.atlassian.net\n"+
			"u@example.com\n"+
			"\n", // accept server-address default (token handled via readTokenFn)
	))
	// Token comes through the readTokenFn seam — no echo path. The
	// fake returns a sentinel that we will later assert is NOT on stdout.
	withTokenReader(t, func(prompt string, errOut io.Writer) (string, error) {
		// Production reader writes the prompt label to errOut; mirror that
		// so the test exercises a realistic stderr stream.
		_, _ = io.WriteString(errOut, prompt)
		return "t-secret", nil
	})

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init"},
		map[string]string{}) // no flag values; no env
	require.Equal(t, 0, code, "stderr=%q stdout=%q", stderr, stdout)

	// Token value must NEVER appear in stdout (the success line is the
	// only stdout content; the prompt label may have written to stderr
	// but never the secret).
	assert.NotContains(t, stdout, "t-secret",
		"token must not be echoed to stdout; got stdout=%q", stdout)

	// File round-trip with the prompted values + server default.
	cfg, err := gojira.LoadFileConfig(configYAMLPath(xdg))
	require.NoError(t, err)
	assert.Equal(t, "https://x.atlassian.net", cfg.Site)
	assert.Equal(t, "u@example.com", cfg.User)
	assert.Equal(t, "t-secret", cfg.Token)
}

// ---------------------------------------------------------------------------
// phase-2-init-13: --output-dir defaults when flag absent
// ---------------------------------------------------------------------------

func TestInit_OutputDirDefaults_WhenFlagAbsent(t *testing.T) {
	xdg := setXDG(t)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init",
			"--site", "https://x.atlassian.net",
			"--user", "u@example.com",
			"--token", "t",
			"--server-address", "127.0.0.1:50051",
		},
		map[string]string{})
	require.Equal(t, 0, code, "stderr=%q", stderr)

	cfg, err := gojira.LoadFileConfig(configYAMLPath(xdg))
	require.NoError(t, err)
	assert.Equal(t, config.DefaultOutputSettings().Dir, cfg.OutputDir,
		"missing --output-dir must fall back to the documented default")
}

// ---------------------------------------------------------------------------
// phase-2-init-14: no HOME and no XDG → clear error
// ---------------------------------------------------------------------------

func TestInit_NoHome_NoXDG_FailsClearly(t *testing.T) {
	// Neutralize BOTH paths so GlobalConfigFile() returns "".
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows-style fallback some libs check

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init",
			"--site", "https://x.atlassian.net",
			"--user", "u@example.com",
			"--token", "t",
			"--output-dir", t.TempDir(),
			"--server-address", "127.0.0.1:50051",
		},
		map[string]string{})
	assert.Equal(t, 1, code)
	combined := strings.ToUpper(stderr)
	assert.True(t,
		strings.Contains(combined, "HOME") || strings.Contains(combined, "XDG_CONFIG_HOME"),
		"stderr must mention HOME or XDG_CONFIG_HOME; got %q", stderr)
}

// ---------------------------------------------------------------------------
// `gojira init --help` is exempt from the guard (extends Phase-1 coverage)
// ---------------------------------------------------------------------------

func TestInit_HelpFlag_Exempt(t *testing.T) {
	// neutralizeXDG isolates from any host config and t.Chdir's to a
	// fresh dir so arm-4 cannot resolve.
	neutralizeXDG(t)
	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init", "--help"}, map[string]string{})
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.NotContains(t, stderr, "gojira init", // the guard's message
		"`init --help` must not trigger the guard")
	assert.Contains(t, stdout, "Create a gojira config file")
}
