package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validCanonicalEnv returns a map containing every required
// canonical GOJIRA_* key set to a valid value. Tests scope-in on a
// single rule by mutating one entry of the returned map.
func validCanonicalEnv() map[string]string {
	return map[string]string{
		"GOJIRA_JIRA_BASE_URL":  "https://example.atlassian.net",
		"GOJIRA_JIRA_EMAIL":     "me@example.com",
		"GOJIRA_JIRA_API_TOKEN": "tok-123",
		"GOJIRA_OUTPUT_DIR":     "/tmp/out",
	}
}

// validPRDYAML is the PRD §2.3 example with a non-empty api_token so
// the file path alone can satisfy the Layer-2 required-fields check
// when the env layer is absent. Tests that want the env layer to win
// override fields per case.
const validPRDYAML = `schema: gojira.config.v1
jira:
  base_url: https://file.atlassian.net
  email: file@example.com
  api_token: file-token
output:
  dir: ./jira-mirror
crawl:
  depth_limit: 0
  issue_cap: 500
  time_cap_seconds: 0
  concurrency: 3
  refetch: false
  include_comments: false
  include_children: true
  child_search_limit: 100
  epic_link_field: ""
  include_dev_status: true
  dev_status_applications: [GitHub]
  dev_status_data_types: [pullrequest, branch, commit, repository, build]
  render_null_custom_fields: false
log:
  level: info
  format: text
`

// TestLoadApp_EmptyOptionsHitsRequired asserts the zero-value
// LoadOptions produces a DefaultApp() and then fails Layer 2 with
// ErrMissingRequired because Jira credentials and output.dir are
// not embedded defaults. This is the documented "no input" path.
func TestLoadApp_EmptyOptionsHitsRequired(t *testing.T) {
	_, err := LoadApp(LoadOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissingRequired,
		"empty cascade must fail with ErrMissingRequired on Jira creds")
}

// TestLoadApp_EnvOnly_Canonical asserts a complete env-only load
// with canonical keys produces a valid App whose ToConfig matches a
// hand-built Config. This is the production-CLI common case.
func TestLoadApp_EnvOnly_Canonical(t *testing.T) {
	env := validCanonicalEnv()
	app, err := LoadApp(LoadOptions{Env: env})
	require.NoError(t, err)

	got := app.ToConfig()
	assert.Equal(t, "https://example.atlassian.net", got.Site)
	assert.Equal(t, "me@example.com", got.User)
	assert.Equal(t, "tok-123", got.Token)
	assert.Equal(t, "/tmp/out", got.OutputDir)

	// Defaults survive the env pass for everything not in env.
	assert.Equal(t, 500, got.IssueCap, "embedded default IssueCap survives")
	assert.Equal(t, 3, got.Concurrency, "embedded default Concurrency survives")
	assert.True(t, got.IncludeChildren, "embedded default IncludeChildren survives")
	assert.Equal(t, "info", got.LogLevel, "embedded default LogLevel survives")
	assert.Equal(t, "text", got.LogFormat, "embedded default LogFormat survives")
	assert.Equal(t,
		[]string{"pullrequest", "branch", "commit", "repository", "build"},
		got.DevStatusDataTypes,
		"embedded default DevStatusDataTypes survives")

	// And the schema gets backfilled to SchemaVersion when env-only.
	assert.Equal(t, SchemaVersion, app.Schema)
}

// TestLoadApp_EnvOnly_AliasesOnly asserts that supplying ONLY the
// deprecated v0.1 flat keys populates the canonical Phase 0 fields.
// This is the backward-compatibility path: an existing user's
// shell environment continues to work unchanged.
func TestLoadApp_EnvOnly_AliasesOnly(t *testing.T) {
	env := map[string]string{
		"GOJIRA_SITE":       "https://alias.atlassian.net",
		"GOJIRA_USER":       "alias@example.com",
		"GOJIRA_TOKEN":      "alias-token",
		"GOJIRA_OUTPUT_DIR": "/tmp/aliased",
	}
	app, err := LoadApp(LoadOptions{Env: env})
	require.NoError(t, err)

	assert.Equal(t, "https://alias.atlassian.net", app.Jira.BaseURL)
	assert.Equal(t, "alias@example.com", app.Jira.Email)
	assert.Equal(t, "alias-token", app.Jira.APIToken)
	assert.Equal(t, "/tmp/aliased", app.Output.Dir)
}

