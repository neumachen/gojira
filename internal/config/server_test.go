package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultServerSettings asserts DefaultServerSettings returns the
// loopback bind address 127.0.0.1:50051 as the embedded default.
func TestDefaultServerSettings(t *testing.T) {
	got := DefaultServerSettings()
	assert.Equal(t, "127.0.0.1:50051", got.Address, "Address")
	assert.Equal(t, ServerSettings{Address: "127.0.0.1:50051"}, got,
		"DefaultServerSettings must equal the expected literal")
}

// TestServerSettings_EffectiveAddress_Precedence covers cli >
// configured > "127.0.0.1:50051", including the nil-receiver path.
func TestServerSettings_EffectiveAddress_Precedence(t *testing.T) {
	configured := &ServerSettings{Address: "0.0.0.0:9090"}

	tests := []struct {
		name string
		recv *ServerSettings
		cli  string
		want string
	}{
		{"cli wins over configured", configured, "0.0.0.0:8080", "0.0.0.0:8080"},
		{"configured wins when cli empty", configured, "", "0.0.0.0:9090"},
		{"default when neither", &ServerSettings{}, "", "127.0.0.1:50051"},
		{"nil receiver tolerated", nil, "", "127.0.0.1:50051"},
		{"nil receiver still honors cli", nil, "0.0.0.0:8080", "0.0.0.0:8080"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.recv.EffectiveAddress(tc.cli))
		})
	}
}

// TestServerSettings_EnvOverride asserts that GOJIRA_SERVER_ADDRESS
// in the environment overrides the embedded default when the cascade
// is run via LoadApp with an env map.
func TestServerSettings_EnvOverride(t *testing.T) {
	env := map[string]string{
		// Minimum required fields so Layer-2 validation passes.
		"GOJIRA_JIRA_BASE_URL":  "https://example.atlassian.net",
		"GOJIRA_JIRA_EMAIL":     "me@example.com",
		"GOJIRA_JIRA_API_TOKEN": "tok-123",
		"GOJIRA_OUTPUT_DIR":     "/tmp/out",
		// The field under test.
		"GOJIRA_SERVER_ADDRESS": "0.0.0.0:9090",
	}

	app, err := LoadApp(LoadOptions{Env: env})
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:9090", app.Server.Address,
		"GOJIRA_SERVER_ADDRESS must override the embedded default")
}

// TestServerSettings_StructTags asserts the GOJIRA_SERVER_ADDRESS env
// tag and yaml/json "address" tags are present and correct.
func TestServerSettings_StructTags(t *testing.T) {
	rt := reflect.TypeOf(ServerSettings{})
	sf, ok := rt.FieldByName("Address")
	if !ok {
		t.Fatal("field Address missing from ServerSettings")
	}
	assert.Equal(t, "GOJIRA_SERVER_ADDRESS", sf.Tag.Get("env"), "env tag")
	assert.Equal(t, "address", sf.Tag.Get("yaml"), "yaml tag")
	assert.Equal(t, "address", sf.Tag.Get("json"), "json tag")
}
