// Package config validates and constructs the runtime configuration
// consumed by every other gojira package. It accepts a plain
// map[string]string (from any source — environment variables, CLI
// flags, a config file) and returns a typed Config value or a
// descriptive error.
//
// # Build
//
// Build is the single entry point. It validates all GOJIRA_* keys
// defined in PRD §6, applies defaults for optional keys, and returns
// a Config or a *ConfigError describing the first validation failure.
//
// Callers that need to distinguish missing-required-key errors from
// invalid-value errors can use errors.Is with ErrMissingRequired or
// ErrInvalidValue, or type-assert to *ConfigError for the key name
// and human-readable reason.
//
// # Implementation: envext
//
// Parsing and validation are delegated to github.com/neumachen/envext.
// The Config struct's `env` tags declare every GOJIRA_* key, its
// default, and the validators that apply. Build translates the typed
// errors returned by envext (*envext.ValidationErrors, *envext.ParseError)
// into the package's stable *ConfigError surface so callers' error
// semantics — in particular errors.Is(err, ErrMissingRequired) and
// errors.Is(err, ErrInvalidValue) — are preserved across this
// refactor.
//
// # No source-specific loading
//
// This package does not read environment variables, parse flags, or
// open files. Source-specific loading is the responsibility of the
// caller (cmd/gojira for the CLI; the gojira facade for library
// consumers). This keeps the package pure and trivially testable.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/neumachen/envext"
	"github.com/neumachen/errext"
)

// Sentinel errors for use with errors.Is.
var (
	// ErrMissingRequired is wrapped by *ConfigError when a required
	// GOJIRA_* key is absent from the input map or set to an empty
	// string.
	ErrMissingRequired = errors.New("config: required key missing")

	// ErrInvalidValue is wrapped by *ConfigError when a key is present
	// but its value fails validation (bad URL, non-integer, unknown
	// log level, etc.).
	ErrInvalidValue = errors.New("config: invalid value")
)

// ConfigError describes a single configuration validation failure.
// It implements the error interface and wraps either ErrMissingRequired
// or ErrInvalidValue so callers can use errors.Is.
type ConfigError struct {
	// Key is the GOJIRA_* environment-variable name that failed.
	Key string
	// Reason is a human-readable description of the failure.
	Reason string
	// sentinel is the wrapped sentinel (ErrMissingRequired or ErrInvalidValue).
	sentinel error
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("config: %s: %s", e.Key, e.Reason)
}

// Unwrap returns the wrapped sentinel so errors.Is works correctly.
func (e *ConfigError) Unwrap() error { return e.sentinel }

