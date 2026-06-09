package integtest

import (
	"os"
	"path/filepath"
	"testing"

	gojira "github.com/neumachen/gojira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadServerConfig exercises the full cascade for LoadServerConfig:
// embedded defaults < YAML file < GOJIRA_SERVER_ADDRESS env var.
//
// The gRPC server calls LoadServerConfig instead of LoadAppConfig so that
// server-only settings never pollute the crawl Config. These tests verify
// the three cascade layers independently and in combination.
func TestLoadServerConfig(t *testing.T) {
	// writeYAML writes a minimal YAML config file and returns its path.
	writeYAML := func(t *testing.T, content string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "gojira.yaml")
		require.NoError(t, os.WriteFile(p, []byte(content), 0600))
		return p
	}

	const defaultAddr = "127.0.0.1:50051"

	tests := []struct {
		name       string
		configPath string
		env        map[string]string
		wantAddr   string
		wantErr    bool
	}{
		{
			name:       "default address when no config and no env",
			configPath: "",
			env:        map[string]string{},
			wantAddr:   defaultAddr,
		},
		{
			name: "YAML file overrides default",
			configPath: writeYAML(t, `schema: gojira.config.v1
server:
  address: "0.0.0.0:9090"
`),
			env:      map[string]string{},
			wantAddr: "0.0.0.0:9090",
		},
		{
			name: "GOJIRA_SERVER_ADDRESS env overrides YAML file",
			configPath: writeYAML(t, `schema: gojira.config.v1
server:
  address: "0.0.0.0:9090"
`),
			env: map[string]string{
				"GOJIRA_SERVER_ADDRESS": "0.0.0.0:7777",
			},
			wantAddr: "0.0.0.0:7777",
		},
		{
			name:       "GOJIRA_SERVER_ADDRESS env overrides default (no file)",
			configPath: "",
			env: map[string]string{
				"GOJIRA_SERVER_ADDRESS": "0.0.0.0:8080",
			},
			wantAddr: "0.0.0.0:8080",
		},
		{
			name:       "explicit but missing config path is a hard error",
			configPath: filepath.Join(t.TempDir(), "does-not-exist.yaml"),
			env:        map[string]string{},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gojira.LoadServerConfig(tt.configPath, tt.env)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantAddr, got.Address)
		})
	}
}
