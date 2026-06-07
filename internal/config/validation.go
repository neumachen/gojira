package config

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidationError describes a single field-level failure produced by
// [Validator.Validate]. The wrapped Err is always either
// [ErrMissingRequired] or [ErrInvalidValue] so callers can keep using
// errors.Is for failure-class checks (the same semantics the existing
// envext-based [Build] already establishes).
//
// Field is the dotted path into the [App] tree, e.g. "jira.base_url".
// Message is human-readable. Value (when relevant) is the offending
// value; it is included verbatim in Error() unless empty/nil.
type ValidationError struct {
	Field   string
	Message string
	Value   any
	Err     error
}

// Error returns a short, single-line description suitable for CLI
// output and structured logs. The format mirrors the existing
// *ConfigError surface ("config: <key>: <reason>") so downstream
// formatters do not need to special-case the new error type.
func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Value != nil && e.Value != "" {
		return fmt.Sprintf("config: %s: %s (got %v)", e.Field, e.Message, e.Value)
	}
	return fmt.Sprintf("config: %s: %s", e.Field, e.Message)
}

// Unwrap returns the wrapped sentinel ([ErrMissingRequired] or
// [ErrInvalidValue]) so errors.Is keeps working with the same
// constants declared in config.go.
func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ValidationErrors aggregates one or more [ValidationError] values.
// It is returned by [Validator.Validate] when any rule fails; an
// empty list is returned as a nil error via [ValidationErrors.ToError].
//
// The aggregate honours errors.Is by walking its children, so
// errors.Is(verrs, ErrMissingRequired) is true when any contained
// error wraps that sentinel. This matches the "any failure of class X"
// semantics callers want (and that the existing translateEnvarError
// in config.go exposes for the legacy envext path).
type ValidationErrors struct {
	Errors []*ValidationError
}