// TestLoadApp_EnvOnly_CanonicalBeatsAlias asserts the documented
// precedence rule: when both the canonical and the alias keys are
// set, canonical wins. The alias is silently ignored at the loader
// level (a future deprecation warning belongs in the CLI).
func TestLoadApp_EnvOnly_CanonicalBeatsAlias(t *testing.T) {
	env := map[string]string{
		"GOJIRA_SITE":           "https://alias.example.com",
		"GOJIRA_JIRA_BASE_URL":  "https://canonical.example.com",
		"GOJIRA_JIRA_EMAIL":     "me@example.com",
		"GOJIRA_JIRA_API_TOKEN": "tok-123",
		"GOJIRA_OUTPUT_DIR":     "/tmp/out",
	}
	app, err := LoadApp(LoadOptions{Env: env})
	require.NoError(t, err)
	assert.Equal(t, "https://canonical.example.com", app.Jira.BaseURL,
		"canonical GOJIRA_JIRA_BASE_URL must beat deprecated GOJIRA_SITE")
}

// TestLoadApp_FileOnly asserts a valid YAML file with credentials
// passes the cascade with no env layer. The file's values land on
// the struct verbatim because no env layer overrides them.
func TestLoadApp_FileOnly(t *testing.T) {
	app, err := LoadApp(LoadOptions{
		YAML: strings.NewReader(validPRDYAML),
	})
	require.NoError(t, err)
	assert.Equal(t, "https://file.atlassian.net", app.Jira.BaseURL)
	assert.Equal(t, "file@example.com", app.Jira.Email)
	assert.Equal(t, "file-token", app.Jira.APIToken)
	assert.Equal(t, "./jira-mirror", app.Output.Dir)
	assert.Equal(t, "info", app.Log.Level)
	assert.Equal(t, "text", app.Log.Format)
	assert.Equal(t, []string{"GitHub"}, app.Crawl.DevStatusApplications)
}

// TestLoadApp_EnvOverridesFile asserts the cascade precedence: a
// field present in the env map wins over the same field in the
// YAML file, and a field absent from the env preserves the file's
// value (which itself wins over the embedded default).
func TestLoadApp_EnvOverridesFile(t *testing.T) {
	env := map[string]string{
		"GOJIRA_JIRA_BASE_URL": "https://env.atlassian.net",
		// Note: no GOJIRA_OUTPUT_DIR — file's "./jira-mirror" must survive.
	}
	app, err := LoadApp(LoadOptions{
		YAML: strings.NewReader(validPRDYAML),
		Env:  env,
	})
	require.NoError(t, err)
	assert.Equal(t, "https://env.atlassian.net", app.Jira.BaseURL,
		"env value must override the file's base_url")
	assert.Equal(t, "file@example.com", app.Jira.Email,
		"file value must survive when env does not override")
	assert.Equal(t, "./jira-mirror", app.Output.Dir,
		"file's output.dir must survive when env does not override")
}

// TestLoadApp_PartialYAML asserts a YAML file that only sets a
// handful of fields leaves the others at their embedded defaults.
// This is the "minimal user config" path — the user opts into the
// few knobs they care about and inherits everything else.
func TestLoadApp_PartialYAML(t *testing.T) {
	const partial = `schema: gojira.config.v1
jira:
  base_url: https://partial.atlassian.net
  email: partial@example.com
  api_token: partial-tok
output:
  dir: /tmp/partial
log:
  level: debug
`
	app, err := LoadApp(LoadOptions{YAML: strings.NewReader(partial)})
	require.NoError(t, err)

	// File set:
	assert.Equal(t, "https://partial.atlassian.net", app.Jira.BaseURL)
	assert.Equal(t, "debug", app.Log.Level)
	// Defaults preserved (not in file, not in env):
	assert.Equal(t, "text", app.Log.Format, "Log.Format default preserved")
	assert.Equal(t, 500, app.Crawl.IssueCap, "Crawl.IssueCap default preserved")
	assert.Equal(t, []string{"GitHub"}, app.Crawl.DevStatusApplications)
}

// TestLoadApp_EmptyYAMLReader asserts an empty (or whitespace-only)
// YAML reader is a no-op for the file layer; defaults remain.
// Combined with a full env, the cascade still completes.
func TestLoadApp_EmptyYAMLReader(t *testing.T) {
	app, err := LoadApp(LoadOptions{
		YAML: strings.NewReader("   \n   "),
		Env:  validCanonicalEnv(),
	})
	require.NoError(t, err)
	// Defaults are still in place where env doesn't override.
	assert.Equal(t, 500, app.Crawl.IssueCap)
	assert.Equal(t, "https://example.atlassian.net", app.Jira.BaseURL)
}

// TestLoadApp_SchemaValidation_UnknownTopLevelKey asserts Layer 1
// rejects YAML with an unknown top-level key. The schema's
// additionalProperties:false constraint makes typos like "jria"
// fatal rather than silent.
func TestLoadApp_SchemaValidation_UnknownTopLevelKey(t *testing.T) {
	const badYAML = `schema: gojira.config.v1
jria:
  base_url: https://typo.example.com
`
	_, err := LoadApp(LoadOptions{YAML: strings.NewReader(badYAML)})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue,
		"unknown top-level key must wrap ErrInvalidValue (Layer 1)")
}

