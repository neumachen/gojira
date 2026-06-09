package config

// CrawlSettings configures the gojira crawl orchestrator: how deep
// to traverse, how many issues to fetch, how aggressively to fan out
// across goroutines, and which optional enrichments to run.
//
// CrawlSettings is one of the four standalone entity structs composed
// under [App]; it can also be used directly by any component that
// only needs crawl-tuning settings. Every env key is hand-named with
// the full GOJIRA_CRAWL_* prefix so the parent App can use
// `env:",nested"` (empty prefix) without introducing nested-prefix
// stutter such as GOJIRA_CRAWL_CRAWL_*.
//
// The defaults returned by [DefaultCrawlSettings] mirror the flat
// gojira v0.1 defaults documented on the existing [Config] type so
// later phases can flatten App → Config without changing observable
// behavior.
type CrawlSettings struct {
	// DepthLimit is the maximum crawl depth from the starting
	// issue. 0 means unlimited. Sourced from
	// GOJIRA_CRAWL_DEPTH_LIMIT. Default: 0.
	DepthLimit int `yaml:"depth_limit" json:"depth_limit" env:"GOJIRA_CRAWL_DEPTH_LIMIT"`

	// IssueCap is the maximum number of issues to fetch per run.
	// 0 means unlimited. Sourced from GOJIRA_CRAWL_ISSUE_CAP.
	// Default: 500.
	IssueCap int `yaml:"issue_cap" json:"issue_cap" env:"GOJIRA_CRAWL_ISSUE_CAP"`

	// TimeCapSeconds is the maximum wall-clock seconds for a run.
	// 0 means unlimited. Sourced from
	// GOJIRA_CRAWL_TIME_CAP_SECONDS. Default: 0.
	TimeCapSeconds int `yaml:"time_cap_seconds" json:"time_cap_seconds" env:"GOJIRA_CRAWL_TIME_CAP_SECONDS"`

	// Concurrency is the number of concurrent Jira API requests.
	// Sourced from GOJIRA_CRAWL_CONCURRENCY. Default: 3.
	Concurrency int `yaml:"concurrency" json:"concurrency" env:"GOJIRA_CRAWL_CONCURRENCY"`

	// Refetch controls whether issues already present on disk are
	// re-fetched and overwritten. Sourced from
	// GOJIRA_CRAWL_REFETCH. Default: false.
	Refetch bool `yaml:"refetch" json:"refetch" env:"GOJIRA_CRAWL_REFETCH"`

	// IncludeComments controls whether issue comments are fetched
	// and rendered. Sourced from
	// GOJIRA_CRAWL_INCLUDE_COMMENTS. Default: false.
	IncludeComments bool `yaml:"include_comments" json:"include_comments" env:"GOJIRA_CRAWL_INCLUDE_COMMENTS"`

	// IncludeChildren controls whether hierarchy children are
	// discovered via JQL search after each successful issue fetch.
	// Sourced from GOJIRA_CRAWL_INCLUDE_CHILDREN. Default: true.
	IncludeChildren bool `yaml:"include_children" json:"include_children" env:"GOJIRA_CRAWL_INCLUDE_CHILDREN"`

	// ChildSearchLimit is the maximum number of children to
	// discover per parent issue. Sourced from
	// GOJIRA_CRAWL_CHILD_SEARCH_LIMIT. Default: 100.
	ChildSearchLimit int `yaml:"child_search_limit" json:"child_search_limit" env:"GOJIRA_CRAWL_CHILD_SEARCH_LIMIT"`

	// EpicLinkField is the optional override for the Epic Link
	// custom field ID (e.g. "customfield_10014"). Sourced from
	// GOJIRA_CRAWL_EPIC_LINK_FIELD. Default: "".
	EpicLinkField string `yaml:"epic_link_field" json:"epic_link_field" env:"GOJIRA_CRAWL_EPIC_LINK_FIELD"`

	// IncludeDevStatus controls whether the crawl orchestrator
	// queries Jira's Dev Status API to surface pull-request URLs
	// associated with each issue. Sourced from
	// GOJIRA_CRAWL_INCLUDE_DEV_STATUS. Default: true.
	IncludeDevStatus bool `yaml:"include_dev_status" json:"include_dev_status" env:"GOJIRA_CRAWL_INCLUDE_DEV_STATUS"`

	// DevStatusApplications is the list of Dev Status
	// applicationType values to query per issue. Sourced from
	// GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS. Default: ["GitHub"].
	DevStatusApplications []string `yaml:"dev_status_applications" json:"dev_status_applications" env:"GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS"`

	// DevStatusDataTypes is the list of Dev Status dataType values
	// to query per issue. Valid values: pullrequest, branch,
	// commit, repository, build. Sourced from
	// GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES. Default: all five.
	DevStatusDataTypes []string `yaml:"dev_status_data_types" json:"dev_status_data_types" env:"GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES"`

	// RenderNullCustomFields controls whether custom fields whose
	// value is JSON null are included in the rendered "## Custom
	// fields" section. Sourced from
	// GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS. Default: false.
	RenderNullCustomFields bool `yaml:"render_null_custom_fields" json:"render_null_custom_fields" env:"GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS"`

	// EmitGraph controls whether the crawler writes a graph.json /
	// graph.d2 pair at the output-dir root summarising the crawled
	// issue graph. The two files are independent of the per-issue
	// Markdown output and never block it: a failure to write them
	// degrades to a warning, never an error. Sourced from
	// GOJIRA_CRAWL_EMIT_GRAPH. Default: false.
	EmitGraph bool `yaml:"emit_graph" json:"emit_graph" env:"GOJIRA_CRAWL_EMIT_GRAPH"`
}

