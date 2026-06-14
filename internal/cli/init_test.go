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

// ---------------------------------------------------------------------------
// --local / --global target selection
// ---------------------------------------------------------------------------

// TestInit_Local_WritesCompleteProjectConfig verifies that
// `gojira init --local` writes a COMPLETE, schema-valid gojira.yaml
// into the current working directory at mode 0600, does NOT create
// the global XDG config, and announces the local absolute path.
func TestInit_Local_WritesCompleteProjectConfig(t *testing.T) {
	xdg := setXDG(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	outputDir := t.TempDir()

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init", "--local",
			"--site", "https://x.atlassian.net",
			"--user", "u@example.com",
			"--token", "t-secret",
			"--output-dir", outputDir,
			"--server-address", "127.0.0.1:50051",
		},
		map[string]string{})
	require.Equal(t, 0, code, "stderr=%q stdout=%q", stderr, stdout)

	localPath := filepath.Join(cwd, config.LocalConfigFileName)
	info, err := os.Stat(localPath)
	require.NoError(t, err, "local config must exist at %s", localPath)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "file perms")
	assert.Contains(t, stdout, localPath,
		"stdout must announce the written local path")

	// Global XDG path must NOT have been created.
	_, gerr := os.Stat(configYAMLPath(xdg))
	assert.True(t, os.IsNotExist(gerr),
		"global config must NOT exist after --local; got err=%v", gerr)

	// Round-trip: every field set by init must come back through the
	// loader, including the server address (which the global-path
	// happy-path test already covers — we re-check it here to prove
	// the local file is self-sufficient, NOT a partial overlay).
	cfg, err := gojira.LoadFileConfig(localPath)
	require.NoError(t, err, "LoadFileConfig")
	assert.Equal(t, "https://x.atlassian.net", cfg.Site)
	assert.Equal(t, "u@example.com", cfg.User)
	assert.Equal(t, "t-secret", cfg.Token)
	assert.Equal(t, outputDir, cfg.OutputDir)

	// Raw-body sanity: the YAML body must explicitly mention each
	// top-level section so the file is recognisably complete even
	// without going through the facade.
	raw, err := os.ReadFile(localPath)
	require.NoError(t, err)
	body := string(raw)
	for _, needle := range []string{
		"base_url", "email", "api_token", // jira section
		"dir:",                     // output section
		"address: 127.0.0.1:50051", // server section
	} {
		assert.Contains(t, body, needle,
			"local config body missing %q; got:\n%s", needle, body)
	}
}

// TestInit_GlobalAndLocal_MutuallyExclusive asserts that passing both
// flags is a hard error with the documented exit code and message,
// and writes NOTHING to either target.
func TestInit_GlobalAndLocal_MutuallyExclusive(t *testing.T) {
	xdg := setXDG(t)
	cwd := t.TempDir()
	t.Chdir(cwd)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "init", "--global", "--local",
			"--site", "https://x.atlassian.net",
			"--user", "u@example.com",
			"--token", "t-secret",
			"--output-dir", t.TempDir(),
			"--server-address", "127.0.0.1:50051",
		},
		map[string]string{})
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "mutually exclusive",
		"stderr must explain the conflict; got %q", stderr)

	// Neither file may exist after the rejection.
	_, gerr := os.Stat(configYAMLPath(xdg))
	assert.True(t, os.IsNotExist(gerr),
		"global config must NOT exist; got err=%v", gerr)
	_, lerr := os.Stat(filepath.Join(cwd, config.LocalConfigFileName))
	assert.True(t, os.IsNotExist(lerr),
		"local config must NOT exist; got err=%v", lerr)
}

// localInitArgs returns the standard --local argv with all required
// values supplied via flags, so the prompt seams stay untouched.
func localInitArgs(outputDir string) []string {
	return []string{"gojira", "init", "--local",
		"--site", "https://x.atlassian.net",
		"--user", "u@example.com",
		"--token", "t-secret",
		"--output-dir", outputDir,
		"--server-address", "127.0.0.1:50051",
	}
}

