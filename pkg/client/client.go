// Package client implements the Jira Cloud HTTP transport for gojira.
//
// It handles Basic authentication (email:token base64-encoded), request
// construction, 429 rate-limit retry with Retry-After / exponential
// backoff, transient network-error retry, and typed sentinel errors for
// callers.
//
// The package knows nothing about issues, ADF, links, Markdown, or
// crawling. Its only project-internal import is internal/config.
package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/httplog"
)

// Sentinel errors that callers can match with errors.Is.
var (
	// ErrUnauthorized is returned when the server responds with 401.
	// This is a total-failure condition: credentials are invalid.
	ErrUnauthorized = errors.New("client: unauthorized (401)")

	// ErrForbidden is returned when the server responds with 403.
	// The caller should render a permission-denied stub and continue.
	ErrForbidden = errors.New("client: forbidden (403)")

	// ErrNotFound is returned when the server responds with 404.
	// The caller should render a not-found stub and continue.
	ErrNotFound = errors.New("client: not found (404)")

	// ErrRateLimited is returned when the server responds with 429 and
	// all retry attempts are exhausted.
	ErrRateLimited = errors.New("client: rate limited (429) after retries")

	// ErrBadRequest is returned when the server responds with 400. The
	// returned error may be a *APIError carrying Jira's per-field error
	// details (see phase-a-transport-2); callers can still match it with
	// errors.Is(err, ErrBadRequest).
	ErrBadRequest = errors.New("client: bad request (400)")

	// ErrConflict is returned when the server responds with 409, e.g. an
	// invalid workflow transition.
	ErrConflict = errors.New("client: conflict (409)")
)

// userAgent is sent on every request.
const userAgent = "gojira/0.1.0"

// Default retry and backoff constants.
const (
	defaultMaxRateLimitRetries  = 5
	defaultMaxNetworkRetries    = 3
	defaultRateLimitBaseBackoff = time.Second
	defaultRateLimitMaxBackoff  = 30 * time.Second
	defaultNetworkBaseBackoff   = 500 * time.Millisecond
	defaultNetworkMaxBackoff    = 5 * time.Second
)

// Client is the Jira Cloud HTTP transport. Construct via New.
// The zero value is not valid.
type Client struct {
	httpClient *http.Client
	siteURL    *url.URL
	authHeader string // pre-computed "Basic <base64(user:token)>"

	// logger, when non-nil, drives the logging RoundTripper installed
	// in New for crawl observability. Nil means no HTTP request logging
	// is emitted — bytes-identical behavior to clients constructed
	// before WithLogger existed.
	logger *slog.Logger

	maxRateLimitRetries  int
	maxNetworkRetries    int
	rateLimitBaseBackoff time.Duration
	rateLimitMaxBackoff  time.Duration
	networkBaseBackoff   time.Duration
	networkMaxBackoff    time.Duration

	// sleepFn is time.Sleep by default; replaced in tests for speed.
	sleepFn func(context.Context, time.Duration) error
}

// Option is a functional option for New.
type Option func(*Client)

// WithHTTPClient replaces the entire http.Client used for requests.
// Useful in tests that inject an httptest.Server-backed client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// WithRoundTripper replaces only the transport on the default http.Client.
// Useful when a custom transport is needed without a full http.Client.
func WithRoundTripper(rt http.RoundTripper) Option {
	return func(c *Client) {
		c.httpClient.Transport = rt
	}
}

// WithLogger installs a logging RoundTripper that emits one INFO summary
// per request (method/url/status/duration_ms/bytes) and, when the logger
// is enabled at [log.LevelTrace], a full [net/http/httptrace] lifecycle
// plus the raw response body. Authorization and other sensitive headers
// are always redacted. A nil logger is a no-op: no request logging is
// emitted and the transport is left untouched, so existing callers see
// byte-identical behavior.
//
// WithLogger composes with [WithHTTPClient] and [WithRoundTripper]: the
// logging RoundTripper is installed by [New] AFTER all options have
// been applied, so it wraps whatever Transport ended up on the client.
// Order between WithLogger and WithRoundTripper / WithHTTPClient is
// irrelevant.
func WithLogger(lg *slog.Logger) Option {
	return func(c *Client) { c.logger = lg }
}

// WithMaxRetries sets the maximum number of 429 retry attempts.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		c.maxRateLimitRetries = n
	}
}

