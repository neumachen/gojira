// Package crawl is the recursive crawl orchestrator for gojira.
//
// # Composition contract
//
// crawl is the only package in the module that knows the end-to-end
// recursive workflow. It composes the following sibling packages, in
// this order for each issue:
//
//  1. internal/fetch  — retrieve raw issue bytes from Jira Cloud
//  2. internal/parse  — convert raw bytes to a typed Issue value
//  3. internal/extract — discover outbound references from the Issue
//  4. internal/render — convert the Issue to Markdown content
//  5. internal/output — write Markdown content to the filesystem
//
// Events are emitted to internal/events throughout. Configuration is
// read from internal/config. classify is used to map classify.Kind
// constants to the string labels that render.OutboundRef expects.
//
// # What crawl does NOT own
//
// - HTTP transport (client owns it)
// - JSON parsing (parse owns it)
// - ADF traversal (adf and render own it)
// - Filesystem layout decisions (output owns them)
// - Flag/env parsing (cmd/gojira owns that)
// - Event formatting (cmd/gojira's sink owns that)
// - Signal handling (cmd/gojira owns that; crawl responds to ctx cancellation)
//
// # Sentinel error import deviation
//
// crawl imports client solely to use errors.Is against client's typed
// sentinel errors (ErrUnauthorized, ErrForbidden, ErrNotFound,
// ErrRateLimited). fetch propagates these sentinels unwrapped, so
// errors.Is works correctly. The design doc §5.1 lists only seven
// allowed imports; this is a documented, minimal deviation. crawl
// does not use any other symbol from client.
package crawl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/fetch"
	"github.com/neumachen/gojira/internal/graph"
	"github.com/neumachen/gojira/internal/hierarchy"
	"github.com/neumachen/gojira/internal/output"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/internal/render"
	"github.com/neumachen/gojira/internal/trace"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
	gojiralog "github.com/neumachen/gojira/pkg/log"
)

// ChildDiscoverer is the interface the crawl orchestrator depends on for
// discovering hierarchy children of an already-fetched issue via JQL search.
// It is satisfied by *hierarchy.Discoverer in production; tests may
// substitute a fake.
//
// Children must return the deduplicated, sorted set of child keys for issue.
// An error is treated as a per-issue non-fatal warning by the crawl
// orchestrator: the issue itself is still rendered, but the
// KindIssueFailed event is emitted with a "child discovery failed" message.
type ChildDiscoverer interface {
	Children(ctx context.Context, issue parse.Issue) ([]string, error)
}

// DevStatusEnricher is the interface the crawl orchestrator depends on
// for discovering the pull-request, branch, commit, repository, and
// build metadata associated with an already-fetched issue via Jira's
// Dev Status API. It is satisfied by *devstatus.Enricher in production;
// tests may substitute a fake.
//
// Enrich must return a deduplicated [parse.DevStatusData] value for
// issue, or the zero value when enrichment is opt-out or the issue has
// no associated entities. The production implementation queries every
// configured (application, dataType) pair unconditionally; there is
// no per-issue gate based on the customfield_10000 summary blob.
//
// Errors are treated as per-issue non-fatal warnings by the crawl
// orchestrator: the issue is still rendered (with any partial
// entities that were collected), and a
// [events.KindDevStatusPartialFailure] event is emitted at WARN level
// with a "dev status enrichment failed" message. The issue is NOT
// counted as Failed in the crawl summary — only the enrichment was
// partial, the issue itself succeeded. This distinction prevents log
// filtering and alerting from conflating a degraded external
// enrichment source with a genuine crawl-level failure.
type DevStatusEnricher interface {
	Enrich(ctx context.Context, issue parse.Issue) (parse.DevStatusData, error)
}

// Summary is the structured result returned by Crawl after the run
// completes (successfully or partially). All counts are non-negative.
// Key slices are sorted alphabetically for determinism.
type Summary struct {
	// Fetched is the number of issues successfully fetched, rendered,
	// and written to disk.
	Fetched int

	// Skipped is the number of issues that already existed on disk and
	// were not re-fetched (cfg.Refetch == false).
	Skipped int

	// Stubbed is the number of issues that could not be fetched due to
	// a 403 or 404 response; a stub index.md was written for each.
	Stubbed int

	// Failed is the number of issues that encountered an unrecoverable
	// per-issue error (not 401) and were NOT rendered (not even as a
	// stub). Rate-limited issues after retries exhausted fall here.
	Failed int

	// CapLimited is the number of issues that were discovered but not
	// fetched because an issue cap, depth cap, or context cancellation
	// prevented them from being enqueued or processed.
	CapLimited int

	// PRsFound is the count of distinct GitHub PR URLs discovered
	// across the entire crawl.
	PRsFound int

	// FetchedKeys lists the keys of successfully fetched issues,
	// sorted alphabetically.
	FetchedKeys []string

	// StubbedKeys lists the keys of stubbed issues, sorted
	// alphabetically.
	StubbedKeys []string

	// FailedKeys maps each failed issue key to a human-readable reason.
	FailedKeys map[string]string

	// CapLimitedKeys lists the keys of cap-limited issues, sorted
	// alphabetically.
	CapLimitedKeys []string

	// Duration is the wall-clock time elapsed during the crawl.
	Duration time.Duration

	// APICallCounts maps a phase label (fetch, hierarchy_jql, dev_status,
	// parse, render, store) to the number of times that phase ran across the
	// entire crawl. Surfaced for measurement attribution; not a stability
	// contract for downstream consumers.
	APICallCounts map[string]int

	// APITimeByPhase maps the same phase labels to the total wall-clock time
	// spent in that phase across the crawl. Useful for answering "where did
	// the 32s go?".
	APITimeByPhase map[string]time.Duration

	// TotalAPITime is the sum of APITimeByPhase values — total wall-clock
	// time spent in any instrumented phase across all issues, summed.
	TotalAPITime time.Duration
}

// workItem is a single unit of work in the crawl queue.
type workItem struct {
	key   string
	depth int
}

