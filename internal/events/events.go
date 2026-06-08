// Package events defines the structured event sink interface used by every
// gojira package to report progress, warnings, errors, and partial-success
// states. No package in the library logs directly; callers inject a Sink
// implementation and decide how to format and route events.
//
// The package ships three built-in Sink implementations: [NoopSink] for
// callers that do not care about events, [RecordingSink] for tests that
// want to assert on the event stream, and [SlogSink] (defined in
// slog_sink.go) which adapts the event stream onto a *slog.Logger so
// callers can route events through the standard structured-logging
// pipeline.
//
// Allowed imports: Go standard library only. No project-internal imports.
package events

import (
	"sync"
	"time"
)

// Kind identifies the category of an event. It is a string so that callers
// can switch on it without importing a separate constants package.
type Kind string

// Event kind constants. Each constant is non-empty and unique.
const (
	// KindIssueQueued is emitted when an issue key is added to the crawl queue.
	KindIssueQueued Kind = "issue.queued"

	// KindIssueFetched is emitted when an issue has been successfully fetched
	// and written to disk.
	KindIssueFetched Kind = "issue.fetched"

	// KindIssueSkipped is emitted when an issue is skipped because its output
	// file already exists and re-fetch is disabled.
	KindIssueSkipped Kind = "issue.skipped"

	// KindIssueStubbed is emitted when an issue cannot be fetched due to a
	// permission error (403) or not-found (404) and a stub file is written
	// instead.
	KindIssueStubbed Kind = "issue.stubbed"

	// KindIssueFailed is emitted when a transient error prevents fetching an
	// issue after all retries are exhausted.
	KindIssueFailed Kind = "issue.failed"

	// KindIssueCapReached is emitted when the crawl stops adding new issues
	// because the configured issue cap has been reached.
	KindIssueCapReached Kind = "issue.cap_reached"

	// KindPRReferenceFound is emitted when a GitHub pull request URL is
	// discovered in an issue's content or relationships.
	KindPRReferenceFound Kind = "pr_reference.found"

	// KindUnknownADFNode is emitted when the ADF traversal encounters a node
	// type it does not recognise. The node is preserved, never silently dropped.
	KindUnknownADFNode Kind = "adf.unknown_node"

	// KindUnknownCustomField is emitted when an issue contains a custom field
	// that the parser does not have a typed mapping for. The raw value is
	// preserved.
	KindUnknownCustomField Kind = "custom_field.unknown"

	// KindCrawlSummary is emitted once at the end of a crawl run with
	// aggregate counts and lists of failed/capped keys.
	KindCrawlSummary Kind = "crawl.summary"

	// KindChildDiscovered is emitted when a hierarchy child key is
	// discovered for an already-fetched parent issue via JQL search
	// (`parent = "KEY"` or `"Epic Link" = "KEY"`). It is purely a
	// diagnostic signal; the actual enqueue is signalled by the
	// subsequent KindIssueQueued event for the same key.
	KindChildDiscovered Kind = "issue.child_discovered"

	// KindDevStatusPartialFailure is emitted when one or more Dev
	// Status calls failed for an issue but the issue itself was
	// rendered successfully with whatever data was retrieved. This
	// distinguishes per-call enrichment glitches (one of N upstream
	// Dev Status applications/dataTypes was unreachable, returned
	// malformed JSON, or produced a soft error) from genuine issue-
	// level failures: the rendered Markdown still contains a
	// ## Development section with the entities that did come back,
	// and the issue is counted as Fetched, not Failed.
	//
	// Operationally this maps to slog.LevelWarn (see slog_sink.go):
	// the crawl is healthy but one external enrichment source is
	// degraded. Filtering or alerting on KindIssueFailed will not
	// flag this kind.
	KindDevStatusPartialFailure Kind = "devstatus.partial_failure"
)

// Event carries a single observable occurrence from the library. All fields
// are optional except Kind and Timestamp; callers should check IssueKey,
// Err, and the extra fields for the zero value before using them.
type Event struct {
	// Kind identifies the category of this event.
	Kind Kind

	// Timestamp is the wall-clock time at which the event was created.
	Timestamp time.Time

	// IssueKey is the Jira issue key associated with this event, e.g.
	// "PLATENG-1147". Empty when the event is not issue-specific.
	IssueKey string

	// Message is a human-readable description of the event.
	Message string

	// Err is the error associated with this event, if any. Non-nil only for
	// failure and warning events.
	Err error

	// PRReference is the GitHub pull request URL for KindPRReferenceFound
	// events. Empty for all other kinds.
	PRReference string

	// RawNode is the raw JSON of an unknown ADF node for KindUnknownADFNode
	// events. Empty for all other kinds.
	RawNode string

	// FieldKey is the custom field key for KindUnknownCustomField events.
	// Empty for all other kinds.
	FieldKey string

	// Summary carries the structured crawl totals for KindCrawlSummary
	// events. Nil for all other kinds. Sinks that want structured access to
	// the final counts read this instead of parsing Message.
	Summary *CrawlSummary
}

// CrawlSummary carries the aggregate counts and key lists from a completed
// crawl. It is attached to the KindCrawlSummary Event so structured Sinks
// (e.g. a gRPC stream adapter) can deliver the totals without re-parsing the
// human-readable Message string.
//
// CrawlSummary is a plain value type using only standard-library types so the
// events package remains free of project-internal imports. The crawl package
// builds one from its own crawl.Summary when it emits the final event.
type CrawlSummary struct {
	Fetched        int
	Skipped        int
	Stubbed        int
	Failed         int
	CapLimited     int
	PRsFound       int
	FetchedKeys    []string
	StubbedKeys    []string
	FailedKeys     map[string]string
	CapLimitedKeys []string
	Duration       time.Duration
}

// Sink is the interface that callers implement to receive events from the
// library. The library never logs directly; it calls Emit on the injected
// Sink for every observable occurrence.
//
// Implementations must be safe for concurrent use: the crawl package calls
// Emit from multiple goroutines simultaneously.
type Sink interface {
	// Emit delivers a single event to the sink. Implementations must not
	// block indefinitely; they should buffer or discard if necessary.
	Emit(Event)
}

// NoopSink is a Sink that discards every event. It is useful as a default
// when the caller does not care about events.
type NoopSink struct{}

// Emit discards the event. It never panics, even on a zero Event.
func (NoopSink) Emit(Event) {}

// RecordingSink is a Sink that appends every emitted event to an internal
// slice. It is intended for use in tests of packages that accept a Sink.
//
// RecordingSink is safe for concurrent use.
type RecordingSink struct {
	mu     sync.Mutex
	events []Event
}

// Emit appends e to the internal event slice.
func (r *RecordingSink) Emit(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Events returns a snapshot of all events recorded so far, in emission order.
// The returned slice is a copy; subsequent Emit calls do not affect it.
func (r *RecordingSink) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}
