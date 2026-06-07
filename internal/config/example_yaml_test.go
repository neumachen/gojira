package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exampleYAMLPath is the repo-relative location of the shipped example
// config, expressed relative to this package's directory. The file is
// authored at the repo root (`gojira.example.yaml`) so the IDE
// language-server "$schema" comment can use a relative path to the
// embedded schema.
const exampleYAMLPath = "../../gojira.example.yaml"

// TestExampleYAML_OnDisk asserts the shipped example file is present
// where the README and AGENTS docs claim it lives. A missing file
// here means downstream documentation is broken before any user runs
// gojira.
func TestExampleYAML_OnDisk(t *testing.T) {
	info, err := os.Stat(exampleYAMLPath)
	require.NoError(t, err, "gojira.example.yaml not found at repo root")
	assert.True(t, info.Mode().IsRegular(), "gojira.example.yaml must be a regular file")
}

// TestExampleYAML_HasSchemaHeader pins the IDE language-server
// directive that gives users (and AI agents) live autocomplete /
// validation against the embedded schema. Without this header, the
// editor experience silently regresses to "plain YAML".
func TestExampleYAML_HasSchemaHeader(t *testing.T) {
	bytes, err := os.ReadFile(exampleYAMLPath)
	require.NoError(t, err)
	first := strings.SplitN(string(bytes), "\n", 2)[0]
	assert.Contains(t, first, "yaml-language-server: $schema=",
		"first line must be the YAML language-server $schema directive")
	assert.Contains(t, first, "internal/config/config.schema.json",
		"directive must point at the embedded config schema")
}

// TestExampleYAML_PassesSchemaValidation is the drift guard: the
// shipped example MUST decode to a map[string]any that passes the
// embedded JSON Schema (Layer 1). Any change to the schema that
// regresses the example, or vice-versa, fails here loudly.
//
// This test ALSO exercises the schema-version check by asserting the
// decoded document carries SchemaVersion; a future v2 bump must keep
// the example aligned.
func TestExampleYAML_PassesSchemaValidation(t *testing.T) {
	bytes, err := os.ReadFile(exampleYAMLPath)
	require.NoError(t, err)

	rawMap, err := decodeYAMLToMap(strings.NewReader(string(bytes)))
	require.NoError(t, err, "example YAML must decode to a mapping")
	require.NotNil(t, rawMap, "decoded example must not be nil")

	require.NoError(t, ValidateRawConfig(rawMap),
		"example YAML must pass embedded JSON Schema validation")

	gotSchema, _ := rawMap["schema"].(string)
	assert.Equal(t, SchemaVersion, gotSchema,
		"example YAML schema identifier must match current SchemaVersion")
}

// TestExampleYAML_DecodesIntoApp asserts the example also decodes
// cleanly into the App struct via the same path the loader uses.
// This catches the "schema accepts it but Go's yaml.v3 rejects it"
// edge case (yaml.v3 KnownFields(true) is strict about Go-side tag
// names, which the schema cannot enforce).
//
// The decoded App is asserted to expose the values authored in the
// example so future edits that change the documented defaults are
// surfaced as a test failure.
func TestExampleYAML_DecodesIntoApp(t *testing.T) {
	f, err := os.Open(exampleYAMLPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	app := DefaultApp()
	require.NoError(t, decodeYAML(f, &app),
		"example YAML must decode into App with KnownFields(true)")

	// Cross-check a representative subset; the full structure is
	// already covered by JSON-Schema validation above.
	assert.Equal(t, SchemaVersion, app.Schema)
	assert.Equal(t, "https://example.atlassian.net", app.Jira.BaseURL)
	assert.Equal(t, "me@example.com", app.Jira.Email)
	assert.Equal(t, "", app.Jira.APIToken,
		"example must not commit a non-empty api_token (secret-handling rule)")
	assert.Equal(t, "./jira-mirror", app.Output.Dir)
	assert.Equal(t, 500, app.Crawl.IssueCap)
	assert.Equal(t, 3, app.Crawl.Concurrency)
	assert.True(t, app.Crawl.IncludeChildren)
	assert.True(t, app.Crawl.IncludeDevStatus)
	assert.Equal(t, []string{"GitHub"}, app.Crawl.DevStatusApplications)
	assert.Equal(t,
		[]string{"pullrequest", "branch", "commit", "repository", "build"},
		app.Crawl.DevStatusDataTypes,
	)
	assert.Equal(t, "info", app.Log.Level)
	assert.Equal(t, "text", app.Log.Format)
}

// TestExampleYAML_FullCascadeWithToken asserts the example YAML +
// the canonical env-only secret combination passes the FULL cascade
// (Layer 1 + decode + env + Layer 2). This is the "what a real user
// experiences" path: file supplies everything except the secret,
// env supplies the secret, no flags. A regression here means a
// minimal configured machine can no longer load gojira.
func TestExampleYAML_FullCascadeWithToken(t *testing.T) {
	bytes, err := os.ReadFile(exampleYAMLPath)
	require.NoError(t, err)

	// We can't pass io.Reader directly to LoadApp + ConfigPath at
	// the same time (YAML wins and bypasses discovery). Use the
	// YAML reader path so the test is filesystem-free with respect
	// to discovery.
	app, err := LoadApp(LoadOptions{
		YAML: strings.NewReader(string(bytes)),
		Env: map[string]string{
			"GOJIRA_JIRA_API_TOKEN": "test-token-not-real",
		},
	})
	require.NoError(t, err, "example + canonical secret env must load cleanly")
	assert.Equal(t, "test-token-not-real", app.Jira.APIToken,
		"env must override the empty api_token in the file")
}

// TestExampleYAML_IsAtRepoRoot asserts the documented location
// matches reality: the file the README points users at lives at the
// repo root and the relative path the IDE directive uses resolves
// against that same root.
func TestExampleYAML_IsAtRepoRoot(t *testing.T) {
	abs, err := filepath.Abs(exampleYAMLPath)
	require.NoError(t, err)
	// Resolve symlinks just in case the workspace is checked out
	// through one; we only care about the basename + parent.
	resolved, err := filepath.EvalSymlinks(abs)
	require.NoError(t, err)
	assert.Equal(t, "gojira.example.yaml", filepath.Base(resolved))

	// internal/config/../.. lands at the repo root by construction;
	// the test passes simply by reading the file successfully.
	parent := filepath.Dir(resolved)
	_, err = os.Stat(filepath.Join(parent, "go.mod"))
	assert.NoError(t, err, "example must sit alongside go.mod (repo root)")
}