// WithRateLimitBackoff overrides the base and max backoff durations for
// 429 retries. Exposed primarily for tests that need sub-second backoffs.
func WithRateLimitBackoff(base, max time.Duration) Option {
	return func(c *Client) {
		c.rateLimitBaseBackoff = base
		c.rateLimitMaxBackoff = max
	}
}

// WithNetworkBackoff overrides the base and max backoff durations for
// transient network-error retries. Exposed primarily for tests.
func WithNetworkBackoff(base, max time.Duration) Option {
	return func(c *Client) {
		c.networkBaseBackoff = base
		c.networkMaxBackoff = max
	}
}

// withSleepFn replaces the context-aware sleep function. Unexported;
// used in tests to eliminate real waits without build tags.
func withSleepFn(fn func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		c.sleepFn = fn
	}
}

// New constructs a Client from cfg and applies any provided options.
//
// It validates cfg.Site as a parseable URL with a non-empty host and
// pre-computes the Basic auth header. Returns an error if cfg.Site is
// not a valid URL.
func New(cfg config.Config, opts ...Option) (*Client, error) {
	siteURL, err := url.Parse(strings.TrimRight(cfg.Site, "/"))
	if err != nil || siteURL.Host == "" {
		return nil, errext.Errorf("client: invalid site URL %q", cfg.Site)
	}

	creds := cfg.User + ":" + cfg.Token
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))

	c := &Client{
		httpClient:           &http.Client{Timeout: 30 * time.Second},
		siteURL:              siteURL,
		authHeader:           auth,
		maxRateLimitRetries:  defaultMaxRateLimitRetries,
		maxNetworkRetries:    defaultMaxNetworkRetries,
		rateLimitBaseBackoff: defaultRateLimitBaseBackoff,
		rateLimitMaxBackoff:  defaultRateLimitMaxBackoff,
		networkBaseBackoff:   defaultNetworkBaseBackoff,
		networkMaxBackoff:    defaultNetworkMaxBackoff,
		sleepFn:              contextSleep,
	}

	for _, opt := range opts {
		opt(c)
	}

	// Install the logging RoundTripper last so it wraps any
	// caller-supplied Transport from WithRoundTripper or
	// WithHTTPClient. The default http.Client constructed above has
	// Transport == nil, which net/http treats as http.DefaultTransport;
	// we resolve it explicitly here so the wrapped chain has a real
	// base. When no logger is supplied this branch is skipped and the
	// transport is left exactly as the options configured it, keeping
	// behavior bytes-identical for callers that have not opted into
	// observability.
	if c.logger != nil {
		base := c.httpClient.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		c.httpClient.Transport = httplog.New(base, c.logger)
	}

	return c, nil
}

// GetIssue fetches the raw JSON body for the Jira issue identified by key
// from the configured site's REST API v3 endpoint:
//
//	GET <site>/rest/api/3/issue/<key>[?expand=<csv>]
//
// On success it returns the raw response bytes. On failure it returns one
// of the typed sentinel errors (ErrUnauthorized, ErrForbidden, ErrNotFound,
// ErrRateLimited) or a wrapped error containing the HTTP status code.
//
// 429 responses are retried up to maxRateLimitRetries times, honouring the
// Retry-After header when present. Transient network errors (timeouts and
// mid-stream io.EOF) are retried up to maxNetworkRetries times.
// Context cancellation is respected throughout.
//
// expand is the list of Jira expansion tokens to pass via the `expand`
// query parameter (e.g. []string{"names"} to receive the top-level
// "names" object mapping every field ID, including customfield_*, to
// its human-readable label). The values are comma-joined into a single
// `expand=<a>,<b>,...` query parameter exactly as documented at
// https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issues/#api-rest-api-3-issue-issueidorkey-get.
// When expand is empty or nil, no `expand` query parameter is sent and
// the response shape matches the pre-expansion behaviour.
//
// expand is a parameter rather than a hard-coded default inside the
// body: callers know what they want from the response, and embedding
// a "blessed" value here would violate the project's signature-honesty
// rule (see docs/engineering-principles.md). Internal callers that
// want human-readable custom-field labels pass []string{"names"}
// explicitly; callers that want the legacy shape pass nil.
func (c *Client) GetIssue(ctx context.Context, key string, expand []string) ([]byte, error) {
	endpoint := c.siteURL.JoinPath("rest", "api", "3", "issue", key)
	if len(expand) > 0 {
		q := endpoint.Query()
		q.Set("expand", strings.Join(expand, ","))
		endpoint.RawQuery = q.Encode()
	}
	endpointStr := endpoint.String()
	return c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newGet(ctx, endpointStr)
	})
}

