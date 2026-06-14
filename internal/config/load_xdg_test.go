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

// ---------------------------------------------------------------------------
// Feature B — local-over-global file-layer layering
// ---------------------------------------------------------------------------
//
// The implicit discovery case (no --config flag, no $GOJIRA_CONFIG_FILE)
// must apply the global XDG config FIRST and the project-local
// ./gojira.yaml SECOND, so per-field local-over-global overrides work.
// The explicit pinned-file cases (--config and $GOJIRA_CONFIG_FILE) must
// keep their single-file semantics — no layering, no surprises.

// writeFullGlobalYAML writes a complete, schema-valid YAML at path
// (creating parent dirs) and returns the path. Distinct from
// writeYAML by being explicit about each field so layering tests can
// assert which file owned which value. The crawl.concurrency knob is
// included to give the layering test a non-Jira knob to override.
func writeFullGlobalYAML(t *testing.T, path string) string {
	t.Helper()
	const body = `schema: gojira.config.v1
jira:
  base_url: https://global.example.com
  email: global@example.com
  api_token: global-token
output:
  dir: /tmp/global-out
crawl:
  concurrency: 5
server:
  address: 127.0.0.1:60000
log:
  level: info
  format: text
`
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// layeringResolver wires the resolver to a fake XDG_CONFIG_HOME so
// the global candidate resolves under xdg/gojira/config.yaml, while
// the local candidate is governed by os.Getwd (controlled in the
// test via t.Chdir). No home directory is configured.
func layeringResolver(xdgConfigHome string) *XDGResolver {
	return NewXDGResolver(
		envLookup(map[string]string{EnvXDGConfigHome: xdgConfigHome}),
		errHomeDir,
	)
}

// TestLoadApp_GlobalAndLocal_LayeringPerField is the headline Feature
// B test: a full global config supplies every required value; a
// partial local file overrides ONLY two fields. The effective App
// must take the two overridden fields from local and the remaining
// fields from global — proving the layering is per-FIELD, not
// winner-takes-all.
func TestLoadApp_GlobalAndLocal_LayeringPerField(t *testing.T) {
	xdg := t.TempDir()
	writeFullGlobalYAML(t,
		filepath.Join(xdg, AppName, GlobalConfigFileName))

	cwd := t.TempDir()
	t.Chdir(cwd)

	// Partial local: only crawl.concurrency and output.dir overridden,
	// every other field inherited from global. NO jira section, NO
	// server section — they MUST come from global for this to pass
	// Layer-2 validation.
	const localBody = `schema: gojira.config.v1
output:
  dir: /tmp/local-out
crawl:
  concurrency: 9
`
	localPath := filepath.Join(cwd, LocalConfigFileName)
	require.NoError(t, os.WriteFile(localPath, []byte(localBody), 0o644))

	app, err := LoadApp(LoadOptions{
		Resolver: layeringResolver(xdg),
	})
	require.NoError(t, err, "layered load must succeed")

	// Local wins on the two overridden fields.
	assert.Equal(t, "/tmp/local-out", app.Output.Dir,
		"local must override global on output.dir")
	assert.Equal(t, 9, app.Crawl.Concurrency,
		"local must override global on crawl.concurrency")

	// Global fills the gaps for everything the local file did not
	// touch — the proof that layering is field-by-field.
	assert.Equal(t, "https://global.example.com", app.Jira.BaseURL,
		"jira.base_url must be inherited from global")
	assert.Equal(t, "global@example.com", app.Jira.Email,
		"jira.email must be inherited from global")
	assert.Equal(t, "global-token", app.Jira.APIToken,
		"jira.api_token must be inherited from global")
	assert.Equal(t, "127.0.0.1:60000", app.Server.Address,
		"server.address must be inherited from global")

	// App.ConfigFile reports the MOST-SPECIFIC contributing file —
	// the local one — because that is the file diagnostics will
	// point users at first.
	assert.Equal(t, localPath, app.ConfigFile,
		"ConfigFile must name the most-specific contributing file (local)")
}

// TestLoadApp_GlobalOnly_NoLocal asserts the global file is used
// wholesale when no local file exists — the same UX the pre-Feature-
// B code already had on this code path, kept stable for users who
// only ever wrote a global config.
func TestLoadApp_GlobalOnly_NoLocal(t *testing.T) {
	xdg := t.TempDir()
	globalPath := writeFullGlobalYAML(t,
		filepath.Join(xdg, AppName, GlobalConfigFileName))

	t.Chdir(t.TempDir()) // clean cwd: no local file

	app, err := LoadApp(LoadOptions{
		Resolver: layeringResolver(xdg),
	})
	require.NoError(t, err)
	assert.Equal(t, "https://global.example.com", app.Jira.BaseURL)
	assert.Equal(t, "/tmp/global-out", app.Output.Dir)
	assert.Equal(t, 5, app.Crawl.Concurrency)
	assert.Equal(t, globalPath, app.ConfigFile,
		"ConfigFile must point at the global file when only global was applied")
}

// TestLoadApp_LocalOnly_NoGlobal asserts a project-local file with
// every required field is loaded wholesale when no global file
// exists; ConfigFile names the local file.
func TestLoadApp_LocalOnly_NoGlobal(t *testing.T) {
	xdg := t.TempDir() // empty: no global file underneath
	cwd := t.TempDir()
	t.Chdir(cwd)
	localPath := writeFullGlobalYAML(t,
		filepath.Join(cwd, LocalConfigFileName))

	app, err := LoadApp(LoadOptions{
		Resolver: layeringResolver(xdg),
	})
	require.NoError(t, err)
	assert.Equal(t, "https://global.example.com", app.Jira.BaseURL,
		"local file (using the writeFullGlobalYAML body) must load wholesale")
	assert.Equal(t, localPath, app.ConfigFile,
		"ConfigFile must point at the local file when only local was applied")
}

// TestLoadApp_ExplicitConfigPath_NoLayering asserts the --config
// pin is exempt from layering: even if a project-local ./gojira.yaml
// exists in the cwd, an explicit ConfigPath uses ONLY that file.
func TestLoadApp_ExplicitConfigPath_NoLayering(t *testing.T) {
	// Drop a "would-override" local file in the cwd; the explicit
	// config path must NOT pull this file's values in.
	cwd := t.TempDir()
	t.Chdir(cwd)
	const localBody = `schema: gojira.config.v1
output:
  dir: /tmp/local-should-not-win
`
	require.NoError(t, os.WriteFile(
		filepath.Join(cwd, LocalConfigFileName),
		[]byte(localBody), 0o644))

	explicit := writeFullGlobalYAML(t,
		filepath.Join(t.TempDir(), "explicit.yaml"))

	app, err := LoadApp(LoadOptions{
		ConfigPath: explicit,
		Resolver:   quietResolver(),
	})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/global-out", app.Output.Dir,
		"explicit --config must not be layered over by ./gojira.yaml")
	assert.Equal(t, explicit, app.ConfigFile,
		"ConfigFile must name the explicit file, not the local")
}