// crawler holds all state for a single Crawl invocation.
//
// # Channel lifecycle and race-freedom
//
// The queue channel is closed exactly once. All sends to the channel
// happen inside enqueue, which is always called with c.mu held.
// The channel is closed inside closeQueueLocked, which is also always
// called with c.mu held. This guarantees that close(c.queue) never
// races with c.queue <- item.
//
// The queueClosed flag (protected by c.mu) prevents double-close and
// prevents sends to a closed channel.
type crawler struct {
	cfg     config.Config
	fetcher fetch.Fetcher
	sink    events.Sink
	// store is the injectable output destination. It is always non-nil
	// after construction; CrawlWithEnrichers defaults to an FSStore
	// rooted at cfg.OutputDir when the caller passes nil.
	store output.Store
	// hier discovers hierarchy children for an already-fetched issue.
	// nil when hierarchy discovery is disabled (cfg.IncludeChildren is
	// false) or when the caller chose to construct the crawler via the
	// legacy Crawl() entry point that does not supply a discoverer.
	hier ChildDiscoverer
	// devStatus discovers full Dev Status enrichment (PRs, branches,
	// commits, repositories, builds) for an already-fetched issue.
	// nil when dev-status enrichment is opt-out
	// (cfg.IncludeDevStatus is false) or when the caller chose a
	// legacy entry point that does not supply an enricher.
	devStatus DevStatusEnricher

	// crawlCtx is cancelled on 401 abort, time cap, or parent ctx cancel.
	crawlCtx    context.Context
	cancelCrawl context.CancelFunc

	// mu protects visited, seenPRs, summary, queueClosed, and all
	// sends/closes of the queue channel.
	mu          sync.Mutex
	visited     map[string]bool
	seenPRs     map[string]bool
	summary     Summary
	queueClosed bool

	// queue is the work channel. It is closed exactly once, always
	// under c.mu, via closeQueueLocked.
	queue chan workItem

	// pending counts items that are either in the queue or being
	// processed by a worker. Decremented atomically after each item
	// is processed. When it reaches zero, the queue is closed.
	pending int64

	// abortErr holds the first fatal error (401).
	abortErr  error
	abortOnce sync.Once

	// logger is the orchestrator's slog sink. It is always non-nil
	// after construction (defaulting to [noopLogger] when the caller
	// of [CrawlWithEnrichers] passes nil) so emission sites can call
	// c.logger.LogAttrs unconditionally.
	logger *slog.Logger

	// rootSpan identifies this crawl run. Every span instrumented by the
	// orchestrator is a descendant of rootSpan. Created once in
	// [CrawlWithEnrichers]; immutable for the lifetime of the run.
	rootSpan trace.Span

	// phaseMu protects phaseCounts and phaseDurations from concurrent
	// updates by parallel workers. Read at end-of-crawl to fold into
	// Summary.
	phaseMu        sync.Mutex
	phaseCounts    map[string]int
	phaseDurations map[string]time.Duration

	// graph collects nodes and edges describing the crawled issue
	// graph. nil when cfg.EmitGraph is false and forceGraph is also
	// false. Accessed only while holding c.mu (the collector itself
	// is not safe for concurrent use).
	graph *graph.Collector

	// forceGraph forces the graph collector ON regardless of
	// cfg.EmitGraph, AND suppresses the post-loop disk write. Set
	// only by [CrawlGraphWithEnrichers]; callers consume the
	// in-memory [graph.Model] via the returned value instead.
	forceGraph bool
}

// noopLogger returns a [*slog.Logger] whose handler always returns false
// from Enabled and silently discards Handle calls — used as the safe
// default when no logger is injected. Avoids nil checks at every
// emission site and lets the span-instrumentation code unconditionally
// call LogAttrs without affecting behavior or producing output.
func noopLogger() *slog.Logger {
	return slog.New(noopHandler{})
}

// noopHandler is the [slog.Handler] backing [noopLogger]. All four
// methods are no-ops; WithAttrs and WithGroup return the same handler
// so chained .With(...) calls remain cheap and side-effect-free.
type noopHandler struct{}

func (noopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (noopHandler) Handle(context.Context, slog.Record) error { return nil }
func (noopHandler) WithAttrs([]slog.Attr) slog.Handler        { return noopHandler{} }
func (noopHandler) WithGroup(string) slog.Handler             { return noopHandler{} }

// recordPhase aggregates a single phase invocation's wall-clock cost into the
// crawler's running tallies. Safe under concurrent invocation by multiple
// workers.
func (c *crawler) recordPhase(phase string, d time.Duration) {
	c.phaseMu.Lock()
	c.phaseCounts[phase]++
	c.phaseDurations[phase] += d
	c.phaseMu.Unlock()
}

// durationMsMap converts a map of phase→Duration into a map of phase→int64 ms
// for log-friendly emission.
func durationMsMap(in map[string]time.Duration) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v.Milliseconds()
	}
	return out
}

// closeQueueLocked closes the work queue. Must be called with c.mu held.
// It is a no-op if the queue is already closed.
func (c *crawler) closeQueueLocked() {
	if !c.queueClosed {
		c.queueClosed = true
		close(c.queue)
	}
}

// enqueue adds key to the queue if it has not been visited and caps
// allow it. Must be called with c.mu held.
func (c *crawler) enqueue(key string, depth int) {
	norm := strings.ToUpper(key)
	if c.visited[norm] {
		return
	}
	// Issue cap: count of visited keys (already-queued or processed).
	if c.cfg.IssueCap > 0 && len(c.visited) >= c.cfg.IssueCap {
		c.summary.CapLimited++
		c.summary.CapLimitedKeys = append(c.summary.CapLimitedKeys, norm)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueCapReached,
			IssueKey:  norm,
			Message:   fmt.Sprintf("issue cap (%d) reached; %s not enqueued", c.cfg.IssueCap, norm),
			Timestamp: time.Now(),
		})
		return
	}
	// Depth cap.
	if c.cfg.DepthLimit > 0 && depth > c.cfg.DepthLimit {
		c.summary.CapLimited++
		c.summary.CapLimitedKeys = append(c.summary.CapLimitedKeys, norm)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueCapReached,
			IssueKey:  norm,
			Message:   fmt.Sprintf("depth cap (%d) reached; %s at depth %d not enqueued", c.cfg.DepthLimit, norm, depth),
			Timestamp: time.Now(),
		})
		return
	}
	if c.queueClosed {
		// Queue was closed (context cancelled); treat as CapLimited.
		c.summary.CapLimited++
		c.summary.CapLimitedKeys = append(c.summary.CapLimitedKeys, norm)
		return
	}
	c.visited[norm] = true
	atomic.AddInt64(&c.pending, 1)
	c.sink.Emit(events.Event{
		Kind:      events.KindIssueQueued,
		IssueKey:  norm,
		Message:   fmt.Sprintf("queued %s at depth %d", norm, depth),
		Timestamp: time.Now(),
	})
	c.queue <- workItem{key: norm, depth: depth}
}

// enqueueFrom is [enqueue] plus a TRACE-level lineage record carrying
// which issue surfaced this reference and via which relationship type
// (e.g. "hierarchy_child", "outward Blocks", "ref"). It is the
// orchestrator's own record of why this key entered the queue,
// distinct from the existing [events.KindIssueQueued] sink events: the
// fan-out tree is reconstructed from these lines by joining on
// discovered_from. Must be called with c.mu held (delegates to
// [enqueue], which has the same precondition).
//
// The TRACE level is deliberate: lineage is the canonical
// traceability use case, dense in big crawls, and worth filtering out
// at INFO/DEBUG.
func (c *crawler) enqueueFrom(key string, depth int, discoveredFrom, relation string) {
	c.logger.LogAttrs(c.crawlCtx, gojiralog.LevelTrace, "crawl.fanout",
		slog.String(trace.AttrTicketID, strings.ToUpper(key)),
		slog.Int(trace.AttrDepth, depth),
		slog.String("discovered_from", discoveredFrom),
		slog.String("relation", relation),
	)
	c.enqueue(key, depth)
}