// SearchResult is the result of a Search call. Keys lists the issue keys
// returned by the JQL query, in the order the Jira API returned them.
// NextPageToken is the opaque cursor for the next page; an empty value
// means there are no more pages. v0.1 does not paginate automatically;
// callers that need more than maxResults must call Search again with the
// returned token (post-MVP capability).
type SearchResult struct {
	Keys          []string
	NextPageToken string
}

// Search executes a JQL query against the modern Jira Cloud search endpoint:
//
//	POST <site>/rest/api/3/search/jql
//	{"jql": "...", "fields": ["key"], "maxResults": N}
//
// Only the issue keys from the response are returned; the rest of each
// issue payload is discarded. This is sufficient for hierarchy discovery,
// which only needs keys to feed back into the crawl queue.
//
// 429, 401, 403, 404, and transient network errors are handled identically
// to GetIssue, using the same retry/backoff machinery.
//
// maxResults caps the maxResults parameter sent in the request body.
// When maxResults <= 0, the Jira API default is used (currently 50).
//
// # Pagination limitation (v0.1)
//
// The modern endpoint uses cursor-based pagination via nextPageToken.
// Search currently returns only the first page (up to maxResults results)
// and surfaces NextPageToken on SearchResult so callers can detect when
// more results exist. Automatic multi-page aggregation is deferred to
// post-MVP.
func (c *Client) Search(ctx context.Context, jql string, maxResults int) (SearchResult, error) {
	endpoint := c.siteURL.JoinPath("rest", "api", "3", "search", "jql").String()

	body := map[string]any{
		"jql":    jql,
		"fields": []string{"key"},
	}
	if maxResults > 0 {
		body["maxResults"] = maxResults
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return SearchResult{}, errext.Errorf("client: marshal search body: %w", err)
	}

	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newPostJSON(ctx, endpoint, bodyBytes)
	})
	if err != nil {
		return SearchResult{}, err
	}

	var resp struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return SearchResult{}, errext.Errorf("client: unmarshal search response: %w", err)
	}

	keys := make([]string, 0, len(resp.Issues))
	for _, iss := range resp.Issues {
		if iss.Key != "" {
			keys = append(keys, iss.Key)
		}
	}
	return SearchResult{Keys: keys, NextPageToken: resp.NextPageToken}, nil
}

// Field is the subset of Jira field metadata that gojira needs. It mirrors
// the relevant fields in the GET /rest/api/3/field response:
//
//	[{"id": "...", "key": "...", "name": "...", "custom": true|false, ...}]
//
// Only ID, Key, Name, and Custom are populated; everything else is dropped.
type Field struct {
	ID     string
	Key    string
	Name   string
	Custom bool
}