// TestLoadApp_GojiraConfigFileEnv_NoLayering mirrors the previous
// test for the env-var pin: $GOJIRA_CONFIG_FILE is treated the same
// as --config and disables global+local layering.
func TestLoadApp_GojiraConfigFileEnv_NoLayering(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	const localBody = `schema: gojira.config.v1
output:
  dir: /tmp/local-should-not-win
`
	require.NoError(t, os.WriteFile(
		filepath.Join(cwd, LocalConfigFileName),
		[]byte(localBody), 0o644))

	envPath := writeFullGlobalYAML(t,
		filepath.Join(t.TempDir(), "via-env.yaml"))

	// A global file ALSO exists; even so, the env-pin wins alone.
	xdg := t.TempDir()
	writeFullGlobalYAML(t,
		filepath.Join(xdg, AppName, GlobalConfigFileName))

	resolver := NewXDGResolver(
		envLookup(map[string]string{
			EnvGojiraConfigFile: envPath,
			EnvXDGConfigHome:    xdg,
		}),
		errHomeDir,
	)

	app, err := LoadApp(LoadOptions{
		Resolver: resolver,
	})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/global-out", app.Output.Dir,
		"$GOJIRA_CONFIG_FILE must not be layered over by ./gojira.yaml")
	assert.Equal(t, envPath, app.ConfigFile,
		"ConfigFile must name the env-pinned file")
}

// TestLoadApp_InvalidLocal_FailsEvenWithValidGlobal asserts Layer-1
// schema validation runs PER FILE: an invalid local file rejects
// the load even when the global file is fine. Otherwise a bad
// override would silently fall back to global, masking the user's
// mistake.
func TestLoadApp_InvalidLocal_FailsEvenWithValidGlobal(t *testing.T) {
	xdg := t.TempDir()
	writeFullGlobalYAML(t,
		filepath.Join(xdg, AppName, GlobalConfigFileName))

	cwd := t.TempDir()
	t.Chdir(cwd)
	// Unknown top-level key triggers Layer-1 additionalProperties:false.
	const badLocal = `schema: gojira.config.v1
this_is_not_a_real_top_level_key: oops
`
	require.NoError(t, os.WriteFile(
		filepath.Join(cwd, LocalConfigFileName),
		[]byte(badLocal), 0o644))

	_, err := LoadApp(LoadOptions{
		Resolver: layeringResolver(xdg),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue,
		"invalid local file must surface Layer-1 ErrInvalidValue, "+
			"not silently fall back to global")
}

// TestLoadApp_EmptyLocal_PreservesGlobal asserts a whitespace-only
// local file does NOT erase the global file's fields — decodeYAML's
// EOF no-op contract holds at the layering boundary too.
func TestLoadApp_EmptyLocal_PreservesGlobal(t *testing.T) {
	xdg := t.TempDir()
	writeFullGlobalYAML(t,
		filepath.Join(xdg, AppName, GlobalConfigFileName))

	cwd := t.TempDir()
	t.Chdir(cwd)
	localPath := filepath.Join(cwd, LocalConfigFileName)
	require.NoError(t, os.WriteFile(localPath, []byte("\n   \n\n"), 0o644))

	app, err := LoadApp(LoadOptions{
		Resolver: layeringResolver(xdg),
	})
	require.NoError(t, err, "empty local file must be a no-op, not an error")
	assert.Equal(t, "https://global.example.com", app.Jira.BaseURL,
		"global jira fields must survive an empty local layer")
	assert.Equal(t, "/tmp/global-out", app.Output.Dir,
		"global output.dir must survive an empty local layer")
	// Empty local contributed nothing → ConfigFile names global.
	assert.Equal(t,
		filepath.Join(xdg, AppName, GlobalConfigFileName),
		app.ConfigFile,
		"ConfigFile must name the global file when local was empty")
}
