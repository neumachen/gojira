package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emptyLookup is a lookup function that reports every key as unset.
// Tests that don't care about env-var overrides use it so the
// resolver sees a clean slate regardless of the real process
// environment.
func emptyLookup(string) (string, bool) { return "", false }

// envLookup builds a lookup function backed by an explicit map.
// Keys not in the map are reported as unset, mirroring os.LookupEnv.
func envLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

// errHomeDir is a homeDir function that simulates an unresolvable
// home directory (e.g. UserHomeDir failing in a sandbox). It is
// used to verify that the resolver skips the home-based candidate
// rather than fabricating a nonsense "/.config/gojira" path.
func errHomeDir() (string, error) {
	return "", errors.New("no home directory in this test")
}

// writeFile creates a regular file at path with empty contents and
// fails the test on error. Tests use it to materialise discoverable
// config files inside t.TempDir() without polluting the real
// filesystem.
func writeFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("schema: gojira.config.v1\n"), 0o644))
}

// TestNewXDGResolver_NilArgsSubstituteOSDefaults asserts the
// constructor wires os.LookupEnv / os.UserHomeDir when either
// dependency is nil. This is the property that lets
// NewDefaultXDGResolver be a one-liner.
func TestNewXDGResolver_NilArgsSubstituteOSDefaults(t *testing.T) {
	r := NewXDGResolver(nil, nil)
	require.NotNil(t, r)
	assert.NotNil(t, r.lookup)
	assert.NotNil(t, r.homeDir)
}

// TestGlobalConfigFile_FromXDGConfigHome asserts the XDG var wins
// when set and non-empty. The home directory must NOT be consulted
// in that case.
func TestGlobalConfigFile_FromXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	r := NewXDGResolver(
		envLookup(map[string]string{EnvXDGConfigHome: xdg}),
		errHomeDir, // would error if consulted
	)
	want := filepath.Join(xdg, AppName, GlobalConfigFileName)
	assert.Equal(t, want, r.GlobalConfigFile())
}

// TestGlobalConfigFile_FromHomeFallback asserts the ~/.config
// fallback is used when XDG_CONFIG_HOME is unset.
func TestGlobalConfigFile_FromHomeFallback(t *testing.T) {
	home := t.TempDir()
	r := NewXDGResolver(
		emptyLookup,
		func() (string, error) { return home, nil },
	)
	want := filepath.Join(home, ".config", AppName, GlobalConfigFileName)
	assert.Equal(t, want, r.GlobalConfigFile())
}

// TestGlobalConfigFile_EmptyWhenNoXDGAndNoHome asserts the resolver
// returns "" when neither XDG_CONFIG_HOME nor a valid home dir is
// available. This is the "no home-based candidate" branch that lets
// DiscoverConfigFile skip cleanly to a (",false) result.
func TestGlobalConfigFile_EmptyWhenNoXDGAndNoHome(t *testing.T) {
	r := NewXDGResolver(emptyLookup, errHomeDir)
	assert.Equal(t, "", r.GlobalConfigFile())
}

// TestDiscoverConfigFile_ExplicitWinsAndIsFound asserts the explicit
// --config flag pins discovery to the supplied path AND that an
// existing file at that path is reported (path, true).
func TestDiscoverConfigFile_ExplicitWinsAndIsFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom-gojira.yaml")
	writeFile(t, path)

	r := NewXDGResolver(emptyLookup, errHomeDir)
	got, found := r.DiscoverConfigFile(path)
	assert.Equal(t, path, got)
	assert.True(t, found)
}

// TestDiscoverConfigFile_ExplicitMissingReturnsPathFalse asserts an
// explicit path that does not exist is surfaced to the caller as
// (path, false) — the contract that lets LoadApp distinguish
// "user asked for X but it's missing" from "no file anywhere".
func TestDiscoverConfigFile_ExplicitMissingReturnsPathFalse(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.yaml")

	r := NewXDGResolver(emptyLookup, errHomeDir)
	got, found := r.DiscoverConfigFile(missing)
	assert.Equal(t, missing, got, "explicit path must be preserved in the result")
	assert.False(t, found, "missing explicit file must report found=false")
}

// TestDiscoverConfigFile_GojiraConfigFileEnv asserts $GOJIRA_CONFIG_FILE
// is honored when the explicit flag is empty, and that it also
// behaves like an explicit candidate (no fall-through; missing file
// surfaces as (path,false)).
func TestDiscoverConfigFile_GojiraConfigFileEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "via-env.yaml")
	writeFile(t, path)

	r := NewXDGResolver(
		envLookup(map[string]string{EnvGojiraConfigFile: path}),
		errHomeDir,
	)
	got, found := r.DiscoverConfigFile("")
	assert.Equal(t, path, got)
	assert.True(t, found)

	t.Run("missing env path is not auto-fallthrough", func(t *testing.T) {
		missing := filepath.Join(dir, "missing-via-env.yaml")
		r := NewXDGResolver(
			envLookup(map[string]string{EnvGojiraConfigFile: missing}),
			errHomeDir,
		)
		got, found := r.DiscoverConfigFile("")
		assert.Equal(t, missing, got)
		assert.False(t, found)
	})
}

// TestDiscoverConfigFile_LocalCwd asserts ./gojira.yaml is picked up
// when the working directory contains it AND no explicit flag or
// env var has been supplied. t.Chdir scopes the cwd change to this
// test so the rest of the test binary sees the original cwd.
func TestDiscoverConfigFile_LocalCwd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LocalConfigFileName)
	writeFile(t, path)
	t.Chdir(dir)

	r := NewXDGResolver(emptyLookup, errHomeDir)
	got, found := r.DiscoverConfigFile("")
	assert.True(t, found)
	assert.Equal(t, path, got)
}

