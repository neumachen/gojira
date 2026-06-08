package gojira

import (
	"fmt"
	"strings"
)

// OutputFormat selects the presentation form of a fetched Jira issue.
//
// # Orthogonality
//
// Format (presentation) and Store (destination) are independent concerns.
// A caller chooses both independently:
//
//   - Format controls HOW the issue data is represented (typed struct,
//     Markdown text, or JSON bytes).
//   - Store controls WHERE the result is written (filesystem, gRPC stream,
//     in-memory buffer, etc.).
//
// For example, a gRPC handler may request [FormatStructured] and stream the
// typed data to a client, while the CLI uses [FormatMarkdown] and writes to
// disk via an FSStore. Neither choice constrains the other.
type OutputFormat int

const (
	// FormatStructured returns the parsed issue as typed Go values
	// ([parse.Issue] + []extract.Reference). No rendering is performed.
	// Use this when the caller needs to inspect or transform the data
	// programmatically (e.g. a gRPC handler that maps fields to proto
	// messages).
	FormatStructured OutputFormat = iota

	// FormatMarkdown returns the issue rendered as Markdown text via
	// [render.RenderIssue]. This is the format written to disk by the
	// default FSStore and displayed by the CLI.
	FormatMarkdown

	// FormatJSON returns the issue serialised as a JSON string. The JSON
	// representation mirrors the structured data ([parse.Issue] +
	// []extract.Reference) and is suitable for machine consumption or
	// embedding in API responses.
	FormatJSON
)

// String returns the canonical lower-case name of the format.
// Unknown values return "OutputFormat(<n>)" so they are always printable.
func (f OutputFormat) String() string {
	switch f {
	case FormatStructured:
		return "structured"
	case FormatMarkdown:
		return "markdown"
	case FormatJSON:
		return "json"
	default:
		return fmt.Sprintf("OutputFormat(%d)", int(f))
	}
}

// ParseOutputFormat converts a string to an [OutputFormat].
//
// Accepted forms (all case-insensitive):
//
//   - "structured", "FORMAT_STRUCTURED"
//   - "markdown",   "FORMAT_MARKDOWN"
//   - "json",       "FORMAT_JSON"
//
// The proto-style "FORMAT_*" aliases make it straightforward to map from a
// proto enum name without an extra translation layer.
//
// On failure the error wraps [ErrConfigInvalidValue] so callers can use
// errors.Is for classification.
func ParseOutputFormat(s string) (OutputFormat, error) {
	switch strings.ToLower(strings.TrimPrefix(strings.ToUpper(s), "FORMAT_")) {
	case "structured":
		return FormatStructured, nil
	case "markdown":
		return FormatMarkdown, nil
	case "json":
		return FormatJSON, nil
	default:
		return 0, fmt.Errorf("gojira: unknown output format %q: %w", s, ErrConfigInvalidValue)
	}
}
