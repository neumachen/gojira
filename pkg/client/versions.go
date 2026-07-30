package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/neumachen/errext"
)

// ---------------------------------------------------------------------------
// Version — Jira Cloud REST v3 project version (release) management
// ---------------------------------------------------------------------------

// Version is the subset of a Jira Cloud project version (a "release") that
// gojira reads and writes. It mirrors the fields returned by the
// /rest/api/3/version endpoints. Versions are PROJECT-scoped: they are
// created against a numeric projectId and never carry an issue reference.
//
// The struct has no JSON tags on purpose: encoding/json matches the
// exported field names against Jira's camelCase keys case-insensitively
// (ProjectID<-projectId, ReleaseDate<-releaseDate, Self<-self, ...), so a
// direct Unmarshal into a Version yields the mapping without a parallel
// wire type. Jira returns projectId as a JSON number and id as a string.
type Version struct {
	ID          string
	Name        string
	ProjectID   int
	Description string
	Released    bool
	Archived    bool
	ReleaseDate string // yyyy-mm-dd as returned
	StartDate   string
	Self        string
}

// versionBody accumulates the genuinely-optional fields of a version
// create/update request. Required, behavior-affecting inputs (name and
// project) are EXPLICIT positional params on RenderCreateVersionBody /
// CreateVersion and are never carried here — signature honesty.
//
// Every optional field is a pointer so an unset option leaves the key out
// of the rendered body entirely (never blanks an existing value on
// update). err records the first date-validation failure so it can be
// surfaced from the render path rather than panicking at option-apply
// time.
type versionBody struct {
	description *string
	released    *bool
	archived    *bool
	releaseDate *string
	startDate   *string
	err         error
}

// VersionOption customizes a version create/update request body. Options
// cover ONLY the optional fields; name and project are positional.
type VersionOption func(*versionBody)

// WithVersionDescription sets the version description.
func WithVersionDescription(s string) VersionOption {
	return func(b *versionBody) { b.description = &s }
}

// WithVersionReleased sets the released flag.
func WithVersionReleased(v bool) VersionOption {
	return func(b *versionBody) { b.released = &v }
}

// WithVersionArchived sets the archived flag.
func WithVersionArchived(v bool) VersionOption {
	return func(b *versionBody) { b.archived = &v }
}

// WithVersionReleaseDate sets the release date. The value must be a
// yyyy-mm-dd calendar date; an invalid value is recorded and surfaced as
// an error from RenderCreateVersionBody / RenderUpdateVersionBody (and
// therefore CreateVersion / UpdateVersion) rather than panicking.
func WithVersionReleaseDate(v string) VersionOption {
	return func(b *versionBody) {
		if err := validateVersionDate("releaseDate", v); err != nil {
			if b.err == nil {
				b.err = err
			}
			return
		}
		b.releaseDate = &v
	}
}

// WithVersionStartDate sets the start date. Same yyyy-mm-dd validation as
// [WithVersionReleaseDate].
func WithVersionStartDate(v string) VersionOption {
	return func(b *versionBody) {
		if err := validateVersionDate("startDate", v); err != nil {
			if b.err == nil {
				b.err = err
			}
			return
		}
		b.startDate = &v
	}
}

// validateVersionDate reports whether v is a yyyy-mm-dd calendar date.
func validateVersionDate(field, v string) error {
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return errext.Errorf("client: invalid %s %q: expected yyyy-mm-dd", field, v)
	}
	return nil
}

