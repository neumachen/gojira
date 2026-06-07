package config

import "sort"

// aliasToCanonical maps the v0.1 flat GOJIRA_* environment-variable
// names onto their canonical Phase 0 equivalents. The table is the
// single source of truth for backward compatibility — both the loader
// (which applies it to env inputs before envext parsing) and the CLI
// (which surfaces the canonical-and-alias union in its env-key list)
// derive their behavior from it.
//
// Keys NOT in this table (GOJIRA_OUTPUT_DIR, GOJIRA_LOG_LEVEL,
// GOJIRA_LOG_FORMAT, GOJIRA_CONFIG_FILE, GOJIRA_SCHEMA) have no alias
// because their v0.1 names already match the canonical Phase 0 names.
//
// The mapping is a tiny, fixed-size constant; declaring it as a
// package-level var keeps the table grep-able and is the same shape
// used by config.go's envKeyForField.
var aliasToCanonical = map[string]string{
	"GOJIRA_SITE":                      "GOJIRA_JIRA_BASE_URL",
	"GOJIRA_USER":                      "GOJIRA_JIRA_EMAIL",
	"GOJIRA_TOKEN":                     "GOJIRA_JIRA_API_TOKEN",
	"GOJIRA_DEPTH_LIMIT":               "GOJIRA_CRAWL_DEPTH_LIMIT",
	"GOJIRA_ISSUE_CAP":                 "GOJIRA_CRAWL_ISSUE_CAP",
	"GOJIRA_TIME_CAP_SECONDS":          "GOJIRA_CRAWL_TIME_CAP_SECONDS",
	"GOJIRA_CONCURRENCY":               "GOJIRA_CRAWL_CONCURRENCY",
	"GOJIRA_REFETCH":                   "GOJIRA_CRAWL_REFETCH",
	"GOJIRA_INCLUDE_COMMENTS":          "GOJIRA_CRAWL_INCLUDE_COMMENTS",
	"GOJIRA_INCLUDE_CHILDREN":          "GOJIRA_CRAWL_INCLUDE_CHILDREN",
	"GOJIRA_CHILD_SEARCH_LIMIT":        "GOJIRA_CRAWL_CHILD_SEARCH_LIMIT",
	"GOJIRA_EPIC_LINK_FIELD":           "GOJIRA_CRAWL_EPIC_LINK_FIELD",
	"GOJIRA_INCLUDE_DEV_STATUS":        "GOJIRA_CRAWL_INCLUDE_DEV_STATUS",
	"GOJIRA_DEV_STATUS_APPLICATIONS":   "GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS",
	"GOJIRA_DEV_STATUS_DATA_TYPES":     "GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES",
	"GOJIRA_RENDER_NULL_CUSTOM_FIELDS": "GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS",
}

// ResolveAliases returns a new map in which each canonical Phase 0
// key is populated from its deprecated v0.1 alias *only* when the
// canonical key is absent or set to the empty string in the input.
// Canonical always wins when both are present; the alias is preserved
// in the output unchanged so callers that still consult it (e.g. a
// one-time deprecation warning) can detect that it was supplied.
//
// The input map is never mutated. Calling ResolveAliases with a nil
// or empty input returns a non-nil empty map so the result is safe to
// range over without a nil check.
func ResolveAliases(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+len(aliasToCanonical))
	for k, v := range in {
		out[k] = v
	}
	for alias, canonical := range aliasToCanonical {
		aliasVal, hasAlias := in[alias]
		canonVal, hasCanon := in[canonical]
		// Canonical wins when present and non-empty. An empty
		// canonical is treated the same as "absent" so a
		// migrating user can stage an `unset GOJIRA_JIRA_*`
		// fallback to the old key without surprises.
		if hasCanon && canonVal != "" {
			continue
		}
		if !hasAlias {
			continue
		}
		out[canonical] = aliasVal
	}
	return out
}

// DeprecatedAliasKeys returns the v0.1 alias keys recognised by
// [ResolveAliases], sorted lexicographically. The CLI uses this list
// (together with the canonical key set) when reporting the union of
// env keys it consults, so a `gojira --help`-style printout can stay
// stable and complete.
func DeprecatedAliasKeys() []string {
	out := make([]string, 0, len(aliasToCanonical))
	for k := range aliasToCanonical {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CanonicalForAlias returns the canonical Phase 0 key that the
// supplied v0.1 alias maps to, and a bool indicating whether the
// input was a recognised alias. It exists so the future deprecation-
// warning emitter can look up the replacement key without re-reading
// the table.
func CanonicalForAlias(alias string) (string, bool) {
	canonical, ok := aliasToCanonical[alias]
	return canonical, ok
}