// ListFields fetches the tenant's field metadata from:
//
//	GET <site>/rest/api/3/field
//
// It is used by the hierarchy discoverer to auto-detect the Epic Link
// custom field ID, which varies per tenant. 429 and transient network
// errors are retried using the same machinery as GetIssue.
func (c *Client) ListFields(ctx context.Context) ([]Field, error) {
	endpoint := c.siteURL.JoinPath("rest", "api", "3", "field").String()

	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newGet(ctx, endpoint)
	})
	if err != nil {
		return nil, err
	}

	var raws []struct {
		ID     string `json:"id"`
		Key    string `json:"key"`
		Name   string `json:"name"`
		Custom bool   `json:"custom"`
	}
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, errext.Errorf("client: unmarshal field list: %w", err)
	}

	out := make([]Field, 0, len(raws))
	for _, f := range raws {
		out = append(out, Field{ID: f.ID, Key: f.Key, Name: f.Name, Custom: f.Custom})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Dev Status (undocumented internal endpoint)
// ---------------------------------------------------------------------------

// DevStatusResponse is the parsed shape of a successful Dev Status API
// response. The endpoint returns a single envelope for any of the five
// supported dataType values (pullrequest, branch, commit, repository,
// build); the per-entity list inside each [DevStatusInstance] differs
// per dataType. A query for dataType=branch populates Branches and
// leaves PullRequests/Commits/Repositories/Builds empty, and so on.
//
// Errors is the upstream response's "errors" array. A non-empty Errors
// slice does not by itself cause [Client.DevStatus] to return a Go
// error: the server returns HTTP 200 with embedded soft-error entries
// (e.g. when one connected GitHub instance is unreachable while another
// succeeded). Callers decide how to surface these.
//
// The element type is intentionally [json.RawMessage] rather than a
// typed struct. The Dev Status endpoint is undocumented and the
// per-error entry shape is not guaranteed stable: production responses
// have been observed carrying both string entries (older tenants) and
// JSON objects with shape `{"code": <int>, "message": <string>,
// "userId": <string>, ...}` (newer tenants — observed on PLATENG-1417
// for dataType=commit and dataType=build). Modelling Errors as a typed
// slice against a single observed shape would break the unmarshal as
// soon as the other shape appears. [json.RawMessage] accepts any valid
// JSON element and lets the (rare) caller that wants to introspect the
// entries decode each one lazily. This is the "signature honesty" rule
// from docs/engineering-principles.md applied to response models: do
// not pretend to know a shape we have not verified across the entire
// observed input space.
type DevStatusResponse struct {
	Errors []json.RawMessage   `json:"errors"`
	Detail []DevStatusInstance `json:"detail"`
}

// DevStatusInstance groups the development entities returned by a
// single development-tool integration (e.g. one connected GitHub app).
//
// Exactly one of the entity lists is non-empty for a given response,
// depending on the dataType the caller requested. The struct unmarshals
// all five tolerantly so a single Go type can carry the response for
// any dataType the [Client.DevStatus] caller asked for.
type DevStatusInstance struct {
	Instance     DevStatusInstanceMeta  `json:"_instance"`
	PullRequests []DevStatusPR          `json:"pullRequests"`
	Branches     []DevStatusBranchEntry `json:"branches"`
	Commits      []DevStatusCommit      `json:"commits"`
	Repositories []DevStatusRepository  `json:"repositories"`
	Builds       []DevStatusBuild       `json:"builds"`
}

// DevStatusInstanceMeta describes the development-tool integration that
// owns a [DevStatusInstance]'s entries.
type DevStatusInstanceMeta struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
}

// DevStatusPR is a single pull-request entry returned by the Dev Status
// endpoint. Field names match Jira's response shape exactly; semantic
// translation (e.g. mapping Name → Title) happens in the devstatus
// package, not here.
type DevStatusPR struct {
	ID            string              `json:"id"`
	URL           string              `json:"url"`
	Name          string              `json:"name"`
	Status        string              `json:"status"`
	LastUpdate    string              `json:"lastUpdate"`
	Source        DevStatusBranch     `json:"source"`
	Destination   DevStatusBranch     `json:"destination"`
	Author        DevStatusPerson     `json:"author"`
	Reviewers     []DevStatusReviewer `json:"reviewers"`
	Repository    string              `json:"repositoryName"`
	RepositoryURL string              `json:"repositoryUrl"`
	CommentCount  int                 `json:"commentCount"`
}

// DevStatusBranch holds a branch reference from a Dev Status PR entry
// (the source/destination "branch" field of a pull request). It is a
// distinct type from [DevStatusBranchEntry] which represents a top-
// level entry returned by a dataType=branch query.
type DevStatusBranch struct {
	Branch string `json:"branch"`
	URL    string `json:"url"`
}

// DevStatusPerson holds the author of a Dev Status PR/commit/branch
// entry. The Jira response also carries an "avatar" URL, which gojira
// does not consume.
type DevStatusPerson struct {
	Name string `json:"name"`
}

// DevStatusReviewer holds a single reviewer entry from a Dev Status PR.
type DevStatusReviewer struct {
	Name     string `json:"name"`
	Approved bool   `json:"approved"`
}

// DevStatusRepoRef is the small repository pointer embedded inside
// other Dev Status entities (branches, commits). The Jira response
// nests a {"name", "url"} pair; gojira preserves both so the renderer
// can label the parent entity with its repository.
type DevStatusRepoRef struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// DevStatusCommitRef is the last-commit pointer embedded inside a
// Dev Status branch entry. It mirrors the same fields as
// [DevStatusCommit] but without the standalone-entity fields
// (fileCount, merge, repository) that only appear on a top-level
// commit query.
type DevStatusCommitRef struct {
	ID              string          `json:"id"`
	DisplayID       string          `json:"displayId"`
	URL             string          `json:"url"`
	Message         string          `json:"message"`
	Author          DevStatusPerson `json:"author"`
	AuthorTimestamp string          `json:"authorTimestamp"`
}

