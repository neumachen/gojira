package events_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/events"
	gojiralog "github.com/neumachen/gojira/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newJSONSink builds a SlogSink whose underlying logger writes JSON records
// at or above level into buf. It is the standard test harness for asserting
// on the structured output shape.
func newJSONSink(buf *bytes.Buffer, level slog.Level) *events.SlogSink {
	logger := slog.New(gojiralog.NewJSONHandler(buf, level))
	return events.NewSlogSink(logger)
}

// decodeLines parses the newline-delimited JSON records written to buf and
// returns one map per record. It fails the test on any parse error so test
// bodies can stay focused on assertions.
func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	raw := strings.TrimRight(buf.String(), "\n")
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	out := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "decode JSON line %d: %q", i, line)
		out = append(out, rec)
	}
	return out
}

// TestKindToLevel verifies the documented kind-to-severity mapping for
// every Kind constant plus the defensive default for an unknown Kind.
//
// kindToLevel is unexported; we exercise it indirectly through Emit and
// observe the "level" field of the resulting JSON record. This both
// validates the mapping and proves that Emit actually dispatches at the
// expected level (the two are conceptually inseparable).
func TestKindToLevel(t *testing.T) {
	tests := []struct {
		name      string
		kind      events.Kind
		wantLevel string
	}{
		{"issue.queued -> INFO", events.KindIssueQueued, "INFO"},
		{"issue.fetched -> INFO", events.KindIssueFetched, "INFO"},
		{"issue.skipped -> INFO", events.KindIssueSkipped, "INFO"},
		{"crawl.summary -> INFO", events.KindCrawlSummary, "INFO"},
		{"issue.stubbed -> WARN", events.KindIssueStubbed, "WARN"},
		{"issue.cap_reached -> WARN", events.KindIssueCapReached, "WARN"},
		{"issue.failed -> ERROR", events.KindIssueFailed, "ERROR"},
		{"devstatus.partial_failure -> WARN", events.KindDevStatusPartialFailure, "WARN"},
		{"pr_reference.found -> DEBUG", events.KindPRReferenceFound, "DEBUG"},
		{"adf.unknown_node -> DEBUG", events.KindUnknownADFNode, "DEBUG"},
		{"custom_field.unknown -> DEBUG", events.KindUnknownCustomField, "DEBUG"},
		{"unknown kind -> INFO (defensive default)", events.Kind("totally.new.kind"), "INFO"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sink := newJSONSink(&buf, slog.LevelDebug)

			sink.Emit(events.Event{Kind: tc.kind, Message: "x"})

			records := decodeLines(t, &buf)
			require.Len(t, records, 1, "expected exactly one record")
			assert.Equal(t, tc.wantLevel, records[0]["level"], "slog level for kind %q", tc.kind)
		})
	}
}

// TestSlogSink_EmitBasic verifies the happy-path mapping for a typical
// fetch event: level Info, message preserved, event and issue attrs set.
func TestSlogSink_EmitBasic(t *testing.T) {
	var buf bytes.Buffer
	sink := newJSONSink(&buf, slog.LevelDebug)

	sink.Emit(events.Event{
		Kind:     events.KindIssueFetched,
		IssueKey: "EXAMPLE-1",
		Message:  "fetched",
	})

	records := decodeLines(t, &buf)
	require.Len(t, records, 1, "expected exactly one record")
	rec := records[0]

	assert.Equal(t, "INFO", rec["level"], "level")
	assert.Equal(t, "fetched", rec["msg"], "msg")
	assert.Equal(t, "issue.fetched", rec["event"], "event attribute")
	assert.Equal(t, "EXAMPLE-1", rec["issue"], "issue attribute")
}

// TestSlogSink_EmitMessageFallback verifies that an empty Message is
// replaced by string(Kind) so every record carries a non-empty msg.
func TestSlogSink_EmitMessageFallback(t *testing.T) {
	var buf bytes.Buffer
	sink := newJSONSink(&buf, slog.LevelDebug)

	sink.Emit(events.Event{Kind: events.KindIssueQueued})

	records := decodeLines(t, &buf)
	require.Len(t, records, 1)
	assert.Equal(t, "issue.queued", records[0]["msg"], "msg should fall back to Kind when empty")
}

// TestSlogSink_EmitWithError verifies that an *errext.TraceError in
// Event.Err round-trips through slog as a structured value carrying the
// cause message. The exact stack-frame shape is errext's responsibility
// and is not asserted on.
func TestSlogSink_EmitWithError(t *testing.T) {
	var buf bytes.Buffer
	sink := newJSONSink(&buf, slog.LevelDebug)

	sink.Emit(events.Event{
		Kind: events.KindIssueFailed,
		Err:  errext.Errorf("boom"),
	})

	records := decodeLines(t, &buf)
	require.Len(t, records, 1)
	rec := records[0]

	assert.Equal(t, "ERROR", rec["level"], "level for issue.failed")
	require.Contains(t, rec, "error", "error attribute must be present when Err is non-nil")

	// errext renders as a structured group; flattening it and asserting
	// that the cause message appears somewhere keeps this test resilient
	// to errext's internal field layout.
	rendered, err := json.Marshal(rec["error"])
	require.NoError(t, err, "marshal error attribute back to JSON")
	assert.Contains(t, string(rendered), "boom", "error attribute should contain the cause message")
}