// decrementPending decrements the pending counter. If it reaches zero,
// the queue is closed (under c.mu) so workers exit their range loop.
func (c *crawler) decrementPending() {
	if atomic.AddInt64(&c.pending, -1) == 0 {
		c.mu.Lock()
		c.closeQueueLocked()
		c.mu.Unlock()
	}
}

// processIssue handles one work item. Returns a non-nil error only for
// a 401 (abort the crawl).
func (c *crawler) processIssue(item workItem) error {
	key := item.key
	depth := item.depth

	// Per-issue span. Phase is left empty here — the issue-level span is
	// an envelope; the per-phase blocks below stamp their own phase via
	// .With on the span logger.
	span := c.rootSpan.Child("", strings.ToUpper(key), depth)
	spanLogger := span.Logger(c.logger)
	spanStart := time.Now()
	spanLogger.LogAttrs(c.crawlCtx, slog.LevelInfo, "issue.process.start",
		slog.String(trace.AttrTicketID, strings.ToUpper(key)),
		slog.Int(trace.AttrDepth, depth),
	)
	// Bind the span logger to ctx so the httplog RoundTripper picks it
	// up for downstream HTTP requests (fetch, hierarchy JQL, dev-status).
	ctx := trace.WithLogger(c.crawlCtx, spanLogger)
	defer func() {
		spanLogger.LogAttrs(c.crawlCtx, slog.LevelInfo, "issue.process.end",
			slog.String(trace.AttrTicketID, strings.ToUpper(key)),
			slog.Int(trace.AttrDepth, depth),
			slog.Int64("duration_ms", time.Since(spanStart).Milliseconds()),
		)
	}()

	// Skip-if-exists probe: check before fetching to avoid burning an
	// API call. This lives in crawl because output.ErrAlreadyExists fires
	// only after a fetch; the pre-fetch short-circuit is a crawl-level
	// optimization.
	if !c.cfg.Refetch && indexExists(c.cfg.OutputDir, key) {
		spanLogger.LogAttrs(ctx, slog.LevelDebug, "crawl.skip_if_exists",
			slog.String(trace.AttrTicketID, key),
			slog.String("reason", "index_md_exists"),
		)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueSkipped,
			IssueKey:  key,
			Message:   fmt.Sprintf("skipped %s (already exists on disk)", key),
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Skipped++
		c.mu.Unlock()
		return nil
	}

	// Fetch.
	var raw []byte
	var fetchErr error
	{
		fl := spanLogger.With(trace.AttrPhase, trace.PhaseFetch)
		fctx := trace.WithLogger(ctx, fl)
		fstart := time.Now()
		fl.LogAttrs(fctx, slog.LevelInfo, "phase.start",
			slog.String(trace.AttrPhase, trace.PhaseFetch),
			slog.String(trace.AttrTicketID, key),
		)
		raw, fetchErr = c.fetcher.Fetch(fctx, key)
		fdur := time.Since(fstart)
		fl.LogAttrs(fctx, slog.LevelInfo, "phase.end",
			slog.String(trace.AttrPhase, trace.PhaseFetch),
			slog.String(trace.AttrTicketID, key),
			slog.Int64("duration_ms", fdur.Milliseconds()),
			slog.Bool("ok", fetchErr == nil),
		)
		c.recordPhase(trace.PhaseFetch, fdur)
	}
	if fetchErr != nil {
		return c.handleFetchError(key, fetchErr)
	}

	// Parse. Pure CPU, but still measured so total per-issue accounting
	// can attribute time to each phase. A single inline INFO line is
	// enough; parse is fast and synchronous.
	pstart := time.Now()
	issue, parseErr := parse.Parse(raw, c.cfg.Site)
	pdur := time.Since(pstart)
	spanLogger.LogAttrs(ctx, slog.LevelInfo, "phase.end",
		slog.String(trace.AttrPhase, trace.PhaseParse),
		slog.String(trace.AttrTicketID, key),
		slog.Int64("duration_ms", pdur.Milliseconds()),
		slog.Bool("ok", parseErr == nil),
	)
	c.recordPhase(trace.PhaseParse, pdur)
	if parseErr != nil {
		reason := fmt.Sprintf("parse error: %v", parseErr)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   reason,
			Err:       parseErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = reason
		c.mu.Unlock()
		return nil
	}

	// Extract outbound references.
	refs, extractErr := extract.Extract(issue, c.cfg.Site)
	if extractErr != nil {
		reason := fmt.Sprintf("extract error: %v", extractErr)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   reason,
			Err:       extractErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = reason
		c.mu.Unlock()
		return nil
	}

	// Hierarchy children: discover synchronously here so we can render the
	// "### Children" subsection alongside the issue's other relationships.
	//
	// This is the "Pattern A — eager inline discovery" decision from the
	// design discussion: one or two JQL calls per hierarchy-capable issue,
	// performed by the same worker that just finished the parse, with the
	// resulting keys flowing back into the existing work queue. No new
	// goroutine types are introduced.
	//
	// The mutation of issue.Children here is safe even though parse.Parse
	// guarantees Children is nil on return: parse is pure and has already
	// returned by this point. Render is called next and observes the
	// populated slice.
	var childKeys []string
	if c.cfg.IncludeChildren && c.hier != nil && hierarchy.HierarchyCapable(issue.IssueType) {
		hl := spanLogger.With(trace.AttrPhase, trace.PhaseHierarchyJQL)
		hctx := trace.WithLogger(ctx, hl)
		hstart := time.Now()
		hl.LogAttrs(hctx, slog.LevelInfo, "phase.start",
			slog.String(trace.AttrPhase, trace.PhaseHierarchyJQL),
			slog.String(trace.AttrTicketID, key),
		)
		discovered, err := c.hier.Children(hctx, issue)
		hdur := time.Since(hstart)
		hl.LogAttrs(hctx, slog.LevelInfo, "phase.end",
			slog.String(trace.AttrPhase, trace.PhaseHierarchyJQL),
			slog.String(trace.AttrTicketID, key),
			slog.Int64("duration_ms", hdur.Milliseconds()),
			slog.Bool("ok", err == nil),
		)
		c.recordPhase(trace.PhaseHierarchyJQL, hdur)
		if err != nil {
			// Non-fatal: the issue itself is rendered; we just missed
			// (some of) its children. Emit a failure event so operators
			// can see the gap without aborting the crawl.
			c.sink.Emit(events.Event{
				Kind:      events.KindIssueFailed,
				IssueKey:  key,
				Message:   fmt.Sprintf("child discovery failed for %s: %v", key, err),
				Err:       err,
				Timestamp: time.Now(),
			})
		}
		// discovered is non-nil on partial success; keep whatever we got.
		childKeys = discovered
		issue.Children = childKeys
	}

	// Dev Status enrichment: discover PRs, branches, commits,
	// repositories, and builds associated with the issue via Jira's
	// undocumented Dev Status API.
	//
	// Mirrors the hierarchy block above: synchronous, per-issue, in
	// the same worker that just parsed the response. Every configured
	// (application, dataType) pair is queried unconditionally — there
	// is no per-issue gate. The customfield_10000 summary blob was
	// historically used to skip dataTypes whose count was zero, but
	// that optimisation produced two silent-miss bugs (PROJ-1578
	// most notably) where a stale-cached summary suppressed calls
	// that would have surfaced real entities. The simpler "always
	// call" contract costs at most five extra HTTP requests per
	// issue and eliminates that class of bug entirely.
	//
	// Errors are non-fatal at the issue level: the issue is still
	// rendered, and any partially-collected entities are kept. A
	// KindDevStatusPartialFailure event is emitted (mapped to WARN
	// by slog_sink) instead of KindIssueFailed (ERROR) so operator
	// log filtering and alerting can distinguish a degraded external
	// enrichment source from a genuine issue-level failure.
	// Summary.Failed is NOT incremented here for the same reason.
	//
	// The mutation of issue.DevStatus here is safe even though
	// parse.Parse guarantees the zero value on return: parse is pure
	// and has already returned by this point. Render observes the
	// populated struct when it runs next.
	if c.devStatus != nil && c.cfg.IncludeDevStatus {
		dl := spanLogger.With(trace.AttrPhase, trace.PhaseDevStatus)
		dctx := trace.WithLogger(ctx, dl)
		dstart := time.Now()
		dl.LogAttrs(dctx, slog.LevelInfo, "phase.start",
			slog.String(trace.AttrPhase, trace.PhaseDevStatus),
			slog.String(trace.AttrTicketID, key),
		)
		discovered, err := c.devStatus.Enrich(dctx, issue)
		ddur := time.Since(dstart)
		dl.LogAttrs(dctx, slog.LevelInfo, "phase.end",
			slog.String(trace.AttrPhase, trace.PhaseDevStatus),
			slog.String(trace.AttrTicketID, key),
			slog.Int64("duration_ms", ddur.Milliseconds()),
			slog.Bool("ok", err == nil),
		)
		c.recordPhase(trace.PhaseDevStatus, ddur)
		if err != nil {
			msg := fmt.Sprintf("dev status enrichment partially failed for %s: %v", key, err)
			if !hasAnyDevStatusEntity(discovered) {
				// No partial data and an error: every per-call request
				// failed. Still NOT KindIssueFailed — the issue itself
				// was rendered fine; only the external enrichment
				// source is degraded. Operators can investigate via
				// the joined error attached to the event.
				msg = fmt.Sprintf("dev status enrichment failed for %s; no entities discovered: %v", key, err)
			}
			c.sink.Emit(events.Event{
				Kind:      events.KindDevStatusPartialFailure,
				IssueKey:  key,
				Message:   msg,
				Err:       err,
				Timestamp: time.Now(),
			})
		}
		// Preserve whatever we got, even partial.
		issue.DevStatus = discovered
	}

	// Emit events for unknown custom fields.
	for fieldKey := range issue.CustomFields {
		c.sink.Emit(events.Event{
			Kind:      events.KindUnknownCustomField,
			IssueKey:  key,
			FieldKey:  fieldKey,
			Message:   fmt.Sprintf("unknown custom field %q in %s", fieldKey, key),
			Timestamp: time.Now(),
		})
	}

	// Build the neighbours set (all visited keys plus this issue's
	// hierarchy children) for relative link resolution in the renderer.
	//
	// Hierarchy children discovered in the previous step are about to be
	// enqueued, so including them here lets the rendered "### Children"
	// subsection use relative paths even when the child has not yet been
	// fetched. The relative path target (<KEY>/index.md) will exist on
	// disk after the child is processed; if a child can't be fetched it
	// is rendered as a stub, which also lives at <KEY>/index.md, so the
	// relative link remains valid.
	c.mu.Lock()
	neighbours := make(map[string]bool, len(c.visited)+len(childKeys))
	for k := range c.visited {
		neighbours[k] = true
	}
	for _, ck := range childKeys {
		neighbours[strings.ToUpper(ck)] = true
	}
	c.mu.Unlock()

	// Map extract.Reference → render.OutboundRef.
	outboundRefs := refsToOutbound(refs)

	// Render index.md. RenderNullCustomFields is threaded through
	// the config so the renderer can decide whether to surface or
	// suppress JSON-null custom-field entries; the default
	// (false) drops them to reduce noise.
	rstart := time.Now()
	indexMD, renderErr := render.RenderIssue(issue, neighbours, c.cfg.RenderNullCustomFields)
	rdur := time.Since(rstart)
	spanLogger.LogAttrs(ctx, slog.LevelInfo, "phase.end",
		slog.String(trace.AttrPhase, trace.PhaseRender),
		slog.String(trace.AttrTicketID, key),
		slog.Int64("duration_ms", rdur.Milliseconds()),
		slog.Bool("ok", renderErr == nil),
	)
	c.recordPhase(trace.PhaseRender, rdur)
	if renderErr != nil {
		reason := fmt.Sprintf("render error: %v", renderErr)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   reason,
			Err:       renderErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = reason
		c.mu.Unlock()
		return nil
	}

	// Render outbound.md.
	outboundMD, outboundErr := render.RenderOutbound(outboundRefs)
	if outboundErr != nil {
		reason := fmt.Sprintf("render outbound error: %v", outboundErr)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   reason,
			Err:       outboundErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = reason
		c.mu.Unlock()
		return nil
	}

	// Write to disk via the injected Store.
	sl := spanLogger.With(trace.AttrPhase, trace.PhaseStore)
	sctx := trace.WithLogger(ctx, sl)
	sstart := time.Now()
	sl.LogAttrs(sctx, slog.LevelInfo, "phase.start",
		slog.String(trace.AttrPhase, trace.PhaseStore),
		slog.String(trace.AttrTicketID, key),
	)
	writeErr := c.store.Write(sctx, key, indexMD, outboundMD)
	sdur := time.Since(sstart)
	sl.LogAttrs(sctx, slog.LevelInfo, "phase.end",
		slog.String(trace.AttrPhase, trace.PhaseStore),
		slog.String(trace.AttrTicketID, key),
		slog.Int64("duration_ms", sdur.Milliseconds()),
		slog.Bool("ok", writeErr == nil),
	)
	c.recordPhase(trace.PhaseStore, sdur)
	if writeErr != nil {
		if errors.Is(writeErr, output.ErrAlreadyExists) {
			// Race: another goroutine wrote this key between our probe
			// and our write. Treat as skipped.
			c.sink.Emit(events.Event{
				Kind:      events.KindIssueSkipped,
				IssueKey:  key,
				Message:   fmt.Sprintf("skipped %s (already exists, race)", key),
				Timestamp: time.Now(),
			})
			c.mu.Lock()
			c.summary.Skipped++
			c.mu.Unlock()
			return nil
		}
		reason := fmt.Sprintf("output error: %v", writeErr)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   reason,
			Err:       writeErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = reason
		c.mu.Unlock()
		return nil
	}

	// Success: record the fetch and discover outbound Jira references.
	c.sink.Emit(events.Event{
		Kind:      events.KindIssueFetched,
		IssueKey:  key,
		Message:   fmt.Sprintf("fetched and rendered %s", key),
		Timestamp: time.Now(),
	})

	c.mu.Lock()
	c.summary.Fetched++
	c.summary.FetchedKeys = append(c.summary.FetchedKeys, key)

	// Record this issue in the graph collector when enabled. The
	// Collector is not concurrent-safe, but we are inside the
	// c.mu critical section so the access is serialized.
	if c.graph != nil {
		c.graph.Add(issue, refs)
	}

	// Discover and enqueue outbound Jira references (still holding c.mu
	// so that enqueue's channel send is protected against concurrent
	// closeQueueLocked calls).
	for _, ref := range refs {
		switch ref.Kind {
		case classify.KindJiraKey, classify.KindJiraURL:
			if ref.IssueKey != "" {
				// Lineage: prefer the structured IssueLink relation
				// (e.g. "outward Blocks"); fall back to the discovery
				// Source label ("Description", "RemoteLink", …) so
				// every fan-out edge carries some relation context.
				rel := ref.Relation
				if rel == "" {
					rel = ref.Source.String()
				}
				c.enqueueFrom(ref.IssueKey, depth+1, key, rel)
			}
		case classify.KindGitHubPR:
			if ref.URL != "" && !c.seenPRs[ref.URL] {
				c.seenPRs[ref.URL] = true
				c.summary.PRsFound++
				c.sink.Emit(events.Event{
					Kind:        events.KindPRReferenceFound,
					IssueKey:    key,
					PRReference: ref.URL,
					Message:     fmt.Sprintf("PR reference found in %s: %s", key, ref.URL),
					Timestamp:   time.Now(),
				})
			}
		}
	}

	// Account for Dev Status pull-request discoveries. The summary
	// counter PRsFound and per-URL KindPRReferenceFound events were
	// historically driven only by classify.KindGitHubPR references in
	// extract.Extract output (ADF body links and remote links). Dev
	// Status surfaces PRs that the issue body never mentions, so we
	// fold those into the same observable counters here. Dedup
	// against c.seenPRs so a PR referenced both in the body and via
	// Dev Status is counted exactly once.
	//
	// Branches/commits/repositories/builds do not have an equivalent
	// summary counter today; they appear only in the rendered
	// "## Development" section.
	for _, pr := range issue.DevStatus.PullRequests {
		if pr.URL == "" || c.seenPRs[pr.URL] {
			continue
		}
		c.seenPRs[pr.URL] = true
		c.summary.PRsFound++
		c.sink.Emit(events.Event{
			Kind:        events.KindPRReferenceFound,
			IssueKey:    key,
			PRReference: pr.URL,
			Message:     fmt.Sprintf("PR reference found in %s via dev status: %s", key, pr.URL),
			Timestamp:   time.Now(),
		})
	}

	// Enqueue hierarchy children discovered by the JQL search.
	// KindChildDiscovered is emitted before enqueue so observers can see
	// the discovery even when caps prevent the enqueue itself.
	for _, ck := range childKeys {
		c.sink.Emit(events.Event{
			Kind:      events.KindChildDiscovered,
			IssueKey:  key,
			Message:   fmt.Sprintf("hierarchy child of %s discovered: %s", key, ck),
			Timestamp: time.Now(),
		})
		c.enqueueFrom(ck, depth+1, key, "hierarchy_child")
	}
	c.mu.Unlock()

	return nil
}

