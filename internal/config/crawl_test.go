package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultCrawlSettings asserts every documented default value on
// CrawlSettings exactly matches PRD §2.1 and the existing flat Config
// defaults. The list is exhaustive on purpose: any new field must
// either be added here with its documented default, or the test will
// fail loudly when the corresponding ToConfig field mapping is added
// in phase-2-validate-2.
func TestDefaultCrawlSettings(t *testing.T) {
	got := DefaultCrawlSettings()

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
		"DevStatusDataTypes",
	)
	assert.False(t, got.RenderNullCustomFields, "RenderNullCustomFields")
}

// TestDefaultCrawlSettings_SlicesAreIndependent guarantees mutating
// one DefaultCrawlSettings() result does not leak into the next call.
// The defaults hold the only embedded slice values in the App tree,
// so this invariant matters once multiple LoadApp calls share the
// package state.
func TestDefaultCrawlSettings_SlicesAreIndependent(t *testing.T) {
	a := DefaultCrawlSettings()
	require.NotEmpty(t, a.DevStatusApplications)
	require.NotEmpty(t, a.DevStatusDataTypes)

	a.DevStatusApplications[0] = "Bitbucket"
	a.DevStatusDataTypes[0] = "commit"

	b := DefaultCrawlSettings()
	assert.Equal(t, []string{"GitHub"}, b.DevStatusApplications,
		"second call must not see first call's mutation")
	assert.Equal(t,
		[]string{"pullrequest", "branch", "commit", "repository", "build"},
		b.DevStatusDataTypes,
		"second call must not see first call's mutation")
}

// TestCrawlSettings_StructTags asserts every field carries the
// canonical GOJIRA_CRAWL_* env tag and snake_case yaml/json tags
// required by PRD §2.2. Hand-naming each env key (rather than
// relying on a nested prefix) is what lets App use `env:",nested"`
// without producing GOJIRA_CRAWL_CRAWL_* stutter.
func TestCrawlSettings_StructTags(t *testing.T) {
	rt := reflect.TypeOf(CrawlSettings{})

	cases := []struct {
		field    string
		wantEnv  string
		wantYAML string
		wantJSON string
	}{
		{"DepthLimit", "GOJIRA_CRAWL_DEPTH_LIMIT", "depth_limit", "depth_limit"},
		{"IssueCap", "GOJIRA_CRAWL_ISSUE_CAP", "issue_cap", "issue_cap"},
		{"TimeCapSeconds", "GOJIRA_CRAWL_TIME_CAP_SECONDS", "time_cap_seconds", "time_cap_seconds"},
		{"Concurrency", "GOJIRA_CRAWL_CONCURRENCY", "concurrency", "concurrency"},
		{"Refetch", "GOJIRA_CRAWL_REFETCH", "refetch", "refetch"},
		{"IncludeComments", "GOJIRA_CRAWL_INCLUDE_COMMENTS", "include_comments", "include_comments"},
		{"IncludeChildren", "GOJIRA_CRAWL_INCLUDE_CHILDREN", "include_children", "include_children"},
		{"ChildSearchLimit", "GOJIRA_CRAWL_CHILD_SEARCH_LIMIT", "child_search_limit", "child_search_limit"},
		{"EpicLinkField", "GOJIRA_CRAWL_EPIC_LINK_FIELD", "epic_link_field", "epic_link_field"},
		{"IncludeDevStatus", "GOJIRA_CRAWL_INCLUDE_DEV_STATUS", "include_dev_status", "include_dev_status"},
		{"DevStatusApplications", "GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS", "dev_status_applications", "dev_status_applications"},
		{"DevStatusDataTypes", "GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES", "dev_status_data_types", "dev_status_data_types"},
		{"RenderNullCustomFields", "GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS", "render_null_custom_fields", "render_null_custom_fields"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			sf, ok := rt.FieldByName(tc.field)
			if !ok {
				t.Fatalf("field %q missing from CrawlSettings", tc.field)
			}
			assert.Equal(t, tc.wantEnv, sf.Tag.Get("env"), "env tag")
			assert.Equal(t, tc.wantYAML, sf.Tag.Get("yaml"), "yaml tag")
			assert.Equal(t, tc.wantJSON, sf.Tag.Get("json"), "json tag")
		})
	}
}
