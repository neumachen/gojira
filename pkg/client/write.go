package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/adf"
)

// ---------------------------------------------------------------------------
// CreateIssue — POST /rest/api/3/issue
// ---------------------------------------------------------------------------

// CreatedIssue is the response payload from a successful [Client.CreateIssue]
// call. Self is the canonical REST URL Jira returns for the new issue.
type CreatedIssue struct {
	Key  string
	ID   string
	Self string
}

// CreateIssue creates a new Jira issue. The project key and issue-type name
// are required and explicit (signature-honesty: behavior-affecting inputs
// are not hidden in options). Everything else — summary, description,
// assignee, labels, parent, custom fields — is supplied via the
// [CreateOption] functional-option set in fields.go, so adding a new Jira
// field will not widen this signature.
//
// On success Jira returns 201 with {id, key, self}; the parsed struct is
// returned. On 400/409 the error is an [*APIError] that still satisfies
// errors.Is against [ErrBadRequest] / [ErrConflict] via Unwrap.
func (c *Client) CreateIssue(ctx context.Context, project, issueType string, opts ...CreateOption) (CreatedIssue, error) {
	if project == "" {
		return CreatedIssue{}, errext.Errorf("client: CreateIssue: project is required")
	}
	if issueType == "" {
		return CreatedIssue{}, errext.Errorf("client: CreateIssue: issueType is required")
	}

	body, err := RenderCreateBody(project, issueType, opts...)
	if err != nil {
		return CreatedIssue{}, err
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "issue").String()
	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPostJSON(ctx, endpoint, body)
	})
	if err != nil {
		return CreatedIssue{}, err
	}

	var resp struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Self string `json:"self"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return CreatedIssue{}, errext.Errorf("client: unmarshal CreateIssue response: %w", err)
	}
	return CreatedIssue{Key: resp.Key, ID: resp.ID, Self: resp.Self}, nil
}

// ---------------------------------------------------------------------------
// UpdateIssue — PUT /rest/api/3/issue/<key>
// ---------------------------------------------------------------------------

// UpdateIssue edits fields on an existing Jira issue identified by key.
// Field selection mirrors [CreateIssue] via the [UpdateOption] set, which
// shares the underlying builder so options stay in lockstep. Jira responds
// with 204 No Content on success; the method therefore returns no value
// beyond the error.
func (c *Client) UpdateIssue(ctx context.Context, key string, opts ...UpdateOption) error {
	if key == "" {
		return errext.Errorf("client: UpdateIssue: key is required")
	}

	body, err := RenderUpdateBody(opts...)
	if err != nil {
		return err
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "issue", key).String()
	_, err = c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPutJSON(ctx, endpoint, body)
	})
	return err
}

// ---------------------------------------------------------------------------
// AddComment — POST /rest/api/3/issue/<key>/comment
// ---------------------------------------------------------------------------

// Comment is the response payload from a successful [Client.AddComment] call,
// carrying the Jira-assigned id, author identity, and creation timestamp
// (left as the raw ISO-8601 string Jira returns).
type Comment struct {
	ID                string
	AuthorAccountID   string
	AuthorDisplayName string
	Created           string
}

// CommentOption customizes an [Client.AddComment] request body. The body is
// always an ADF document; [WithCommentText] converts plain text, while
// [WithCommentADF] lets callers supply a rich pre-built document.
type CommentOption func(*commentBody)

// commentBody holds the ADF body for an in-flight comment request. It is
// unexported because callers compose it only via the option constructors.
type commentBody struct {
	body json.RawMessage
}

// WithCommentText sets the comment body from plain text, converting it to
// ADF via [adf.BuildParagraphDoc].
func WithCommentText(text string) CommentOption {
	doc := adf.BuildParagraphDoc(text)
	return func(b *commentBody) { b.body = doc }
}

// WithCommentADF sets the comment body from a caller-supplied ADF document.
// Use this for rich-text content (lists, code blocks, links) the plain-text
// helper cannot express.
func WithCommentADF(doc json.RawMessage) CommentOption {
	return func(b *commentBody) { b.body = doc }
}

// AddComment appends a comment to an existing Jira issue. When no option
// supplies a body the default is an empty ADF paragraph; Jira accepts this
// as a deliberately empty comment.
func (c *Client) AddComment(ctx context.Context, key string, opts ...CommentOption) (Comment, error) {
	if key == "" {
		return Comment{}, errext.Errorf("client: AddComment: key is required")
	}

	cb := &commentBody{}
	for _, opt := range opts {
		opt(cb)
	}
	if len(cb.body) == 0 {
		cb.body = adf.BuildParagraphDoc("")
	}

	// Use json.RawMessage in the request envelope so the ADF doc is
	// embedded as a JSON object, NOT a double-encoded string.
	req := struct {
		Body json.RawMessage `json:"body"`
	}{Body: cb.body}
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return Comment{}, errext.Errorf("client: marshal AddComment body: %w", err)
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "issue", key, "comment").String()
	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPostJSON(ctx, endpoint, bodyBytes)
	})
	if err != nil {
		return Comment{}, err
	}

	var resp struct {
		ID     string `json:"id"`
		Author struct {
			AccountID   string `json:"accountId"`
			DisplayName string `json:"displayName"`
		} `json:"author"`
		Created string `json:"created"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Comment{}, errext.Errorf("client: unmarshal AddComment response: %w", err)
	}
	return Comment{
		ID:                resp.ID,
		AuthorAccountID:   resp.Author.AccountID,
		AuthorDisplayName: resp.Author.DisplayName,
		Created:           resp.Created,
	}, nil
}

