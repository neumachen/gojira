package log_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    log.Format
		wantErr bool
	}{
		{name: "empty defaults to text", input: "", want: log.FormatText},
		{name: "lowercase text", input: "text", want: log.FormatText},
		{name: "uppercase TEXT", input: "TEXT", want: log.FormatText},
		{name: "mixed-case Text", input: "Text", want: log.FormatText},
		{name: "lowercase json", input: "json", want: log.FormatJSON},
		{name: "uppercase JSON", input: "JSON", want: log.FormatJSON},
		{name: "mixed-case Json", input: "Json", want: log.FormatJSON},

		{name: "invalid yaml", input: "yaml", wantErr: true},
		{name: "invalid xml", input: "xml", wantErr: true},
		{name: "whitespace only", input: "  ", wantErr: true},
		{name: "text with surrounding whitespace", input: " text ", wantErr: true},
		{name: "unknown random", input: "logfmt", wantErr: true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := log.ParseFormat(tc.input)
			if tc.wantErr {
				assert.Error(t, err, "expected error for input %q", tc.input)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "text", log.FormatText.String())
	assert.Equal(t, "json", log.FormatJSON.String())

	// Unknown values should fall back to a sensible debuggable label.
	unknown := log.Format(99).String()
	assert.Equal(t, "Format(99)", unknown)
}

func TestNewTextHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h := log.NewTextHandler(&buf, slog.LevelInfo)
	require.NotNil(t, h)

	logger := slog.New(h)
	logger.Info("hello", "key", "value")

	out := buf.String()
	assert.Contains(t, out, "hello", "expected message in output, got %q", out)
	assert.Contains(t, out, "key=value", "expected key=value pair in output, got %q", out)
	assert.Contains(t, out, "level=INFO", "expected level=INFO in output, got %q", out)
}

func TestNewJSONHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h := log.NewJSONHandler(&buf, slog.LevelInfo)
	require.NotNil(t, h)

	logger := slog.New(h)
	logger.Info("hello", "key", "value")

	// JSON handler should produce exactly one valid JSON object terminated
	// by a newline.
	out := buf.String()
	require.NotEmpty(t, out)

	var record map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record),
		"expected valid JSON, got %q", out)

	assert.Equal(t, "hello", record["msg"])
	assert.Equal(t, "INFO", record["level"])
	assert.Equal(t, "value", record["key"])
}

func TestNew_Text(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(log.FormatText, slog.LevelInfo, &buf)
	require.NotNil(t, logger)

	logger.Info("startup", "version", "v0.1.0")

	out := buf.String()
	assert.Contains(t, out, "startup")
	assert.Contains(t, out, "version=v0.1.0")
	assert.Contains(t, out, "level=INFO")

	// Text output is not valid JSON; confirm we did not accidentally
	// return a JSON handler.
	var m map[string]any
	assert.Error(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m),
		"text output should not parse as JSON")
}

func TestNew_JSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(log.FormatJSON, slog.LevelInfo, &buf)
	require.NotNil(t, logger)

	logger.Info("startup", "version", "v0.1.0")

	var record map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record),
		"expected valid JSON, got %q", buf.String())

	assert.Equal(t, "startup", record["msg"])
	assert.Equal(t, "INFO", record["level"])
	assert.Equal(t, "v0.1.0", record["version"])
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(log.FormatText, slog.LevelWarn, &buf)

	logger.Debug("debug-message")
	logger.Info("info-message")
	assert.Empty(t, buf.String(),
		"debug and info should be filtered at LevelWarn, got %q", buf.String())

	logger.Warn("warn-message")
	out := buf.String()
	assert.Contains(t, out, "warn-message",
		"warn should pass through at LevelWarn, got %q", out)
	assert.Contains(t, out, "level=WARN")
}

// TestErrextLogsCleanly verifies that *errext.TraceError values, which
// implement slog.LogValuer, are logged through the JSON handler without
// panicking and that the underlying cause message is preserved in the
// structured output. This protects the project's "errors carry their
// own stack frames" contract from regressions in this package.
func TestErrextLogsCleanly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(log.FormatJSON, slog.LevelInfo, &buf)

	const causeMsg = "fetch issue EXAMPLE-1: token expired"
	err := errext.Errorf("%s", causeMsg)
	require.NotNil(t, err)

	// Must not panic.
	require.NotPanics(t, func() {
		logger.Error("crawl failed", slog.Any("error", err))
	})

	out := buf.String()
	require.NotEmpty(t, out)

	// The output must be valid JSON.
	var record map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record),
		"expected valid JSON, got %q", out)

	assert.Equal(t, "crawl failed", record["msg"])
	assert.Equal(t, "ERROR", record["level"])

	// The cause message must be present somewhere in the serialised
	// output. errext renders *TraceError as a structured group via
	// LogValue, so the cause string ends up in a nested field rather
	// than at the top level; a substring check on the raw JSON is the
	// most robust way to assert this without coupling to errext's
	// internal field layout.
	assert.True(t, strings.Contains(out, causeMsg),
		"expected cause %q in JSON output, got %q", causeMsg, out)
}
