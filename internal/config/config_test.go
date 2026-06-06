package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullValid returns a map containing every GOJIRA_* key set to a valid
// value. Tests that need to exercise a single key can delete or
// overwrite entries in the returned map.
func fullValid() map[string]string {
	return map[string]string{
		"GOJIRA_SITE":             "https://example.atlassian.net",
		"GOJIRA_USER":             "user@example.com",
		"GOJIRA_TOKEN":            "secret-token",
		"GOJIRA_OUTPUT_DIR":       "/tmp/output",
		"GOJIRA_DEPTH_LIMIT":      "5",
		"GOJIRA_ISSUE_CAP":        "100",
		"GOJIRA_TIME_CAP_SECONDS": "3600",
		"GOJIRA_CONCURRENCY":      "4",
		"GOJIRA_REFETCH":          "true",
		"GOJIRA_LOG_LEVEL":        "debug",
		"GOJIRA_INCLUDE_COMMENTS": "true",
	}
}

// TestBuild_RequiredKeyAbsent verifies that Build returns an error
// wrapping ErrMissingRequired when each required key is absent.
func TestBuild_RequiredKeyAbsent(t *testing.T) {
	requiredKeys := []string{
		"GOJIRA_SITE",
		"GOJIRA_USER",
		"GOJIRA_TOKEN",
		"GOJIRA_OUTPUT_DIR",
	}

	for _, key := range requiredKeys {
		t.Run("missing_"+key, func(t *testing.T) {
			kv := fullValid()
			delete(kv, key)

			_, err := Build(kv)
			require.Error(t, err, "expected error for missing %s", key)
			assert.ErrorIs(t, err, ErrMissingRequired, "expected ErrMissingRequired for %s", key)
			var ce *ConfigError
			require.ErrorAs(t, err, &ce, "expected *ConfigError for %s", key)
			assert.Equal(t, key, ce.Key, "ConfigError.Key")
		})
	}
}

// TestBuild_RequiredKeyEmpty verifies that an empty string value for a
// required key is treated the same as absent.
func TestBuild_RequiredKeyEmpty(t *testing.T) {
	requiredKeys := []string{
		"GOJIRA_SITE",
		"GOJIRA_USER",
		"GOJIRA_TOKEN",
		"GOJIRA_OUTPUT_DIR",
	}

	for _, key := range requiredKeys {
		t.Run("empty_"+key, func(t *testing.T) {
			kv := fullValid()
			kv[key] = ""

			_, err := Build(kv)
			require.Error(t, err, "expected error for empty %s", key)
			assert.ErrorIs(t, err, ErrMissingRequired, "expected ErrMissingRequired for empty %s", key)
		})
	}
}

// TestBuild_InvalidSiteURL verifies that a non-URL value for GOJIRA_SITE
// returns an error wrapping ErrInvalidValue.
func TestBuild_InvalidSiteURL(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"not_a_url", "not-a-url"},
		{"bare_hostname", "example.atlassian.net"},
		{"empty_scheme", "://example.atlassian.net"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kv := fullValid()
			kv["GOJIRA_SITE"] = tc.value

			_, err := Build(kv)
			require.Error(t, err, "expected error for GOJIRA_SITE=%q", tc.value)
			assert.ErrorIs(t, err, ErrInvalidValue, "expected ErrInvalidValue for GOJIRA_SITE=%q", tc.value)
			var ce *ConfigError
			require.ErrorAs(t, err, &ce, "expected *ConfigError")
			assert.Equal(t, "GOJIRA_SITE", ce.Key, "ConfigError.Key")
		})
	}
}

