package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neumachen/gojira"
)

// ---------------------------------------------------------------------------
// Tool input shapes (JSON schemas are SDK-derived via reflection)
// ---------------------------------------------------------------------------

type classifyIn struct {
	Input    string `json:"input" jsonschema:"the bare issue key or URL to classify"`
	JiraSite string `json:"jira_site,omitempty" jsonschema:"the Jira Cloud base URL; defaults to the server's configured site"`
}

type getIssueIn struct {
	Key string `json:"key" jsonschema:"the Jira issue key (e.g. PROJ-1)"`
}

type crawlIn struct {
	StartKeys []string `json:"start_keys" jsonschema:"one or more Jira issue keys to seed the crawl"`
}

type getGraphIn struct {
	StartKeys []string `json:"start_keys" jsonschema:"one or more Jira issue keys to seed the graph crawl"`
}

type listTransitionsIn struct {
	Key string `json:"key" jsonschema:"the Jira issue key whose available transitions to list"`
}

type createIssueIn struct {
	Project       string         `json:"project" jsonschema:"target Jira project key"`
	IssueType     string         `json:"issue_type" jsonschema:"Jira issue type name (e.g. Task, Story, Bug)"`
	Summary       string         `json:"summary" jsonschema:"the new issue summary"`
	Description   string         `json:"description,omitempty" jsonschema:"plain-text description (server converts to ADF)"`
	Assignee      string         `json:"assignee,omitempty" jsonschema:"assignee accountId"`
	Labels        []string       `json:"labels,omitempty" jsonschema:"labels to apply"`
	ParentKey     string         `json:"parent_key,omitempty" jsonschema:"parent issue key for hierarchy children"`
	FixVersions   []string       `json:"fix_versions,omitempty" jsonschema:"fix versions by name"`
	FixVersionIDs []string       `json:"fix_version_ids,omitempty" jsonschema:"fix versions by id"`
	RawFields     map[string]any `json:"raw_fields,omitempty" jsonschema:"additional Jira fields by id (e.g. customfield_*)"`
}

type updateIssueIn struct {
	Key           string         `json:"key" jsonschema:"the issue key to update"`
	Summary       string         `json:"summary,omitempty"`
	Description   string         `json:"description,omitempty"`
	Assignee      string         `json:"assignee,omitempty"`
	Labels        []string       `json:"labels,omitempty"`
	FixVersions   []string       `json:"fix_versions,omitempty" jsonschema:"replacement fix versions by name (set-replace)"`
	FixVersionIDs []string       `json:"fix_version_ids,omitempty" jsonschema:"replacement fix versions by id (set-replace)"`
	RawFields     map[string]any `json:"raw_fields,omitempty"`
}

type listVersionsIn struct {
	Project string `json:"project" jsonschema:"the Jira project key or id whose versions to list"`
}

type createVersionIn struct {
	Project     string `json:"project" jsonschema:"target Jira project key or numeric id"`
	Name        string `json:"name" jsonschema:"the new version name"`
	Description string `json:"description,omitempty"`
	Released    *bool  `json:"released,omitempty" jsonschema:"whether the version is released"`
	Archived    *bool  `json:"archived,omitempty" jsonschema:"whether the version is archived"`
	ReleaseDate string `json:"release_date,omitempty" jsonschema:"release date (yyyy-mm-dd)"`
	StartDate   string `json:"start_date,omitempty" jsonschema:"start date (yyyy-mm-dd)"`
}

type updateVersionIn struct {
	ID          string `json:"id" jsonschema:"the numeric version id to update"`
	Description string `json:"description,omitempty"`
	Released    *bool  `json:"released,omitempty"`
	Archived    *bool  `json:"archived,omitempty"`
	ReleaseDate string `json:"release_date,omitempty" jsonschema:"release date (yyyy-mm-dd)"`
	StartDate   string `json:"start_date,omitempty" jsonschema:"start date (yyyy-mm-dd)"`
}

type addCommentIn struct {
	Key  string `json:"key"`
	Text string `json:"text" jsonschema:"comment body (plain text)"`
}

