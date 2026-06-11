// Package fetch provides a thin adapter between the crawl orchestrator
// and the Jira Cloud HTTP client. Its single responsibility is: given an
// issue key, return the raw JSON bytes for that issue.
//
// fetch knows nothing about parsing, link extraction, Markdown rendering,
// or crawl queue management. It delegates all HTTP concerns (auth, retries,
// rate-limit backoff, typed error sentinels) to the client package.
//
// # Fetcher interface
//
// The Fetcher interface is the contract the crawl package depends on.
// Callers that need to inject a fake in tests can implement Fetcher
// without spinning up an httptest.Server.
//
// # Error propagation
//
// Typed sentinel errors from the client package (ErrUnauthorized,
// ErrForbidden, ErrNotFound, ErrRateLimited) are propagated without
// additional wrapping so that callers can use errors.Is directly:
//
//	raw, err := f.Fetch(ctx, "PROJ-1")
//	if errors.Is(err, client.ErrUnauthorized) { /* abort crawl */ }
//	if errors.Is(err, client.ErrForbidden)    { /* render stub  */ }
//	if errors.Is(err, client.ErrNotFound)     { /* render stub  */ }
package fetch

import (
	"context"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/pkg/client"
)

// Fetcher is the interface the crawl package depends on for retrieving
// raw Jira issue bytes. Implement this interface in tests to avoid
// spinning up a real HTTP server.
type Fetcher interface {
	Fetch(ctx context.Context, key string) ([]byte, error)
}

// ClientFetcher is the production implementation of Fetcher. It wraps a
// *client.Client and delegates all HTTP concerns to it.
// Construct via New or NewFromConfig; the zero value is not valid.
type ClientFetcher struct {
	c *client.Client
}

// New constructs a ClientFetcher from an already-constructed *client.Client.
// Use this when the caller already holds a client (e.g. the gojira facade
// that shares one client across fetch and other operations).
func New(c *client.Client) *ClientFetcher {
	return &ClientFetcher{c: c}
}

// NewFromConfig is a convenience constructor that builds both a *client.Client
// and a *ClientFetcher from a validated config.Config. Useful for callers
// that do not need to share the underlying client with other components.
//
// Any client.Option values (e.g. WithHTTPClient for tests) are forwarded
// to client.New unchanged. In addition, client.WithLogger and
// client.WithRoundTripper are commonly forwarded by the crawl observability
// instrument to install the httplog RoundTripper around the underlying
// client's transport; both flow through this variadic unchanged.
func NewFromConfig(cfg config.Config, opts ...client.Option) (*ClientFetcher, error) {
	c, err := client.New(cfg, opts...)
	if err != nil {
		return nil, errext.Errorf("fetch: build client: %w", err)
	}
	return New(c), nil
}

// Fetch retrieves the raw JSON bytes for the Jira issue identified by key.
//
// On success it returns the bytes exactly as received from the Jira API.
// On failure it returns one of the client sentinel errors
// (client.ErrUnauthorized, client.ErrForbidden, client.ErrNotFound,
// client.ErrRateLimited) or a wrapped transport error. All sentinels
// remain reachable via errors.Is.
//
// Fetch does not add retry logic (that is the client's responsibility),
// does not emit events (that is the crawl's responsibility), and does
// not parse the returned bytes.
//
// The underlying GET request is issued with `expand=names` so the
// response includes a top-level "names" object mapping every field
// ID (including customfield_NNNNN) to its human-readable label.
// `internal/parse` consumes that object and surfaces it as
// `parse.Issue.Names`, which `internal/render` uses to render custom
// fields with the label the Jira UI shows rather than the opaque
// numeric ID. The expansion is per-issue and adds no extra HTTP
// requests; the cost is a slightly larger response body.
func (f *ClientFetcher) Fetch(ctx context.Context, key string) ([]byte, error) {
	raw, err := f.c.GetIssue(ctx, key, []string{"names"})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// compile-time assertion: *ClientFetcher satisfies Fetcher.
var _ Fetcher = (*ClientFetcher)(nil)