// TestBuild_InvalidInt verifies that a non-integer value for each
// integer key returns an error wrapping ErrInvalidValue.
func TestBuild_InvalidInt(t *testing.T) {
	intKeys := []string{
		"GOJIRA_DEPTH_LIMIT",
		"GOJIRA_ISSUE_CAP",
		"GOJIRA_TIME_CAP_SECONDS",
		"GOJIRA_CONCURRENCY",
	}

	for _, key := range intKeys {
		t.Run("invalid_int_"+key, func(t *testing.T) {
			kv := fullValid()
			kv[key] = "not-an-int"

			_, err := Build(kv)
			require.Error(t, err, "expected error for %s=not-an-int", key)
			assert.ErrorIs(t, err, ErrInvalidValue, "expected ErrInvalidValue for %s", key)
			var ce *ConfigError
			require.ErrorAs(t, err, &ce, "expected *ConfigError")
			assert.Equal(t, key, ce.Key, "ConfigError.Key")
		})
	}
}

// TestBuild_InvalidBool verifies that a non-boolean value for each
// boolean key returns an error wrapping ErrInvalidValue.
func TestBuild_InvalidBool(t *testing.T) {
	boolKeys := []string{
		"GOJIRA_REFETCH",
		"GOJIRA_INCLUDE_COMMENTS",
	}

	for _, key := range boolKeys {
		t.Run("invalid_bool_"+key, func(t *testing.T) {
			kv := fullValid()
			kv[key] = "yes" // not accepted by strconv.ParseBool

			_, err := Build(kv)
			require.Error(t, err, "expected error for %s=yes", key)
			assert.ErrorIs(t, err, ErrInvalidValue, "expected ErrInvalidValue for %s", key)
			var ce *ConfigError
			require.ErrorAs(t, err, &ce, "expected *ConfigError")
			assert.Equal(t, key, ce.Key, "ConfigError.Key")
		})
	}
}

// TestBuild_InvalidLogLevel verifies that an unrecognised log level
// returns an error wrapping ErrInvalidValue.
func TestBuild_InvalidLogLevel(t *testing.T) {
	cases := []string{"verbose", "WARNING", "trace", "INFO"} // wrong case or unknown

	for _, v := range cases {
		t.Run("log_level_"+v, func(t *testing.T) {
			kv := fullValid()
			kv["GOJIRA_LOG_LEVEL"] = v

			_, err := Build(kv)
			require.Error(t, err, "expected error for GOJIRA_LOG_LEVEL=%q", v)
			assert.ErrorIs(t, err, ErrInvalidValue, "expected ErrInvalidValue for GOJIRA_LOG_LEVEL=%q", v)
			var ce *ConfigError
			require.ErrorAs(t, err, &ce, "expected *ConfigError")
			assert.Equal(t, "GOJIRA_LOG_LEVEL", ce.Key, "ConfigError.Key")
		})
	}
}

// TestBuild_ValidFullConfig verifies that a map with all keys set to
// valid values produces the expected Config with no error.
func TestBuild_ValidFullConfig(t *testing.T) {
	kv := fullValid()

	cfg, err := Build(kv)
	require.NoError(t, err)

	assert.Equal(t, "https://example.atlassian.net", cfg.Site, "Site")
	assert.Equal(t, "user@example.com", cfg.User, "User")
	assert.Equal(t, "secret-token", cfg.Token, "Token")
	assert.Equal(t, "/tmp/output", cfg.OutputDir, "OutputDir")
	assert.Equal(t, 5, cfg.DepthLimit, "DepthLimit")
	assert.Equal(t, 100, cfg.IssueCap, "IssueCap")
	assert.Equal(t, 3600, cfg.TimeCapSeconds, "TimeCapSeconds")
	assert.Equal(t, 4, cfg.Concurrency, "Concurrency")
	assert.True(t, cfg.Refetch, "Refetch")
	assert.Equal(t, "debug", cfg.LogLevel, "LogLevel")
	assert.True(t, cfg.IncludeComments, "IncludeComments")
}