// Config holds the validated runtime configuration for a gojira run.
// All fields are exported so that other packages can read them directly.
// The zero value is not valid; always construct via Build.
//
// The `env` struct tags drive parsing via github.com/neumachen/envext.
// The four required fields use the built-in `required` validator (key
// must be present in the lookup) combined with `not_empty` (value must
// not be the empty string after default substitution). The site URL is
// further validated by the custom `url_request_uri` validator, and the
// log level by the custom `oneof_log_level` validator.
type Config struct {
	// Site is the Jira Cloud base URL, e.g. "https://example.atlassian.net".
	// Sourced from GOJIRA_SITE. Required.
	Site string `env:"GOJIRA_SITE,validate=required|not_empty|url_request_uri"`

	// User is the Atlassian account email used for Basic auth.
	// Sourced from GOJIRA_USER. Required.
	User string `env:"GOJIRA_USER,validate=required|not_empty"`

	// Token is the Atlassian API token used for Basic auth.
	// Sourced from GOJIRA_TOKEN. Required.
	Token string `env:"GOJIRA_TOKEN,validate=required|not_empty"`

	// OutputDir is the root directory for Markdown output.
	// Sourced from GOJIRA_OUTPUT_DIR. Required.
	OutputDir string `env:"GOJIRA_OUTPUT_DIR,validate=required|not_empty"`

	// DepthLimit is the maximum crawl depth from the starting issue.
	// 0 means unlimited. Sourced from GOJIRA_DEPTH_LIMIT. Default: 0.
	DepthLimit int `env:"GOJIRA_DEPTH_LIMIT,default=0"`

	// IssueCap is the maximum number of issues to fetch per run.
	// 0 means unlimited. Sourced from GOJIRA_ISSUE_CAP. Default: 500.
	IssueCap int `env:"GOJIRA_ISSUE_CAP,default=500"`

	// TimeCapSeconds is the maximum wall-clock seconds for a run.
	// 0 means unlimited. Sourced from GOJIRA_TIME_CAP_SECONDS. Default: 0.
	TimeCapSeconds int `env:"GOJIRA_TIME_CAP_SECONDS,default=0"`

	// Concurrency is the number of concurrent Jira API requests.
	// Sourced from GOJIRA_CONCURRENCY. Default: 3.
	Concurrency int `env:"GOJIRA_CONCURRENCY,default=3"`

	// Refetch controls whether issues already present on disk are
	// re-fetched and overwritten. Sourced from GOJIRA_REFETCH. Default: false.
	Refetch bool `env:"GOJIRA_REFETCH,default=false"`

	// LogLevel is the logging verbosity. One of "error", "warn", "info",
	// "debug", "trace". Sourced from GOJIRA_LOG_LEVEL. Default: "info".
	// "trace" is gojira's own level below slog.LevelDebug (see
	// log.LevelTrace); it powers the crawl observability instrument.
	LogLevel string `env:"GOJIRA_LOG_LEVEL,default=info,validate=oneof_log_level"`

	// IncludeComments controls whether issue comments are fetched and
	// rendered. Sourced from GOJIRA_INCLUDE_COMMENTS. Default: false.
	IncludeComments bool `env:"GOJIRA_INCLUDE_COMMENTS,default=false"`

	// LogFormat is the log output format. One of "text" (human-
	// readable) or "json" (one JSON object per line). Sourced
	// from GOJIRA_LOG_FORMAT. Default: "text".
	LogFormat string `env:"GOJIRA_LOG_FORMAT,default=text,validate=oneof_log_format"`

	// IncludeChildren controls whether hierarchy children are
	// discovered via JQL search after each successful issue fetch.
	// When true, the crawler queries `parent = "KEY"` (and the Epic
	// Link field, when configured or auto-detected) for every
	// hierarchy-capable issue and enqueues the discovered child keys.
	// Sourced from GOJIRA_INCLUDE_CHILDREN. Default: true.
	IncludeChildren bool `env:"GOJIRA_INCLUDE_CHILDREN,default=true"`

	// ChildSearchLimit is the maximum number of children to discover
	// per parent issue. It caps the maxResults parameter sent in each
	// hierarchy JQL search call. Sourced from GOJIRA_CHILD_SEARCH_LIMIT.
	// Default: 100.
	ChildSearchLimit int `env:"GOJIRA_CHILD_SEARCH_LIMIT,default=100"`

	// EpicLinkField is the optional override for the Epic Link custom
	// field ID (e.g. "customfield_10014"). When empty, gojira auto-
	// detects the field from the tenant's field metadata on first use,
	// or skips Epic Link discovery if no such field exists on the
	// tenant. Sourced from GOJIRA_EPIC_LINK_FIELD. Default: "".
	EpicLinkField string `env:"GOJIRA_EPIC_LINK_FIELD,default="`

	// IncludeDevStatus controls whether the crawl orchestrator queries
	// Jira's Dev Status API to surface pull-request URLs associated
	// with each issue. The Dev Status endpoint
	// (/rest/dev-status/1.0/issue/detail) is not part of the
	// documented Jira Cloud platform REST API, but it has powered
	// Atlassian's own UI for over a decade and is empirically stable.
	// The documented customfield_10000 development-summary field is
	// used as a per-issue gate so issues with zero PRs do not incur
	// any API call. Disable to opt out of the undocumented endpoint
	// entirely. Sourced from GOJIRA_INCLUDE_DEV_STATUS. Default: true.
	IncludeDevStatus bool `env:"GOJIRA_INCLUDE_DEV_STATUS,default=true"`

	// DevStatusApplications is the comma-separated list of Dev Status
	// applicationType values to query per issue. Common values:
	// "GitHub", "Bitbucket", "GitLab", "GitHubEnterprise". One Dev
	// Status request is made per application type per non-gated
	// dataType per issue, and the results are merged and deduplicated
	// per entity type. Sourced from GOJIRA_DEV_STATUS_APPLICATIONS.
	// Default: ["GitHub"].
	DevStatusApplications []string `env:"GOJIRA_DEV_STATUS_APPLICATIONS,default=GitHub"`

	// RenderNullCustomFields controls whether custom fields whose
	// value is JSON null are included in the rendered "## Custom
	// fields" section. When true, each null-valued custom field
	// renders as `- <label>: null`. When false (the default), null-
	// valued custom fields are skipped entirely; on a typical Jira
	// tenant roughly 90% of customfield_* entries are null per
	// issue, and rendering them adds visual noise without
	// information. Sourced from GOJIRA_RENDER_NULL_CUSTOM_FIELDS.
	// Default: false.
	RenderNullCustomFields bool `env:"GOJIRA_RENDER_NULL_CUSTOM_FIELDS,default=false"`

	// DevStatusDataTypes is the comma-separated list of Dev Status
	// dataType values to query per issue. The Jira UI Development
	// panel surfaces five entity groupings; gojira queries every one
	// by default so the rendered "## Development" section mirrors what
	// the Jira UI shows. Users can subset to avoid querying types
	// their tenant does not use, e.g.
	// GOJIRA_DEV_STATUS_DATA_TYPES=pullrequest restricts enrichment
	// to PRs only.
	//
	// Valid values: pullrequest, branch, commit, repository, build.
	// Unknown values produce a config error wrapping
	// [ErrInvalidValue]. Sourced from GOJIRA_DEV_STATUS_DATA_TYPES.
	// Default: ["pullrequest", "branch", "commit", "repository",
	// "build"].
	DevStatusDataTypes []string `env:"GOJIRA_DEV_STATUS_DATA_TYPES,default=pullrequest|branch|commit|repository|build,validate=oneof_dev_status_data_types"`

	// EmitGraph controls whether the crawler writes graph.json and
	// graph.d2 (a D2-language diagram source) at the output-dir root
	// describing the crawled issue graph. Default: false. The graph
	// is always best-effort: a failure to write either file degrades
	// to a warning and never fails the crawl. Sourced from
	// GOJIRA_EMIT_GRAPH.
	EmitGraph bool `env:"GOJIRA_EMIT_GRAPH,default=false"`

	// MCPMode selects the `gojira mcp` server backend: "self" (run
	// the gojira facade in-process) or "bridge" (forward to a
	// running gojira serve gRPC server). It has NO embedded default:
	// the mcp command enforces presence and enum at startup, so
	// non-mcp commands (crawl/serve) keep loading configs without
	// an mcp section. Sourced from GOJIRA_MCP_MODE.
	MCPMode string `env:"GOJIRA_MCP_MODE"`

	// MCPAllowWrites gates the mutating MCP tools (create_issue,
	// update_issue, add_comment, transition_issue) on the MCP
	// server. When false (the default), those tools are absent from
	// the server's tools/list response. Sourced from
	// GOJIRA_MCP_ALLOW_WRITES. Default: false.
	MCPAllowWrites bool `env:"GOJIRA_MCP_ALLOW_WRITES,default=false"`
}

