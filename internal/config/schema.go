package config

import _ "embed"

// configSchemaJSON holds the canonical JSON Schema (draft 2020-12)
// describing the structure of a gojira.yaml configuration document.
// The schema is the source of truth for the configuration shape; the
// Go [Validator] in validation.go only enforces invariants the schema
// cannot express cleanly (required Jira credentials, parseable URL,
// the ErrMissingRequired / ErrInvalidValue sentinel contract).
//
// The bytes are embedded at compile time so the binary needs no
// filesystem access to validate configuration.
//
//go:embed config.schema.json
var configSchemaJSON []byte

// ConfigSchemaJSON returns the embedded JSON Schema bytes. The
// returned slice MUST be treated as read-only; callers that need to
// mutate the schema document (e.g. to inject a $defs entry for
// vendor extensions) should make their own copy.
func ConfigSchemaJSON() []byte {
	return configSchemaJSON
}
