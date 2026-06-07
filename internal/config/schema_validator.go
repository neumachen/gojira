package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// configSchemaURL is the in-memory URL under which the embedded JSON
// Schema is registered with the jsonschema compiler. It mirrors the
// "$id" inside config.schema.json so $ref resolution would still work
// if the schema were ever split across multiple files.
const configSchemaURL = "https://github.com/neumachen/gojira/schemas/config.schema.json"

// SchemaValidator is Layer 1 of the two-layer validation pipeline.
// It validates the RAW decoded-YAML document (a map[string]any tree,
// or whatever the YAML decoder produced) against the embedded JSON
// Schema BEFORE the document is reflected into an [App] struct.
//
// Catching structural problems (unknown keys, wrong types, enum
// violations) at Layer 1 keeps Layer 2 — the App-struct [Validator]
// in validation.go — focused on the invariants schema cannot express
// cleanly: required Jira credentials, parseable URL, the
// ErrMissingRequired / ErrInvalidValue sentinel contract.
//
// SchemaValidator is safe for concurrent use; the underlying
// *jsonschema.Schema is immutable after compilation.
type SchemaValidator struct {
	sch *jsonschema.Schema
}

// schemaOnce, compiledConfigSchema and compiledConfigSchemaErr cache
// the result of compiling the embedded JSON Schema. Compilation is
// non-trivial (decode JSON, walk every keyword, populate the
// compiler's resource map) and the result is immutable, so a single
// sync.Once is the idiomatic cache. A compilation failure is sticky
// (every subsequent call returns the same error) so misconfigurations
// surface deterministically rather than racing.
var (
	schemaOnce              sync.Once
	compiledConfigSchema    *jsonschema.Schema
	compiledConfigSchemaErr error
)

// compiledSchema returns the singleton compiled JSON Schema for the
// embedded config.schema.json. The first call compiles; subsequent
// calls return the cached pointer (and cached error, if compilation
// itself failed — which would be a programmer error, not user input).
func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(ConfigSchemaJSON()))
		if err != nil {
			compiledConfigSchemaErr = fmt.Errorf("config: decode embedded schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(configSchemaURL, doc); err != nil {
			compiledConfigSchemaErr = fmt.Errorf("config: register embedded schema: %w", err)
			return
		}
		sch, err := c.Compile(configSchemaURL)
		if err != nil {
			compiledConfigSchemaErr = fmt.Errorf("config: compile embedded schema: %w", err)
			return
		}
		compiledConfigSchema = sch
	})
	return compiledConfigSchema, compiledConfigSchemaErr
}

// NewSchemaValidator compiles (on first use) the embedded JSON Schema
// and returns a SchemaValidator ready for [ValidateRaw]. A non-nil
// error indicates the embedded schema itself is broken — this is a
// programmer error, not a user-facing config error, so callers
// generally surface it as an internal failure rather than wrapping
// the sentinels.
func NewSchemaValidator() (*SchemaValidator, error) {
	sch, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	return &SchemaValidator{sch: sch}, nil
}

// ValidateRaw runs the embedded JSON Schema against the supplied
// decoded document. The data argument is whatever the YAML (or JSON)
// decoder produced — typically a map[string]any with nested maps and
// slices, but ValidateRaw accepts any since the schema covers all
// allowed shapes.
//
// On success ValidateRaw returns nil. On failure it walks the
// jsonschema/v6 *ValidationError tree, flattens each leaf into a
// [*ValidationError] (the existing 4-field shape declared in
// validation.go) wrapping [ErrInvalidValue], and returns the
// aggregate as a [*ValidationErrors]. Structural, type, enum,
// additionalProperties, and schema-level "required" failures are all
// "invalid value" from the caller's perspective; the
// "required by Go code" sentinel [ErrMissingRequired] is exclusively
// owned by Layer 2 ([Validator]).
func (v *SchemaValidator) ValidateRaw(data any) error {
	if v == nil || v.sch == nil {
		return errors.New("config: SchemaValidator not initialized")
	}
	if err := v.sch.Validate(data); err != nil {
		return convertSchemaError(err)
	}
	return nil
}

// ValidateRawConfig is a convenience that constructs a default
// [SchemaValidator] and runs it against data. It exists so callers
// that validate once per process can avoid the boilerplate of
// constructing a validator value; long-running services should hold
// a reusable [SchemaValidator] for the (small) allocation savings.
func ValidateRawConfig(data any) error {
	sv, err := NewSchemaValidator()
	if err != nil {
		return err
	}
	return sv.ValidateRaw(data)
}

// convertSchemaError translates a jsonschema/v6 *ValidationError tree
// into a [*ValidationErrors] aggregate of leaf failures. Each leaf is
// reported as a [*ValidationError] wrapping [ErrInvalidValue], with
// Field set to the dotted instance-location path (e.g.
// "jira.base_url"). A non-jsonschema error (which should not happen
// for a successful compile, but is handled defensively) is surfaced
// as a single ValidationError with an empty Field.
func convertSchemaError(err error) error {
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		agg := &ValidationErrors{}
		agg.Add("", err.Error(), nil, ErrInvalidValue)
		return agg.ToError()
	}

	agg := &ValidationErrors{}
	flattenSchemaLeaves(ve, agg)
	if !agg.HasErrors() {
		// Defensive fallback: ensure callers always see at least
		// one leaf wrapping the sentinel so errors.Is(err,
		// ErrInvalidValue) holds even when the v6 tree contains
		// only a synthetic root with no leaf causes.
		agg.Add("", ve.Error(), nil, ErrInvalidValue)
	}
	return agg.ToError()
}

// flattenSchemaLeaves walks the jsonschema/v6 ValidationError tree
// and appends each leaf (a node with no further Causes) to agg as a
// [*ValidationError] wrapping [ErrInvalidValue]. Internal nodes are
// recursed into but not reported, so the resulting aggregate
// contains only the actionable leaf failures rather than the
// synthetic "validation failed at /" headers v6 inserts at every
// level.
func flattenSchemaLeaves(node *jsonschema.ValidationError, agg *ValidationErrors) {
	if node == nil {
		return
	}
	if len(node.Causes) > 0 {
		for _, c := range node.Causes {
			flattenSchemaLeaves(c, agg)
		}
		return
	}
	field := strings.Join(node.InstanceLocation, ".")
	agg.Add(field, node.Error(), nil, ErrInvalidValue)
}
