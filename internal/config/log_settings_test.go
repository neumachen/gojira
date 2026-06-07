package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultLogSettings asserts the documented defaults: level=info,
// format=text.
func TestDefaultLogSettings(t *testing.T) {
	got := DefaultLogSettings()
	assert.Equal(t, "info", got.Level, "Level")
	assert.Equal(t, "text", got.Format, "Format")
}

// TestLogSettings_EffectiveLevel_Precedence covers cli > configured >
// default, including the nil-receiver path. When neither cli nor
// configured supplies a value, the embedded default ("info") is
// returned — distinguishing LogSettings from JiraSettings, which has
// no embedded default.
func TestLogSettings_EffectiveLevel_Precedence(t *testing.T) {
	configured := &LogSettings{Level: "debug", Format: "json"}

	tests := []struct {
		name      string
		recv      *LogSettings
		cli       string
		wantLevel string
	}{
		{"cli wins over configured", configured, "warn", "warn"},
		{"configured wins when cli empty", configured, "", "debug"},
		{"default when neither", &LogSettings{}, "", "info"},
		{"nil receiver returns default", nil, "", "info"},
		{"nil receiver still honors cli", nil, "error", "error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantLevel, tc.recv.EffectiveLevel(tc.cli))
		})
	}
}

// TestLogSettings_EffectiveFormat_Precedence mirrors the level test
// for the format field; the default falls back to "text".
func TestLogSettings_EffectiveFormat_Precedence(t *testing.T) {
	configured := &LogSettings{Level: "debug", Format: "json"}

	tests := []struct {
		name       string
		recv       *LogSettings
		cli        string
		wantFormat string
	}{
		{"cli wins over configured", configured, "text", "text"},
		{"configured wins when cli empty", configured, "", "json"},
		{"default when neither", &LogSettings{}, "", "text"},
		{"nil receiver returns default", nil, "", "text"},
		{"nil receiver still honors cli", nil, "json", "json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantFormat, tc.recv.EffectiveFormat(tc.cli))
		})
	}
}

// TestLogSettings_StructTags asserts the GOJIRA_LOG_LEVEL /
// GOJIRA_LOG_FORMAT env tags and yaml/json tags.
func TestLogSettings_StructTags(t *testing.T) {
	rt := reflect.TypeOf(LogSettings{})

	cases := []struct {
		field    string
		wantEnv  string
		wantYAML string
		wantJSON string
	}{
		{"Level", "GOJIRA_LOG_LEVEL", "level", "level"},
		{"Format", "GOJIRA_LOG_FORMAT", "format", "format"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			sf, ok := rt.FieldByName(tc.field)
			if !ok {
				t.Fatalf("field %q missing from LogSettings", tc.field)
			}
			assert.Equal(t, tc.wantEnv, sf.Tag.Get("env"), "env tag")
			assert.Equal(t, tc.wantYAML, sf.Tag.Get("yaml"), "yaml tag")
			assert.Equal(t, tc.wantJSON, sf.Tag.Get("json"), "json tag")
		})
	}
}