// validLogLevels is the set of accepted GOJIRA_LOG_LEVEL values. The
// invalidLogLevelValue placeholder is used by oneofLogLevel to embed
// the offending value into the resulting *envext.FieldError so it can
// be surfaced verbatim via the *ConfigError.Reason string.
var validLogLevels = map[string]bool{
	"error": true,
	"warn":  true,
	"info":  true,
	"debug": true,
	"trace": true,
}

// oneofLogLevel is the custom envext validator that enforces the
// {error, warn, info, debug, trace} constraint on GOJIRA_LOG_LEVEL.
// It mirrors envext's built-in not_empty in falling back to the
// default value when the env value is absent, which lets the
// documented default of "info" satisfy the validator when
// GOJIRA_LOG_LEVEL is unset. "trace" is gojira's own level below
// slog.LevelDebug; see log.LevelTrace and log.ParseLevel.
func oneofLogLevel(info envext.FieldInfo) error {
	v := info.EnvValue
	if v == "" {
		v = info.DefaultValue
	}
	if validLogLevels[v] {
		return nil
	}
	return &envext.FieldError{
		Field:  info.Name,
		EnvKey: info.EnvKey,
		Rule:   "oneof_log_level",
		Err:    errext.Errorf("must be one of error/warn/info/debug/trace, got %q", v),
	}
}

