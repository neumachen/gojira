package grpcserver_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/grpcserver"
)

// fakeStream is a test double for grpc.ServerStreamingServer[gojirav1.CrawlEvent].
// It collects all sent messages in order and records any Send errors injected
// via SendErr.
type fakeStream struct {
	mu      sync.Mutex
	sent    []*gojirav1.CrawlEvent
	SendErr error // if non-nil, Send returns this error
}

// Send appends msg to the collected slice (or returns SendErr if set).
func (f *fakeStream) Send(msg *gojirav1.CrawlEvent) error {
	if f.SendErr != nil {
		return f.SendErr
	}
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.mu.Unlock()
	return nil
}

// Sent returns a snapshot of all collected messages.
func (f *fakeStream) Sent() []*gojirav1.CrawlEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*gojirav1.CrawlEvent, len(f.sent))
	copy(out, f.sent)
	return out
}

// grpc.ServerStream stub methods — not exercised by the sink.
func (f *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}
func (f *fakeStream) Context() context.Context     { return context.Background() }
func (f *fakeStream) SendMsg(any) error            { return nil }
func (f *fakeStream) RecvMsg(any) error            { return nil }

// Compile-time assertion that *fakeStream satisfies the required interface.
var _ grpc.ServerStreamingServer[gojirav1.CrawlEvent] = (*fakeStream)(nil)

// fixedTime is a stable timestamp used across all test cases.
var fixedTime = time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