// TestInit_Local_GitignoreNudge_AppendsWhenMissing covers case (a):
// a ./.gitignore exists WITHOUT a gojira.yaml entry → after
// `init --local` the file ends with a "gojira.yaml" line and stdout
// reports the addition.
func TestInit_Local_GitignoreNudge_AppendsWhenMissing(t *testing.T) {
	setXDG(t)
	cwd := t.TempDir()
	t.Chdir(cwd)

	gitignorePath := filepath.Join(cwd, ".gitignore")
	preBody := "node_modules/\n*.log\n"
	require.NoError(t, os.WriteFile(gitignorePath, []byte(preBody), 0o644))

	stdout, stderr, code := captureRun(context.Background(),
		localInitArgs(t.TempDir()), map[string]string{})
	require.Equal(t, 0, code, "stderr=%q stdout=%q", stderr, stdout)

	assert.Contains(t, stdout, "added gojira.yaml to .gitignore",
		"stdout must announce the .gitignore amendment")

	post, err := os.ReadFile(gitignorePath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(post), "\n"), "\n")
	found := false
	for _, line := range lines {
		if strings.TrimSpace(line) == config.LocalConfigFileName {
			found = true
			break
		}
	}
	assert.True(t, found,
		"gojira.yaml must appear as a line in .gitignore; got:\n%s", string(post))
	// Existing entries must be preserved.
	assert.Contains(t, string(post), "node_modules/")
	assert.Contains(t, string(post), "*.log")
}

// TestInit_Local_GitignoreNudge_NoOpWhenAlreadyPresent covers case
// (b): an existing .gitignore that already lists "gojira.yaml" is
// left BYTE-IDENTICAL and no add-message is printed.
func TestInit_Local_GitignoreNudge_NoOpWhenAlreadyPresent(t *testing.T) {
	setXDG(t)
	cwd := t.TempDir()
	t.Chdir(cwd)

	gitignorePath := filepath.Join(cwd, ".gitignore")
	preBody := "node_modules/\ngojira.yaml\n*.log\n"
	require.NoError(t, os.WriteFile(gitignorePath, []byte(preBody), 0o644))

	stdout, stderr, code := captureRun(context.Background(),
		localInitArgs(t.TempDir()), map[string]string{})
	require.Equal(t, 0, code, "stderr=%q stdout=%q", stderr, stdout)

	assert.NotContains(t, stdout, "added gojira.yaml to .gitignore",
		"stdout must NOT announce an amendment when the entry already exists")

	post, err := os.ReadFile(gitignorePath)
	require.NoError(t, err)
	assert.Equal(t, preBody, string(post),
		".gitignore must be byte-identical when gojira.yaml is already listed")
}

// TestInit_Local_GitignoreNudge_WarnsWhenAbsent covers case (c): no
// .gitignore at all → a warning is written to stderr and NO
// .gitignore is created (we don't silently fabricate VCS files).
func TestInit_Local_GitignoreNudge_WarnsWhenAbsent(t *testing.T) {
	setXDG(t)
	cwd := t.TempDir()
	t.Chdir(cwd)

	_, stderr, code := captureRun(context.Background(),
		localInitArgs(t.TempDir()), map[string]string{})
	require.Equal(t, 0, code, "stderr=%q", stderr)

	assert.Contains(t, stderr, "warning:",
		"stderr must carry a warning prefix; got %q", stderr)
	assert.Contains(t, stderr, ".gitignore",
		"warning must mention .gitignore; got %q", stderr)
	assert.Contains(t, stderr, "gojira.yaml",
		"warning must mention the secret-bearing filename; got %q", stderr)

	_, err := os.Stat(filepath.Join(cwd, ".gitignore"))
	assert.True(t, os.IsNotExist(err),
		"warning path must NOT create a .gitignore; got err=%v", err)
}
