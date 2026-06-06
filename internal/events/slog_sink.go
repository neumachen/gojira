package events

import (
	"context"
	"log/slog"
)

// SlogSink is a Sink that emits each Event as a structured slog record
// through the wrapped *slog.Logger.
//
// The Event-to-slog mapping is:
//
//   - Event.Kind        → slog.Level via [kindToLevel].
//   - Event.Message     → slog record message; if empty, string(Event.Kind)
//     is used so every record carries a non-empty message.
//   - Event.Kind        → "event" attribute (string), always present.
//   - Event.IssueKey    → "issue" attribute (string); omitted when empty.
//   - Event.Err         → "error" attribute, attached via slog.Any so
//     that *errext.TraceError values (which implement slog.LogValuer)
//     render as a structured group with the captured stack frames.
//     Omitted when nil.
//   - Event.PRReference → "pr_url" attribute (string); omitted when empty.
//   - Event.RawNode     → "raw_node" attribute (string); omitted when empty.
//   - Event.FieldKey    → "field_key" attribute (string); omitted when empty.
//
// Event.Timestamp is not forwarded as an explicit attribute: slog records
// carry their own time field and double-stamping would only add noise. The
// timestamp slog records is the moment Emit is called, which for the
// typical synchronous Emit path is effectively the same instant.
//
// SlogSink is safe for concurrent use because *slog.Logger is.
type SlogSink struct {
	// Logger is the destination logger. It is never nil for sinks
	// constructed through [NewSlogSink]; callers that construct a SlogSink
	// literal must populate Logger themselves.
	Logger *slog.Logger
}

// NewSlogSink constructs a SlogSink wrapping the given logger. A nil
// logger is replaced with [slog.Default] so the resulting sink is always
// safe to call.
func NewSlogSink(logger *slog.Logger) *SlogSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogSink{Logger: logger}
}

// Emit converts e into a slog record and dispatches it through the
// wrapped Logger at the level returned by [kindToLevel] for e.Kind.
//
// Emit relies on the wrapped Logger's handler to perform level filtering:
// records below the handler's threshold are dropped by slog itself, so
// constructing a SlogSink with a low-verbosity handler is sufficient to
// suppress chatty Debug-level events.
func (s *SlogSink) Emit(e Event) {
	logger := s.Logger
	if logger == nil {
		// Defensive: a zero-value SlogSink falls back to slog.Default so
		// that misconfigured callers do not crash on Emit.
		logger = slog.Default()
	}

	level := kindToLevel(e.Kind)

	msg := e.Message
	if msg == "" {
		msg = string(e.Kind)
	}

	// Attribute assembly order is deterministic and matches the field
	// ordering in the doc comment above:
	//   event, issue, pr_url, raw_node, field_key, error
	attrs := make([]slog.Attr, 0, 6)
	attrs = append(attrs, slog.String("event", string(e.Kind)))
	if e.IssueKey != "" {
		attrs = append(attrs, slog.String("issue", e.IssueKey))
	}
	if e.PRReference != "" {
		attrs = append(attrs, slog.String("pr_url", e.PRReference))
	}
	if e.RawNode != "" {
		attrs = append(attrs, slog.String("raw_node", e.RawNode))
	}
	if e.FieldKey != "" {
		attrs = append(attrs, slog.String("field_key", e.FieldKey))
	}
	if e.Err != nil {
		// slog.Any so that LogValuer implementations (such as
		// *errext.TraceError) are honored by the handler.
		attrs = append(attrs, slog.Any("error", e.Err))
	}

	logger.LogAttrs(context.Background(), level, msg, attrs...)
}

// kindToLevel maps an event Kind to a slog severity.
//
// The mapping reflects the operational meaning of each kind:
//
//   - Failures and authorization aborts log at Error because they
//     represent crawl progress that did not succeed.
//   - Stubs, cap-reached, and Dev Status partial failures log at Warn
//     because they represent intentional partial-success states an
//     operator should notice without conflating them with issue-level
//     failures (KindDevStatusPartialFailure specifically: the issue
//     itself was rendered, only one enrichment source degraded).
//   - Routine progress (queued, fetched, skipped) and the final crawl
//     summary log at Info as the normal narrative of a crawl run.
//   - Chatty discoveries (PR references, unknown ADF nodes, unknown
//     custom fields) log at Debug so they are available for diagnostics
//     without dominating a normal-verbosity log.
//
// Unknown kinds fall through to Info as a defensive default so a
// future-added Kind never accidentally emits at a noisy level.
func kindToLevel(k Kind) slog.Level {
	switch k {
	case KindIssueFailed:
		return slog.LevelError
	case KindIssueStubbed, KindIssueCapReached, KindDevStatusPartialFailure:
		return slog.LevelWarn
	case KindIssueQueued, KindIssueFetched, KindIssueSkipped, KindCrawlSummary:
		return slog.LevelInfo
	case KindPRReferenceFound, KindUnknownADFNode, KindUnknownCustomField, KindChildDiscovered:
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

// Compile-time assertion that *SlogSink satisfies the Sink interface.
var _ Sink = (*SlogSink)(nil)
