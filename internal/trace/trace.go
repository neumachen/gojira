// Package trace provides the lightweight correlation primitives the gojira
// crawl uses to emit a reconstructable fan-out tree of log records.
//
// A run is one crawl invocation; it owns a [Span] tree. Each span carries:
//
//   - [RunID]      — the same value across every record of one run.
//   - [SpanID]     — a fresh opaque short ID per logical unit of work.
//   - [ParentSpanID] — the SpanID of whoever enqueued this work.
//   - [TicketID]   — the Jira issue key when work is issue-scoped.
//   - [Phase]      — one of the [PhaseFetch], [PhaseParse], … labels.
//   - [Depth]      — the issue's crawl depth (root = 0).
//
// Span values are pure values: copy freely, no synchronization. The [Span.Logger]
// method returns a [*slog.Logger] pre-tagged with the span's non-empty
// correlation attributes, so every record the returned logger emits is
// auto-correlated without callers having to repeat With(...) at each call site.
//
// IDs are opaque short hex strings drawn from [crypto/rand]. They are NOT
// path/hierarchical IDs — a crawl that bleeds across projects or boards is not
// sequential, so encoding lineage in the ID itself would lie about structure.
// The parent link reconstructs the tree without misrepresenting it.
//
// This package has no project-internal imports; it depends only on the Go
// standard library.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strconv"
	"time"
)

// ---- Phase constants --------------------------------------------------------

// Phase labels a unit of work in the crawl pipeline. Producers stamp the
// current phase onto each span so records can be grouped, counted, and
// timed per phase without parsing free-text messages.
const (
	PhaseFetch        = "fetch"
	PhaseParse        = "parse"
	PhaseHierarchyJQL = "hierarchy_jql"
	PhaseDevStatus    = "dev_status"
	PhaseRender       = "render"
	PhaseStore        = "store"
	PhaseEnqueue      = "enqueue"
)

// ---- Attribute key + trace-stream constants --------------------------------

// Attribute keys for the canonical slog correlation fields. Centralising
// them here keeps producers (crawl, client/httplog) and consumers (jq
// filters, future TUI/MCP frontends) in agreement on names.
const (
	AttrRunID        = "run_id"
	AttrSpanID       = "span_id"
	AttrParentSpanID = "parent_span_id"
	AttrTicketID     = "ticket_id"
	AttrPhase        = "phase"
	AttrDepth        = "depth"
	AttrTraceStream  = "trace_stream"
)

// Trace-stream values. Both streams share run/ticket/span ids so a
// reader can pivot from "this response" to "the stream activity it
// caused" without re-correlating manually.
const (
	StreamResponse = "response" // external/data side (HTTP round-tripper)
	StreamStream   = "stream"   // internal/orchestration side (events/fan-out)
)

// ---- ID generation ---------------------------------------------------------

// randRead is the source of randomness used by newID. It is a package
// variable purely so tests can substitute a failing reader to exercise
// the fallback path; production code never touches it.
var randRead = rand.Read