// validLogFormats is the set of accepted GOJIRA_LOG_FORMAT values. The
// set mirrors the formats supported by the gojira/log package
// ([log.FormatText], [log.FormatJSON]).
var validLogFormats = map[string]bool{
	"text": true,
	"json": true,
}

// oneofLogFormat is the custom envext validator that enforces the
// {text, json} constraint on GOJIRA_LOG_FORMAT. It mirrors oneofLogLevel
// in falling back to the documented default ("text") when the env value
// is absent so the default satisfies the validator with no user input.
func oneofLogFormat(info envext.FieldInfo) error {
	v := info.EnvValue
	if v == "" {
		v = info.DefaultValue
	}
	if validLogFormats[v] {
		return nil
	}
	return &envext.FieldError{
		Field:  info.Name,
		EnvKey: info.EnvKey,
		Rule:   "oneof_log_format",
		Err:    errext.Errorf("must be one of text/json, got %q", v),
	}
}

// validDevStatusDataTypes is the set of accepted GOJIRA_DEV_STATUS_DATA_TYPES
// member values. The set mirrors the five dataType values the Jira UI
// Development panel surfaces.
var validDevStatusDataTypes = map[string]bool{
	"pullrequest": true,
	"branch":      true,
	"commit":      true,
	"repository":  true,
	"build":       true,
}

// oneofDevStatusDataTypes is the custom envext validator that enforces
// that every member of the comma-separated GOJIRA_DEV_STATUS_DATA_TYPES
// list is one of the five recognised dataType values. An empty list
// (after default substitution) is accepted; the enricher treats it as
// "no dataTypes configured" and skips Dev Status entirely.
//
// The validator mirrors the slice-parsing convention used by the
// upstream envext package: the *env* value is split on comma, but the
// *default* value is split on pipe (envext's setSliceField behavior).
// The validator runs against whichever value is in play and applies
// the matching delimiter so both shapes are honoured.
func oneofDevStatusDataTypes(info envext.FieldInfo) error {
	v := info.EnvValue
	delim := ","
	if v == "" {
		v = info.DefaultValue
		delim = "|"
	}
	if v == "" {
		return nil
	}
	for _, dt := range strings.Split(v, delim) {
		dt = strings.TrimSpace(dt)
		if dt == "" {
			continue
		}
		if !validDevStatusDataTypes[dt] {
			return &envext.FieldError{
				Field:  info.Name,
				EnvKey: info.EnvKey,
				Rule:   "oneof_dev_status_data_types",
				Err: errext.Errorf(
					"must be a comma-separated list of pullrequest/branch/commit/repository/build, got %q",
					dt,
				),
			}
		}
	}
	return nil
}

