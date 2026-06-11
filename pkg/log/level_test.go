package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	gojiralog "github.com/neumachen/gojira/pkg/log"
)

// ---------------------------------------------------------------------------
// LevelTrace constant + ParseLevel
// ---------------------------------------------------------------------------

// TestLevelTrace_Value pins the conventional offset: gojira's TRACE sits
// four steps below slog.LevelDebug, matching the de-facto "level-below-debug"
// convention slog ecosystems use when extending the four-level ladder.
func TestLevelTrace_Value(t *testing.T) {
	t.Parallel()
	if got, want := gojiralog.LevelTrace, slog.LevelDebug-4; got != want {
		t.Errorf("LevelTrace: got %d, want %d (slog.LevelDebug-4)", got, want)
	}
	// And confirm it really is below debug (defence-in-depth: a future
	// renumbering of slog.LevelDebug would break TRACE-as-most-verbose).
	if gojiralog.LevelTrace >= slog.LevelDebug {
		t.Errorf("LevelTrace must be below LevelDebug, got LevelTrace=%d Debug=%d",
			gojiralog.LevelTrace, slog.LevelDebug)
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"error", "error", slog.LevelError, false},
		{"warn", "warn", slog.LevelWarn, false},
		{"info", "info", slog.LevelInfo, false},
		{"debug", "debug", slog.LevelDebug, false},
		{"trace", "trace", gojiralog.LevelTrace, false},
		{"mixed case", "InFo", slog.LevelInfo, false},
		{"upper case", "TRACE", gojiralog.LevelTrace, false},
		{"with spaces", "  warn\t", slog.LevelWarn, false},
		{"empty", "", 0, true},
		{"unknown", "fatal", 0, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := gojiralog.ParseLevel(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q): expected error, got level %v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseLevel(%q): got %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TRACE renders as "TRACE", not slog's default "DEBUG-4"
// ---------------------------------------------------------------------------

func TestTraceRendersAsTRACE_JSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := gojiralog.New(gojiralog.FormatJSON, gojiralog.LevelTrace, &buf)

	logger.Log(context.Background(), gojiralog.LevelTrace, "weaving")

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected a log line at TRACE level, got empty buffer")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("decode JSON log line: %v\nline: %s", err, line)
	}
	if got, _ := rec["level"].(string); got != "TRACE" {
		t.Errorf(`json level: got %q, want "TRACE" (full record: %v)`, got, rec)
	}
	if got, _ := rec["msg"].(string); got != "weaving" {
		t.Errorf(`json msg: got %q, want "weaving"`, got)
	}
}

func TestTraceRendersAsTRACE_Text(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := gojiralog.New(gojiralog.FormatText, gojiralog.LevelTrace, &buf)

	logger.Log(context.Background(), gojiralog.LevelTrace, "weaving")

	out := buf.String()
	if !strings.Contains(out, "level=TRACE") && !strings.Contains(out, " TRACE ") {
		// slog's text handler emits `level=TRACE`. Defence-in-depth: also
		// accept a future-compatible bare "TRACE" token.
		t.Errorf("text output must contain TRACE level token; got: %s", out)
	}
	// And make sure the default "DEBUG-4" rendering is NOT present.
	if strings.Contains(out, "DEBUG-4") {
		t.Errorf("text output must NOT contain slog's default DEBUG-4; got: %s", out)
	}
}

// TestNonTraceLevelsUnchanged guards against a regression where the new
// ReplaceAttr clobbers INFO/DEBUG/etc. rendering.
func TestNonTraceLevelsUnchanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		lv   slog.Level
		want string
	}{
		{"info", slog.LevelInfo, "INFO"},
		{"warn", slog.LevelWarn, "WARN"},
		{"error", slog.LevelError, "ERROR"},
		{"debug", slog.LevelDebug, "DEBUG"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := gojiralog.New(gojiralog.FormatJSON, slog.LevelDebug, &buf)
			logger.Log(context.Background(), tc.lv, "x")

			var rec map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
				t.Fatalf("decode: %v (buf=%s)", err, buf.String())
			}
			if got, _ := rec["level"].(string); got != tc.want {
				t.Errorf("level: got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TRACE is filtered out by handlers configured above it
// ---------------------------------------------------------------------------

// TestTraceFilteredOutAtInfo confirms a logger at LevelInfo silently
// drops LevelTrace records (so the trace ladder is opt-in, not noise).
func TestTraceFilteredOutAtInfo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := gojiralog.New(gojiralog.FormatJSON, slog.LevelInfo, &buf)
	logger.Log(context.Background(), gojiralog.LevelTrace, "should be filtered")
	if buf.Len() != 0 {
		t.Errorf("expected empty buffer at LevelInfo, got: %s", buf.String())
	}

	// And conversely: a logger AT LevelTrace must emit the record.
	buf.Reset()
	logger = gojiralog.New(gojiralog.FormatJSON, gojiralog.LevelTrace, &buf)
	logger.Log(context.Background(), gojiralog.LevelTrace, "should appear")
	if buf.Len() == 0 {
		t.Error("expected a record at LevelTrace; got empty buffer")
	}
}

// Silence unused-imports check that arises when one of the helpers above
// is reorganised in a future edit.
var _ = errors.New