// Error renders every aggregated failure on its own line, prefixed
// with "config: " (so a wrapping log line stays readable). When the
// aggregate is empty, Error returns the empty string; callers should
// use [ValidationErrors.ToError] to convert empty aggregates to nil.
func (v *ValidationErrors) Error() string {
	if v == nil || len(v.Errors) == 0 {
		return ""
	}
	if len(v.Errors) == 1 {
		return v.Errors[0].Error()
	}
	parts := make([]string, len(v.Errors))
	for i, e := range v.Errors {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Unwrap returns the child errors so errors.Is / errors.As can walk
// the aggregate. Go 1.20+ understands this []error shape.
func (v *ValidationErrors) Unwrap() []error {
	if v == nil {
		return nil
	}
	out := make([]error, 0, len(v.Errors))
	for _, e := range v.Errors {
		out = append(out, e)
	}
	return out
}

// Add appends a new [ValidationError] to the aggregate. It is a
// convenience used by [Validator.Validate] to keep rule code compact.
func (v *ValidationErrors) Add(field, message string, value any, sentinel error) {
	if v == nil {
		return
	}
	v.Errors = append(v.Errors, &ValidationError{
		Field:   field,
		Message: message,
		Value:   value,
		Err:     sentinel,
	})
}

// HasErrors reports whether the aggregate contains at least one
// failure. It is the cheap predicate alternative to checking
// ToError() != nil at call sites that do not want to allocate an
// error value just to inspect emptiness.
func (v *ValidationErrors) HasErrors() bool {
	return v != nil && len(v.Errors) > 0
}

// ToError returns the aggregate as a Go error when [HasErrors] is
// true, and nil otherwise. The conversion keeps the
// "if err := v.Validate(...); err != nil" idiom natural at every
// call site without forcing callers to know about ValidationErrors.
func (v *ValidationErrors) ToError() error {
	if !v.HasErrors() {
		return nil
	}
	return v
}

// Validator validates an [App] tree against the rules documented in
// the Phase 0 PRD (§2.2 canonical keys, §2.5 schema versioning).
//
// RequireSchema controls whether an empty [App.Schema] is rejected.
// The default, set by [NewValidator], is true: gojira.yaml is required
// to carry a schema identifier so future breaking changes can be
// detected before they cascade through the loader. Callers that need
// to validate a partially-populated App (e.g. unit tests for the
// loader cascade, before the schema layer is applied) can set
// RequireSchema=false; a non-empty mismatched schema still errors.
type Validator struct {
	RequireSchema bool
}

// NewValidator returns a Validator with the documented production
// defaults (RequireSchema=true).
func NewValidator() *Validator {
	return &Validator{RequireSchema: true}
}

// validLogLevelSet and validLogFormatSet mirror the rules already
// enforced by config.go's envext validators, kept in this file so the
// App-tree validator does not depend on package-private state that
// only the envext path uses.
var (
	validLogLevelSet = map[string]struct{}{
		"error": {}, "warn": {}, "info": {}, "debug": {},
	}
	validLogFormatSet = map[string]struct{}{
		"text": {}, "json": {},
	}
	validDevStatusDataTypeSet = map[string]struct{}{
		"pullrequest": {}, "branch": {}, "commit": {},
		"repository": {}, "build": {},
	}
)

// Validate inspects every field of a in PRD-documented order and
// returns a [ValidationErrors] aggregate (or nil if every rule
// passes). The aggregate wraps the existing [ErrMissingRequired] and
// [ErrInvalidValue] sentinels so errors.Is keeps working unchanged.
//
// A nil receiver uses the default Validator (RequireSchema=true), so
// ValidateApp(&a) and (&Validator{...}).Validate(&a) read identically.
// A nil App is itself a missing-required error against "app".
func (v *Validator) Validate(a *App) error {
	if v == nil {
		v = NewValidator()
	}
	verrs := &ValidationErrors{}

	if a == nil {
		verrs.Add("app", "is required", nil, ErrMissingRequired)
		return verrs.ToError()
	}

	// Schema: required (when RequireSchema) and, when present,
	// must match the current SchemaVersion. The latter check runs
	// even when RequireSchema=false so a future v2 file does not
	// silently load on a v1 binary.
	switch {
	case a.Schema == "":
		if v.RequireSchema {
			verrs.Add("schema", "is required", "",
				ErrInvalidValue)
		}
	case a.Schema != SchemaVersion:
		verrs.Add("schema",
			fmt.Sprintf("must equal %q", SchemaVersion),
			a.Schema, ErrInvalidValue)
	}

	// Jira block: BaseURL, Email, APIToken all required+non-empty.
	if strings.TrimSpace(a.Jira.BaseURL) == "" {
		verrs.Add("jira.base_url", "is required", "",
			ErrMissingRequired)
	} else if _, err := url.ParseRequestURI(a.Jira.BaseURL); err != nil {
		verrs.Add("jira.base_url",
			fmt.Sprintf("must be a valid URL: %v", err),
			a.Jira.BaseURL, ErrInvalidValue)
	}
	if strings.TrimSpace(a.Jira.Email) == "" {
		verrs.Add("jira.email", "is required", "",
			ErrMissingRequired)
	}
	if strings.TrimSpace(a.Jira.APIToken) == "" {
		verrs.Add("jira.api_token", "is required", "",
			ErrMissingRequired)
	}

	// Output block.
	if strings.TrimSpace(a.Output.Dir) == "" {
		verrs.Add("output.dir", "is required", "",
			ErrMissingRequired)
	}

	// Log block. Empty is accepted at validate time (the loader
	// applies DefaultLogSettings before validation in the normal
	// path); the rules below catch non-empty unknown values.
	if a.Log.Level != "" {
		if _, ok := validLogLevelSet[a.Log.Level]; !ok {
			verrs.Add("log.level",
				"must be one of error/warn/info/debug",
				a.Log.Level, ErrInvalidValue)
		}
	}
	if a.Log.Format != "" {
		if _, ok := validLogFormatSet[a.Log.Format]; !ok {
			verrs.Add("log.format",
				"must be one of text/json",
				a.Log.Format, ErrInvalidValue)
		}
	}

	// Crawl block: only DevStatusDataTypes has a closed-set rule;
	// the remaining knobs are numeric/boolean and validated by
	// envext's parser on the legacy path. The App-level validator
	// catches the same set so the file-only path (no env input)
	// also rejects invalid data types.
	for _, dt := range a.Crawl.DevStatusDataTypes {
		dt = strings.TrimSpace(dt)
		if dt == "" {
			continue
		}
		if _, ok := validDevStatusDataTypeSet[dt]; !ok {
			verrs.Add("crawl.dev_status_data_types",
				"must be a list of pullrequest/branch/commit/repository/build",
				dt, ErrInvalidValue)
			break
		}
	}

	return verrs.ToError()
}

// ValidateApp is a convenience that constructs a default [Validator]
// and runs it against a. It is the entry point most callers want.
func ValidateApp(a *App) error {
	return NewValidator().Validate(a)
}

// compile-time assurance that the public types satisfy the standard
// error interface so the surface stays consistent with config.go's
// *ConfigError.
var (
	_ error = (*ValidationError)(nil)
	_ error = (*ValidationErrors)(nil)
)