// TestLoadApp_SchemaValidation_BadLogLevel asserts Layer 1 rejects
// an invalid enum value (e.g. log.level: trace). The Layer-2 check
// catches the same case for App-struct callers; Layer 1 catches it
// against the raw document before envext gets near it.
func TestLoadApp_SchemaValidation_BadLogLevel(t *testing.T) {
	const badYAML = `schema: gojira.config.v1
jira:
  base_url: https://x.atlassian.net
  email: x@example.com
  api_token: tok
output:
  dir: /tmp/x
log:
  level: trace
`
	_, err := LoadApp(LoadOptions{YAML: strings.NewReader(badYAML)})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue)
}

// TestLoadApp_MissingRequired_NoCredentials asserts Layer 2
// rejects a cascade that never supplies the Jira credentials,
// regardless of input order. The error wraps ErrMissingRequired
// so callers' existing failure-class checks keep working.
func TestLoadApp_MissingRequired_NoCredentials(t *testing.T) {
	env := map[string]string{
		"GOJIRA_OUTPUT_DIR": "/tmp/out",
		// No Jira credentials at all.
	}
	_, err := LoadApp(LoadOptions{Env: env})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissingRequired)
}

// TestLoadAppFromEnv_Convenience asserts the LoadAppFromEnv
// convenience is equivalent to LoadApp with only the Env field set.
// It exists so the most common call site reads naturally.
func TestLoadAppFromEnv_Convenience(t *testing.T) {
	env := validCanonicalEnv()
	a1, err := LoadApp(LoadOptions{Env: env})
	require.NoError(t, err)
	a2, err := LoadAppFromEnv(env)
	require.NoError(t, err)
	assert.Equal(t, a1, a2)
}

// TestLoadApp_CompleteCascade_ToConfigEquivalence asserts a fully
// valid env produces a Config equivalent to a hand-built one. This
// pins the App→Config bridge invariant end-to-end through the
// cascade (the same invariant Phase 1 covers for ToConfig directly,
// now exercised via the loader path).
func TestLoadApp_CompleteCascade_ToConfigEquivalence(t *testing.T) {
	env := map[string]string{
		"GOJIRA_JIRA_BASE_URL":   "https://example.atlassian.net",
		"GOJIRA_JIRA_EMAIL":      "me@example.com",
		"GOJIRA_JIRA_API_TOKEN":  "tok",
		"GOJIRA_OUTPUT_DIR":      "/tmp/out",
		"GOJIRA_CRAWL_ISSUE_CAP": "42",
		"GOJIRA_LOG_LEVEL":       "debug",
	}
	app, err := LoadApp(LoadOptions{Env: env})
	require.NoError(t, err)

	got := app.ToConfig()
	assert.Equal(t, "https://example.atlassian.net", got.Site)
	assert.Equal(t, "me@example.com", got.User)
	assert.Equal(t, "tok", got.Token)
	assert.Equal(t, "/tmp/out", got.OutputDir)
	assert.Equal(t, 42, got.IssueCap, "env override applied")
	assert.Equal(t, "debug", got.LogLevel, "env override applied")
	assert.Equal(t, 3, got.Concurrency, "untouched default preserved")
}

// TestDecodeYAML_NilReader asserts the YAML decoder is a safe no-op
// when given a nil reader. The loader relies on this so the
// "no-file" path needs no extra branching.
func TestDecodeYAML_NilReader(t *testing.T) {
	app := DefaultApp()
	require.NoError(t, decodeYAML(nil, &app))
	assert.Equal(t, DefaultApp(), app, "nil reader must leave App untouched")
}

// TestDecodeYAML_UnknownKey asserts the yaml.v3 KnownFields(true)
// pass rejects unknown keys at the file-layer struct decode (the
// second decode pass, distinct from the Layer-1 schema validation
// against the map). This is defense-in-depth.
func TestDecodeYAML_UnknownKey(t *testing.T) {
	const bad = `schema: gojira.config.v1
bogus_key: 1
`
	app := DefaultApp()
	err := decodeYAML(strings.NewReader(bad), &app)
	require.Error(t, err, "yaml KnownFields(true) must reject unknown keys")
}

// TestDecodeYAMLToMap_NonObjectRoot asserts the schema-input
// decoder rejects a YAML document whose root is not a mapping. The
// JSON Schema is rooted at type:object, so a list or scalar root
// would fail Layer 1 too; failing fast at decode time gives a
// cleaner error message.
func TestDecodeYAMLToMap_NonObjectRoot(t *testing.T) {
	_, err := decodeYAMLToMap(strings.NewReader("- one\n- two\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue)
}