type transitionIssueIn struct {
	Key          string `json:"key"`
	TransitionID string `json:"transition_id,omitempty" jsonschema:"workflow transition id (pass this OR to_status, not both)"`
	ToStatus     string `json:"to_status,omitempty" jsonschema:"target status name to resolve server-side"`
	CommentText  string `json:"comment_text,omitempty" jsonschema:"optional comment posted alongside the transition"`
}

// ---------------------------------------------------------------------------
// registerTools — the single registration entry point
// ---------------------------------------------------------------------------

// registerTools registers the gojira MCP tool set onto server. The
// read tools (classify, get_issue, crawl, get_graph, list_transitions)
// are registered unconditionally; the mutating tools (create_issue,
// update_issue, add_comment, transition_issue) are registered only
// when allowWrites is true. Disabled write tools are ABSENT from
// tools/list — they do not appear as stubs that error on call.
func registerTools(server *mcpsdk.Server, b mcpBackend, allowWrites bool) {
	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "classify",
			Description: "Classify a string as a Jira issue key, Jira URL, GitHub PR URL, or external URL.",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in classifyIn) (*mcpsdk.CallToolResult, any, error) {
			res, err := b.Classify(ctx, in.Input, in.JiraSite)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(res), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "get_issue",
			Description: "Fetch a single Jira issue with its outbound references.",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in getIssueIn) (*mcpsdk.CallToolResult, any, error) {
			issue, refs, err := b.GetIssue(ctx, in.Key)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(map[string]any{"issue": issue, "references": refs}), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "crawl",
			Description: "Crawl Jira issues from one or more start keys; returns a summary on completion. Progress notifications are sent per fetched issue when the client supplies a progress token.",
		},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, in crawlIn) (*mcpsdk.CallToolResult, any, error) {
			progress := progressFromRequest(ctx, req)
			summary, err := b.Crawl(ctx, in.StartKeys, progress)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(summary), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "get_graph",
			Description: "Crawl Jira issues in-memory and return the discovered issue graph as {nodes, edges}; no files are written.",
		},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, in getGraphIn) (*mcpsdk.CallToolResult, any, error) {
			progress := progressFromRequest(ctx, req)
			summary, model, err := b.GetGraph(ctx, in.StartKeys, progress)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(map[string]any{"summary": summary, "graph": model}), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "list_transitions",
			Description: "List the workflow transitions currently available for an issue.",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in listTransitionsIn) (*mcpsdk.CallToolResult, any, error) {
			ts, err := b.ListTransitions(ctx, in.Key)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(map[string]any{"transitions": ts}), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "list_versions",
			Description: "List the versions (releases) for a Jira project (id or key).",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in listVersionsIn) (*mcpsdk.CallToolResult, any, error) {
			vs, err := b.ListVersions(ctx, in.Project)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(map[string]any{"versions": vs}), nil, nil
		})

	if !allowWrites {
		return
	}

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "create_issue",
			Description: "Create a new Jira issue.",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in createIssueIn) (*mcpsdk.CallToolResult, any, error) {
			res, err := b.CreateIssue(ctx, in.Project, in.IssueType, CreateIssueFields{
				Summary:       in.Summary,
				Description:   in.Description,
				Assignee:      in.Assignee,
				Labels:        in.Labels,
				ParentKey:     in.ParentKey,
				FixVersions:   in.FixVersions,
				FixVersionIDs: in.FixVersionIDs,
				RawFields:     in.RawFields,
			})
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(res), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "update_issue",
			Description: "Update fields on an existing Jira issue.",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in updateIssueIn) (*mcpsdk.CallToolResult, any, error) {
			err := b.UpdateIssue(ctx, in.Key, UpdateIssueFields{
				Summary:       in.Summary,
				Description:   in.Description,
				Assignee:      in.Assignee,
				Labels:        in.Labels,
				FixVersions:   in.FixVersions,
				FixVersionIDs: in.FixVersionIDs,
				RawFields:     in.RawFields,
			})
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(map[string]any{"ok": true, "key": in.Key}), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "add_comment",
			Description: "Add a plain-text comment to a Jira issue.",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in addCommentIn) (*mcpsdk.CallToolResult, any, error) {
			c, err := b.AddComment(ctx, in.Key, in.Text)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(c), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "transition_issue",
			Description: "Move an issue through a workflow transition by id or by target status name.",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in transitionIssueIn) (*mcpsdk.CallToolResult, any, error) {
			err := b.TransitionIssue(ctx, in.Key, in.TransitionID, in.ToStatus, TransitionFields{
				CommentText: in.CommentText,
			})
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(map[string]any{"ok": true, "key": in.Key}), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "create_version",
			Description: "Create a new Jira project version (release).",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in createVersionIn) (*mcpsdk.CallToolResult, any, error) {
			v, err := b.CreateVersion(ctx, in.Name, in.Project, VersionFields{
				Description: in.Description,
				Released:    in.Released,
				Archived:    in.Archived,
				ReleaseDate: in.ReleaseDate,
				StartDate:   in.StartDate,
			})
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(v), nil, nil
		})

	mcpsdk.AddTool(server,
		&mcpsdk.Tool{
			Name:        "update_version",
			Description: "Edit an existing Jira project version (release).",
		},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in updateVersionIn) (*mcpsdk.CallToolResult, any, error) {
			v, err := b.UpdateVersion(ctx, in.ID, VersionFields{
				Description: in.Description,
				Released:    in.Released,
				Archived:    in.Archived,
				ReleaseDate: in.ReleaseDate,
				StartDate:   in.StartDate,
			})
			if err != nil {
				return errorResult(err), nil, nil
			}
			return jsonResult(v), nil, nil
		})
}