// handleFetchError classifies a fetch error and takes the appropriate
// action. Returns a non-nil error only for 401 (abort the crawl).
func (c *crawler) handleFetchError(key string, fetchErr error) error {
	switch {
	case errors.Is(fetchErr, client.ErrUnauthorized):
		reason := "Unauthorized"
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   fmt.Sprintf("fetch %s: %s", key, reason),
			Err:       fetchErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = reason
		c.mu.Unlock()
		return errext.Errorf("crawl: unauthorized: %w", fetchErr)

	case errors.Is(fetchErr, client.ErrForbidden):
		c.writeStub(key, "Permission denied (403)")
		return nil

	case errors.Is(fetchErr, client.ErrNotFound):
		c.writeStub(key, "Not found (404)")
		return nil

	case errors.Is(fetchErr, client.ErrRateLimited):
		reason := "Rate limited (429) exhausted"
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   fmt.Sprintf("fetch %s: %s", key, reason),
			Err:       fetchErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = reason
		c.mu.Unlock()
		return nil

	default:
		// Network/transport error or context cancellation.
		if c.crawlCtx.Err() != nil {
			// Context was cancelled; count as CapLimited.
			c.mu.Lock()
			c.summary.CapLimited++
			c.summary.CapLimitedKeys = append(c.summary.CapLimitedKeys, key)
			c.mu.Unlock()
			return nil
		}
		// Genuine network error: render a stub.
		c.writeStub(key, fmt.Sprintf("Fetch failed: %v", fetchErr))
		return nil
	}
}

