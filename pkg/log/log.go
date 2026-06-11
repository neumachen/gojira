// Package log is gojira's small slog-compatible logging facade.
//
// It is a thin wrapper around the standard library's log/slog package
// that fixes two project-wide decisions and nothing more:
//
//  1. Output format. gojira supports exactly two encodings: "text"
//     (human-readable key=value lines, suitable for terminals) and
//     "json" (one JSON object per line, suitable for log shippers).
//     Both encodings are implemented by stdlib handlers
//     (slog.NewTextHandler and slog.NewJSONHandler); this package
//     simply selects between them via the [Format] type.
//
//  2. Default handler options. Both handlers are constructed with
//     AddSource=false. Adding file:line to every record is noisy for
//     normal CLI use and is redundant for errors: gojira's error
//     values are *errext.TraceError, which implement slog.LogValuer
//     and emit a structured group containing the captured stack
//     frames whenever they are logged.
//
// The package is a public leaf: it imports only the Go standard
// library. It does not import any project-internal package and does
// not import any third-party logging library. Callers that need a
// *slog.Logger can use [New]; callers that want to compose handlers
// themselves (for example to add their own slog.Handler middleware)
// can use [NewTextHandler] or [NewJSONHandler] directly.
//
// Goroutine safety: the handlers returned here are stdlib slog
// handlers, which are safe for concurrent use by multiple goroutines.
package log

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Format selects the output encoding used by the handlers returned by
// [New], [NewTextHandler], and [NewJSONHandler].
type Format int

const (
	// FormatText emits human-readable key=value lines via
	// slog.NewTextHandler. It is the default format and is suitable
	// for interactive terminal use.
	FormatText Format = iota

	// FormatJSON emits one JSON object per line via
	// slog.NewJSONHandler. It is suitable for log shippers and any
	// downstream consumer that parses structured logs.
	FormatJSON
)

// String makes Format printable for debugging and for flag display.
// Unknown values are rendered as Format(N) where N is the integer
// value, mirroring the convention used by stringer-generated code.
func (f Format) String() string {
	switch f {
	case FormatText:
		return "text"
	case FormatJSON:
		return "json"
	default:
		return fmt.Sprintf("Format(%d)", int(f))
	}
}

// ParseFormat accepts the case-insensitive strings "text" and "json"
// and returns the corresponding [Format]. Leading and trailing
// whitespace is not trimmed: callers are expected to normalise their
// input.
//
// The empty string returns FormatText (the default) without an error
// so callers can pass an unset configuration value straight through
// without needing to check for emptiness first.
//
// Any other input returns a non-nil error and the zero-value Format.
func ParseFormat(s string) (Format, error) {
	if s == "" {
		return FormatText, nil
	}
	switch strings.ToLower(s) {
	case "text":
		return FormatText, nil
	case "json":
		return FormatJSON, nil
	default:
		return FormatText, fmt.Errorf("log: unknown format %q (want \"text\" or \"json\")", s)
	}
}

// handlerOptions returns the standard slog.HandlerOptions used by
// both [NewTextHandler] and [NewJSONHandler]. AddSource is left
// false: file:line information is noisy for routine logging and
// errors already carry their own stack frames via errext.
//
// The ReplaceAttr below is a single, targeted shim: it rewrites the
// level field for [LevelTrace] records from slog's default
// "DEBUG-4" rendering to the human-readable "TRACE". Every other
// level (Debug/Info/Warn/Error) flows through untouched, so this
// keeps the existing log surface byte-identical for non-trace runs.
func handlerOptions(level slog.Level) *slog.HandlerOptions {
	return &slog.HandlerOptions{
		Level:     level,
		AddSource: false,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				if lv, ok := a.Value.Any().(slog.Level); ok && lv == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	}
}

// NewTextHandler returns a slog.Handler that emits human-readable
// key=value lines at or above the given level. The returned handler
// is goroutine-safe.
func NewTextHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewTextHandler(w, handlerOptions(level))
}

// NewJSONHandler returns a slog.Handler that emits one JSON object
// per line at or above the given level. The returned handler is
// goroutine-safe.
func NewJSONHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewJSONHandler(w, handlerOptions(level))
}

// New constructs a *slog.Logger that emits in the chosen format at or
// above the given level to w. It is the most convenient entry point
// for callers that do not need direct handler access.
//
// An unknown Format value falls back to FormatText so a misconfigured
// caller still produces readable output rather than panicking.
func New(format Format, level slog.Level, w io.Writer) *slog.Logger {
	var h slog.Handler
	switch format {
	case FormatJSON:
		h = NewJSONHandler(w, level)
	case FormatText:
		h = NewTextHandler(w, level)
	default:
		h = NewTextHandler(w, level)
	}
	return slog.New(h)
}
