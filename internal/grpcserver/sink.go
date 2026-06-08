// Package grpcserver provides gRPC server-side adapters for the gojira
// library interfaces.
package grpcserver

import (
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/events"
)

// grpcSink is a Sink that forwards each Event as a CrawlEvent proto message
// onto a gRPC server-streaming response stream.
//
// grpcSink is safe for concurrent use only if the underlying stream's Send
// method is safe for concurrent use. The standard gRPC generated server
// streams are NOT safe for concurrent Send calls; callers must ensure that
// Emit is called from a single goroutine, or wrap the stream with external
// synchronisation.
type grpcSink struct {
	stream grpc.ServerStreamingServer[gojirav1.CrawlEvent]
}

// NewGRPCStreamSink constructs a Sink that sends each emitted Event as a
// CrawlEvent proto message on stream. Send errors are silently discarded
// because the Sink interface does not return errors; the gRPC framework will
// surface the broken stream to the RPC handler through the next Send or the
// stream's context.
func NewGRPCStreamSink(stream grpc.ServerStreamingServer[gojirav1.CrawlEvent]) events.Sink {
	return &grpcSink{stream: stream}
}

// Emit converts e to a CrawlEvent proto message and sends it on the stream.
// If Send returns an error it is silently discarded (see NewGRPCStreamSink).
func (s *grpcSink) Emit(e events.Event) {
	msg := &gojirav1.CrawlEvent{
		Kind:      kindToProto(e.Kind),
		IssueKey:  e.IssueKey,
		Message:   e.Message,
		Timestamp: timestamppb.New(e.Timestamp),
	}

	// For KindCrawlSummary the crawl package currently encodes the summary
	// counts in the human-readable Message string; the structured Summary
	// oneof is populated only when the event carries a non-nil Summary value.
	// events.Event does not yet carry a structured Summary field, so the
	// oneof is left nil and the Message string carries the summary text.

	//nolint:errcheck // Send errors are intentionally discarded; see doc comment.
	_ = s.stream.Send(msg)
}

// kindToProto maps an events.Kind string to the corresponding
// CrawlEvent_Kind proto enum value. Unknown kinds map to
// CrawlEvent_KIND_UNSPECIFIED.
func kindToProto(k events.Kind) gojirav1.CrawlEvent_Kind {
	switch k {
	case events.KindIssueQueued:
		return gojirav1.CrawlEvent_KIND_ISSUE_QUEUED
	case events.KindIssueFetched:
		return gojirav1.CrawlEvent_KIND_ISSUE_FETCHED
	case events.KindIssueSkipped:
		return gojirav1.CrawlEvent_KIND_ISSUE_SKIPPED
	case events.KindIssueStubbed:
		return gojirav1.CrawlEvent_KIND_ISSUE_STUBBED
	case events.KindIssueFailed:
		return gojirav1.CrawlEvent_KIND_ISSUE_FAILED
	case events.KindIssueCapReached:
		return gojirav1.CrawlEvent_KIND_ISSUE_CAP_REACHED
	case events.KindPRReferenceFound:
		return gojirav1.CrawlEvent_KIND_PR_REFERENCE_FOUND
	case events.KindUnknownADFNode:
		return gojirav1.CrawlEvent_KIND_UNKNOWN_ADF_NODE
	case events.KindUnknownCustomField:
		return gojirav1.CrawlEvent_KIND_UNKNOWN_CUSTOM_FIELD
	case events.KindCrawlSummary:
		return gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY
	case events.KindChildDiscovered:
		return gojirav1.CrawlEvent_KIND_CHILD_DISCOVERED
	case events.KindDevStatusPartialFailure:
		return gojirav1.CrawlEvent_KIND_DEV_STATUS_PARTIAL_FAILURE
	default:
		return gojirav1.CrawlEvent_KIND_UNSPECIFIED
	}
}

// Compile-time assertion that *grpcSink satisfies the events.Sink interface.
var _ events.Sink = (*grpcSink)(nil)
