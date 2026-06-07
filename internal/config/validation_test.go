package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validApp returns an App with every required field populated to a
// sensible value. Tests scope-in on a single rule by mutating one
// field of the returned value.
func validApp() *App {
	a := DefaultApp()
	a.Jira.BaseURL = "https://example.atlassian.net"
	a.Jira.Email = "me@example.com"
	a.Jira.APIToken = "tok-123"
	a.Output.Dir = "/tmp/out"
	return &a
}

// TestValidator_FullyValid asserts that a populated App with every
// required field set passes. This is the load-then-validate happy
// path the cascade loader will hit on a well-configured machine.
func TestValidator_FullyValid(t *testing.T) {
	err := ValidateApp(validApp())
	require.NoError(t, err)
}

// TestValidator_MissingRequired exhaustively covers the
// missing-required rules: every field that should wrap
// ErrMissingRequired is checked, both for absence and for an
// "all whitespace" value (TrimSpace must catch the latter).
func TestValidator_MissingRequired(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(a *App)
		wantField string
	}{
		{"jira.base_url empty", func(a *App) { a.Jira.BaseURL = "" }, "jira.base_url"},
		{"jira.base_url whitespace", func(a *App) { a.Jira.BaseURL = "   " }, "jira.base_url"},
		{"jira.email empty", func(a *App) { a.Jira.Email = "" }, "jira.email"},
		{"jira.email whitespace", func(a *App) { a.Jira.Email = "\t" }, "jira.email"},
		{"jira.api_token empty", func(a *App) { a.Jira.APIToken = "" }, "jira.api_token"},
		{"output.dir empty", func(a *App) { a.Output.Dir = "" }, "output.dir"},
		{"output.dir whitespace", func(a *App) { a.Output.Dir = " " }, "output.dir"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := validApp()
			tc.mutate(a)
			err := ValidateApp(a)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrMissingRequired,
				"must wrap ErrMissingRequired")
			assert.NotErrorIs(t, err, ErrInvalidValue,
				"must not wrap ErrInvalidValue for missing-required")

			var ve *ValidationError
			require.True(t, errorsAsAny(err, &ve),
				"aggregate must surface a *ValidationError via errors.As")
			assert.Equal(t, tc.wantField, ve.Field)
		})
	}
}

// TestValidator_InvalidValue covers the closed-set rules and the URL
// parseability rule, all of which must wrap ErrInvalidValue.
func TestValidator_InvalidValue(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(a *App)
		wantField string
	}{
		{
			name:      "jira.base_url not a URL",
			mutate:    func(a *App) { a.Jira.BaseURL = "not-a-url" },
			wantField: "jira.base_url",
		},
		{
			name:      "log.level unknown",
			mutate:    func(a *App) { a.Log.Level = "trace" },
			wantField: "log.level",
		},
		{
			name:      "log.format unknown",
			mutate:    func(a *App) { a.Log.Format = "xml" },
			wantField: "log.format",
		},
		{
			name: "crawl.dev_status_data_types invalid entry",
			mutate: func(a *App) {
				a.Crawl.DevStatusDataTypes = []string{"pullrequest", "bogus"}
			},
			wantField: "crawl.dev_status_data_types",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := validApp()
			tc.mutate(a)
			err := ValidateApp(a)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidValue,
				"must wrap ErrInvalidValue")

			var ve *ValidationError
			require.True(t, errorsAsAny(err, &ve))
			assert.Equal(t, tc.wantField, ve.Field)
		})
	}
}

