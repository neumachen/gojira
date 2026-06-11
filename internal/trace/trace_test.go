package trace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/neumachen/gojira/internal/trace"
)

// ---------------------------------------------------------------------------
// ID generation
// ---------------------------------------------------------------------------

// TestNewIDs_UniqueNonEmpty stress-tests the opaque-ID helpers: many
// calls must each produce a non-empty value and the set of values
// must be unique. 1000 iterations is enough to fail loudly if the
// generator collapses to a constant or a counter that wraps.
func TestNewIDs_UniqueNonEmpty(t *testing.T) {
	t.Parallel()
	const n = 1000

	runs := make(map[string]struct{}, n)
	spans := make(map[string]struct{}, n)

	for i := 0; i < n; i++ {
		r := trace.NewRunID()
		s := trace.NewSpanID()
		if r == "" {
			t.Fatalf("NewRunID returned empty on iter %d", i)
		}
		if s == "" {
			t.Fatalf("NewSpanID returned empty on iter %d", i)
		}
		if _, dup := runs[r]; dup {
			t.Fatalf("NewRunID collision after %d iterations: %q", i, r)
		}
		runs[r] = struct{}{}
		if _, dup := spans[s]; dup {
			t.Fatalf("NewSpanID collision after %d iterations: %q", i, s)
		}
		spans[s] = struct{}{}
	}

	if got := len(runs); got != n {
		t.Errorf("RunID unique count: got %d, want %d", got, n)
	}
	if got := len(spans); got != n {
		t.Errorf("SpanID unique count: got %d, want %d", got, n)
	}
}

// ---------------------------------------------------------------------------
// Span shape: NewRoot + Child
// ---------------------------------------------------------------------------

func TestNewRoot_HasFreshRunAndSpan_NoParent(t *testing.T) {
	t.Parallel()
	root := trace.NewRoot()

	if root.RunID == "" {
		t.Error("root.RunID must be set")
	}
	if root.SpanID == "" {
		t.Error("root.SpanID must be set")
	}
	if root.ParentSpanID != "" {
		t.Errorf("root.ParentSpanID must be empty for a root span; got %q", root.ParentSpanID)
	}
	if root.TicketID != "" || root.Phase != "" || root.Depth != 0 {
		t.Errorf("root span must start with zero-valued issue scope; got %+v", root)
	}
}

// TestSpan_ChildLinksParent confirms the fan-out semantics: a child
// inherits RunID, gets a fresh SpanID, and points back at the parent.
func TestSpan_ChildLinksParent(t *testing.T) {
	t.Parallel()
	root := trace.NewRoot()
	child := root.Child(trace.PhaseFetch, "PROJ-1417", 1)

	if child.RunID != root.RunID {
		t.Errorf("child.RunID must match root: got %q, want %q", child.RunID, root.RunID)
	}
	if child.ParentSpanID != root.SpanID {
		t.Errorf("child.ParentSpanID must equal root.SpanID: got %q, want %q",
			child.ParentSpanID, root.SpanID)
	}
	if child.SpanID == "" {
		t.Error("child.SpanID must be set")
	}
	if child.SpanID == root.SpanID {
		t.Errorf("child.SpanID must be fresh, not equal to root.SpanID (%q)", root.SpanID)
	}
	if child.TicketID != "PROJ-1417" {
		t.Errorf("child.TicketID: got %q", child.TicketID)
	}
	if child.Phase != trace.PhaseFetch {
		t.Errorf("child.Phase: got %q", child.Phase)
	}
	if child.Depth != 1 {
		t.Errorf("child.Depth: got %d, want 1", child.Depth)
	}
}

// ---------------------------------------------------------------------------
// Span.Logger — slog attribute attachment
// ---------------------------------------------------------------------------

// captureLogger returns a logger that writes JSON records into buf, so
// tests can decode and inspect attribute presence/values.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// decodeLastRecord parses the last JSON record from buf and returns it.
func decodeLastRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	// Each slog JSON record is newline-delimited.
	out := buf.Bytes()
	if len(out) == 0 {
		t.Fatalf("captured buffer is empty")
	}
	// Trim trailing newline; if there are multiple records, take the
	// last one.
	for len(out) > 0 && (out[len(out)-1] == '\n' || out[len(out)-1] == '\r') {
		out = out[:len(out)-1]
	}
	if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	var rec map[string]any
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatalf("decode record: %v; raw: %s", err, string(out))
	}
	return rec
}

