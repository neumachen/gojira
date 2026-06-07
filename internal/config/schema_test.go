package config

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigSchemaJSON_NotEmpty asserts the embedded bytes loaded. A
// build-tag mistake or missing config.schema.json file would yield an
// empty slice; we want a loud failure at test time, not a silent
// "valid against an empty schema" footgun at runtime.
func TestConfigSchemaJSON_NotEmpty(t *testing.T) {
	b := ConfigSchemaJSON()
	assert.NotEmpty(t, b, "ConfigSchemaJSON returned no bytes")
}

// TestConfigSchemaJSON_IsValidJSON guards against a syntactically
// broken schema. The compiler in TestConfigSchemaJSON_Compiles would
// also reject malformed JSON, but a dedicated assertion gives a
// cleaner failure message.
func TestConfigSchemaJSON_IsValidJSON(t *testing.T) {
	var v any
	require.NoError(t, json.Unmarshal(ConfigSchemaJSON(), &v),
		"embedded schema is not valid JSON")
}

// TestConfigSchemaJSON_Compiles verifies the schema compiles against
// santhosh-tekuri/jsonschema/v6 using exactly the loader pattern the
// Phase 2 SchemaValidator uses: UnmarshalJSON → AddResource → Compile.
// If this ever fails after a schema edit we want to know before any
// production code tries to validate against it.
func TestConfigSchemaJSON_Compiles(t *testing.T) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(ConfigSchemaJSON()))
	require.NoError(t, err, "UnmarshalJSON")

	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource(configSchemaURL, doc), "AddResource")

	sch, err := c.Compile(configSchemaURL)
	require.NoError(t, err, "Compile")
	assert.NotNil(t, sch)
}
