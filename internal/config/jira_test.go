package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultJiraSettings asserts that the zero-value default leaves
// every credential field empty; Jira credentials are not embedded.
func TestDefaultJiraSettings(t *testing.T) {
	got := DefaultJiraSettings()
	assert.Equal(t, "", got.BaseURL, "BaseURL")
	assert.Equal(t, "", got.Email, "Email")
	assert.Equal(t, "", got.APIToken, "APIToken")
	assert.Equal(t, JiraSettings{}, got, "DefaultJiraSettings is the zero value")
}

// TestJiraSettings_Effective_Precedence covers the three-tier
// precedence rule (cli > configured > "") for all three Effective*
// methods, including the nil-receiver path.
func TestJiraSettings_Effective_Precedence(t *testing.T) {
	configured := JiraSettings{
		BaseURL:  "https://configured.atlassian.net",
		Email:    "configured@example.com",
		APIToken: "configured-token",
	}

	tests := []struct {
		name           string
		recv           *JiraSettings
		cli            string
		wantBaseURL    string
		wantEmail      string
		wantAPIToken   string
		baseURLCLI     string
		emailCLI       string
		apiTokenCLI    string
		wantBaseURLOut string
		wantEmailOut   string
		wantTokenOut   string
	}{
		{
			name:           "cli wins over configured",
			recv:           &configured,
			baseURLCLI:     "https://cli.atlassian.net",
			emailCLI:       "cli@example.com",
			apiTokenCLI:    "cli-token",
			wantBaseURLOut: "https://cli.atlassian.net",
			wantEmailOut:   "cli@example.com",
			wantTokenOut:   "cli-token",
		},
		{
			name:           "configured wins when cli empty",
			recv:           &configured,
			wantBaseURLOut: "https://configured.atlassian.net",
			wantEmailOut:   "configured@example.com",
			wantTokenOut:   "configured-token",
		},
		{
			name:           "empty when neither cli nor configured",
			recv:           &JiraSettings{},
			wantBaseURLOut: "",
			wantEmailOut:   "",
			wantTokenOut:   "",
		},
		{
			name:           "nil receiver tolerated",
			recv:           nil,
			wantBaseURLOut: "",
			wantEmailOut:   "",
			wantTokenOut:   "",
		},
		{
			name:           "nil receiver still honors cli",
			recv:           nil,
			baseURLCLI:     "https://cli.atlassian.net",
			emailCLI:       "cli@example.com",
			apiTokenCLI:    "cli-token",
			wantBaseURLOut: "https://cli.atlassian.net",
			wantEmailOut:   "cli@example.com",
			wantTokenOut:   "cli-token",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantBaseURLOut, tc.recv.EffectiveBaseURL(tc.baseURLCLI), "EffectiveBaseURL")
			assert.Equal(t, tc.wantEmailOut, tc.recv.EffectiveEmail(tc.emailCLI), "EffectiveEmail")
			assert.Equal(t, tc.wantTokenOut, tc.recv.EffectiveAPIToken(tc.apiTokenCLI), "EffectiveAPIToken")
		})
	}
}

// TestJiraSettings_StructTags asserts the GOJIRA_-prefixed env tags
// and the snake_case YAML/JSON tags are present and correct on every
// field. The Phase 0 PRD §2.2 mandates these exact canonical keys,
// and the App-root `env:",nested"` (empty prefix) only works if every
// leaf carries the full GOJIRA_ prefix here.
func TestJiraSettings_StructTags(t *testing.T) {
	rt := reflect.TypeOf(JiraSettings{})

	cases := []struct {
		field    string
		wantEnv  string
		wantYAML string
		wantJSON string
	}{
		{"BaseURL", "GOJIRA_JIRA_BASE_URL", "base_url", "base_url"},
		{"Email", "GOJIRA_JIRA_EMAIL", "email", "email"},
		{"APIToken", "GOJIRA_JIRA_API_TOKEN", "api_token", "api_token"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			sf, ok := rt.FieldByName(tc.field)
			if !ok {
				t.Fatalf("field %q missing from JiraSettings", tc.field)
			}
			assert.Equal(t, tc.wantEnv, sf.Tag.Get("env"), "env tag")
			assert.Equal(t, tc.wantYAML, sf.Tag.Get("yaml"), "yaml tag")
			assert.Equal(t, tc.wantJSON, sf.Tag.Get("json"), "json tag")
		})
	}
}