// TestBuild_Defaults verifies that optional keys absent from the map
// receive their documented default values.
func TestBuild_Defaults(t *testing.T) {
	// Only required keys; all optional keys absent.
	kv := map[string]string{
		"GOJIRA_SITE":       "https://example.atlassian.net",
		"GOJIRA_USER":       "user@example.com",
		"GOJIRA_TOKEN":      "secret-token",
		"GOJIRA_OUTPUT_DIR": "/tmp/output",
	}

	cfg, err := Build(kv)
	require.NoError(t, err)

	assert.Equal(t, 0, cfg.DepthLimit, "DepthLimit default")
	assert.Equal(t, 500, cfg.IssueCap, "IssueCap default")
	assert.Equal(t, 0, cfg.TimeCapSeconds, "TimeCapSeconds default")
	assert.Equal(t, 3, cfg.Concurrency, "Concurrency default")
	assert.False(t, cfg.Refetch, "Refetch default")
	assert.Equal(t, "info", cfg.LogLevel, "LogLevel default")
	assert.False(t, cfg.IncludeComments, "IncludeComments default")
}

// TestBuild_ValidLogLevels verifies that all four accepted log level
// values are accepted without error.
func TestBuild_ValidLogLevels(t *testing.T) {
	levels := []string{"error", "warn", "info", "debug"}

	for _, level := range levels {
		t.Run("log_level_"+level, func(t *testing.T) {
			kv := fullValid()
			kv["GOJIRA_LOG_LEVEL"] = level

			cfg, err := Build(kv)
			require.NoError(t, err, "GOJIRA_LOG_LEVEL=%q", level)
			assert.Equal(t, level, cfg.LogLevel, "LogLevel")
		})
	}
}

// TestBuild_ValidBoolVariants verifies that strconv.ParseBool-accepted
// variants ("1", "t", "T", "TRUE", "true", "True", "0", "f", "F",
// "FALSE", "false", "False") are accepted for boolean keys.
func TestBuild_ValidBoolVariants(t *testing.T) {
	trueVariants := []string{"1", "t", "T", "TRUE", "true", "True"}
	falseVariants := []string{"0", "f", "F", "FALSE", "false", "False"}

	for _, v := range trueVariants {
		t.Run("refetch_true_"+v, func(t *testing.T) {
			kv := fullValid()
			kv["GOJIRA_REFETCH"] = v
			cfg, err := Build(kv)
			require.NoError(t, err, "GOJIRA_REFETCH=%q", v)
			assert.True(t, cfg.Refetch, "Refetch for value %q", v)
		})
	}

	for _, v := range falseVariants {
		t.Run("refetch_false_"+v, func(t *testing.T) {
			kv := fullValid()
			kv["GOJIRA_REFETCH"] = v
			cfg, err := Build(kv)
			require.NoError(t, err, "GOJIRA_REFETCH=%q", v)
			assert.False(t, cfg.Refetch, "Refetch for value %q", v)
		})
	}
}

// TestBuild_ZeroIntValues verifies that "0" is a valid value for
// integer keys (not treated as absent/default).
func TestBuild_ZeroIntValues(t *testing.T) {
	kv := fullValid()
	kv["GOJIRA_DEPTH_LIMIT"] = "0"
	kv["GOJIRA_ISSUE_CAP"] = "0"
	kv["GOJIRA_TIME_CAP_SECONDS"] = "0"
	kv["GOJIRA_CONCURRENCY"] = "0"

	cfg, err := Build(kv)
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.DepthLimit, "DepthLimit")
	assert.Equal(t, 0, cfg.IssueCap, "IssueCap")
	assert.Equal(t, 0, cfg.TimeCapSeconds, "TimeCapSeconds")
	assert.Equal(t, 0, cfg.Concurrency, "Concurrency")
}

// TestConfigError_ErrorMessage verifies that ConfigError.Error() returns
// a useful human-readable string.
func TestConfigError_ErrorMessage(t *testing.T) {
	ce := &ConfigError{
		Key:      "GOJIRA_SITE",
		Reason:   "is required",
		sentinel: ErrMissingRequired,
	}
	msg := ce.Error()
	assert.NotEmpty(t, msg, "ConfigError.Error() should not be empty")
	// Should contain the key name.
	assert.Contains(t, msg, "GOJIRA_SITE", "ConfigError.Error() should contain key name")
}