// TestSpan_Logger_RootOmitsParent confirms that a root span's logger
// emits run_id/span_id/depth but NOT parent_span_id (omitted because
// empty) and NOT ticket_id/phase (also omitted because empty on root).
func TestSpan_Logger_RootOmitsParent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := captureLogger(&buf)

	root := trace.NewRoot()
	root.Logger(base).Info("hello")

	rec := decodeLastRecord(t, &buf)

	if got, ok := rec[trace.AttrRunID].(string); !ok || got != root.RunID {
		t.Errorf("run_id: got %v, want %q", rec[trace.AttrRunID], root.RunID)
	}
	if got, ok := rec[trace.AttrSpanID].(string); !ok || got != root.SpanID {
		t.Errorf("span_id: got %v, want %q", rec[trace.AttrSpanID], root.SpanID)
	}
	if _, present := rec[trace.AttrParentSpanID]; present {
		t.Errorf("parent_span_id must be omitted for a root span; got %v", rec[trace.AttrParentSpanID])
	}
	if _, present := rec[trace.AttrTicketID]; present {
		t.Errorf("ticket_id must be omitted when empty; got %v", rec[trace.AttrTicketID])
	}
	if _, present := rec[trace.AttrPhase]; present {
		t.Errorf("phase must be omitted when empty; got %v", rec[trace.AttrPhase])
	}
	// depth is always present (0 is meaningful for the root).
	if got, ok := rec[trace.AttrDepth].(float64); !ok || int(got) != 0 {
		t.Errorf("depth: got %v, want 0", rec[trace.AttrDepth])
	}
}

// TestSpan_Logger_ChildAttachesAllAttrs confirms a child span's logger
// emits every correlation attribute including parent_span_id, ticket_id,
// phase, and a non-zero depth.
func TestSpan_Logger_ChildAttachesAllAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := captureLogger(&buf)

	root := trace.NewRoot()
	child := root.Child(trace.PhaseHierarchyJQL, "PROJ-1417", 2)
	child.Logger(base).Info("hello")

	rec := decodeLastRecord(t, &buf)

	want := map[string]any{
		trace.AttrRunID:        root.RunID,
		trace.AttrSpanID:       child.SpanID,
		trace.AttrParentSpanID: root.SpanID,
		trace.AttrTicketID:     "PROJ-1417",
		trace.AttrPhase:        trace.PhaseHierarchyJQL,
	}
	for k, v := range want {
		if got := rec[k]; got != v {
			t.Errorf("%s: got %v, want %v", k, got, v)
		}
	}
	if got, ok := rec[trace.AttrDepth].(float64); !ok || int(got) != 2 {
		t.Errorf("depth: got %v, want 2", rec[trace.AttrDepth])
	}
}

// TestSpan_Logger_NilBaseUsesDefault confirms a nil base logger does
// not panic and uses slog.Default. We swap slog.Default for the
// duration of the test so output is captured into a buffer.
func TestSpan_Logger_NilBaseUsesDefault(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	slog.SetDefault(captureLogger(&buf))

	root := trace.NewRoot()
	root.Logger(nil).Info("via default")

	rec := decodeLastRecord(t, &buf)
	if rec[trace.AttrRunID] != root.RunID {
		t.Errorf("expected root.RunID via slog.Default; got %v", rec[trace.AttrRunID])
	}
}

// ---------------------------------------------------------------------------
// Context propagation: WithLogger / LoggerFrom
// ---------------------------------------------------------------------------

func TestWithLogger_RoundTrip(t *testing.T) {
	t.Parallel()

	// LoggerFrom on an empty context must return nil.
	if got := trace.LoggerFrom(context.Background()); got != nil {
		t.Errorf("LoggerFrom(empty ctx): got %v, want nil", got)
	}

	// A logger stored via WithLogger must round-trip identically.
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := trace.WithLogger(context.Background(), base)
	if got := trace.LoggerFrom(ctx); got != base {
		t.Errorf("LoggerFrom must return the same logger that was stored; got %v, want %v",
			got, base)
	}
}

// ---------------------------------------------------------------------------
// Fallback path — never empty even on rand failure
// ---------------------------------------------------------------------------

// TestNewID_FallbackNeverEmpty drives the rand source to fail to
// confirm the fallback path produces a non-empty ID. The package
// exposes its rand-source seam via the test-helper SetRandReadForTest
// (only compiled in tests; see export_test.go).
func TestNewID_FallbackNeverEmpty(t *testing.T) {
	// Cannot use t.Parallel here because SetRandReadForTest mutates a
	// package-level seam; concurrent tests would race.

	restore := trace.SetRandReadForTest(func(p []byte) (int, error) {
		return 0, errors.New("forced rand failure")
	})
	t.Cleanup(restore)

	if got := trace.NewRunID(); got == "" {
		t.Error("NewRunID must never return empty even on rand failure")
	}
	if got := trace.NewSpanID(); got == "" {
		t.Error("NewSpanID must never return empty even on rand failure")
	}
}