// DevStatusBranchEntry is a single entry returned by a Dev Status
// query for dataType=branch. The Jira UI surfaces these in the
// Development panel alongside pull requests and commits; gojira mirrors
// that grouping in its Markdown output.
type DevStatusBranchEntry struct {
	Name                 string             `json:"name"`
	URL                  string             `json:"url"`
	CreatePullRequestURL string             `json:"createPullRequestUrl"`
	Repository           DevStatusRepoRef   `json:"repository"`
	LastCommit           DevStatusCommitRef `json:"lastCommit"`
}

// DevStatusCommit is a single entry returned by a Dev Status query for
// dataType=commit. Fields mirror the upstream response exactly; gojira
// preserves the full SHA in ID and the seven-character abbreviation in
// DisplayID rather than re-deriving the latter, in case future Jira
// versions change the abbreviation length.
type DevStatusCommit struct {
	ID              string           `json:"id"`
	DisplayID       string           `json:"displayId"`
	URL             string           `json:"url"`
	Message         string           `json:"message"`
	Author          DevStatusPerson  `json:"author"`
	AuthorTimestamp string           `json:"authorTimestamp"`
	FileCount       int              `json:"fileCount"`
	Merge           bool             `json:"merge"`
	Repository      DevStatusRepoRef `json:"repository"`
}

// DevStatusRepository is a single entry returned by a Dev Status query
// for dataType=repository. The Jira UI surfaces these so users can see
// which repositories have referenced an issue even when no PR or
// branch carries the issue key directly.
//
// The upstream response also nests truncated commits[] and branches[]
// arrays inside each repository entry; gojira does not consume them
// (they are sparser than the top-level dataType=commit/branch queries
// and would risk duplication if rendered separately).
type DevStatusRepository struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Avatar string `json:"avatar"`
}

