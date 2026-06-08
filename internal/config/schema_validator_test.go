package config

import (
	"bytes"
	"errors"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validRawDoc returns a decoded-YAML map equivalent to the PRD §2.3
// example. Tests scope-in on a single rule by mutating one key of the
// returned value. The map is constructed with jsonschema.UnmarshalJSON
// so the numeric types match what the v6 compiler expects (json.Number
// via the package's own decoder); this keeps the Layer-1 tests aligned
// with the production path, where the loader will also feed any-typed
// values into ValidateRaw.
func validRawDoc(t *testing.T) map[string]any {
	t.Helper()
	const src = `{
		"schema": "gojira.config.v1",
		"jira": {
			"base_url": "https://example.atlassian.net",
			"email": "me@example.com",
			"api_token": ""
		},
		"output": { "dir": "./jira-mirror" },
		"crawl": {
			"depth_limit": 0,
			"issue_cap": 500,
			"time_cap_seconds": 0,
			"concurrency": 3,
			"refetch": false,
			"include_comments": false,
			"include_children": true,
			"child_search_limit": 100,
			"epic_link_field": "",
			"include_dev_status": true,
			"dev_status_applications": ["GitHub"],
			"dev_status_data_types": ["pullrequest", "branch", "commit", "repository", "build"],
			"render_null_custom_fields": false
		},
		"log": { "level": "info", "format": "text" }
	}`
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(src)))
	require.NoError(t, err, "decode test fixture")
	m, ok := v.(map[string]any)
	require.True(t, ok, "fixture must decode to a JSON object")
	return m
}

// TestSchemaValidator_ValidPRDExample asserts the PRD §2.3 example
// document — the canonical "this should work" config — passes the
// embedded schema. This is the load-then-validate happy path Layer 1
// runs on a well-formed gojira.yaml before envext parsing.
func TestSchemaValidator_ValidPRDExample(t *testing.T) {
	sv, err := NewSchemaValidator()
	require.NoError(t, err)

	require.NoError(t, sv.ValidateRaw(validRawDoc(t)))
}

// TestSchemaValidator_UnknownTopLevelKey asserts the
// additionalProperties:false constraint at the root rejects unknown
// top-level keys. A typo like "jria" must fail loudly rather than
// silently being dropped on the floor by the YAML decoder.
func TestSchemaValidator_UnknownTopLevelKey(t *testing.T) {
	sv, err := NewSchemaValidator()
	require.NoError(t, err)

	doc := validRawDoc(t)
	doc["jria"] = map[string]any{"base_url": "https://typo.example.com"}

	err = sv.ValidateRaw(doc)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue,
		"additionalProperties failures wrap ErrInvalidValue")

	var verrs *ValidationErrors
	require.True(t, errors.As(err, &verrs))
	require.True(t, verrs.HasErrors())
}

// TestSchemaValidator_BadLogLevelEnum asserts the schema's enum
// constraint on log.level catches unknown levels at Layer 1 (before
// envext / struct population). The Layer-2 validator catches the
// same case for App-struct callers, but Layer 1 is the source of
// truth for raw file input. Note that "trace" was added to the
// accepted set in the crawl-observability phase; the bad example
// here uses "verbose" instead.
func TestSchemaValidator_BadLogLevelEnum(t *testing.T) {
	sv, err := NewSchemaValidator()
	require.NoError(t, err)

	doc := validRawDoc(t)
	log := doc["log"].(map[string]any)
	log["level"] = "verbose"

	err = sv.ValidateRaw(doc)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue)
}

// TestSchemaValidator_BadDevStatusDataType asserts the schema's item
// enum on crawl.dev_status_data_types catches invalid members at
// Layer 1. Each invalid member is a structural failure (the value
// isn't one of the allowed strings); Layer 2 catches the same case
// for completeness when the App is constructed directly in Go.
func TestSchemaValidator_BadDevStatusDataType(t *testing.T) {
	sv, err := NewSchemaValidator()
	require.NoError(t, err)

	doc := validRawDoc(t)
	crawl := doc["crawl"].(map[string]any)
	crawl["dev_status_data_types"] = []any{"pullrequest", "bogus"}

	err = sv.ValidateRaw(doc)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue)
}

// TestValidateRawConfig_Convenience asserts the package-level
// convenience does the same work as constructing a SchemaValidator
// directly. The convenience is the entry point most one-shot callers
// will use, so it gets its own targeted test.
func TestValidateRawConfig_Convenience(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		require.NoError(t, ValidateRawConfig(validRawDoc(t)))
	})
	t.Run("invalid", func(t *testing.T) {
		doc := validRawDoc(t)
		doc["schema"] = "gojira.config.v999"
		err := ValidateRawConfig(doc)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidValue)
	})
}

// TestSchemaValidator_NilReceiver asserts a nil receiver does not
// panic; it returns a sentinel-less error so the package never
// crashes a host process from a missed initialization.
func TestSchemaValidator_NilReceiver(t *testing.T) {
	var sv *SchemaValidator
	err := sv.ValidateRaw(map[string]any{})
	require.Error(t, err)
}

// TestSchemaValidator_ServerAddress_Valid asserts that a document
// containing a well-formed server.address string passes Layer-1
// schema validation. The server block is optional in the schema, so
// its presence with a valid string value must not cause a rejection.
func TestSchemaValidator_ServerAddress_Valid(t *testing.T) {
	sv, err := NewSchemaValidator()
	require.NoError(t, err)

	doc := validRawDoc(t)
	doc["server"] = map[string]any{"address": "0.0.0.0:9090"}

	require.NoError(t, sv.ValidateRaw(doc),
		"server.address with a valid string must pass Layer-1 validation")
}

// TestSchemaValidator_ServerNotObject asserts that the schema rejects
// a server value that is a plain string instead of an object. The
// schema declares server as type:object with additionalProperties:false,
// so a scalar value must fail at Layer 1 and wrap ErrInvalidValue.
func TestSchemaValidator_ServerNotObject(t *testing.T) {
	sv, err := NewSchemaValidator()
	require.NoError(t, err)

	doc := validRawDoc(t)
	doc["server"] = "not-an-object"

	err = sv.ValidateRaw(doc)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue,
		"type mismatch on server must wrap ErrInvalidValue")

	var verrs *ValidationErrors
	require.True(t, errors.As(err, &verrs))
	require.True(t, verrs.HasErrors())
}