// writeStub renders and writes a stub index.md for an issue that could
// not be fetched. Emits KindIssueStubbed on success; counts as Failed
// on write error.
func (c *crawler) writeStub(key, reason string) {
	sourceURL := ""
	if c.cfg.Site != "" {
		sourceURL = strings.TrimRight(c.cfg.Site, "/") + "/browse/" + key
	}
	stubMD, err := render.RenderStub(key, reason, sourceURL)
	if err != nil {
		r := fmt.Sprintf("render stub error: %v", err)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   r,
			Err:       err,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = r
		c.mu.Unlock()
		return
	}

	// Write stub via the injected Store.
	writeErr := c.store.Write(c.crawlCtx, key, stubMD, "")
	if writeErr != nil {
		r := fmt.Sprintf("write stub error: %v", writeErr)
		c.sink.Emit(events.Event{
			Kind:      events.KindIssueFailed,
			IssueKey:  key,
			Message:   r,
			Err:       writeErr,
			Timestamp: time.Now(),
		})
		c.mu.Lock()
		c.summary.Failed++
		c.summary.FailedKeys[key] = r
		c.mu.Unlock()
		return
	}

	c.sink.Emit(events.Event{
		Kind:      events.KindIssueStubbed,
		IssueKey:  key,
		Message:   fmt.Sprintf("stubbed %s: %s", key, reason),
		Timestamp: time.Now(),
	})
	c.mu.Lock()
	c.summary.Stubbed++
	c.summary.StubbedKeys = append(c.summary.StubbedKeys, key)
	c.mu.Unlock()
}

