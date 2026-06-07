package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTempConfigYAML writes a complete, valid gojira.yaml document
// to a fresh temp file rooted at t.TempDir and returns the path. The
// supplied site URL is the only template parameter; everything else
// (creds, output-dir placeholder) is fixed so the test reads naturally.
// The caller is expected to override output-dir via env or flag, since
// the YAML's value cannot be parametrised at compile time.
func writeTempConfigYAML(t *testing.T, siteURL, outputDir string) string {
	t.Helper()
	body := fmt.Sprintf(`schema: gojira.config.v1
jira:
  base_url: %s
  email: test@example.com
  api_token: test-token
output:
  dir: %s
crawl:
  concurrency: 1
  issue_cap: 0
log:
  level: info
  format: text
`, siteURL, outputDir)
	path := filepath.Join(t.TempDir(), "gojira.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// TestRun_ConfigFlag_LoadsFromYAML asserts the --config flag points
// the CLI at a YAML file and the values in that file flow through to
// the crawl. The env map is intentionally empty so the YAML is the
// ONLY source of configuration aside from the positional ISSUE-KEY;
// the resulting exit 0 and on-disk output prove the cascade reached
// the crawler with the file's settings intact.
func TestRun_ConfigFlag_LoadsFromYAML(t *testing.T) {
	outputDir := t.TempDir()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}
		body := minimalIssueJSON(key, srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	configPath := writeTempConfigYAML(t, srv.URL, outputDir)
	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--config", configPath, "EXAMPLE-1"},
		nil) // nil env: the YAML is the only config source
	_ = stdout
	if code != 0 {
		t.Logf("stderr: %s", stderr)
	}
	assert.Equal(t, 0, code, "exit 0 expected when --config supplies all required values")
	_, err := os.Stat(filepath.Join(outputDir, "EXAMPLE-1", "index.md"))
	assert.NoError(t, err, "expected output file written under YAML's output.dir")
}

// TestRun_ConfigFlag_MissingFileIsExit1 asserts that an explicit
// --config path pointing at a non-existent file is a hard error,
// exits with code 1, and surfaces a message that names the missing
// path so the user can diagnose the failure.
func TestRun_ConfigFlag_MissingFileIsExit1(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	// A valid env is supplied so the only reason for failure is the
	// missing --config path.
	env := baseEnv("https://x.atlassian.net", t.TempDir())
	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--config", missing, "EXAMPLE-1"},
		env)
	assert.Equal(t, 1, code, "explicit-but-missing --config must exit 1")
	assert.Contains(t, stderr, missing,
		"stderr must name the missing path so the user can act on it")
}

// TestRun_ConfigFlag_FlagOverridesYAML asserts the precedence
// contract for --output-dir: when both the YAML and the CLI flag
// supply output.dir, the flag wins. This pins the documented
// "flag > env > file" precedence at the CLI surface.
func TestRun_ConfigFlag_FlagOverridesYAML(t *testing.T) {
	fileDir := t.TempDir()
	flagDir := t.TempDir()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}
		body := minimalIssueJSON(key, srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	configPath := writeTempConfigYAML(t, srv.URL, fileDir)
	_, _, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--config", configPath,
			"--output-dir", flagDir, "EXAMPLE-1"},
		nil)
	assert.Equal(t, 0, code, "exit 0 expected")

	// Output must land in flagDir, not in fileDir.
	_, err := os.Stat(filepath.Join(flagDir, "EXAMPLE-1", "index.md"))
	assert.NoError(t, err, "expected output in --output-dir, not file's output.dir")
	_, err = os.Stat(filepath.Join(fileDir, "EXAMPLE-1", "index.md"))
	assert.Error(t, err, "file's output.dir must NOT receive output when --output-dir is supplied")
}

// TestRun_ConfigFlag_EnvOverridesYAML asserts the middle precedence
// tier: when both the YAML file and the env map supply output.dir,
// env wins. Combined with TestRun_ConfigFlag_FlagOverridesYAML, the
// full file < env < flag chain is pinned.
func TestRun_ConfigFlag_EnvOverridesYAML(t *testing.T) {
	fileDir := t.TempDir()
	envDir := t.TempDir()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/rest/api/3/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}
		body := minimalIssueJSON(key, srv.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	configPath := writeTempConfigYAML(t, srv.URL, fileDir)
	env := map[string]string{
		"GOJIRA_OUTPUT_DIR": envDir,
		// site/user/token come from the YAML — no need to set them.
	}
	_, _, code := captureRun(context.Background(),
		[]string{"gojira", "crawl", "--config", configPath, "EXAMPLE-1"},
		env)
	assert.Equal(t, 0, code, "exit 0 expected")
	_, err := os.Stat(filepath.Join(envDir, "EXAMPLE-1", "index.md"))
	assert.NoError(t, err, "GOJIRA_OUTPUT_DIR env value must override file's output.dir")
}