// urlRequestURI is the custom envext validator that enforces a parseable
// absolute URL for GOJIRA_SITE. It uses net/url.ParseRequestURI, which
// rejects relative URLs (matching the prior hand-rolled validator's
// semantics: "not-a-url", a bare hostname, and "://example..." all
// fail).
//
// The validator only runs when the field is present and non-empty;
// the required|not_empty pair upstream handles the absent/empty case
// and emits ErrMissingRequired instead.
func urlRequestURI(info envext.FieldInfo) error {
	v := info.EnvValue
	if v == "" {
		v = info.DefaultValue
	}
	if v == "" {
		// Let required|not_empty produce the missing-required error.
		return nil
	}
	if _, err := url.ParseRequestURI(v); err != nil {
		return &envext.FieldError{
			Field:  info.Name,
			EnvKey: info.EnvKey,
			Rule:   "url_request_uri",
			Err:    errext.Errorf("must be a valid URL: %v", err),
		}
	}
	return nil
}

// Build validates the key-value pairs in kv against the canonical
// GOJIRA_* key set defined in PRD §6, applies defaults for optional
// keys, and returns a populated Config.
//
// On the first validation failure, Build returns a zero Config and a
// *ConfigError. Callers can use errors.Is(err, ErrMissingRequired) or
// errors.Is(err, ErrInvalidValue) to distinguish failure classes, or
// type-assert to *ConfigError for the key name and reason.
//
// A fresh *envext.Parser is constructed per call rather than cached at
// package level. The construction cost is small (one map copy plus the
// built-in registry clone) and using WithEnvMap per call keeps the
// supplied lookup map immutable for the duration of the parse.
func Build(kv map[string]string) (Config, error) {
	var cfg Config

	parser, err := envext.New(
		envext.WithEnvMap(kv),
		envext.WithValidator("oneof_log_level", oneofLogLevel),
		envext.WithValidator("oneof_log_format", oneofLogFormat),
		envext.WithValidator("url_request_uri", urlRequestURI),
		envext.WithValidator("oneof_dev_status_data_types", oneofDevStatusDataTypes),
	)
	if err != nil {
		// envext.New only fails for invalid options; the options used
		// here are statically valid, so this branch is defensive.
		return Config{}, &ConfigError{
			Key:      "",
			Reason:   fmt.Sprintf("envext parser construction failed: %v", err),
			sentinel: ErrInvalidValue,
		}
	}

	if _, err := parser.Parse(&cfg); err != nil {
		return Config{}, translateEnvarError(err)
	}

	return cfg, nil
}

// translateEnvarError converts a typed error returned by envext.Parse
// into a *ConfigError that wraps either ErrMissingRequired or
// ErrInvalidValue. The mapping rules — kept stable so downstream code
// continues to satisfy errors.Is — are:
//
//   - A validation failure on the `required` or `not_empty` rule is a
//     missing-required error. envext's `required` fires when the env
//     key is absent; `not_empty` fires when the key is present but the
//     value is the empty string. Both shapes are surfaced the same way
//     by the pre-envext implementation, so they share a sentinel here.
//   - Any other validation failure (custom validators on this package,
//     including the URL and log-level rules) is an invalid-value error.
//   - A parse failure (e.g. non-integer GOJIRA_CONCURRENCY) is an
//     invalid-value error.
//
// The first matching FieldError wins, mirroring the prior
// implementation's "return on first failure" semantics.
func translateEnvarError(err error) error {
	// 1) Validation errors aggregate one *FieldError per failing rule.
	//    Honour iteration order so the surfaced failure is deterministic
	//    and matches the test expectations (which assert .Key).
	var ve *envext.ValidationErrors
	if errors.As(err, &ve) && len(ve.Fields) > 0 {
		fe := ve.Fields[0]
		sentinel := ErrInvalidValue
		reason := ""
		switch fe.Rule {
		case "required":
			sentinel = ErrMissingRequired
			reason = "is required"
		case "not_empty":
			sentinel = ErrMissingRequired
			reason = "is required"
		default:
			// Custom validators put a useful message in fe.Err.
			if fe.Err != nil {
				reason = fe.Err.Error()
			} else {
				reason = fmt.Sprintf("failed validator %q", fe.Rule)
			}
		}
		return &ConfigError{
			Key:      fe.EnvKey,
			Reason:   reason,
			sentinel: sentinel,
		}
	}

	// 2) Parse errors (e.g. strconv failure) are invalid-value failures
	//    against the offending env key. envext surfaces the struct field
	//    name on *ParseError; map it back to the GOJIRA_* key via the
	//    Config struct tags.
	var pe *envext.ParseError
	if errors.As(err, &pe) {
		key := envKeyForField(pe.Field)
		reason := fmt.Sprintf("must be a %s, got invalid value: %v",
			humanTypeName(pe.Type.Kind().String()), pe.Err)
		return &ConfigError{
			Key:      key,
			Reason:   reason,
			sentinel: ErrInvalidValue,
		}
	}

	// 3) Fallback: any other envext error is surfaced as an
	//    invalid-value failure with no specific key.
	return &ConfigError{
		Key:      "",
		Reason:   err.Error(),
		sentinel: ErrInvalidValue,
	}
}