// newID returns n random bytes hex-encoded (length 2*n). When the
// random source fails (extremely unlikely under crypto/rand) it falls
// back to a deterministic-but-non-empty value derived from the current
// monotonic clock, so callers can rely on a non-empty return.
func newID(n int) string {
	buf := make([]byte, n)
	if _, err := randRead(buf); err != nil {
		// Fallback: never return "". Use the current nanosecond clock
		// as the only source of bits we still have access to. This
		// path is never hit in production but keeps the API total.
		return "t" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

// NewRunID returns an opaque short identifier for a single crawl
// invocation: 12 hex characters (6 random bytes) — collision-resistant
// enough for the scale this tool runs at while staying short enough to
// scan visually in a terminal.
func NewRunID() string { return newID(6) }

// NewSpanID returns an opaque short identifier for one unit of work:
// 10 hex characters (5 random bytes). Slightly shorter than RunID
// because spans are far more numerous within a single run.
func NewSpanID() string { return newID(5) }

// ---- Span ------------------------------------------------------------------

// Span is a single unit of work in the crawl, carrying the correlation
// identity that ties its log lines (and the HTTP requests it triggers)
// into a reconstructable fan-out tree. Span is a value type; copy
// freely. There is no mutability concern.
type Span struct {
	// RunID is constant across every span and every record of a run.
	RunID string

	// SpanID identifies this unit of work uniquely within the run.
	SpanID string

	// ParentSpanID links to the span that enqueued/spawned this one.
	// Empty for a root span.
	ParentSpanID string

	// TicketID is the Jira issue key (e.g. "PLATENG-1417") when the
	// span is issue-scoped. Empty for cross-cutting work (the root
	// span, an end-of-run summary, etc.).
	TicketID string

	// Phase labels what this span is doing; see [PhaseFetch] et al.
	Phase string

	// Depth is the crawl depth at which this span operates; 0 for
	// the root and for seed issues, incremented by [Span.Child] when
	// fan-out enqueues children.
	Depth int
}

// NewRoot mints a fresh root span — a new [RunID] and a fresh [SpanID]
// with no parent. The Phase / TicketID / Depth fields remain zero-valued
// because the root represents the whole invocation, not a specific
// per-issue task.
func NewRoot() Span {
	return Span{
		RunID:  NewRunID(),
		SpanID: NewSpanID(),
	}
}

// Child mints a new span under s: same [RunID], a fresh [SpanID], and
// ParentSpanID set to s.SpanID. The supplied phase, ticketID, and
// depth become the child's identity.
//
// This is the only fan-out primitive: every per-issue or per-API-call
// span in the crawl goes through Child, so the parent_span_id linkage
// is consistent across the codebase.
func (s Span) Child(phase, ticketID string, depth int) Span {
	return Span{
		RunID:        s.RunID,
		SpanID:       NewSpanID(),
		ParentSpanID: s.SpanID,
		TicketID:     ticketID,
		Phase:        phase,
		Depth:        depth,
	}
}

// Logger returns base annotated with this span's non-empty correlation
// attributes via [slog.Logger.With], so every record the returned
// logger emits is auto-tagged with run_id / span_id / parent_span_id
// (when present) / ticket_id / phase / depth.
//
// A nil base is replaced with [slog.Default] so the method is safe to
// call from anywhere a logger may not have been threaded through yet.
// Empty correlation fields (e.g. the root span has no parent) are
// omitted rather than logged as empty strings, keeping records terse.
// Depth is always included because 0 is a meaningful value (the root /
// seed level) and the difference between "depth=0" and "absent" is
// information that downstream consumers can rely on.
func (s Span) Logger(base *slog.Logger) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	attrs := make([]any, 0, 12)
	attrs = append(attrs, AttrRunID, s.RunID, AttrSpanID, s.SpanID)
	if s.ParentSpanID != "" {
		attrs = append(attrs, AttrParentSpanID, s.ParentSpanID)
	}
	if s.TicketID != "" {
		attrs = append(attrs, AttrTicketID, s.TicketID)
	}
	if s.Phase != "" {
		attrs = append(attrs, AttrPhase, s.Phase)
	}
	attrs = append(attrs, AttrDepth, s.Depth)
	return base.With(attrs...)
}

// ---- Context propagation ---------------------------------------------------

// ctxKey is the unexported type used as the context key for an active
// logger. Using a private type prevents collisions with any other
// package that stores values on the same context.
type ctxKey struct{}

// WithLogger returns a derived context carrying lg, so a downstream
// component (notably the HTTP round-tripper added in
// phase-c-httptrace-1) can retrieve the active span-bound logger via
// [LoggerFrom] and tag its own records with the same correlation
// attributes.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, lg)
}

// LoggerFrom returns the logger stored on ctx by [WithLogger], or
// nil if none has been set. A nil return is the caller's signal to
// fall back to a package-level or default logger.
func LoggerFrom(ctx context.Context) *slog.Logger {
	lg, _ := ctx.Value(ctxKey{}).(*slog.Logger)
	return lg
}
