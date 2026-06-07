package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultOutputSettings asserts the zero-value default leaves
// Dir empty; the output directory is not embedded.
func TestDefaultOutputSettings(t *testing.T) {
	got := DefaultOutputSettings()
	assert.Equal(t, "", got.Dir, "Dir")
	assert.Equal(t, OutputSettings{}, got, "DefaultOutputSettings is the zero value")
}

// TestOutputSettings_EffectiveDir_Precedence covers cli > configured >
// "", including the nil-receiver path.
func TestOutputSettings_EffectiveDir_Precedence(t *testing.T) {
	configured := &OutputSettings{Dir: "/configured"}

	tests := []struct {
		name string
		recv *OutputSettings
		cli  string
		want string
	}{
		{"cli wins over configured", configured, "/cli", "/cli"},
		{"configured wins when cli empty", configured, "", "/configured"},
		{"empty when neither", &OutputSettings{}, "", ""},
		{"nil receiver tolerated", nil, "", ""},
		{"nil receiver still honors cli", nil, "/cli", "/cli"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.recv.EffectiveDir(tc.cli))
		})
	}
}

// TestOutputSettings_StructTags asserts the GOJIRA_OUTPUT_DIR env tag
// and yaml/json `dir` tags are present and correct.
func TestOutputSettings_StructTags(t *testing.T) {
	rt := reflect.TypeOf(OutputSettings{})
	sf, ok := rt.FieldByName("Dir")
	if !ok {
		t.Fatal("field Dir missing from OutputSettings")
	}
	assert.Equal(t, "GOJIRA_OUTPUT_DIR", sf.Tag.Get("env"), "env tag")
	assert.Equal(t, "dir", sf.Tag.Get("yaml"), "yaml tag")
	assert.Equal(t, "dir", sf.Tag.Get("json"), "json tag")
}