// Crawl executes a recursive Jira issue crawl starting from startKeys.
//
// It seeds a work queue with startKeys at depth 0, then runs up to
// cfg.Concurrency workers (minimum 1) that each pull a key from the
// queue, fetch the issue, parse it, extract outbound references,
// render Markdown, and write the output to disk.
//
// # Error handling policy
//
//   - 401 Unauthorized: the crawl is aborted immediately. All in-flight
//     workers finish their current fetch+render+write, then Crawl returns
//     a partial Summary and an error wrapping client.ErrUnauthorized.
//     The caller should map this to exit code 1.
//   - 403 Forbidden / 404 Not Found: a stub index.md is written for the
//     issue. The crawl continues. The issue is counted in Stubbed.
//   - Rate limited (429, retries exhausted): the issue is counted in
//     Failed with reason "Rate limited (429) exhausted". No stub is
//     written. The crawl continues.
//   - Network/transport error (after client retries): a stub is written
//     with reason "Fetch failed: <err>". The crawl continues.
//   - Parse/render/output error: the issue is counted in Failed with the
//     error message. No stub is written — these are bugs, not expected
//     operational failures, and papering over them with a stub would hide
//     the problem.
//
// # Skip-if-exists probe
//
// Before calling fetcher.Fetch, crawl checks whether <outputDir>/<KEY>/
// index.md already exists. If it does and cfg.Refetch is false, the
// issue is counted as Skipped without burning an API call. This probe
// lives in crawl (not output) because output.ErrAlreadyExists fires
// only after a fetch has already happened; the pre-fetch short-circuit
// is a crawl-level optimization.
//
// # Graceful shutdown
//
// When ctx is cancelled (SIGINT/SIGTERM from the CLI, a 401 abort, or
// a time cap), workers finish their in-flight fetch+render+write and
// exit. Items remaining in the queue are drained and counted as
// CapLimited.
//
// # Concurrency model
//
// Workers are plain goroutines managed with sync.WaitGroup. The work
// queue is a buffered channel. An atomic "pending" counter tracks the
// number of items that are either in the queue or being processed by a
// worker. When pending reaches zero, the queue channel is closed (under
// c.mu), which causes all workers to exit their range loop.
//
// All sends to the queue channel happen inside enqueue, which is always
// called with c.mu held. The channel is closed inside closeQueueLocked,
// which is also always called with c.mu held. This guarantees that
// close(c.queue) never races with c.queue <- item.
//
// A dedicated "closer" goroutine watches for context cancellation and
// drains + closes the queue when the context is done. The visited map
// and summary are protected by c.mu.
func Crawl(
	ctx context.Context,
	cfg config.Config,
	startKeys []string,
	fetcher fetch.Fetcher,
	sink events.Sink,
) (Summary, error) {
	return CrawlWithDiscoverer(ctx, cfg, startKeys, fetcher, sink, nil)
}

// CrawlWithDiscoverer is the extended entry point that additionally accepts
// a ChildDiscoverer for hierarchy expansion.
//
// When hier is nil OR cfg.IncludeChildren is false, hierarchy discovery is
// skipped and the crawl behaves exactly as the legacy [Crawl] function.
// When both are present, every hierarchy-capable issue (per
// hierarchy.HierarchyCapable) has its JQL children fetched after the
// fetch+parse+extract sequence completes and the resulting keys are
// enqueued at depth+1, subject to the usual issue and depth caps.
//
// This entry point is preserved as a thin wrapper over
// [CrawlWithEnrichers] for callers that only need hierarchy expansion.
// Tests that do not care about hierarchy may continue to call [Crawl].
func CrawlWithDiscoverer(
	ctx context.Context,
	cfg config.Config,
	startKeys []string,
	fetcher fetch.Fetcher,
	sink events.Sink,
	hier ChildDiscoverer,
) (Summary, error) {
	return CrawlWithEnrichers(ctx, cfg, startKeys, fetcher, sink, hier, nil, nil, nil)
}

// CrawlWithEnrichers is the extended entry point that accepts both a
// [ChildDiscoverer] for hierarchy expansion and a [DevStatusEnricher]
// for Dev Status pull-request, branch, commit, repository, and build
// enrichment.
//
// hier and devStatus are independent: passing nil for either disables
// that enrichment (also independently gated by cfg.IncludeChildren and
// cfg.IncludeDevStatus respectively). The gojira facade constructs
// both from the shared *client.Client and supplies them here.
//
// The store parameter is additive over the previous signature: it
// selects the destination for rendered Markdown. Passing nil defaults
// to an [output.FSStore] rooted at cfg.OutputDir, preserving the
// historical on-disk behavior (skip-if-exists vs. refetch semantics
// continue to be honored). Alternative [output.Store] implementations
// can be injected by callers that want to deliver crawl output to a
// non-filesystem destination (e.g. an in-memory buffer or a future
// service front-end).
//
// The logger parameter is additive over the previous signature: it is
// the orchestrator's [*slog.Logger] sink for the structured span
// instrumentation (crawl.start / issue.process.start / phase.* /
// issue.process.end / crawl.end / crawl.measurement). Passing nil
// defaults to a no-op logger, preserving the prior behavior of
// emitting nothing for callers that have not yet adopted the
// observability instrument.
//
// The signature is additive over [CrawlWithDiscoverer]; existing
// callers that only need hierarchy expansion are unaffected.
func CrawlWithEnrichers(
	ctx context.Context,
	cfg config.Config,
	startKeys []string,
	fetcher fetch.Fetcher,
	sink events.Sink,
	hier ChildDiscoverer,
	devStatus DevStatusEnricher,
	store output.Store,
	logger *slog.Logger,
) (Summary, error) {
	sum, _, err := crawlImpl(ctx, cfg, startKeys, fetcher, sink, hier, devStatus, store, logger, false)
	return sum, err
}

// CrawlGraphWithEnrichers is the in-memory graph variant of
// [CrawlWithEnrichers]. It runs the same crawl with graph collection
// FORCED ON (independent of cfg.EmitGraph) and SUPPRESSES the
// post-loop disk write of graph.json / graph.d2, returning the
// collected [graph.Model] alongside the [Summary] instead.
//
// All other observable behavior — events, summary, exit-code mapping
// of errors — is byte-identical to [CrawlWithEnrichers]. This is the
// entry point the gRPC GetGraph handler uses and is also useful to
// library callers who want the graph in memory without touching the
// filesystem.
func CrawlGraphWithEnrichers(
	ctx context.Context,
	cfg config.Config,
	startKeys []string,
	fetcher fetch.Fetcher,
	sink events.Sink,
	hier ChildDiscoverer,
	devStatus DevStatusEnricher,
	store output.Store,
	logger *slog.Logger,
) (Summary, graph.Model, error) {
	return crawlImpl(ctx, cfg, startKeys, fetcher, sink, hier, devStatus, store, logger, true)
}