// Default crawl-tuning constants. These mirror the flat [Config]
// defaults documented on the existing v0.1 type so [App.ToConfig]
// produces an identical Config from a DefaultApp.
const (
	DefaultCrawlDepthLimit       = 0
	DefaultCrawlIssueCap         = 500
	DefaultCrawlTimeCapSeconds   = 0
	DefaultCrawlConcurrency      = 3
	DefaultCrawlRefetch          = false
	DefaultCrawlIncludeComments  = false
	DefaultCrawlIncludeChildren  = true
	DefaultCrawlChildSearchLimit = 100
	DefaultCrawlEpicLinkField    = ""
	DefaultCrawlIncludeDevStatus = true
)

// defaultDevStatusApplications and defaultDevStatusDataTypes are
// package-level slices so [DefaultCrawlSettings] can return a fresh
// copy on each call (slices are reference types; sharing them across
// callers would let one caller's mutation leak into another).
var (
	defaultDevStatusApplications = []string{"GitHub"}
	defaultDevStatusDataTypes    = []string{
		"pullrequest",
		"branch",
		"commit",
		"repository",
		"build",
	}
)

// DefaultCrawlSettings returns the embedded crawl defaults. The
// returned value owns fresh copies of the slice fields so callers
// can mutate them without affecting subsequent DefaultCrawlSettings
// calls.
func DefaultCrawlSettings() CrawlSettings {
	apps := make([]string, len(defaultDevStatusApplications))
	copy(apps, defaultDevStatusApplications)
	dts := make([]string, len(defaultDevStatusDataTypes))
	copy(dts, defaultDevStatusDataTypes)

	return CrawlSettings{
		DepthLimit:             DefaultCrawlDepthLimit,
		IssueCap:               DefaultCrawlIssueCap,
		TimeCapSeconds:         DefaultCrawlTimeCapSeconds,
		Concurrency:            DefaultCrawlConcurrency,
		Refetch:                DefaultCrawlRefetch,
		IncludeComments:        DefaultCrawlIncludeComments,
		IncludeChildren:        DefaultCrawlIncludeChildren,
		ChildSearchLimit:       DefaultCrawlChildSearchLimit,
		EpicLinkField:          DefaultCrawlEpicLinkField,
		IncludeDevStatus:       DefaultCrawlIncludeDevStatus,
		DevStatusApplications:  apps,
		DevStatusDataTypes:     dts,
		RenderNullCustomFields: false,
		EmitGraph:              false,
	}
}