// isAllDigits reports whether s is non-empty and consists solely of ASCII
// digits — the discriminator between a numeric project id ("10000", used
// directly as projectId) and a project key ("EXAMPLE", resolved to a
// numeric id by CreateVersion before the render path is reached).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// RenderCreateVersionBody assembles the JSON body for [Client.CreateVersion]
// from the required name + project plus zero or more options. It performs
// NO network I/O — it is the pure builder that also backs the CLI/facade
// dry-run affordance (Option A: network-free dry-run).
//
// name and project are required. project may be a numeric id string
// ("10000"), which is emitted as an integer projectId, or a project key
// ("EXAMPLE"), in which case projectId is OMITTED from the body. When a
// key is supplied the caller (CreateVersion) resolves it to a numeric id
// BEFORE calling this function; RenderCreateVersionBody itself never
// resolves keys.
//
// Body shape (optional keys included only when set):
//
//	{"name":<name>,"projectId":<int>,"description":...,"released":...,
//	 "archived":...,"releaseDate":...,"startDate":...}
func RenderCreateVersionBody(name, project string, opts ...VersionOption) ([]byte, error) {
	if name == "" {
		return nil, errext.Errorf("client: RenderCreateVersionBody: name is required")
	}
	if project == "" {
		return nil, errext.Errorf("client: RenderCreateVersionBody: project is required")
	}

	b := &versionBody{}
	for _, opt := range opts {
		opt(b)
	}
	if b.err != nil {
		return nil, b.err
	}

	out := map[string]any{"name": name}
	if isAllDigits(project) {
		id, err := strconv.Atoi(project)
		if err != nil {
			return nil, errext.Errorf("client: RenderCreateVersionBody: parse projectId %q: %w", project, err)
		}
		// Emit as an integer so it marshals to a JSON number, matching
		// Jira's projectId contract.
		out["projectId"] = id
	}
	applyVersionOptionalFields(out, b)

	bz, err := json.Marshal(out)
	if err != nil {
		return nil, errext.Errorf("client: marshal create version body: %w", err)
	}
	return bz, nil
}

// RenderUpdateVersionBody assembles the JSON body for [Client.UpdateVersion]
// from zero or more options. With no options the result is "{}" — a no-op
// edit Jira accepts. It performs NO network I/O and backs the dry-run
// path. name/project are not part of an update (renaming/moving versions
// is out of scope); only the optional fields are emitted.
func RenderUpdateVersionBody(opts ...VersionOption) ([]byte, error) {
	b := &versionBody{}
	for _, opt := range opts {
		opt(b)
	}
	if b.err != nil {
		return nil, b.err
	}

	out := map[string]any{}
	applyVersionOptionalFields(out, b)

	bz, err := json.Marshal(out)
	if err != nil {
		return nil, errext.Errorf("client: marshal update version body: %w", err)
	}
	return bz, nil
}

// applyVersionOptionalFields writes the set optional fields of b into out.
// Shared by the create and update render paths so the key names live in
// exactly one place.
func applyVersionOptionalFields(out map[string]any, b *versionBody) {
	if b.description != nil {
		out["description"] = *b.description
	}
	if b.released != nil {
		out["released"] = *b.released
	}
	if b.archived != nil {
		out["archived"] = *b.archived
	}
	if b.releaseDate != nil {
		out["releaseDate"] = *b.releaseDate
	}
	if b.startDate != nil {
		out["startDate"] = *b.startDate
	}
}

// parseVersion unmarshals a single-version JSON body (the 201/200 response
// of create/update) into a [Version].
func parseVersion(raw []byte) (Version, error) {
	var v Version
	if err := json.Unmarshal(raw, &v); err != nil {
		return Version{}, errext.Errorf("client: unmarshal version response: %w", err)
	}
	return v, nil
}

// ResolveProjectID resolves a project key (e.g. "EXAMPLE") to its numeric
// project id via GET /rest/api/3/project/{key}. Jira returns id as a
// string; it is converted to an int for the caller.
func (c *Client) ResolveProjectID(ctx context.Context, key string) (int, error) {
	if key == "" {
		return 0, errext.Errorf("client: ResolveProjectID: key is required")
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "project", key).String()
	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newGet(ctx, endpoint)
	})
	if err != nil {
		return 0, err
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, errext.Errorf("client: unmarshal project response: %w", err)
	}
	id, err := strconv.Atoi(resp.ID)
	if err != nil {
		return 0, errext.Errorf("client: ResolveProjectID: parse id %q: %w", resp.ID, err)
	}
	return id, nil
}

