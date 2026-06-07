package config

// JiraSettings configures the Jira Cloud connection used by the
// gojira client. It is one of the four standalone entity structs
// composed under [App]; it can also be used directly by any component
// that only needs Jira-connection settings.
//
// The struct tags declare three configuration layers:
//
//   - yaml: how the field is named under the "jira:" section of
//     gojira.yaml (see PRD §2.3 example).
//   - json: how the field is named when the App is serialised to
//     JSON (e.g. for logging or diagnostics).
//   - env: the canonical GOJIRA_*-prefixed environment variable used
//     when [envext] parses configuration over a file-populated App.
//     Every env key is hand-named with the full GOJIRA_ prefix so the
//     parent App can use `env:",nested"` (empty prefix) without
//     introducing nested-prefix stutter such as GOJIRA_JIRA_JIRA_*.
//
// The empty zero value is a meaningful "no Jira configured" state;
// validation in a later phase enforces that BaseURL/Email/APIToken
// are non-empty for any operation that actually contacts Jira.
type JiraSettings struct {
	// BaseURL is the Jira Cloud base URL, e.g.
	// "https://example.atlassian.net". Sourced from
	// GOJIRA_JIRA_BASE_URL or the "jira.base_url" YAML key.
	BaseURL string `yaml:"base_url" json:"base_url" env:"GOJIRA_JIRA_BASE_URL"`

	// Email is the Atlassian account email used for Basic auth.
	// Sourced from GOJIRA_JIRA_EMAIL or the "jira.email" YAML key.
	Email string `yaml:"email" json:"email" env:"GOJIRA_JIRA_EMAIL"`

	// APIToken is the Atlassian API token used for Basic auth.
	// Sourced from GOJIRA_JIRA_API_TOKEN or the "jira.api_token"
	// YAML key. Treat as a secret; never log or render.
	APIToken string `yaml:"api_token" json:"api_token" env:"GOJIRA_JIRA_API_TOKEN"`
}

// DefaultJiraSettings returns the zero-valued [JiraSettings]. Jira
// credentials have no sensible embedded default; the loader cascade
// expects them to be supplied via the config file, environment, or
// CLI flags.
func DefaultJiraSettings() JiraSettings {
	return JiraSettings{}
}

// EffectiveBaseURL returns the Jira base URL with precedence
// cli > configured > "". A nil receiver is tolerated and treated as
// an empty configured value, mirroring the tobi internal/config
// pattern so callers can dereference Effective* without first
// nil-checking the entity.
func (j *JiraSettings) EffectiveBaseURL(cli string) string {
	if cli != "" {
		return cli
	}
	if j != nil && j.BaseURL != "" {
		return j.BaseURL
	}
	return ""
}

// EffectiveEmail returns the Atlassian account email with precedence
// cli > configured > "". A nil receiver is tolerated.
func (j *JiraSettings) EffectiveEmail(cli string) string {
	if cli != "" {
		return cli
	}
	if j != nil && j.Email != "" {
		return j.Email
	}
	return ""
}

// EffectiveAPIToken returns the Atlassian API token with precedence
// cli > configured > "". A nil receiver is tolerated.
func (j *JiraSettings) EffectiveAPIToken(cli string) string {
	if cli != "" {
		return cli
	}
	if j != nil && j.APIToken != "" {
		return j.APIToken
	}
	return ""
}