// TestValidator_Schema_RequireSchemaTrue covers the schema rule with
// the default RequireSchema=true: empty schema is rejected, mismatched
// schema is rejected, exact match passes.
func TestValidator_Schema_RequireSchemaTrue(t *testing.T) {
	t.Run("empty schema rejected", func(t *testing.T) {
		a := validApp()
		a.Schema = ""
		err := NewValidator().Validate(a)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidValue)
		var ve *ValidationError
		require.True(t, errorsAsAny(err, &ve))
		assert.Equal(t, "schema", ve.Field)
	})

	t.Run("mismatched schema rejected", func(t *testing.T) {
		a := validApp()
		a.Schema = "gojira.config.v2"
		err := NewValidator().Validate(a)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidValue)
		var ve *ValidationError
		require.True(t, errorsAsAny(err, &ve))
		assert.Equal(t, "schema", ve.Field)
	})

	t.Run("exact match passes", func(t *testing.T) {
		a := validApp()
		a.Schema = SchemaVersion
		require.NoError(t, NewValidator().Validate(a))
	})
}

// TestValidator_Schema_RequireSchemaFalse asserts that an empty
// schema is tolerated when the caller opts out (e.g. mid-cascade
// validation), but a non-empty mismatched schema is still rejected.
func TestValidator_Schema_RequireSchemaFalse(t *testing.T) {
	v := &Validator{RequireSchema: false}

	t.Run("empty schema accepted", func(t *testing.T) {
		a := validApp()
		a.Schema = ""
		require.NoError(t, v.Validate(a))
	})

	t.Run("non-empty mismatch still rejected", func(t *testing.T) {
		a := validApp()
		a.Schema = "gojira.config.v999"
		err := v.Validate(a)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidValue)
	})
}

// TestValidator_NilApp asserts that a nil App is itself a
// missing-required failure rather than a panic.
func TestValidator_NilApp(t *testing.T) {
	err := ValidateApp(nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissingRequired)
}

// TestValidator_NilReceiverUsesDefaults asserts that a nil Validator
// receiver behaves identically to NewValidator().
func TestValidator_NilReceiverUsesDefaults(t *testing.T) {
	var v *Validator
	err := v.Validate(validApp())
	require.NoError(t, err, "nil Validator must default to NewValidator()")
}

// TestValidationErrors_AggregatesMultiple asserts that a single
// Validate call surfaces every failing rule, not just the first.
func TestValidationErrors_AggregatesMultiple(t *testing.T) {
	a := validApp()
	a.Jira.Email = ""
	a.Output.Dir = ""
	a.Log.Level = "trace"

	err := ValidateApp(a)
	require.Error(t, err)

	var verrs *ValidationErrors
	require.True(t, errors.As(err, &verrs),
		"aggregate must surface as *ValidationErrors via errors.As")
	require.True(t, verrs.HasErrors())
	assert.GreaterOrEqual(t, len(verrs.Errors), 3,
		"all three failures must be reported, not short-circuited")

	// errors.Is on the aggregate must report true for any sentinel
	// that any child wraps.
	assert.ErrorIs(t, err, ErrMissingRequired)
	assert.ErrorIs(t, err, ErrInvalidValue)
}

// TestValidationError_ErrorMessage spot-checks the formatted string
// — primarily to lock in the "config: <field>: <message>" prefix
// downstream loggers depend on.
func TestValidationError_ErrorMessage(t *testing.T) {
	ve := &ValidationError{
		Field:   "jira.base_url",
		Message: "is required",
		Err:     ErrMissingRequired,
	}
	assert.Equal(t, "config: jira.base_url: is required", ve.Error())

	ve2 := &ValidationError{
		Field:   "log.level",
		Message: "must be one of error/warn/info/debug",
		Value:   "trace",
		Err:     ErrInvalidValue,
	}
	assert.Contains(t, ve2.Error(), "config: log.level:")
	assert.Contains(t, ve2.Error(), "trace")
}

// errorsAsAny is a tiny helper so the test reads naturally with the
// errors.As-via-pointer idiom. It exists because errors.As panics on
// nil targets; the helper isolates the failure and returns false so
// the caller's require.True message is the diagnostic that fires.
func errorsAsAny(err error, target any) bool {
	return errors.As(err, target)
}
