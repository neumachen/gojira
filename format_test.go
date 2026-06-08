// Package gojira_test — unit tests for the OutputFormat enum.
package gojira_test

import (
	"testing"

	gojira "github.com/neumachen/gojira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// OutputFormat — String()
// ---------------------------------------------------------------------------

func TestOutputFormat_String(t *testing.T) {
	tests := []struct {
		format gojira.OutputFormat
		want   string
	}{
		{gojira.FormatStructured, "structured"},
		{gojira.FormatMarkdown, "markdown"},
		{gojira.FormatJSON, "json"},
		// Unknown value must not panic and must return a non-empty string.
		{gojira.OutputFormat(99), "OutputFormat(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.format.String()
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// ParseOutputFormat
// ---------------------------------------------------------------------------

func TestParseOutputFormat_ValidInputs(t *testing.T) {
	tests := []struct {
		input string
		want  gojira.OutputFormat
	}{
		// Canonical lower-case forms.
		{"structured", gojira.FormatStructured},
		{"markdown", gojira.FormatMarkdown},
		{"json", gojira.FormatJSON},
		// Case-insensitive variants.
		{"Structured", gojira.FormatStructured},
		{"MARKDOWN", gojira.FormatMarkdown},
		{"Json", gojira.FormatJSON},
		// Aliases / proto-style names.
		{"FORMAT_STRUCTURED", gojira.FormatStructured},
		{"FORMAT_MARKDOWN", gojira.FormatMarkdown},
		{"FORMAT_JSON", gojira.FormatJSON},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := gojira.ParseOutputFormat(tt.input)
			require.NoError(t, err, "ParseOutputFormat(%q) must not error", tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseOutputFormat_InvalidInputs(t *testing.T) {
	invalids := []string{"", "raw", "html", "unknown", "FORMAT_UNKNOWN"}

	for _, s := range invalids {
		t.Run(s, func(t *testing.T) {
			_, err := gojira.ParseOutputFormat(s)
			assert.Error(t, err, "ParseOutputFormat(%q) must return an error", s)
			assert.ErrorIs(t, err, gojira.ErrConfigInvalidValue,
				"error must wrap ErrConfigInvalidValue")
		})
	}
}

// ---------------------------------------------------------------------------
// Round-trip: String → ParseOutputFormat
// ---------------------------------------------------------------------------

func TestOutputFormat_RoundTrip(t *testing.T) {
	formats := []gojira.OutputFormat{
		gojira.FormatStructured,
		gojira.FormatMarkdown,
		gojira.FormatJSON,
	}

	for _, f := range formats {
		t.Run(f.String(), func(t *testing.T) {
			parsed, err := gojira.ParseOutputFormat(f.String())
			require.NoError(t, err)
			assert.Equal(t, f, parsed, "round-trip must be identity")
		})
	}
}
