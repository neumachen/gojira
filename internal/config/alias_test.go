package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveAliases_TableDriven exercises the canonical-wins
// precedence rule and the alias-fills-when-canonical-absent fallback
// across every documented alias key. The test is exhaustive on
// purpose: any new alias must be added here when the table grows.
func TestResolveAliases_TableDriven(t *testing.T) {
	type want struct {
		key   string
		value string
	}
	tests := []struct {
		name string
		in   map[string]string
		want []want
	}{
		{
			name: "empty input yields empty output",
			in:   nil,
			want: nil,
		},
		{
			name: "alias only -> canonical filled",
			in: map[string]string{
				"GOJIRA_SITE":  "https://alias.example.com",
				"GOJIRA_USER":  "alias@example.com",
				"GOJIRA_TOKEN": "alias-token",
			},
			want: []want{
				{"GOJIRA_JIRA_BASE_URL", "https://alias.example.com"},
				{"GOJIRA_JIRA_EMAIL", "alias@example.com"},
				{"GOJIRA_JIRA_API_TOKEN", "alias-token"},
				// Alias keys are preserved in the output for the
				// deprecation-warning path.
				{"GOJIRA_SITE", "https://alias.example.com"},
			},
		},
		{
			name: "canonical wins over alias",
			in: map[string]string{
				"GOJIRA_SITE":          "https://alias.example.com",
				"GOJIRA_JIRA_BASE_URL": "https://canonical.example.com",
			},
			want: []want{
				{"GOJIRA_JIRA_BASE_URL", "https://canonical.example.com"},
			},
		},
		{
			name: "empty canonical falls back to alias",
			in: map[string]string{
				"GOJIRA_SITE":          "https://alias.example.com",
				"GOJIRA_JIRA_BASE_URL": "",
			},
			want: []want{
				{"GOJIRA_JIRA_BASE_URL", "https://alias.example.com"},
			},
		},
		{
			name: "every crawl alias maps correctly",
			in: map[string]string{
				"GOJIRA_DEPTH_LIMIT":               "5",
				"GOJIRA_ISSUE_CAP":                 "100",
				"GOJIRA_TIME_CAP_SECONDS":          "60",
				"GOJIRA_CONCURRENCY":               "8",
				"GOJIRA_REFETCH":                   "true",
				"GOJIRA_INCLUDE_COMMENTS":          "true",
				"GOJIRA_INCLUDE_CHILDREN":          "false",
				"GOJIRA_CHILD_SEARCH_LIMIT":        "42",
				"GOJIRA_EPIC_LINK_FIELD":           "customfield_10014",
				"GOJIRA_INCLUDE_DEV_STATUS":        "false",
				"GOJIRA_DEV_STATUS_APPLICATIONS":   "GitHub",
				"GOJIRA_DEV_STATUS_DATA_TYPES":     "pullrequest",
				"GOJIRA_RENDER_NULL_CUSTOM_FIELDS": "true",
			},
			want: []want{
				{"GOJIRA_CRAWL_DEPTH_LIMIT", "5"},
				{"GOJIRA_CRAWL_ISSUE_CAP", "100"},
				{"GOJIRA_CRAWL_TIME_CAP_SECONDS", "60"},
				{"GOJIRA_CRAWL_CONCURRENCY", "8"},
				{"GOJIRA_CRAWL_REFETCH", "true"},
				{"GOJIRA_CRAWL_INCLUDE_COMMENTS", "true"},
				{"GOJIRA_CRAWL_INCLUDE_CHILDREN", "false"},
				{"GOJIRA_CRAWL_CHILD_SEARCH_LIMIT", "42"},
				{"GOJIRA_CRAWL_EPIC_LINK_FIELD", "customfield_10014"},
				{"GOJIRA_CRAWL_INCLUDE_DEV_STATUS", "false"},
				{"GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS", "GitHub"},
				{"GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES", "pullrequest"},
				{"GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS", "true"},
			},
		},
		{
			name: "unrelated keys pass through unchanged",
			in: map[string]string{
				"GOJIRA_OUTPUT_DIR": "/tmp/out",
				"GOJIRA_LOG_LEVEL":  "debug",
				"GOJIRA_LOG_FORMAT": "json",
				"GOJIRA_SCHEMA":     "gojira.config.v1",
			},
			want: []want{
				{"GOJIRA_OUTPUT_DIR", "/tmp/out"},
				{"GOJIRA_LOG_LEVEL", "debug"},
				{"GOJIRA_LOG_FORMAT", "json"},
				{"GOJIRA_SCHEMA", "gojira.config.v1"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveAliases(tc.in)
			require.NotNil(t, got, "ResolveAliases must always return a non-nil map")
			for _, w := range tc.want {
				assert.Equal(t, w.value, got[w.key],
					"key %q: want %q, got %q", w.key, w.value, got[w.key])
			}
		})
	}
}

// TestResolveAliases_InputNotMutated guarantees the resolver treats
// its input as read-only. Mutating the caller's map would be a
// nasty surprise for the CLI, which inspects the original env-map
// for deprecation warnings after calling ResolveAliases.
func TestResolveAliases_InputNotMutated(t *testing.T) {
	in := map[string]string{
		"GOJIRA_SITE": "https://alias.example.com",
	}
	snapshot := map[string]string{}
	for k, v := range in {
		snapshot[k] = v
	}
	_ = ResolveAliases(in)
	assert.Equal(t, snapshot, in, "ResolveAliases must not mutate its input")
}

// TestDeprecatedAliasKeys asserts the alias-key list is sorted and
// covers every alias the table recognises. The CLI uses this list
// for `--env-keys`-style output, so stability matters.
func TestDeprecatedAliasKeys(t *testing.T) {
	got := DeprecatedAliasKeys()
	require.NotEmpty(t, got)
	for i := 1; i < len(got); i++ {
		assert.Less(t, got[i-1], got[i],
			"DeprecatedAliasKeys must be sorted lexicographically")
	}
	// Spot-check that key v0.1 names are present.
	want := []string{
		"GOJIRA_SITE", "GOJIRA_USER", "GOJIRA_TOKEN",
		"GOJIRA_DEPTH_LIMIT", "GOJIRA_ISSUE_CAP",
		"GOJIRA_RENDER_NULL_CUSTOM_FIELDS",
	}
	for _, k := range want {
		assert.Contains(t, got, k, "alias %q must appear in DeprecatedAliasKeys", k)
	}
	assert.Equal(t, len(aliasToCanonical), len(got),
		"DeprecatedAliasKeys must cover every entry in aliasToCanonical")
}

// TestCanonicalForAlias covers the known/unknown cases used by the
// future deprecation-warning emitter.
func TestCanonicalForAlias(t *testing.T) {
	t.Run("known alias", func(t *testing.T) {
		got, ok := CanonicalForAlias("GOJIRA_SITE")
		require.True(t, ok)
		assert.Equal(t, "GOJIRA_JIRA_BASE_URL", got)
	})
	t.Run("unknown alias", func(t *testing.T) {
		got, ok := CanonicalForAlias("GOJIRA_NOT_AN_ALIAS")
		assert.False(t, ok)
		assert.Equal(t, "", got)
	})
	t.Run("canonical itself is not an alias", func(t *testing.T) {
		_, ok := CanonicalForAlias("GOJIRA_JIRA_BASE_URL")
		assert.False(t, ok,
			"canonical keys must not be reported as aliases of themselves")
	})
}