// DevStatusBuild is a single entry returned by a Dev Status query for
// dataType=build. State is the upstream lifecycle string ("SUCCESSFUL",
// "FAILED", "IN_PROGRESS", "STOPPED", "PENDING", ...); gojira does not
// normalise it. TestSummary is a pointer so a build that does not
// report test counts can be distinguished from one that reports zero.
type DevStatusBuild struct {
	ID          string                `json:"id"`
	BuildNumber int                   `json:"buildNumber"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	URL         string                `json:"url"`
	State       string                `json:"state"`
	LastUpdated string                `json:"lastUpdated"`
	TestSummary *DevStatusTestSummary `json:"testSummary,omitempty"`
	References  []DevStatusBuildRef   `json:"references"`
}

// DevStatusTestSummary holds the per-build test counts surfaced by
// Dev Status for CI integrations that publish them (Bitbucket
// Pipelines, GitHub Actions via the Atlassian app, etc.). Fields with
// no value default to zero.
type DevStatusTestSummary struct {
	TotalNumber   int `json:"totalNumber"`
	PassedNumber  int `json:"passedNumber"`
	FailedNumber  int `json:"failedNumber"`
	SkippedNumber int `json:"skippedNumber"`
}

// DevStatusBuildRef is a single git reference (branch or tag) tied to
// a build. Jira surfaces it so the build can be attributed to the
// branch its CI ran on.
type DevStatusBuildRef struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
}

// DevStatus fetches the development metadata (pull requests, branches,
// commits, repositories, builds — depending on dataType) that Jira
// surfaces in its UI Development panel for the issue identified by its
// numeric ID.
//
//	GET <site>/rest/dev-status/1.0/issue/detail
//	    ?issueId=<numeric>&applicationType=<application>&dataType=<dataType>
//
// # Important: this endpoint is NOT in the documented Jira Cloud platform
// REST API
//
// The /rest/dev-status/1.0 path is not part of the platform OpenAPI
// specification at developer.atlassian.com. It is the same endpoint
// Atlassian's own Jira UI consumes to populate the Development panel
// on every issue page; it has been stable for over a decade for that
// reason. gojira treats it as best-effort enrichment: callers can opt
// out cleanly by setting GOJIRA_INCLUDE_DEV_STATUS=false (handled at
// the devstatus enricher and crawl orchestrator layers), in which case
// this method is never invoked.
//
// 429, 401, 403, 404, and transient network errors are handled
// identically to [Client.GetIssue], using the same retry/backoff
// machinery and propagating the same typed sentinel errors.
//
// issueNumericID must be the numeric issue id (the top-level "id" field
// of the standard GET /issue/<KEY> response, e.g. "86679"). The endpoint
// silently returns an empty detail array for issue *keys* — passing the
// human-readable key by mistake therefore looks like "no entities" rather
// than an error. application is the upstream applicationType value
// (e.g. "GitHub", "Bitbucket", "GitLab", "GitHubEnterprise"); the
// caller is expected to fan out one call per configured application.
//
// dataType selects which entity list the response will populate. Valid
// values mirror the Jira UI Development panel groupings:
//
//   - "pullrequest" populates [DevStatusInstance.PullRequests]
//   - "branch"      populates [DevStatusInstance.Branches]
//   - "commit"      populates [DevStatusInstance.Commits]
//   - "repository"  populates [DevStatusInstance.Repositories]
//   - "build"       populates [DevStatusInstance.Builds]
//
// The endpoint accepts any string and silently returns an empty detail
// for unrecognised values; callers (the devstatus enricher in
// production) restrict dataType to the configured set.
func (c *Client) DevStatus(ctx context.Context, issueNumericID, application, dataType string) (DevStatusResponse, error) {
	endpoint := c.siteURL.JoinPath("rest", "dev-status", "1.0", "issue", "detail")
	q := endpoint.Query()
	q.Set("issueId", issueNumericID)
	q.Set("applicationType", application)
	q.Set("dataType", dataType)
	endpoint.RawQuery = q.Encode()
	endpointStr := endpoint.String()

	raw, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newGet(ctx, endpointStr)
	})
	if err != nil {
		return DevStatusResponse{}, err
	}

	var resp DevStatusResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return DevStatusResponse{}, errext.Errorf("client: unmarshal dev status response: %w", err)
	}
	return resp, nil
}

// newGet constructs an authenticated GET request with the standard headers.
func (c *Client) newGet(ctx context.Context, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// newPostJSON constructs an authenticated POST request with a JSON body.
// The body is wrapped in a fresh bytes.Reader so each retry attempt can
// re-read it from the start.
func (c *Client) newPostJSON(ctx context.Context, rawURL string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// newPutJSON constructs an authenticated PUT request with a JSON body.
// It is the sibling of [newPostJSON] for endpoints that take a PUT —
// Jira's `PUT /rest/api/3/issue/<key>` (edit issue) is the immediate
// caller in the Phase 2 write surface. Like newPostJSON, the body is
// wrapped in a fresh bytes.Reader so each retry attempt can re-read
// it from the start.
func (c *Client) newPutJSON(ctx context.Context, rawURL string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// rawResponse carries the parts of an HTTP response the retry loop needs.
type rawResponse struct {
	status     int
	body       []byte
	retryAfter string // value of the Retry-After response header, may be empty
}

// doWithRetry executes the request returned by buildReq, retrying on 429
// and transient network errors according to the client's retry config.
// buildReq is called once per attempt so that POST bodies (which may not
// be re-readable) get a fresh request with a fresh body reader each time.
func (c *Client) doWithRetry(ctx context.Context, buildReq func() (*http.Request, error)) ([]byte, error) {
	var (
		networkAttempts  int
		rateLimitAttempt int
	)

	for {
		resp, err := c.doOnce(buildReq)

		// --- transport / network error ---
		if err != nil {
			if ctx.Err() != nil {
				return nil, errext.Errorf("client: request cancelled: %w", ctx.Err())
			}
			if isTransientNetworkError(err) && networkAttempts < c.maxNetworkRetries {
				networkAttempts++
				delay := exponentialBackoff(networkAttempts, c.networkBaseBackoff, c.networkMaxBackoff)
				if sleepErr := c.sleepFn(ctx, delay); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, errext.Errorf("client: request failed: %w", err)
		}

		// --- HTTP status handling ---
		switch resp.status {
		// 200 OK is the historical read-path success. 201 Created and
		// 204 No Content cover the Phase 2 write paths: CreateIssue
		// returns 201 with a {id,key,self} body, UpdateIssue and
		// TransitionIssue return 204 with no body. Returning resp.body
		// for 204 is safe — it is just an empty byte slice that
		// write-method callers ignore.
		case http.StatusOK, http.StatusCreated, http.StatusNoContent:
			return resp.body, nil

		// 400 and 409 carry Jira's standard error body
		// ({"errorMessages": [...], "errors": {fieldID: msg}}). We parse
		// it into an *APIError that Unwraps to the matching sentinel,
		// so errors.Is still classifies the failure while errors.As
		// exposes which fields the server rejected. parseAPIError
		// degrades gracefully on non-JSON bodies (HTML from a WAF, an
		// empty body) by returning an *APIError with only Status +
		// sentinel set — classification is never lost.
		case http.StatusBadRequest:
			return nil, parseAPIError(http.StatusBadRequest, ErrBadRequest, resp.body)

		case http.StatusUnauthorized:
			return nil, ErrUnauthorized

		case http.StatusForbidden:
			return nil, ErrForbidden

		case http.StatusNotFound:
			return nil, ErrNotFound

		case http.StatusConflict:
			return nil, parseAPIError(http.StatusConflict, ErrConflict, resp.body)

		case http.StatusTooManyRequests:
			if rateLimitAttempt >= c.maxRateLimitRetries {
				return nil, ErrRateLimited
			}
			rateLimitAttempt++
			delay := c.rateLimitDelay(resp.retryAfter, rateLimitAttempt)
			if sleepErr := c.sleepFn(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
			continue

		default:
			return nil, errext.Errorf("client: unexpected status %d", resp.status)
		}
	}
}

// doOnce performs a single HTTP request using the supplied builder.
// The caller is responsible for retry logic.
func (c *Client) doOnce(buildReq func() (*http.Request, error)) (*rawResponse, error) {
	req, err := buildReq()
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// Mid-stream read failure — treat as transient.
		return nil, &midStreamError{err}
	}

	return &rawResponse{
		status:     resp.StatusCode,
		body:       body,
		retryAfter: resp.Header.Get("Retry-After"),
	}, nil
}

// rateLimitDelay returns the duration to wait before the next 429 retry.
// It honours the Retry-After header when present and parseable; otherwise
// it falls back to exponential backoff.
func (c *Client) rateLimitDelay(retryAfterHeader string, attempt int) time.Duration {
	if d := parseRetryAfter(retryAfterHeader); d > 0 {
		if d > c.rateLimitMaxBackoff {
			return c.rateLimitMaxBackoff
		}
		return d
	}
	return exponentialBackoff(attempt, c.rateLimitBaseBackoff, c.rateLimitMaxBackoff)
}

// contextSleep waits for d, returning a wrapped ctx.Err() if the context
// is cancelled before the wait completes. This is the default sleepFn.
func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errext.Errorf("client: wait interrupted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// exponentialBackoff returns min(base * 2^(attempt-1), max).
// attempt is 1-based.
func exponentialBackoff(attempt int, base, max time.Duration) time.Duration {
	if attempt <= 1 {
		return base
	}
	exp := math.Pow(2, float64(attempt-1))
	d := time.Duration(float64(base) * exp)
	if d > max || d < 0 {
		return max
	}
	return d
}

// isTransientNetworkError reports whether err is a retryable network error:
// a net.Error with Timeout() == true, or a mid-stream body read failure.
func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var mse *midStreamError
	return errors.As(err, &mse)
}

// midStreamError wraps an error that occurred while reading the response
// body after the HTTP connection was established. Treated as transient.
type midStreamError struct{ cause error }

func (e *midStreamError) Error() string {
	return "client: mid-stream read error: " + e.cause.Error()
}
func (e *midStreamError) Unwrap() error { return e.cause }

// parseRetryAfter parses the Retry-After header value per RFC 7231:
// either a delay-seconds integer or an HTTP-date. Returns the duration
// to wait, or 0 if the header is absent or unparseable.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	// Try integer seconds first.
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	// Try HTTP-date formats (RFC 1123, RFC 850, ANSI C asctime).
	formats := []string{
		time.RFC1123,
		"Monday, 02-Jan-06 15:04:05 MST",
		time.ANSIC,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, strings.TrimSpace(header)); err == nil {
			d := time.Until(t)
			if d < 0 {
				return 0
			}
			return d
		}
	}
	return 0
}