// TestDiscoverConfigFile_XDGConfigHome asserts the XDG_CONFIG_HOME
// candidate is picked up when the higher-precedence candidates fall
// through. The home() dependency is errored to prove XDG wins on
// its own when present.
func TestDiscoverConfigFile_XDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	path := filepath.Join(xdg, AppName, GlobalConfigFileName)
	writeFile(t, path)
	// Move cwd somewhere clean so ./gojira.yaml does not interfere.
	t.Chdir(t.TempDir())

	r := NewXDGResolver(
		envLookup(map[string]string{EnvXDGConfigHome: xdg}),
		errHomeDir,
	)
	got, found := r.DiscoverConfigFile("")
	assert.True(t, found)
	assert.Equal(t, path, got)
}

// TestDiscoverConfigFile_HomeFallback asserts the ~/.config fallback
// is picked up when XDG_CONFIG_HOME is unset and the working
// directory has no ./gojira.yaml.
func TestDiscoverConfigFile_HomeFallback(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", AppName, GlobalConfigFileName)
	writeFile(t, path)
	t.Chdir(t.TempDir())

	r := NewXDGResolver(
		emptyLookup,
		func() (string, error) { return home, nil },
	)
	got, found := r.DiscoverConfigFile("")
	assert.True(t, found)
	assert.Equal(t, path, got)
}

// TestDiscoverConfigFile_NothingAnywhere asserts the resolver
// returns ("", false) when every candidate is absent. This is the
// fall-through-to-defaults+env path; LoadApp treats it as a
// non-error.
func TestDiscoverConfigFile_NothingAnywhere(t *testing.T) {
	t.Chdir(t.TempDir()) // clean cwd, no gojira.yaml
	r := NewXDGResolver(emptyLookup, errHomeDir)
	got, found := r.DiscoverConfigFile("")
	assert.False(t, found)
	assert.Equal(t, "", got)
}

// TestDiscoverConfigFile_DirectoryIsNotConfig asserts a directory
// at the candidate path is NOT treated as a config file. Picking
// up a stray "gojira.yaml/" directory would yield a confusing
// "is a directory" open error several layers later; rejecting it
// at stat time gives a clean fall-through.
func TestDiscoverConfigFile_DirectoryIsNotConfig(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, LocalConfigFileName), 0o755))
	t.Chdir(dir)

	r := NewXDGResolver(emptyLookup, errHomeDir)
	got, found := r.DiscoverConfigFile("")
	assert.False(t, found, "a directory must not be discovered as a config file")
	assert.Equal(t, "", got)
}

// TestDiscoverConfigFile_PrecedenceExplicitOverEnv asserts the
// documented precedence between candidates 1 and 2: --config wins
// over $GOJIRA_CONFIG_FILE.
func TestDiscoverConfigFile_PrecedenceExplicitOverEnv(t *testing.T) {
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "flag.yaml")
	envPath := filepath.Join(dir, "env.yaml")
	writeFile(t, flagPath)
	writeFile(t, envPath)

	r := NewXDGResolver(
		envLookup(map[string]string{EnvGojiraConfigFile: envPath}),
		errHomeDir,
	)
	got, found := r.DiscoverConfigFile(flagPath)
	assert.True(t, found)
	assert.Equal(t, flagPath, got)
}

// TestDiscoverConfigFile_PrecedenceEnvOverCwd asserts candidate 2
// beats candidate 3 when both exist. Combined with the test above,
// the full precedence chain is pinned.
func TestDiscoverConfigFile_PrecedenceEnvOverCwd(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env.yaml")
	writeFile(t, envPath)

	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, LocalConfigFileName))
	t.Chdir(cwd)

	r := NewXDGResolver(
		envLookup(map[string]string{EnvGojiraConfigFile: envPath}),
		errHomeDir,
	)
	got, found := r.DiscoverConfigFile("")
	assert.True(t, found)
	assert.Equal(t, envPath, got)
}

// TestDiscoverConfigFile_PrecedenceCwdOverGlobal asserts candidate 3
// beats candidate 4 when both exist.
func TestDiscoverConfigFile_PrecedenceCwdOverGlobal(t *testing.T) {
	xdg := t.TempDir()
	writeFile(t, filepath.Join(xdg, AppName, GlobalConfigFileName))

	cwd := t.TempDir()
	cwdPath := filepath.Join(cwd, LocalConfigFileName)
	writeFile(t, cwdPath)
	t.Chdir(cwd)

	r := NewXDGResolver(
		envLookup(map[string]string{EnvXDGConfigHome: xdg}),
		errHomeDir,
	)
	got, found := r.DiscoverConfigFile("")
	assert.True(t, found)
	assert.Equal(t, cwdPath, got)
}

// TestNilResolver_SafeAndEmpty asserts a nil receiver does not panic
// and returns empty/false. This keeps LoadApp robust if a caller
// passes a zero LoadOptions{Resolver: nil} accidentally; the safety
// net is documented but undertested-against-panics by design.
func TestNilResolver_SafeAndEmpty(t *testing.T) {
	var r *XDGResolver
	assert.Equal(t, "", r.GlobalConfigFile())
	got, found := r.DiscoverConfigFile("")
	assert.Equal(t, "", got)
	assert.False(t, found)
}
