package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
)

// facadeBackend implements [mcpBackend] in "self" mode by delegating
// every call to the gojira facade. It owns nothing beyond its
// configuration; each method is a thin shell around the matching
// facade entry point.
type facadeBackend struct {
	cfg gojira.Config
}

// NewFacadeBackend constructs the self-mode backend. cfg must be the
// fully resolved [gojira.Config] returned by the standard cascade.
func NewFacadeBackend(cfg gojira.Config) *facadeBackend {
	return &facadeBackend{cfg: cfg}
}

// Classify wraps gojira.Classify. The facade signature does not take
// a context; one is still required by the interface for symmetry
// with the bridge backend (which makes an RPC).
func (f *facadeBackend) Classify(_ context.Context, input, jiraSite string) (classify.Result, error) {
	if jiraSite == "" {
		jiraSite = f.cfg.Site
	}
	return gojira.Classify(input, jiraSite), nil
}

func (f *facadeBackend) GetIssue(ctx context.Context, key string) (parse.Issue, []extract.Reference, error) {
	return gojira.GetIssue(ctx, f.cfg, key)
}

// Crawl runs gojira.CrawlWithLogger with a [progressSink] adapter so
// every fetched issue invokes progress(). The logger is wired to
// io.Discard — the cmd layer owns the real logger and the MCP path
// must keep stdout pure (no log records leak into the protocol
// stream from this backend even if the cmd-level wiring slips).
func (f *facadeBackend) Crawl(ctx context.Context, startKeys []string, progress ProgressFn) (gojira.Summary, error) {
	if progress == nil {
		progress = noopProgress
	}
	sink := &progressSink{fn: progress}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return gojira.CrawlWithLogger(ctx, f.cfg, startKeys, sink, logger)
}

// GetGraph runs gojira.CrawlGraph with the same progressSink adapter
// and returns the in-memory [gojira.GraphModel] without touching
// disk (CrawlGraph forces graph collection on and suppresses the
// graph.json / graph.d2 disk export — that is its whole purpose).
func (f *facadeBackend) GetGraph(ctx context.Context, startKeys []string, progress ProgressFn) (gojira.Summary, gojira.GraphModel, error) {
	if progress == nil {
		progress = noopProgress
	}
	sink := &progressSink{fn: progress}
	return gojira.CrawlGraph(ctx, f.cfg, startKeys, sink)
}

func (f *facadeBackend) CreateIssue(ctx context.Context, project, issueType string, fields CreateIssueFields) (client.CreatedIssue, error) {
	opts := make([]client.CreateOption, 0, 6)
	if fields.Summary != "" {
		opts = append(opts, client.WithSummary(fields.Summary))
	}
	if fields.Description != "" {
		opts = append(opts, client.WithDescriptionText(fields.Description))
	}
	if fields.Assignee != "" {
		opts = append(opts, client.WithAssigneeAccountID(fields.Assignee))
	}
	if len(fields.Labels) > 0 {
		opts = append(opts, client.WithLabels(fields.Labels...))
	}
	if fields.ParentKey != "" {
		opts = append(opts, client.WithParent(fields.ParentKey))
	}
	for id, v := range fields.RawFields {
		opts = append(opts, client.WithField(id, v))
	}
	return gojira.CreateIssue(ctx, f.cfg, project, issueType, opts...)
}

func (f *facadeBackend) UpdateIssue(ctx context.Context, key string, fields UpdateIssueFields) error {
	opts := make([]client.UpdateOption, 0, 5)
	if fields.Summary != "" {
		opts = append(opts, client.WithSummaryUpdate(fields.Summary))
	}
	if fields.Description != "" {
		opts = append(opts, client.WithDescriptionTextUpdate(fields.Description))
	}
	if fields.Assignee != "" {
		opts = append(opts, client.WithAssigneeAccountIDUpdate(fields.Assignee))
	}
	if len(fields.Labels) > 0 {
		opts = append(opts, client.WithLabelsUpdate(fields.Labels...))
	}
	for id, v := range fields.RawFields {
		opts = append(opts, client.WithFieldUpdate(id, v))
	}
	if len(opts) == 0 {
		return errors.New("update_issue: nothing to update (no fields supplied)")
	}
	return gojira.UpdateIssue(ctx, f.cfg, key, opts...)
}

func (f *facadeBackend) AddComment(ctx context.Context, key, text string) (client.Comment, error) {
	if text == "" {
		return client.Comment{}, errors.New("add_comment: text is required")
	}
	return gojira.AddComment(ctx, f.cfg, key, client.WithCommentText(text))
}

func (f *facadeBackend) ListTransitions(ctx context.Context, key string) ([]client.Transition, error) {
	return gojira.ListTransitions(ctx, f.cfg, key)
}

func (f *facadeBackend) TransitionIssue(ctx context.Context, key, transitionID, toStatus string, fields TransitionFields) error {
	switch {
	case transitionID != "" && toStatus != "":
		return errors.New("transition_issue: pass exactly one of transition_id or to_status, not both")
	case transitionID == "" && toStatus == "":
		return errors.New("transition_issue: pass exactly one of transition_id or to_status")
	}
	opts := []client.TransitionOption{}
	if fields.CommentText != "" {
		opts = append(opts, client.WithTransitionCommentText(fields.CommentText))
	}
	if transitionID != "" {
		return gojira.TransitionIssue(ctx, f.cfg, key, transitionID, opts...)
	}
	return gojira.TransitionIssueByStatus(ctx, f.cfg, key, toStatus, opts...)
}

// ---------------------------------------------------------------------------
// progressSink — events.Sink adapter that drives a ProgressFn
// ---------------------------------------------------------------------------

// progressSink converts gojira's [events.KindIssueFetched] events into
// [ProgressFn] callbacks suitable for forwarding to an MCP client.
// The orchestrator does not surface a forecasted total, so done is
// also passed as the total argument — consistent with the
// [ProgressFn] contract that "0 means unknown" and showing the
// running count as both helps host UIs that prefer a numerator-
// denominator format.
//
// Emit is invoked from worker goroutines inside the crawl
// orchestrator; the atomic counter keeps the path lock-free and
// safe for concurrent fan-out.
type progressSink struct {
	fn   ProgressFn
	done atomic.Int32
}

// Emit implements events.Sink.
func (s *progressSink) Emit(e events.Event) {
	if e.Kind != events.KindIssueFetched {
		return
	}
	n := int(s.done.Add(1))
	msg := fmt.Sprintf("fetched %s", e.IssueKey)
	s.fn(n, n, msg)
}

// Compile-time assertions: progressSink IS the events.Sink interface
// the facade expects, and the matching alias on the public surface
// keeps the swap painless.
var (
	_ events.Sink = (*progressSink)(nil)
	_ gojira.Sink = (*progressSink)(nil)
)
