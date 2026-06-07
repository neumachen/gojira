package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeYAML writes a small valid gojira.yaml document to path and
// returns the path it wrote to. Tests use it to materialise a
// discoverable config file inside t.TempDir() without depending on
// the real filesystem layout. The contents are intentionally
// distinctive (custom output dir) so a follow-up assertion can prove
// the loaded App came from THIS file rather than from defaults or env.
func writeYAML(t *testing.T, path string) string {
	t.Helper()
	const body = `schema: gojira.config.v1
jira:
  base_url: https://file.example.com
  email: file@example.com
  api_token: file-token
output:
  dir: /tmp/from-discovered-file
log:
  level: info
  format: text
`
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// quietResolver returns an XDGResolver that pretends the process has
// no XDG_CONFIG_HOME, no GOJIRA_CONFIG_FILE, and no home directory.
// Combined with t.Chdir to a fresh t.TempDir, this gives tests a
// clean slate where ONLY the inputs they explicitly write are
// discoverable.
func quietResolver() *XDGResolver {
	return NewXDGResolver(emptyLookup, errHomeDir)
}

// TestLoadApp_ConfigPath_DiscoversAndLoads asserts the
// LoadOptions.ConfigPath wiring: an explicit path is opened, parsed,
// and the file's values flow through the cascade.
func TestLoadApp_ConfigPath_DiscoversAndLoads(t *testing.T) {
	path := writeYAML(t, filepath.Join(t.TempDir(), "explicit.yaml"))

	app, err := LoadApp(LoadOptions{
		ConfigPath: path,
		Resolver:   quietResolver(),
	})
	require.NoError(t, err)
	assert.Equal(t, "https://file.example.com", app.Jira.BaseURL)
	assert.Equal(t, "/tmp/from-discovered-file", app.Output.Dir)
	assert.Equal(t, path, app.ConfigFile,
		"discovered file path must be backfilled onto App.ConfigFile")
}

// TestLoadApp_ConfigPath_MissingIsHardError asserts the explicit-
// but-missing branch: an explicit ConfigPath that does not exist
// triggers a hard error wrapping ErrInvalidValue (the user asked
// for a specific file that isn't there). This is distinct from
// "no file found anywhere", which is a successful fall-through.
func TestLoadApp_ConfigPath_MissingIsHardError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	_, err := LoadApp(LoadOptions{
		ConfigPath: missing,
		Resolver:   quietResolver(),
		Env:        validCanonicalEnv(),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue,
		"explicit-but-missing config path must wrap ErrInvalidValue")
	assert.Contains(t, err.Error(), missing,
		"error message should name the missing path")
}

// TestLoadApp_NoConfigFile_AnywhereFallsThroughToEnv asserts the
// successful fall-through path: nothing on disk, nothing in env-
// based discovery, but a complete Env map → load succeeds and the
// App carries no ConfigFile.
func TestLoadApp_NoConfigFile_AnywhereFallsThroughToEnv(t *testing.T) {
	t.Chdir(t.TempDir()) // clean cwd

	app, err := LoadApp(LoadOptions{
		Resolver: quietResolver(),
		Env:      validCanonicalEnv(),
	})
	require.NoError(t, err)
	assert.Equal(t, "", app.ConfigFile,
		"no discovered file → App.ConfigFile stays empty")
	assert.Equal(t, "https://example.atlassian.net", app.Jira.BaseURL)
}

// TestLoadApp_YAMLReaderBypassesDiscovery asserts the Phase 3
// back-compat contract: when LoadOptions.YAML is non-nil, the
// resolver and ConfigPath are ignored entirely. This is the
// property that lets Phase 3 tests keep working unmodified.
func TestLoadApp_YAMLReaderBypassesDiscovery(t *testing.T) {
	// Put a "decoy" file at an explicit path so we can prove the
	// reader path won and the decoy was NOT loaded.
	decoy := filepath.Join(t.TempDir(), "decoy.yaml")
	require.NoError(t, os.WriteFile(decoy, []byte(`schema: gojira.config.v1
jira:
  base_url: https://decoy.example.com
  email: decoy@example.com
  api_token: decoy-tok
output:
  dir: /tmp/from-decoy
`), 0o644))

	app, err := LoadApp(LoadOptions{
		// In-memory reader wins, even though ConfigPath points
		// at a real file.
		YAML:       openYAMLFile(t, writeYAML(t, filepath.Join(t.TempDir(), "real.yaml"))),
		ConfigPath: decoy,
		Resolver:   quietResolver(),
	})
	require.NoError(t, err)
	assert.Equal(t, "https://file.example.com", app.Jira.BaseURL,
		"reader path must win over ConfigPath")
	assert.Equal(t, "", app.ConfigFile,
		"caller-supplied reader leaves ConfigFile empty (no discovered path)")
}

// TestLoadApp_LocalCwdDiscovery asserts the implicit ./gojira.yaml
// candidate: with no explicit ConfigPath and no env var, a
// gojira.yaml in the current working directory is picked up and
// loaded. t.Chdir scopes the cwd change to this test.
func TestLoadApp_LocalCwdDiscovery(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	want := writeYAML(t, filepath.Join(dir, LocalConfigFileName))

	app, err := LoadApp(LoadOptions{
		Resolver: quietResolver(),
	})
	require.NoError(t, err)
	assert.Equal(t, want, app.ConfigFile)
	assert.Equal(t, "https://file.example.com", app.Jira.BaseURL)
}

// TestLoadApp_GojiraConfigFileEnvDiscovery asserts the second
// discovery candidate (the GOJIRA_CONFIG_FILE env var) is honored
// when no explicit ConfigPath is supplied.
func TestLoadApp_GojiraConfigFileEnvDiscovery(t *testing.T) {
	t.Chdir(t.TempDir()) // clean cwd so candidate 3 cannot win
	path := writeYAML(t, filepath.Join(t.TempDir(), "via-env.yaml"))

	resolver := NewXDGResolver(
		envLookup(map[string]string{EnvGojiraConfigFile: path}),
		errHomeDir,
	)
	app, err := LoadApp(LoadOptions{
		Resolver: resolver,
	})
	require.NoError(t, err)
	assert.Equal(t, path, app.ConfigFile)
	assert.Equal(t, "https://file.example.com", app.Jira.BaseURL)
}

// TestLoadApp_GojiraConfigFileEnvMissing_HardError asserts the
// explicit-but-missing rule applies to the env-var candidate too:
// when GOJIRA_CONFIG_FILE is set but the file doesn't exist, the
// loader returns ErrInvalidValue rather than silently falling
// through to ./gojira.yaml or ~/.config.
func TestLoadApp_GojiraConfigFileEnvMissing_HardError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-via-env.yaml")
	resolver := NewXDGResolver(
		envLookup(map[string]string{EnvGojiraConfigFile: missing}),
		errHomeDir,
	)
	_, err := LoadApp(LoadOptions{
		Resolver: resolver,
		Env:      validCanonicalEnv(),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), missing)
}

// openYAMLFile opens the file at path read-only and registers a
// t.Cleanup so the handle is closed even when the test fails. It's
// a small helper so the back-compat test can prove an external
// io.Reader (a real *os.File, not just strings.NewReader) is
// accepted unchanged.
func openYAMLFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}