// crawlImpl is the shared implementation behind [CrawlWithEnrichers]
// and [CrawlGraphWithEnrichers]. forceGraph=true forces the
// in-memory graph collector ON regardless of cfg.EmitGraph and
// suppresses the disk write; the returned graph.Model is the
// caller-visible artifact in that mode (zero-valued otherwise).
func crawlImpl(
	ctx context.Context,
	cfg config.Config,
	startKeys []string,
	fetcher fetch.Fetcher,
	sink events.Sink,
	hier ChildDiscoverer,
	devStatus DevStatusEnricher,
	store output.Store,
	logger *slog.Logger,
	forceGraph bool,
) (Summary, graph.Model, error) {
	// When the caller passes a nil logger, default to a no-op handler so the
	// rest of the orchestrator can emit unconditionally. This preserves the
	// historical no-output behavior for callers that have not adopted the
	// observability instrument.
	if logger == nil {
		logger = noopLogger()
	}
	// When the caller passes a nil store, default to an FSStore rooted
	// at cfg.OutputDir, preserving the historical on-disk behavior.
	if store == nil {
		store = output.NewFSStore(cfg.OutputDir, cfg.Refetch)
	}
	start := time.Now()

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 3
	}

	// Build the crawl context. Apply time cap if configured.
	crawlCtx, cancelCrawl := context.WithCancel(ctx)
	defer cancelCrawl()
	if cfg.TimeCapSeconds > 0 {
		var timeCancelFn context.CancelFunc
		crawlCtx, timeCancelFn = context.WithTimeout(crawlCtx, time.Duration(cfg.TimeCapSeconds)*time.Second)
		defer timeCancelFn()
	}

	queueBuf := len(startKeys) + 1024
	if queueBuf < 64 {
		queueBuf = 64
	}

	c := &crawler{
		cfg:            cfg,
		fetcher:        fetcher,
		sink:           sink,
		store:          store,
		hier:           hier,
		devStatus:      devStatus,
		crawlCtx:       crawlCtx,
		cancelCrawl:    cancelCrawl,
		visited:        make(map[string]bool),
		seenPRs:        make(map[string]bool),
		summary:        Summary{FailedKeys: make(map[string]string)},
		queue:          make(chan workItem, queueBuf),
		logger:         logger,
		phaseCounts:    make(map[string]int),
		phaseDurations: make(map[string]time.Duration),
	}
	// Opt-in graph collector. Constructing here keeps the exported
	// CrawlWithEnrichers signature unchanged: the disk-export feature
	// is driven entirely by cfg.EmitGraph through the standard config
	// cascade. The in-memory variant [CrawlGraphWithEnrichers] passes
	// forceGraph=true so the collector is constructed regardless of
	// cfg.EmitGraph (and the disk write below is suppressed).
	c.forceGraph = forceGraph
	if cfg.EmitGraph || forceGraph {
		c.graph = graph.NewCollector()
	}
	// Tag every record emitted by the orchestrator with
	// trace_stream=stream so a consumer can distinguish orchestration
	// lines from the HTTP RoundTripper's response-stream lines (which
	// stamp trace_stream=response). The .With is harmless on a noop
	// handler and meaningful on any real handler installed by the
	// caller.
	c.logger = c.logger.With(trace.AttrTraceStream, trace.StreamStream)
	// rootSpan is created exactly once and identifies the whole run.
	// Every per-issue span is a child of rootSpan via [Span.Child].
	c.rootSpan = trace.NewRoot()

	// Seed the queue with start keys.
	c.mu.Lock()
	for _, k := range startKeys {
		c.enqueue(k, 0)
	}
	// If nothing was seeded, close immediately.
	if atomic.LoadInt64(&c.pending) == 0 {
		c.closeQueueLocked()
	}
	c.mu.Unlock()

	// Run-level span envelope: crawl.start opens the run, crawl.end
	// closes it with the structured summary counts and total
	// duration. Both lines carry run_id/span_id from rootSpan so
	// downstream consumers can group every per-issue line into this
	// run.
	c.logger.LogAttrs(crawlCtx, slog.LevelInfo, "crawl.start",
		slog.String(trace.AttrRunID, c.rootSpan.RunID),
		slog.String(trace.AttrSpanID, c.rootSpan.SpanID),
		slog.Int("concurrency", concurrency),
		slog.Int("seed_count", len(startKeys)),
	)
	defer func() {
		c.logger.LogAttrs(crawlCtx, slog.LevelInfo, "crawl.end",
			slog.String(trace.AttrRunID, c.rootSpan.RunID),
			slog.String(trace.AttrSpanID, c.rootSpan.SpanID),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int("fetched", c.summary.Fetched),
			slog.Int("skipped", c.summary.Skipped),
			slog.Int("stubbed", c.summary.Stubbed),
			slog.Int("failed", c.summary.Failed),
			slog.Int("cap_limited", c.summary.CapLimited),
		)
	}()

	// Closer goroutine: when the context is cancelled, drain the queue
	// and close it so workers exit their range loop.
	//
	// All draining and closing happens under c.mu to prevent races with
	// enqueue (which also holds c.mu when sending to the channel).
	go func() {
		<-crawlCtx.Done()
		c.mu.Lock()
		defer c.mu.Unlock()
		// Drain remaining items from the queue and mark them CapLimited.
		for {
			select {
			case item, ok := <-c.queue:
				if !ok {
					// Already closed.
					return
				}
				atomic.AddInt64(&c.pending, -1)
				c.summary.CapLimited++
				c.summary.CapLimitedKeys = append(c.summary.CapLimitedKeys, item.key)
			default:
				// Queue is empty; close it.
				c.closeQueueLocked()
				return
			}
		}
	}()

	// Start workers.
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range c.queue {
				fatalErr := c.processIssue(item)
				c.decrementPending()
				if fatalErr != nil {
					c.abortOnce.Do(func() {
						c.abortErr = fatalErr
						cancelCrawl()
					})
				}
			}
		}()
	}

	// Wait for all workers to finish.
	wg.Wait()

	// Sort deterministic slices.
	sort.Strings(c.summary.FetchedKeys)
	sort.Strings(c.summary.StubbedKeys)
	sort.Strings(c.summary.CapLimitedKeys)

	c.summary.Duration = time.Since(start)

	// Fold per-phase tallies into Summary and emit a single INFO
	// measurement line so a normal --log-level info run already shows
	// time attribution. The maps are lazy-allocated: when no phase ran
	// (e.g. a zero-seed crawl) the Summary's new fields stay nil so the
	// zero-value Summary{} continues to compare equal to a real
	// no-phase Summary.
	c.phaseMu.Lock()
	var totalAPI time.Duration
	if len(c.phaseCounts) > 0 {
		c.summary.APICallCounts = make(map[string]int, len(c.phaseCounts))
		c.summary.APITimeByPhase = make(map[string]time.Duration, len(c.phaseDurations))
		for k, v := range c.phaseCounts {
			c.summary.APICallCounts[k] = v
		}
		for k, v := range c.phaseDurations {
			c.summary.APITimeByPhase[k] = v
			totalAPI += v
		}
		c.summary.TotalAPITime = totalAPI
	}
	c.phaseMu.Unlock()

	c.logger.LogAttrs(crawlCtx, slog.LevelInfo, "crawl.measurement",
		slog.Int64("total_api_time_ms", totalAPI.Milliseconds()),
		slog.Int64("total_duration_ms", time.Since(start).Milliseconds()),
		slog.Any("call_counts", c.summary.APICallCounts),
		slog.Any("time_by_phase_ms", durationMsMap(c.summary.APITimeByPhase)),
	)

	// Best-effort graph export. A failure to write either file
	// degrades to a warn-level log line; the Markdown output is the
	// primary artifact and the crawl exit code must not change.
	//
	// The disk write is gated on cfg.EmitGraph (NOT c.forceGraph):
	// [CrawlGraphWithEnrichers] forces collection on so the model
	// can be returned in memory, but it never wants files written.
	if c.graph != nil && cfg.EmitGraph {
		writeGraphFiles(crawlCtx, c.logger, c.graph.Model(), cfg.OutputDir)
	}

	// Emit crawl summary event. The Message string is preserved verbatim
	// for text-only consumers (slog sink, RecordingSink dumps, etc.); the
	// new Summary field gives structured sinks (e.g. the grpcSink) typed
	// access to the same totals without re-parsing the message.
	sink.Emit(events.Event{
		Kind: events.KindCrawlSummary,
		Message: fmt.Sprintf(
			"crawl complete: fetched=%d skipped=%d stubbed=%d failed=%d capLimited=%d prs=%d duration=%s",
			c.summary.Fetched, c.summary.Skipped, c.summary.Stubbed,
			c.summary.Failed, c.summary.CapLimited, c.summary.PRsFound,
			c.summary.Duration,
		),
		Timestamp: time.Now(),
		Summary: &events.CrawlSummary{
			Fetched:        c.summary.Fetched,
			Skipped:        c.summary.Skipped,
			Stubbed:        c.summary.Stubbed,
			Failed:         c.summary.Failed,
			CapLimited:     c.summary.CapLimited,
			PRsFound:       c.summary.PRsFound,
			FetchedKeys:    c.summary.FetchedKeys,
			StubbedKeys:    c.summary.StubbedKeys,
			FailedKeys:     c.summary.FailedKeys,
			CapLimitedKeys: c.summary.CapLimitedKeys,
			Duration:       c.summary.Duration,
		},
	})

	// In the in-memory variant the caller consumes the graph via the
	// returned [graph.Model]; in the file variant we return the zero
	// model so the second return is meaningless to callers of the
	// thin wrapper [CrawlWithEnrichers].
	var model graph.Model
	if c.graph != nil && c.forceGraph {
		model = c.graph.Model()
	}
	return c.summary, model, c.abortErr
}

