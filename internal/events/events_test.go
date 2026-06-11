package events_test

import (
	"sync"
	"testing"
	"time"

	"github.com/neumachen/gojira/internal/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoopSink_EmitDoesNotPanic verifies that NoopSink.Emit never panics,
// including when called with a zero-value Event.
func TestNoopSink_EmitDoesNotPanic(t *testing.T) {
	var sink events.NoopSink

	// zero-value Event
	sink.Emit(events.Event{})

	// populated Event
	sink.Emit(events.Event{
		Kind:     events.KindIssueFetched,
		IssueKey: "PROJ-1",
		Message:  "fetched",
	})
}

// TestRecordingSink_RecordsInOrder verifies that RecordingSink stores events
// in the order they were emitted.
func TestRecordingSink_RecordsInOrder(t *testing.T) {
	var sink events.RecordingSink

	e1 := events.Event{Kind: events.KindIssueQueued, IssueKey: "A-1", Message: "queued"}
	e2 := events.Event{Kind: events.KindIssueFetched, IssueKey: "A-1", Message: "fetched"}
	e3 := events.Event{Kind: events.KindCrawlSummary, Message: "done"}

	sink.Emit(e1)
	sink.Emit(e2)
	sink.Emit(e3)

	got := sink.Events()
	require.Len(t, got, 3, "expected 3 events")
	assert.Equal(t, events.KindIssueQueued, got[0].Kind, "event[0].Kind")
	assert.Equal(t, events.KindIssueFetched, got[1].Kind, "event[1].Kind")
	assert.Equal(t, events.KindCrawlSummary, got[2].Kind, "event[2].Kind")
}

// TestRecordingSink_EventsReturnsCopy verifies that the slice returned by
// Events() is a copy: subsequent Emit calls do not mutate the previously
// returned slice.
func TestRecordingSink_EventsReturnsCopy(t *testing.T) {
	var sink events.RecordingSink

	sink.Emit(events.Event{Kind: events.KindIssueQueued})
	snapshot := sink.Events()

	sink.Emit(events.Event{Kind: events.KindIssueFetched})

	assert.Len(t, snapshot, 1, "snapshot length should not change after subsequent Emit")
}

// TestRecordingSink_ConcurrentEmit verifies that RecordingSink is safe for
// concurrent use. N goroutines each emit M events; the final count must equal
// N*M with no data race (run with -race).
func TestRecordingSink_ConcurrentEmit(t *testing.T) {
	const goroutines = 20
	const eventsPerGoroutine = 50

	var sink events.RecordingSink
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < eventsPerGoroutine; j++ {
				sink.Emit(events.Event{
					Kind:    events.KindIssueFetched,
					Message: "concurrent",
				})
			}
		}()
	}
	wg.Wait()

	got := sink.Events()
	want := goroutines * eventsPerGoroutine
	assert.Len(t, got, want, "concurrent emit count")
}

// TestKindConstants_NonEmptyAndDistinct verifies that every exported Kind
// constant is non-empty and that no two constants share the same value.
func TestKindConstants_NonEmptyAndDistinct(t *testing.T) {
	kinds := []events.Kind{
		events.KindIssueQueued,
		events.KindIssueFetched,
		events.KindIssueSkipped,
		events.KindIssueStubbed,
		events.KindIssueFailed,
		events.KindIssueCapReached,
		events.KindPRReferenceFound,
		events.KindUnknownADFNode,
		events.KindUnknownCustomField,
		events.KindCrawlSummary,
	}

	seen := make(map[events.Kind]bool, len(kinds))
	for _, k := range kinds {
		assert.NotEmpty(t, k, "found empty Kind constant")
		assert.False(t, seen[k], "duplicate Kind constant value: %q", k)
		seen[k] = true
	}
}

// TestSinkInterface_NoopSinkImplementsSink is a compile-time assertion that
// NoopSink satisfies the Sink interface.
var _ events.Sink = events.NoopSink{}

// TestSinkInterface_RecordingSinkImplementsSink is a compile-time assertion
// that *RecordingSink satisfies the Sink interface.
var _ events.Sink = (*events.RecordingSink)(nil)

// ---------------------------------------------------------------------------
// CrawlSummary — Event carries structured totals end-to-end
// ---------------------------------------------------------------------------

// TestEvent_CrawlSummaryRoundTrip verifies that an Event can carry a non-nil
// *CrawlSummary and that RecordingSink preserves it byte-for-byte. The
// structured Summary field is the bridge the grpcSink uses to populate the
// proto CrawlEvent.summary oneof; if RecordingSink dropped or shallow-
// copied the value the sink-side mapping would silently corrupt totals.
func TestEvent_CrawlSummaryRoundTrip(t *testing.T) {
	t.Parallel()

	var sink events.RecordingSink

	summary := &events.CrawlSummary{
		Fetched:        3,
		Skipped:        1,
		Stubbed:        2,
		Failed:         0,
		CapLimited:     0,
		PRsFound:       4,
		FetchedKeys:    []string{"A-1", "A-2", "A-3"},
		StubbedKeys:    []string{"S-1", "S-2"},
		FailedKeys:     map[string]string{"X-9": "boom"},
		CapLimitedKeys: nil,
		Duration:       1500 * time.Millisecond,
	}

	sink.Emit(events.Event{
		Kind:      events.KindCrawlSummary,
		Message:   "crawl complete: fetched=3",
		Timestamp: time.Now(),
		Summary:   summary,
	})

	got := sink.Events()
	require.Len(t, got, 1, "RecordingSink must record the summary event")

	require.NotNil(t, got[0].Summary, "Summary field must round-trip non-nil")
	assert.Equal(t, 3, got[0].Summary.Fetched, "Fetched")
	assert.Equal(t, 4, got[0].Summary.PRsFound, "PRsFound")
	assert.Equal(t, []string{"A-1", "A-2", "A-3"}, got[0].Summary.FetchedKeys, "FetchedKeys")
	assert.Equal(t, map[string]string{"X-9": "boom"}, got[0].Summary.FailedKeys, "FailedKeys")
	assert.Equal(t, 1500*time.Millisecond, got[0].Summary.Duration, "Duration")
}

// TestEvent_NonSummaryHasNilSummary confirms that an Event of any other Kind
// leaves the Summary field nil by default; the proto sink relies on this
// invariant to skip oneof population for non-summary events.
func TestEvent_NonSummaryHasNilSummary(t *testing.T) {
	t.Parallel()

	var sink events.RecordingSink
	sink.Emit(events.Event{
		Kind:      events.KindIssueFetched,
		IssueKey:  "A-1",
		Message:   "fetched A-1",
		Timestamp: time.Now(),
	})

	got := sink.Events()
	require.Len(t, got, 1)
	assert.Nil(t, got[0].Summary, "non-summary Event must have nil Summary by default")
}