func TestNewGRPCStreamSink_KindMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		event    events.Event
		wantKind gojirav1.CrawlEvent_Kind
	}{
		{
			name:     "issue queued",
			event:    events.Event{Kind: events.KindIssueQueued, IssueKey: "PROJ-1", Message: "queued", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_ISSUE_QUEUED,
		},
		{
			name:     "issue fetched",
			event:    events.Event{Kind: events.KindIssueFetched, IssueKey: "PROJ-2", Message: "fetched", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_ISSUE_FETCHED,
		},
		{
			name:     "issue skipped",
			event:    events.Event{Kind: events.KindIssueSkipped, IssueKey: "PROJ-3", Message: "skipped", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_ISSUE_SKIPPED,
		},
		{
			name:     "issue stubbed",
			event:    events.Event{Kind: events.KindIssueStubbed, IssueKey: "PROJ-4", Message: "stubbed", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_ISSUE_STUBBED,
		},
		{
			name:     "issue failed",
			event:    events.Event{Kind: events.KindIssueFailed, IssueKey: "PROJ-5", Message: "failed", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_ISSUE_FAILED,
		},
		{
			name:     "issue cap reached",
			event:    events.Event{Kind: events.KindIssueCapReached, IssueKey: "PROJ-6", Message: "cap reached", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_ISSUE_CAP_REACHED,
		},
		{
			name:     "pr reference found",
			event:    events.Event{Kind: events.KindPRReferenceFound, IssueKey: "PROJ-7", Message: "pr found", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_PR_REFERENCE_FOUND,
		},
		{
			name:     "unknown adf node",
			event:    events.Event{Kind: events.KindUnknownADFNode, IssueKey: "PROJ-8", Message: "unknown node", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_UNKNOWN_ADF_NODE,
		},
		{
			name:     "unknown custom field",
			event:    events.Event{Kind: events.KindUnknownCustomField, IssueKey: "PROJ-9", Message: "unknown field", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_UNKNOWN_CUSTOM_FIELD,
		},
		{
			name:     "crawl summary",
			event:    events.Event{Kind: events.KindCrawlSummary, Message: "crawl complete: fetched=5", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY,
		},
		{
			name:     "child discovered",
			event:    events.Event{Kind: events.KindChildDiscovered, IssueKey: "PROJ-10", Message: "child found", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_CHILD_DISCOVERED,
		},
		{
			name:     "devstatus partial failure",
			event:    events.Event{Kind: events.KindDevStatusPartialFailure, IssueKey: "PROJ-11", Message: "partial failure", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_DEV_STATUS_PARTIAL_FAILURE,
		},
		{
			name:     "unknown kind maps to unspecified",
			event:    events.Event{Kind: events.Kind("future.unknown"), Message: "unknown", Timestamp: fixedTime},
			wantKind: gojirav1.CrawlEvent_KIND_UNSPECIFIED,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := &fakeStream{}
			sink := grpcserver.NewGRPCStreamSink(stream)
			sink.Emit(tc.event)

			got := stream.Sent()
			if len(got) != 1 {
				t.Fatalf("expected 1 message sent, got %d", len(got))
			}
			msg := got[0]

			if msg.Kind != tc.wantKind {
				t.Errorf("Kind: got %v, want %v", msg.Kind, tc.wantKind)
			}
			if msg.IssueKey != tc.event.IssueKey {
				t.Errorf("IssueKey: got %q, want %q", msg.IssueKey, tc.event.IssueKey)
			}
			if msg.Message != tc.event.Message {
				t.Errorf("Message: got %q, want %q", msg.Message, tc.event.Message)
			}
			if msg.Timestamp == nil {
				t.Fatal("Timestamp: got nil, want non-nil")
			}
			if !msg.Timestamp.AsTime().Equal(tc.event.Timestamp) {
				t.Errorf("Timestamp: got %v, want %v", msg.Timestamp.AsTime(), tc.event.Timestamp)
			}
		})
	}
}

func TestNewGRPCStreamSink_FieldMapping(t *testing.T) {
	t.Parallel()

	stream := &fakeStream{}
	sink := grpcserver.NewGRPCStreamSink(stream)

	e := events.Event{
		Kind:      events.KindIssueFetched,
		IssueKey:  "PLATENG-1147",
		Message:   "issue fetched successfully",
		Timestamp: fixedTime,
	}
	sink.Emit(e)

	got := stream.Sent()
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	msg := got[0]

	if msg.Kind != gojirav1.CrawlEvent_KIND_ISSUE_FETCHED {
		t.Errorf("Kind: got %v, want KIND_ISSUE_FETCHED", msg.Kind)
	}
	if msg.IssueKey != "PLATENG-1147" {
		t.Errorf("IssueKey: got %q, want %q", msg.IssueKey, "PLATENG-1147")
	}
	if msg.Message != "issue fetched successfully" {
		t.Errorf("Message: got %q, want %q", msg.Message, "issue fetched successfully")
	}
	if msg.Timestamp == nil {
		t.Fatal("Timestamp: got nil")
	}
	if !msg.Timestamp.AsTime().Equal(fixedTime) {
		t.Errorf("Timestamp: got %v, want %v", msg.Timestamp.AsTime(), fixedTime)
	}
}

func TestNewGRPCStreamSink_MultipleEvents(t *testing.T) {
	t.Parallel()

	stream := &fakeStream{}
	sink := grpcserver.NewGRPCStreamSink(stream)

	evts := []events.Event{
		{Kind: events.KindIssueQueued, IssueKey: "A-1", Timestamp: fixedTime},
		{Kind: events.KindIssueFetched, IssueKey: "A-1", Timestamp: fixedTime},
		{Kind: events.KindIssueQueued, IssueKey: "A-2", Timestamp: fixedTime},
		{Kind: events.KindCrawlSummary, Message: "done", Timestamp: fixedTime},
	}
	for _, e := range evts {
		sink.Emit(e)
	}

	got := stream.Sent()
	if len(got) != len(evts) {
		t.Fatalf("expected %d messages, got %d", len(evts), len(got))
	}

	wantKinds := []gojirav1.CrawlEvent_Kind{
		gojirav1.CrawlEvent_KIND_ISSUE_QUEUED,
		gojirav1.CrawlEvent_KIND_ISSUE_FETCHED,
		gojirav1.CrawlEvent_KIND_ISSUE_QUEUED,
		gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY,
	}
	for i, msg := range got {
		if msg.Kind != wantKinds[i] {
			t.Errorf("msg[%d].Kind: got %v, want %v", i, msg.Kind, wantKinds[i])
		}
	}
}

func TestNewGRPCStreamSink_SendErrorSilentlyDiscarded(t *testing.T) {
	t.Parallel()

	// A stream that always returns an error from Send.
	stream := &fakeStream{SendErr: context.Canceled}
	sink := grpcserver.NewGRPCStreamSink(stream)

	// Emit must not panic even when Send returns an error.
	sink.Emit(events.Event{Kind: events.KindIssueQueued, IssueKey: "X-1", Timestamp: fixedTime})

	// No messages collected because Send returned an error before appending.
	if len(stream.Sent()) != 0 {
		t.Errorf("expected 0 collected messages when Send errors, got %d", len(stream.Sent()))
	}
}

func TestNewGRPCStreamSink_CrawlSummaryNoSummaryOneof(t *testing.T) {
	t.Parallel()

	// events.Event does not carry a structured Summary field; the Summary
	// oneof on CrawlEvent must be nil for KindCrawlSummary events.
	stream := &fakeStream{}
	sink := grpcserver.NewGRPCStreamSink(stream)

	sink.Emit(events.Event{
		Kind:      events.KindCrawlSummary,
		Message:   "crawl complete: fetched=3 skipped=0 stubbed=0 failed=0 capLimited=0 prs=1 duration=2s",
		Timestamp: fixedTime,
	})

	got := stream.Sent()
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	msg := got[0]

	if msg.Kind != gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY {
		t.Errorf("Kind: got %v, want KIND_CRAWL_SUMMARY", msg.Kind)
	}
	// Summary oneof is nil because events.Event has no structured Summary field.
	if msg.GetSummary() != nil {
		t.Errorf("Summary oneof: expected nil, got %v", msg.GetSummary())
	}
	if msg.Message == "" {
		t.Error("Message: expected non-empty summary text")
	}
}

// ---------------------------------------------------------------------------
// CrawlEvent.summary oneof — structured totals on the wire
// ---------------------------------------------------------------------------

// TestGRPCSink_PopulatesSummaryOneof confirms that when an events.Event
// carries a non-nil *CrawlSummary, the grpcSink populates the proto
// CrawlEvent.summary oneof with the typed totals — in addition to (not
// instead of) the human-readable Message string. This is the wiring that
// lets gRPC clients read structured counts without re-parsing text.
func TestGRPCSink_PopulatesSummaryOneof(t *testing.T) {
	t.Parallel()

	stream := &fakeStream{}
	sink := grpcserver.NewGRPCStreamSink(stream)

	sink.Emit(events.Event{
		Kind:      events.KindCrawlSummary,
		Message:   "crawl complete: fetched=3",
		Timestamp: fixedTime,
		Summary: &events.CrawlSummary{
			Fetched:     3,
			Skipped:     1,
			Stubbed:     0,
			Failed:      0,
			CapLimited:  0,
			PRsFound:    2,
			FetchedKeys: []string{"A-1"},
			FailedKeys:  map[string]string{"B-2": "boom"},
			Duration:    1500 * time.Millisecond,
		},
	})

	got := stream.Sent()
	if len(got) != 1 {
		t.Fatalf("expected 1 streamed message, got %d", len(got))
	}
	msg := got[0]

	if msg.GetKind() != gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY {
		t.Errorf("Kind: got %v, want KIND_CRAWL_SUMMARY", msg.GetKind())
	}
	if msg.GetMessage() == "" {
		t.Error("Message must still be set for text-only consumers")
	}

	pbSummary := msg.GetSummary()
	if pbSummary == nil {
		t.Fatal("Summary oneof must be populated when events.Event carries a Summary")
	}
	if got, want := pbSummary.GetFetched(), int32(3); got != want {
		t.Errorf("Summary.Fetched: got %d, want %d", got, want)
	}
	if got, want := pbSummary.GetSkipped(), int32(1); got != want {
		t.Errorf("Summary.Skipped: got %d, want %d", got, want)
	}
	if got, want := pbSummary.GetPrsFound(), int32(2); got != want {
		t.Errorf("Summary.PrsFound: got %d, want %d", got, want)
	}
	if got, want := pbSummary.GetFetchedKeys(), []string{"A-1"}; !equalStringSlice(got, want) {
		t.Errorf("Summary.FetchedKeys: got %v, want %v", got, want)
	}
	if got := pbSummary.GetFailedKeys()["B-2"]; got != "boom" {
		t.Errorf("Summary.FailedKeys[B-2]: got %q, want \"boom\"", got)
	}
	if got, want := pbSummary.GetDurationMs(), int64(1500); got != want {
		t.Errorf("Summary.DurationMs: got %d, want %d", got, want)
	}
}

// TestGRPCSink_NonSummaryLeavesOneofNil is the negative companion to
// TestGRPCSink_PopulatesSummaryOneof: events whose Summary field is nil
// (every non-KindCrawlSummary event, plus legacy callers that don't
// attach one) must leave the proto oneof unset.
func TestGRPCSink_NonSummaryLeavesOneofNil(t *testing.T) {
	t.Parallel()

	stream := &fakeStream{}
	sink := grpcserver.NewGRPCStreamSink(stream)

	sink.Emit(events.Event{
		Kind:      events.KindIssueFetched,
		IssueKey:  "A-1",
		Message:   "fetched A-1",
		Timestamp: fixedTime,
	})

	got := stream.Sent()
	if len(got) != 1 {
		t.Fatalf("expected 1 streamed message, got %d", len(got))
	}
	if got[0].GetSummary() != nil {
		t.Errorf("Summary oneof must remain nil for non-summary events; got %+v", got[0].GetSummary())
	}
}

// equalStringSlice is a small local helper to keep the test free of an
// extra reflect/dependency import.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