// TestSlogSink_OmitsEmptyFields verifies that absent optional fields do
// not produce empty attributes in the output.
func TestSlogSink_OmitsEmptyFields(t *testing.T) {
	var buf bytes.Buffer
	sink := newJSONSink(&buf, slog.LevelDebug)

	sink.Emit(events.Event{
		Kind:    events.KindCrawlSummary,
		Message: "done",
	})

	records := decodeLines(t, &buf)
	require.Len(t, records, 1)
	rec := records[0]

	for _, key := range []string{"issue", "pr_url", "raw_node", "field_key", "error"} {
		_, present := rec[key]
		assert.False(t, present, "key %q must be omitted when its source field is empty", key)
	}

	// Sanity: the mandatory attrs are still present.
	assert.Equal(t, "crawl.summary", rec["event"])
	assert.Equal(t, "done", rec["msg"])
}

// TestSlogSink_EmitAllOptionalFields verifies that every optional
// attribute is emitted when its corresponding Event field is non-empty.
// This pins the attribute names callers will see in production logs.
func TestSlogSink_EmitAllOptionalFields(t *testing.T) {
	var buf bytes.Buffer
	sink := newJSONSink(&buf, slog.LevelDebug)

	sink.Emit(events.Event{
		Kind:        events.KindPRReferenceFound,
		IssueKey:    "EXAMPLE-1",
		Message:     "pr discovered",
		PRReference: "https://github.com/owner/repo/pull/42",
		RawNode:     `{"type":"unknown"}`,
		FieldKey:    "customfield_10001",
	})

	records := decodeLines(t, &buf)
	require.Len(t, records, 1)
	rec := records[0]

	assert.Equal(t, "DEBUG", rec["level"])
	assert.Equal(t, "pr discovered", rec["msg"])
	assert.Equal(t, "pr_reference.found", rec["event"])
	assert.Equal(t, "EXAMPLE-1", rec["issue"])
	assert.Equal(t, "https://github.com/owner/repo/pull/42", rec["pr_url"])
	assert.Equal(t, `{"type":"unknown"}`, rec["raw_node"])
	assert.Equal(t, "customfield_10001", rec["field_key"])
}

// TestSlogSink_NilLogger verifies that NewSlogSink(nil) yields a sink
// that does not panic on Emit. The sink falls back to slog.Default, whose
// destination is implementation-defined; we only assert the absence of a
// panic, not where the record ends up.
func TestSlogSink_NilLogger(t *testing.T) {
	sink := events.NewSlogSink(nil)
	require.NotNil(t, sink, "NewSlogSink(nil) must not return nil")
	require.NotNil(t, sink.Logger, "NewSlogSink(nil) must populate Logger with slog.Default")

	assert.NotPanics(t, func() {
		sink.Emit(events.Event{})
		sink.Emit(events.Event{Kind: events.KindIssueFetched, IssueKey: "X-1"})
	}, "Emit on a nil-logger-constructed sink must not panic")
}

// TestSlogSink_ZeroValueDoesNotPanic verifies the defensive fallback in
// Emit: a SlogSink literal with a nil Logger field still does not panic.
func TestSlogSink_ZeroValueDoesNotPanic(t *testing.T) {
	var sink events.SlogSink

	assert.NotPanics(t, func() {
		sink.Emit(events.Event{Kind: events.KindIssueFetched})
	}, "Emit on a zero-value SlogSink must not panic")
}

// TestSlogSink_LevelFiltering verifies that the wrapped handler's level
// threshold controls which events reach the writer. The sink itself does
// no filtering; slog drops the record below the threshold.
func TestSlogSink_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(gojiralog.NewJSONHandler(&buf, slog.LevelWarn))
	sink := events.NewSlogSink(logger)

	// Info-level event is filtered out.
	sink.Emit(events.Event{Kind: events.KindIssueFetched, IssueKey: "X-1"})
	assert.Empty(t, buf.String(), "Info event must be suppressed by a Warn-threshold handler")

	// Warn-level event passes through.
	sink.Emit(events.Event{Kind: events.KindIssueStubbed, IssueKey: "X-2", Message: "stubbed"})
	records := decodeLines(t, &buf)
	require.Len(t, records, 1, "Warn event must reach the writer")
	assert.Equal(t, "WARN", records[0]["level"])
	assert.Equal(t, "issue.stubbed", records[0]["event"])
}

// TestSlogSink_ConcurrentEmit verifies that the sink is safe for
// concurrent use. 100 goroutines each emit one event; the writer must
// receive exactly 100 records and the test must remain clean under -race.
func TestSlogSink_ConcurrentEmit(t *testing.T) {
	const goroutines = 100

	// Guard the shared buffer: slog handlers serialise writes per
	// handler instance, but bytes.Buffer is not itself goroutine-safe
	// against concurrent reads from the test. Using a mutex-wrapped
	// writer keeps the writes ordered without changing what slog sees.
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	logger := slog.New(gojiralog.NewJSONHandler(syncWriter{mu: &mu, w: &buf}, slog.LevelDebug))
	sink := events.NewSlogSink(logger)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			sink.Emit(events.Event{
				Kind:    events.KindIssueFetched,
				Message: "concurrent",
			})
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	records := decodeLines(t, &buf)
	assert.Len(t, records, goroutines, "every concurrent Emit must produce one record")
}

// syncWriter serialises writes to an inner writer via a mutex. It exists
// so the concurrent-emit test can read back the buffer without racing on
// the buffer's internal state.
type syncWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