// envKeyForField returns the GOJIRA_* env key associated with the
// supplied Go struct field name on Config. The map is exhaustive over
// the Config fields parsed by envext.
func envKeyForField(field string) string {
	switch field {
	case "Site":
		return "GOJIRA_SITE"
	case "User":
		return "GOJIRA_USER"
	case "Token":
		return "GOJIRA_TOKEN"
	case "OutputDir":
		return "GOJIRA_OUTPUT_DIR"
	case "DepthLimit":
		return "GOJIRA_DEPTH_LIMIT"
	case "IssueCap":
		return "GOJIRA_ISSUE_CAP"
	case "TimeCapSeconds":
		return "GOJIRA_TIME_CAP_SECONDS"
	case "Concurrency":
		return "GOJIRA_CONCURRENCY"
	case "Refetch":
		return "GOJIRA_REFETCH"
	case "LogLevel":
		return "GOJIRA_LOG_LEVEL"
	case "IncludeComments":
		return "GOJIRA_INCLUDE_COMMENTS"
	case "LogFormat":
		return "GOJIRA_LOG_FORMAT"
	case "IncludeChildren":
		return "GOJIRA_INCLUDE_CHILDREN"
	case "ChildSearchLimit":
		return "GOJIRA_CHILD_SEARCH_LIMIT"
	case "EpicLinkField":
		return "GOJIRA_EPIC_LINK_FIELD"
	case "IncludeDevStatus":
		return "GOJIRA_INCLUDE_DEV_STATUS"
	case "DevStatusApplications":
		return "GOJIRA_DEV_STATUS_APPLICATIONS"
	case "DevStatusDataTypes":
		return "GOJIRA_DEV_STATUS_DATA_TYPES"
	case "RenderNullCustomFields":
		return "GOJIRA_RENDER_NULL_CUSTOM_FIELDS"
	case "EmitGraph":
		return "GOJIRA_EMIT_GRAPH"
	case "MCPMode":
		return "GOJIRA_MCP_MODE"
	case "MCPAllowWrites":
		return "GOJIRA_MCP_ALLOW_WRITES"
	default:
		return field
	}
}

// humanTypeName returns a friendlier label for the integer and boolean
// kinds that appear in *envext.ParseError reports. The default branch
// returns the reflect kind name unchanged.
func humanTypeName(kind string) string {
	switch kind {
	case "int", "int8", "int16", "int32", "int64":
		return "integer"
	case "uint", "uint8", "uint16", "uint32", "uint64":
		return "unsigned integer"
	case "bool":
		return "boolean (true/false/1/0)"
	default:
		return kind
	}
}