// ---------------------------------------------------------------------------
// ListTransitions + TransitionIssue
// ---------------------------------------------------------------------------

// Transition is one workflow transition available for an issue, projected
// from Jira's GET .../transitions response: id + display name + the name of
// the status the issue moves to when the transition is executed.
type Transition struct {
	ID       string
	Name     string
	ToStatus string
}

// ListTransitions fetches the workflow transitions currently available for
// the issue identified by key. The list is workflow-state dependent: Jira
// only returns transitions whose preconditions are met for the current
// state.
func (c *Client) ListTransitions(ctx context.Context, key string) ([]Transition, error) {
	if key == "" {
		return nil, errext.Errorf("client: ListTransitions: key is required")
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "issue", key, "transitions").String()
	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newGet(ctx, endpoint)
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, errext.Errorf("client: unmarshal ListTransitions response: %w", err)
	}

	out := make([]Transition, 0, len(resp.Transitions))
	for _, t := range resp.Transitions {
		out = append(out, Transition{ID: t.ID, Name: t.Name, ToStatus: t.To.Name})
	}
	return out, nil
}

// TransitionOption customizes a [Client.TransitionIssue] request body —
// either by merging additional fields/update ops onto the standard
// {"transition": {"id": "..."}} body, or by appending a comment via
// [WithTransitionCommentText]. It is layered on the same internal
// [fieldsBuilder] used by Create/Update so the marshalling logic does not
// diverge.
type TransitionOption func(*fieldsBuilder)

// WithTransitionField sets fields[fieldID]=value during a transition.
// Useful for transitions that require a resolution, a comment, or any
// custom field. Mirrors [WithField] but for the transition body shape.
func WithTransitionField(fieldID string, value any) TransitionOption {
	return func(b *fieldsBuilder) { b.setField(fieldID, value) }
}

// WithTransitionCommentText appends a comment-add op with an ADF body
// (built via [adf.BuildParagraphDoc]) to the transition's update map. The
// resulting body is {"transition":{"id":...}, "update":{"comment":[{"add":{"body":<ADF>}}]}}.
func WithTransitionCommentText(text string) TransitionOption {
	doc := adf.BuildParagraphDoc(text)
	return func(b *fieldsBuilder) {
		b.addUpdate("comment", "add", map[string]any{
			"body": doc,
		})
	}
}

// TransitionIssue moves an issue through the given workflow transition
// (identified by Jira's tenant/workflow-specific transition id). Options
// allow setting fields or appending a comment during the transition. Jira
// responds with 204 on success.
func (c *Client) TransitionIssue(ctx context.Context, key, transitionID string, opts ...TransitionOption) error {
	if key == "" {
		return errext.Errorf("client: TransitionIssue: key is required")
	}
	if transitionID == "" {
		return errext.Errorf("client: TransitionIssue: transitionID is required")
	}

	// Build the fields/update sub-objects via the shared builder; then
	// merge them under the transition-specific top-level envelope.
	b := newFieldsBuilder()
	for _, opt := range opts {
		opt(b)
	}
	merged := b.body() // {"fields":..., "update":...} omitting empties.
	merged["transition"] = map[string]any{"id": transitionID}

	bodyBytes, err := json.Marshal(merged)
	if err != nil {
		return errext.Errorf("client: marshal TransitionIssue body: %w", err)
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "issue", key, "transitions").String()
	_, err = c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPostJSON(ctx, endpoint, bodyBytes)
	})
	return err
}
