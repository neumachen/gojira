// Package mcp implements the gojira MCP server: a backend
// interface with two implementations (facade-backed "self" mode and
// gRPC-bridge "bridge" mode), a shared tool-registration layer that
// gates mutating tools behind allow_writes, and the stdio bootstrap
// the `gojira mcp` command consumes.
//
// The package is internal to gojira and stays focused on the MCP
// adapter; domain logic lives in the gojira facade or behind the
// gRPC client and is not reimplemented here.
package mcp

import (
	"context"

	"github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
)

// ProgressFn is the callback the crawl-style tools use to surface
// progress through the MCP transport. done is the running fetched
// count; total is "0" when unknown (the crawl orchestrator does not
// expose a forecasted total). message is a human-readable hint
// suitable for a host UI status line.
type ProgressFn func(done, total int, message string)

// noopProgress is the [ProgressFn] used when the MCP client did not
// supply a progress token. The backend still calls progress so the
// crawl path is identical in both cases; the no-op simply drops the
// event.
func noopProgress(int, int, string) {}

// CreateIssueFields carries the primitive inputs a create_issue MCP
// tool collects from the client. Using primitive fields (rather than
// the facade's typed [client.CreateOption] slice) keeps the bridge
// implementation simple — it builds a proto request from the same
// shape — while still covering the documented Phase-A surface.
//
// RawFields lets callers set arbitrary Jira custom fields the
// typed flags do not cover; it is mapped onto client.WithField in
// the facade backend.
type CreateIssueFields struct {
	Summary     string
	Description string
	Assignee    string
	Labels      []string
	ParentKey   string
	RawFields   map[string]any
}

// UpdateIssueFields is the update_issue analog of [CreateIssueFields].
// Each field is applied only when non-empty / non-nil so unset values
// do not clobber existing Jira state with empty strings.
type UpdateIssueFields struct {
	Summary     string
	Description string
	Assignee    string
	Labels      []string
	RawFields   map[string]any
}

// TransitionFields carries the optional comment that the
// transition_issue tool can post alongside the workflow move.
type TransitionFields struct {
	CommentText string
}

// mcpBackend abstracts the two execution backends behind a single
// interface so the tool-registration code in [registerTools] never
// branches on mode.
//
// Method signatures intentionally avoid leaking facade-only types
// (e.g. [client.CreateOption]) into method bodies — the bridge
// implementation cannot build option closures out of arbitrary
// inputs after the fact. Primitive request structs ([CreateIssueFields],
// [UpdateIssueFields], [TransitionFields]) cross both paths
// uniformly.
type mcpBackend interface {
	Classify(ctx context.Context, input, jiraSite string) (classify.Result, error)
	GetIssue(ctx context.Context, key string) (parse.Issue, []extract.Reference, error)
	Crawl(ctx context.Context, startKeys []string, progress ProgressFn) (gojira.Summary, error)
	GetGraph(ctx context.Context, startKeys []string, progress ProgressFn) (gojira.Summary, gojira.GraphModel, error)

	CreateIssue(ctx context.Context, project, issueType string, fields CreateIssueFields) (client.CreatedIssue, error)
	UpdateIssue(ctx context.Context, key string, fields UpdateIssueFields) error
	AddComment(ctx context.Context, key, text string) (client.Comment, error)
	ListTransitions(ctx context.Context, key string) ([]client.Transition, error)
	// TransitionIssue takes EITHER transitionID OR toStatus (exactly
	// one non-empty); when both are empty the implementation returns
	// an error. When toStatus is supplied the facade resolves it via
	// [gojira.TransitionIssueByStatus]; the bridge forwards the
	// equivalent gRPC request and lets the server resolve.
	TransitionIssue(ctx context.Context, key, transitionID, toStatus string, fields TransitionFields) error
}