// CreateVersion creates a new project version (release) via
// POST /rest/api/3/version. name and project are required and explicit;
// optional fields flow through the [VersionOption] set.
//
// project may be a numeric project id ("10000"), used directly, or a
// project key ("EXAMPLE"), which is first resolved to a numeric id via
// [Client.ResolveProjectID] BEFORE the body is rendered — so the render
// path stays network-free. On success Jira returns 201 with the created
// version; the parsed [Version] is returned. On 400/409 the error is an
// [*APIError] that still satisfies errors.Is against the sentinels.
func (c *Client) CreateVersion(ctx context.Context, name, project string, opts ...VersionOption) (Version, error) {
	if name == "" {
		return Version{}, errext.Errorf("client: CreateVersion: name is required")
	}
	if project == "" {
		return Version{}, errext.Errorf("client: CreateVersion: project is required")
	}

	numeric := project
	if !isAllDigits(project) {
		id, err := c.ResolveProjectID(ctx, project)
		if err != nil {
			return Version{}, err
		}
		numeric = strconv.Itoa(id)
	}

	body, err := RenderCreateVersionBody(name, numeric, opts...)
	if err != nil {
		return Version{}, err
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "version").String()
	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPostJSON(ctx, endpoint, body)
	})
	if err != nil {
		return Version{}, err
	}
	return parseVersion(raw)
}

// UpdateVersion edits an existing version identified by its numeric id via
// PUT /rest/api/3/version/{id}. Only the optional fields carried by opts
// are sent — released/archived flags, dates, description. name/project are
// not updatable through this method. Jira responds 200 with the updated
// version.
func (c *Client) UpdateVersion(ctx context.Context, id string, opts ...VersionOption) (Version, error) {
	if id == "" {
		return Version{}, errext.Errorf("client: UpdateVersion: id is required")
	}

	body, err := RenderUpdateVersionBody(opts...)
	if err != nil {
		return Version{}, err
	}

	endpoint := c.siteURL.JoinPath("rest", "api", "3", "version", id).String()
	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPutJSON(ctx, endpoint, body)
	})
	if err != nil {
		return Version{}, err
	}
	return parseVersion(raw)
}

// ListVersions returns all versions for a project via the paginated
// GET /rest/api/3/project/{projectIDOrKey}/version endpoint. It pages
// through the {values, startAt, maxResults, total, isLast} envelope,
// accumulating until isLast is true (or a page returns no values).
func (c *Client) ListVersions(ctx context.Context, projectIDOrKey string) ([]Version, error) {
	if projectIDOrKey == "" {
		return nil, errext.Errorf("client: ListVersions: projectIDOrKey is required")
	}

	const pageSize = 50
	var out []Version
	startAt := 0
	for {
		endpoint := c.siteURL.JoinPath("rest", "api", "3", "project", projectIDOrKey, "version")
		q := endpoint.Query()
		q.Set("startAt", strconv.Itoa(startAt))
		q.Set("maxResults", strconv.Itoa(pageSize))
		endpoint.RawQuery = q.Encode()
		endpointStr := endpoint.String()

		raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
			return c.newGet(ctx, endpointStr)
		})
		if err != nil {
			return nil, err
		}

		var page struct {
			Values     []Version `json:"values"`
			StartAt    int       `json:"startAt"`
			MaxResults int       `json:"maxResults"`
			Total      int       `json:"total"`
			IsLast     bool      `json:"isLast"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, errext.Errorf("client: unmarshal version list: %w", err)
		}

		out = append(out, page.Values...)
		if page.IsLast || len(page.Values) == 0 {
			break
		}
		startAt += len(page.Values)
	}
	return out, nil
}