// hasAnyDevStatusEntity reports whether d carries at least one entity
// across any of the five lists. Used by the Dev Status partial-
// failure event-message split: an error alongside non-empty data is a
// "partial" failure; an error alongside empty data is a "no entities
// discovered" failure. Either way the issue itself was rendered, so
// the distinction is purely informational for operator logs.
func hasAnyDevStatusEntity(d parse.DevStatusData) bool {
	return len(d.PullRequests) > 0 ||
		len(d.Branches) > 0 ||
		len(d.Commits) > 0 ||
		len(d.Repositories) > 0 ||
		len(d.Builds) > 0
}

// indexExists reports whether <outputDir>/<key>/index.md exists on disk.
// It is used as a pre-fetch skip-if-exists probe.
func indexExists(outputDir, key string) bool {
	path := filepath.Join(outputDir, key, "index.md")
	_, err := os.Stat(path)
	return err == nil
}

// refsToOutbound converts a slice of extract.Reference values to the
// render.OutboundRef slice that render.RenderOutbound expects.
// The Kind string labels are:
//
//	classify.KindJiraKey  → "jira"
//	classify.KindJiraURL  → "jira"
//	classify.KindGitHubPR → "github-pr"
//	classify.KindExternal → "external"
func refsToOutbound(refs []extract.Reference) []render.OutboundRef {
	out := make([]render.OutboundRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, render.OutboundRef{
			Kind:     outboundKind(r.Kind),
			IssueKey: r.IssueKey,
			URL:      r.URL,
			Text:     r.Text,
			Owner:    r.ClassifyResult.Owner,
			Repo:     r.ClassifyResult.Repo,
			PRNumber: r.ClassifyResult.PRNumber,
		})
	}
	return out
}

// outboundKind maps a classify.Kind to the string label expected by
// render.OutboundRef.Kind.
func outboundKind(k classify.Kind) string {
	switch k {
	case classify.KindJiraKey, classify.KindJiraURL:
		return "jira"
	case classify.KindGitHubPR:
		return "github-pr"
	default:
		return "external"
	}
}

// writeGraphFiles persists graph.json and graph.d2 at outputDir.
// Failures are reported via logger at warn level and intentionally
// swallowed: the per-issue Markdown is the primary crawl artifact, so
// a graph-export problem must never fail the run.
func writeGraphFiles(ctx context.Context, logger *slog.Logger, m graph.Model, outputDir string) {
	if outputDir == "" {
		logger.LogAttrs(ctx, slog.LevelWarn, "graph.skipped",
			slog.String("reason", "empty output_dir"))
		return
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "graph.write_failed",
			slog.String("path", outputDir),
			slog.String("error", err.Error()))
		return
	}

	jsonBytes, err := graph.RenderJSON(m)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "graph.render_failed",
			slog.String("file", "graph.json"),
			slog.String("error", err.Error()))
		return
	}
	jsonPath := filepath.Join(outputDir, "graph.json")
	if err := os.WriteFile(jsonPath, jsonBytes, 0o644); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "graph.write_failed",
			slog.String("path", jsonPath),
			slog.String("error", err.Error()))
		return
	}

	d2Src, err := graph.RenderD2(m)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "graph.render_failed",
			slog.String("file", "graph.d2"),
			slog.String("error", err.Error()))
		return
	}
	d2Path := filepath.Join(outputDir, "graph.d2")
	if err := os.WriteFile(d2Path, []byte(d2Src), 0o644); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "graph.write_failed",
			slog.String("path", d2Path),
			slog.String("error", err.Error()))
		return
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "graph.written",
		slog.String("json", jsonPath),
		slog.String("d2", d2Path),
		slog.Int("nodes", len(m.Nodes)),
		slog.Int("edges", len(m.Edges)),
	)
}
