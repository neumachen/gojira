package config

// SchemaVersion is the current configuration schema identifier. A
// loaded [App] must carry exactly this value in its Schema field, or
// validation in phase-2 rejects it. Bumping the version is the
// mechanism by which gojira will signal a breaking change to the
// gojira.yaml schema.
const SchemaVersion = "gojira.config.v1"

// App is the root, app-level configuration tree for gojira. It is a
// composition of independent, standalone entity structs ([JiraSettings],
// [CrawlSettings], [OutputSettings], [LogSettings]), each of which can
// also be used in isolation by the component that owns it.
//
// App is the authoring surface for the loader cascade documented in
// PRD §2.1: embedded defaults < YAML file < GOJIRA_ environment
// variables (parsed by envext) < CLI flags. Downstream packages
// (client, internal/crawl, internal/fetch, the gojira facade)
// continue to read the existing flat [Config] type; App flattens onto
// Config via [App.ToConfig] so the v0.1 surface remains stable.
//
// Each entity field carries `env:",nested"` so envext recurses into
// it without prepending a prefix. Combined with the hand-named
// GOJIRA_-prefixed env keys on every leaf field, this keeps the
// environment-variable surface flat (e.g. GOJIRA_JIRA_BASE_URL, not
// GOJIRA_JIRA_JIRA_BASE_URL) while the Go struct stays cleanly
// nested. The envext nested helper, with an empty parent prefix and
// an empty per-field envName (the part before the comma in the tag),
// preserves each leaf's declared env key verbatim — verified in
// internal/config/app_test.go.
type App struct {
	// Schema is the configuration schema identifier; must equal
	// [SchemaVersion] when validated. Sourced from
	// GOJIRA_SCHEMA or the top-level "schema:" YAML key.
	Schema string `yaml:"schema" json:"schema" env:"GOJIRA_SCHEMA"`

	// ConfigFile is the resolved path to the gojira.yaml file
	// that populated this App, or empty when no file was used.
	// It is populated by the loader after path resolution, never
	// by the file itself (hence yaml:"-"). Exposed for diagnostics
	// (e.g. logging the effective config source) and sourced from
	// GOJIRA_CONFIG_FILE when used as an env-only signal.
	ConfigFile string `yaml:"-" json:"-" env:"GOJIRA_CONFIG_FILE"`

	// Jira holds the Jira Cloud connection settings.
	Jira JiraSettings `yaml:"jira" json:"jira" env:",nested"`

	// Crawl holds the crawl-orchestrator tuning knobs.
	Crawl CrawlSettings `yaml:"crawl" json:"crawl" env:",nested"`

	// Output holds the Markdown output settings.
	Output OutputSettings `yaml:"output" json:"output" env:",nested"`

	// Log holds the logging-subsystem settings.
	Log LogSettings `yaml:"log" json:"log" env:",nested"`

	// Server holds the gRPC server settings.
	Server ServerSettings `yaml:"server" json:"server" env:",nested"`
}

// DefaultApp returns an [App] populated with [SchemaVersion] and each
// entity's [DefaultX] result. Calling DefaultApp is the canonical way
// to seed the cascade in the loader (YAML and env then layer their
// overrides onto this baseline).
func DefaultApp() App {
	return App{
		Schema:     SchemaVersion,
		ConfigFile: "",
		Jira:       DefaultJiraSettings(),
		Crawl:      DefaultCrawlSettings(),
		Output:     DefaultOutputSettings(),
		Log:        DefaultLogSettings(),
		Server:     DefaultServerSettings(),
	}
}

// ToConfig flattens the composed [App] tree onto the existing flat
// [Config] type so downstream consumers (client, internal/crawl,
// internal/fetch, the gojira facade) need no changes.
//
// The mapping is 1:1 and exhaustive over the Config fields: every
// Config field has exactly one source on App, and every App field
// (other than Schema and ConfigFile, which are loader metadata)
// flows into exactly one Config field. The slice fields are copied
// so a mutation on the returned Config does not alias back into the
// source App.
func (a App) ToConfig() Config {
	apps := make([]string, len(a.Crawl.DevStatusApplications))
	copy(apps, a.Crawl.DevStatusApplications)
	dts := make([]string, len(a.Crawl.DevStatusDataTypes))
	copy(dts, a.Crawl.DevStatusDataTypes)

	return Config{
		Site:                   a.Jira.BaseURL,
		User:                   a.Jira.Email,
		Token:                  a.Jira.APIToken,
		OutputDir:              a.Output.Dir,
		DepthLimit:             a.Crawl.DepthLimit,
		IssueCap:               a.Crawl.IssueCap,
		TimeCapSeconds:         a.Crawl.TimeCapSeconds,
		Concurrency:            a.Crawl.Concurrency,
		Refetch:                a.Crawl.Refetch,
		LogLevel:               a.Log.Level,
		IncludeComments:        a.Crawl.IncludeComments,
		LogFormat:              a.Log.Format,
		IncludeChildren:        a.Crawl.IncludeChildren,
		ChildSearchLimit:       a.Crawl.ChildSearchLimit,
		EpicLinkField:          a.Crawl.EpicLinkField,
		IncludeDevStatus:       a.Crawl.IncludeDevStatus,
		DevStatusApplications:  apps,
		RenderNullCustomFields: a.Crawl.RenderNullCustomFields,
		DevStatusDataTypes:     dts,
	}
}