// ---------------------------------------------------------------------------
// helpers — result construction + progress adapter
// ---------------------------------------------------------------------------

// jsonResult returns a CallToolResult whose Content carries a single
// text block holding the indented JSON of v. The text block keeps the
// result readable in hosts that do not render structured_content;
// production callers may also rely on the SDK's auto-populated
// StructuredContent when the handler returns a typed Out.
func jsonResult(v any) *mcpsdk.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResult(fmt.Errorf("marshal result: %w", err))
	}
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: string(b)},
		},
	}
}

// errorResult returns a CallToolResult with IsError=true carrying the
// gojira sentinel meaning in plain text. The SDK's ToolHandlerFor
// would auto-set IsError if we returned the error directly, but
// explicit packaging here lets us preserve the precise wording
// (including any wrapped *client.APIError field details) that
// downstream LLMs need to self-correct.
func errorResult(err error) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: classifyError(err)},
		},
	}
}

// classifyError prefixes the sentinel category onto the error
// message so the host's LLM can route on it without parsing.
// Mapping mirrors toStatusError in internal/grpc but stays
// SDK-side (the gRPC bridge surfaces server-side status errors
// directly; we still want a uniform classification on the MCP
// boundary).
func classifyError(err error) string {
	switch {
	case errors.Is(err, gojira.ErrUnauthorized):
		return "unauthorized: " + err.Error()
	case errors.Is(err, gojira.ErrForbidden):
		return "forbidden: " + err.Error()
	case errors.Is(err, gojira.ErrNotFound):
		return "not_found: " + err.Error()
	case errors.Is(err, gojira.ErrRateLimited):
		return "rate_limited: " + err.Error()
	case errors.Is(err, gojira.ErrBadRequest):
		return "bad_request: " + err.Error()
	case errors.Is(err, gojira.ErrConflict):
		return "conflict: " + err.Error()
	default:
		return err.Error()
	}
}

// progressFromRequest returns a [ProgressFn] suitable for forwarding
// crawl progress to the MCP client. When the request carries a
// progress token, each invocation sends a progress notification on
// the request's session; otherwise the returned function is a
// silent no-op so the backend code path stays identical.
func progressFromRequest(ctx context.Context, req *mcpsdk.CallToolRequest) ProgressFn {
	if req == nil || req.Params == nil {
		return noopProgress
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return noopProgress
	}
	session := req.Session
	if session == nil {
		return noopProgress
	}
	return func(done, total int, message string) {
		_ = session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      float64(done),
			Total:         float64(total),
			Message:       message,
		})
	}
}
