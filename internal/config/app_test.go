package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaVersion pins the schema identifier to its PRD value. A
// change here is a deliberate, breaking schema bump — anyone touching
// it must also bump the validator in phase-2-validate-1.
func TestSchemaVersion(t *testing.T) {
	assert.Equal(t, "gojira.config.v1", SchemaVersion)
}

// TestDefaultApp asserts the root composition: SchemaVersion set,
// every entity is its DefaultX(), and ConfigFile is empty (the path
// resolver populates it later, never the file itself).
func TestDefaultApp(t *testing.T) {
	got := DefaultApp()
	assert.Equal(t, SchemaVersion, got.Schema, "Schema")
	assert.Equal(t, "", got.ConfigFile, "ConfigFile")
	assert.Equal(t, DefaultJiraSettings(), got.Jira, "Jira")
	assert.Equal(t, DefaultCrawlSettings(), got.Crawl, "Crawl")
	assert.Equal(t, DefaultOutputSettings(), got.Output, "Output")
	assert.Equal(t, DefaultLogSettings(), got.Log, "Log")
}

// TestApp_NestedEnvTags asserts that every entity field carries
// `env:",nested"` exactly. Any drift (an accidental prefix on the
// entity tag) would cause envext to prepend a prefix to every leaf
// key, breaking the GOJIRA_JIRA_BASE_URL contract documented in PRD
// §2.2.
func TestApp_NestedEnvTags(t *testing.T) {
	rt := reflect.TypeOf(App{})
	for _, name := range []string{"Jira", "Crawl", "Output", "Log"} {
		sf, ok := rt.FieldByName(name)
		require.True(t, ok, "field %q missing from App", name)
		assert.Equal(t, ",nested", sf.Tag.Get("env"),
			"entity %s must use empty-prefix nested env tag", name)
	}

	// And the two leaf fields on App itself carry their expected
	// canonical env keys.
	schemaField, ok := rt.FieldByName("Schema")
	require.True(t, ok)
	assert.Equal(t, "GOJIRA_SCHEMA", schemaField.Tag.Get("env"))
	configFileField, ok := rt.FieldByName("ConfigFile")
	require.True(t, ok)
	assert.Equal(t, "GOJIRA_CONFIG_FILE", configFileField.Tag.Get("env"))
	assert.Equal(t, "-", configFileField.Tag.Get("yaml"),
		"ConfigFile must not be sourced from the file itself")
}

// TestApp_ToConfig_DefaultsRoundTrip is the golden table test for
// the App → Config flattener. It asserts every documented Config
// default surfaces through ToConfig when the App is DefaultApp().
// This is the compatibility-bridge invariant: client.New,
// internal/crawl, internal/fetch, and the gojira facade keep
// observing the same Config they always have.
func TestApp_ToConfig_DefaultsRoundTrip(t *testing.T) {
	got := DefaultApp().ToConfig()

	// Jira block: no embedded credentials.
	assert.Equal(t, "", got.Site, "Site")
	assert.Equal(t, "", got.User, "User")
	assert.Equal(t, "", got.Token, "Token")

	// Output block: no embedded output directory.
	assert.Equal(t, "", got.OutputDir, "OutputDir")

	// Crawl block: every default mirrored from CrawlSettings.
	assert.Equal(t, 0, got.DepthLimit, "DepthLimit")
	assert.Equal(t, 500, got.IssueCap, "IssueCap")
	assert.Equal(t, 0, got.TimeCapSeconds, "TimeCapSeconds")
	assert.Equal(t, 3, got.Concurrency, "Concurrency")
	assert.False(t, got.Refetch, "Refetch")
	assert.False(t, got.IncludeComments, "IncludeComments")
	assert.True(t, got.IncludeChildren, "IncludeChildren")
	assert.Equal(t, 100, got.ChildSearchLimit, "ChildSearchLimit")
	assert.Equal(t, "", got.EpicLinkField, "EpicLinkField")
	assert.True(t, got.IncludeDevStatus, "IncludeDevStatus")
	assert.Equal(t, []string{"GitHub"}, got.DevStatusApplications, "DevStatusApplications")
	assert.Equal(t,
		[]string{"pullrequest", "branch", "commit", "repository", "build"},
		got.DevStatusDataTypes,
		"DevStatusDataTypes")
	assert.False(t, got.RenderNullCustomFields, "RenderNullCustomFields")

	// Log block.
	assert.Equal(t, "info", got.LogLevel, "LogLevel")
	assert.Equal(t, "text", got.LogFormat, "LogFormat")
}

// TestApp_ToConfig_FullyPopulated exercises the 1:1 field mapping
// with a fully populated App (every field set to a distinct,
// non-default value). The expectation is computed by hand from the
// authoritative source-of-fields table in the implementation plan;
// any drift in the mapping fails this test loudly.
func TestApp_ToConfig_FullyPopulated(t *testing.T) {
	a := App{
		Schema:     SchemaVersion,
		ConfigFile: "/etc/gojira/config.yaml",
		Jira: JiraSettings{
			BaseURL:  "https://example.atlassian.net",
			Email:    "me@example.com",
			APIToken: "tok-123",
		},
		Crawl: CrawlSettings{
			DepthLimit:             7,
			IssueCap:               123,
			TimeCapSeconds:         600,
			Concurrency:            8,
			Refetch:                true,
			IncludeComments:        true,
			IncludeChildren:        false,
			ChildSearchLimit:       42,
			EpicLinkField:          "customfield_10014",
			IncludeDevStatus:       false,
			DevStatusApplications:  []string{"Bitbucket", "GitLab"},
			DevStatusDataTypes:     []string{"pullrequest", "commit"},
			RenderNullCustomFields: true,
		},
		Output: OutputSettings{Dir: "/tmp/jira-mirror"},
		Log:    LogSettings{Level: "debug", Format: "json"},
	}

	want := Config{
		Site:                   "https://example.atlassian.net",
		User:                   "me@example.com",
		Token:                  "tok-123",
		OutputDir:              "/tmp/jira-mirror",
		DepthLimit:             7,
		IssueCap:               123,
		TimeCapSeconds:         600,
		Concurrency:            8,
		Refetch:                true,
		LogLevel:               "debug",
		IncludeComments:        true,
		LogFormat:              "json",
		IncludeChildren:        false,
		ChildSearchLimit:       42,
		EpicLinkField:          "customfield_10014",
		IncludeDevStatus:       false,
		DevStatusApplications:  []string{"Bitbucket", "GitLab"},
		RenderNullCustomFields: true,
		DevStatusDataTypes:     []string{"pullrequest", "commit"},
	}

	assert.Equal(t, want, a.ToConfig())
}

// TestApp_ToConfig_SlicesAreCopied guarantees the flattener does not
// alias slice fields back into the source App. Without copying,
// downstream packages mutating Config.DevStatusApplications would
// silently mutate App.Crawl.DevStatusApplications.
func TestApp_ToConfig_SlicesAreCopied(t *testing.T) {
	a := DefaultApp()
	c := a.ToConfig()

	require.NotEmpty(t, c.DevStatusApplications)
	require.NotEmpty(t, c.DevStatusDataTypes)

	c.DevStatusApplications[0] = "Mutated"
	c.DevStatusDataTypes[0] = "Mutated"

	assert.Equal(t, []string{"GitHub"}, a.Crawl.DevStatusApplications,
		"App slice must not alias Config slice")
	assert.Equal(t,
		[]string{"pullrequest", "branch", "commit", "repository", "build"},
		a.Crawl.DevStatusDataTypes,
		"App slice must not alias Config slice")
}
