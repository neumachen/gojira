// Package gojira is the public library facade for the gojira Jira-to-Markdown
// mirror tool. It exposes exactly four named capabilities committed in PRD §8:
//
//  1. [Classify] — classify a URL or bare issue key.
//  2. [LoadConfig] / [Config] — build and validate a runtime configuration.
//  3. [FetchAndRender] — fetch one Jira issue and return rendered Markdown.
//  4. [Crawl] / [Summary] — run a full recursive crawl to disk.
//
// # Public surface invariants
//
//   - No flag parsing, CLI argument handling, or process-level signal handling.
//   - No os.Exit calls.
//   - No hard-coded credentials, Jira domains, or project keys.
//   - All internal packages are hidden; only the four capabilities and their
//     supporting types are exported.
//   - The CLI binary (cmd/gojira) is a thin consumer of this package; it is
//     never required to use the library.
//
// # Minimal usage example (third-party consumer workflow from PRD §8)
//
//	cfg, err := gojira.LoadConfig(map[string]string{
//	    "GOJIRA_SITE":       "https://mycompany.atlassian.net",
//	    "GOJIRA_USER":       "me@example.com",
//	    "GOJIRA_TOKEN":      os.Getenv("JIRA_TOKEN"),
//	    "GOJIRA_OUTPUT_DIR": "./jira-mirror",
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	summary, err := gojira.Crawl(ctx, cfg, []string{"PROJ-1"}, nil)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Printf("fetched=%d stubbed=%d failed=%d\n",
//	    summary.Fetched, summary.Stubbed, summary.Failed)
package gojira

import (
	"context"
	"log/slog"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/classify"
	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/crawl"
	"github.com/neumachen/gojira/internal/devstatus"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/fetch"
	"github.com/neumachen/gojira/internal/hierarchy"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/internal/render"
)

// ---------------------------------------------------------------------------
// Version
// ---------------------------------------------------------------------------

// Version is the current library version. The authoritative release tag is
// set in Phase 7; this constant is declared early so callers can read it.
const Version = "v0.1.0"

// ---------------------------------------------------------------------------
// Type aliases — expose internal types under the gojira package name
// without re-defining them or leaking the internal package paths.
// ---------------------------------------------------------------------------

// Config is the validated runtime configuration for a gojira run.
// It is an alias for the internal config type; construct it via [LoadConfig].
type Config = config.Config

// Summary is the structured result returned by [Crawl] after a run completes.
// It is an alias for the internal crawl summary type.
type Summary = crawl.Summary

// Sink is the interface callers implement to receive structured events from
// the library. It is an alias for the internal events.Sink interface, so
// callers can implement their own sink without importing internal/events.
type Sink = events.Sink

// Event is a single observable occurrence emitted by the library.
// It is an alias for the internal events.Event type.
type Event = events.Event

// ---------------------------------------------------------------------------
// Event kind constants — re-exported so callers can switch on them without
// importing internal/events.
// ---------------------------------------------------------------------------

const (
	KindIssueFetched     = events.KindIssueFetched
	KindIssueStubbed     = events.KindIssueStubbed
	KindIssueFailed      = events.KindIssueFailed
	KindIssueCapReached  = events.KindIssueCapReached
	KindPRReferenceFound = events.KindPRReferenceFound
	KindCrawlSummary     = events.KindCrawlSummary
)

// ---------------------------------------------------------------------------
// Sentinel errors — re-exported so callers can errors.Is without importing
// the client package.
// ---------------------------------------------------------------------------

var (
	// ErrUnauthorized is returned when Jira responds with 401.
	// This is a total-failure condition: credentials are invalid.
	ErrUnauthorized = client.ErrUnauthorized

	// ErrForbidden is returned when Jira responds with 403.
	// The crawl renders a permission-denied stub and continues.
	ErrForbidden = client.ErrForbidden

	// ErrNotFound is returned when Jira responds with 404.
	// The crawl renders a not-found stub and continues.
	ErrNotFound = client.ErrNotFound

	// ErrRateLimited is returned when Jira responds with 429 and all
	// retry attempts are exhausted.
	ErrRateLimited = client.ErrRateLimited
)

// ---------------------------------------------------------------------------
// NoopSink — a zero-effort default sink for callers that do not need events.
// ---------------------------------------------------------------------------

// NoopSink discards every event. Pass it (or nil) to [Crawl] when you do not
// need to observe crawl progress.
var NoopSink Sink = events.NoopSink{}

// NewSlogSink returns a [Sink] that emits each event as a structured slog
// record through logger. It is the recommended way for callers (including
// the gojira CLI) to bridge gojira's event stream onto the standard
// log/slog pipeline without importing internal/events directly.
//
// A nil logger is replaced with [slog.Default] so the resulting sink is
// always safe to call. The Event-to-slog mapping (kind → level, attribute
// names) is documented on the underlying events.SlogSink.
func NewSlogSink(logger *slog.Logger) Sink {
	return events.NewSlogSink(logger)
}

// ---------------------------------------------------------------------------
// Capability 1: Classify
// ---------------------------------------------------------------------------

// Classify determines the kind of link represented by input.
//
// It is a direct re-export of [classify.Classify]. The returned
// [classify.Result] carries the Kind and any extracted fields (IssueKey,
// Owner, Repo, PRNumber, URL). classify is a public package; callers may
// import it directly for the Kind constants.
//
// jiraSite is the Jira Cloud base URL (e.g. "https://mycompany.atlassian.net").
// Only the host portion is used for matching.
func Classify(input, jiraSite string) classify.Result {
	return classify.Classify(input, jiraSite)
}

// ---------------------------------------------------------------------------
// Capability 2: LoadConfig
// ---------------------------------------------------------------------------

// LoadConfig validates the key-value pairs in kv against the canonical
// GOJIRA_* key set defined in PRD §6, applies defaults for optional keys,
// and returns a populated [Config].
//
// kv may come from any source: environment variables, CLI flags, a config
// file, or a test fixture. This package does not read environment variables
// itself.
//
// On the first validation failure, LoadConfig returns a zero Config and a
// descriptive error. Use errors.Is with config.ErrMissingRequired or
// config.ErrInvalidValue to distinguish failure classes.
func LoadConfig(kv map[string]string) (Config, error) {
	return config.Build(kv)
}

// ---------------------------------------------------------------------------
// Capability 3: FetchAndRender
// ---------------------------------------------------------------------------

// FetchAndRender fetches a single Jira issue identified by key, parses its
// ADF description and relationships, and returns the rendered Markdown content
// for both the issue page and its outbound reference index.
//
// It does NOT write anything to disk. The caller decides what to do with the
// returned strings (write them, embed them in a larger pipeline, etc.).
//
// # Parameters
//
//   - ctx: controls the lifetime of the HTTP request.
//   - cfg: validated runtime configuration (construct via [LoadConfig]).
//   - key: Jira issue key, e.g. "PROJ-1".
//   - opts: optional [client.Option] values (e.g. client.WithHTTPClient for
//     tests). Pass nil or omit for production use.
//
// # Return values
//
//   - indexMD: Markdown content for <KEY>/index.md.
//   - outboundMD: Markdown content for <KEY>/references/outbound.md.
//     Empty string when the issue has no outbound references.
//   - discoveredKeys: Jira issue keys found in the issue's description and
//     relationships, in extract's documented order (description → parent →
//     subtasks → issuelinks → remotelinks). May contain duplicates; the
//     caller is responsible for deduplication.
//   - err: non-nil on fetch, parse, extract, or render failure.
//
// # Neighbour resolution
//
// Because FetchAndRender fetches a single issue in isolation, the neighbours
// set passed to the renderer is always empty. Relationship links therefore
// render as absolute Jira browse URLs rather than relative Markdown paths.
// Use [Crawl] for a full recursive crawl where relative links are resolved.
func FetchAndRender(ctx context.Context, cfg Config, key string, opts ...client.Option) (indexMD, outboundMD string, discoveredKeys []string, err error) {
	// Build the HTTP client.
	c, err := client.New(cfg, opts...)
	if err != nil {
		return "", "", nil, errext.Errorf("gojira: build client: %w", err)
	}

	// Fetch raw issue bytes.
	f := fetch.New(c)
	raw, err := f.Fetch(ctx, key)
	if err != nil {
		return "", "", nil, errext.Errorf("gojira: fetch %s: %w", key, err)
	}

	// Parse.
	issue, err := parse.Parse(raw, cfg.Site)
	if err != nil {
		return "", "", nil, errext.Errorf("gojira: parse %s: %w", key, err)
	}

	// Extract outbound references.
	refs, err := extract.Extract(issue, cfg.Site)
	if err != nil {
		return "", "", nil, errext.Errorf("gojira: extract %s: %w", key, err)
	}

	// Collect discovered Jira keys (in extract's documented order).
	for _, r := range refs {
		if (r.Kind == classify.KindJiraKey || r.Kind == classify.KindJiraURL) && r.IssueKey != "" {
			discoveredKeys = append(discoveredKeys, r.IssueKey)
		}
	}

	// Render index.md. neighbours is empty for a single-issue render;
	// all relationship links will use absolute Jira browse URLs. The
	// RenderNullCustomFields flag from cfg controls whether null-valued
	// customfield_* entries are surfaced or suppressed.
	indexMD, err = render.RenderIssue(issue, map[string]bool{}, cfg.RenderNullCustomFields)
	if err != nil {
		return "", "", nil, errext.Errorf("gojira: render issue %s: %w", key, err)
	}

	// Map extract.Reference → render.OutboundRef using the same kind
	// mapping that crawl uses.
	outboundRefs := refsToOutbound(refs)

	// Render outbound.md.
	outboundMD, err = render.RenderOutbound(outboundRefs)
	if err != nil {
		return "", "", nil, errext.Errorf("gojira: render outbound %s: %w", key, err)
	}

	return indexMD, outboundMD, discoveredKeys, nil
}

// ---------------------------------------------------------------------------
// Capability 4: Crawl
// ---------------------------------------------------------------------------

// Crawl executes a full recursive Jira issue crawl starting from startKeys.
//
// It constructs the real HTTP client and fetcher from cfg, then delegates to
// the internal crawl orchestrator. Output files are written to cfg.OutputDir.
//
// sink receives structured events for every state transition (issue queued,
// fetched, skipped, stubbed, failed, cap reached, PR found, crawl summary).
// Pass nil to use [NoopSink] and discard all events.
//
// # Error handling
//
//   - 401 Unauthorized: the crawl is aborted immediately. Crawl returns a
//     partial [Summary] and an error wrapping [ErrUnauthorized]. Map to exit 1.
//   - 403 / 404: a stub index.md is written; the crawl continues.
//   - Rate limited (429, retries exhausted): counted in Summary.Failed.
//   - Network/transport error: a stub is written; the crawl continues.
//   - Context cancellation: in-flight fetches complete; no new fetches start.
//
// # Skip-if-exists
//
// When cfg.Refetch is false (the default), issues whose index.md already
// exists on disk are skipped without making an API call. This makes repeated
// runs additive and fast.
func Crawl(ctx context.Context, cfg Config, startKeys []string, sink Sink) (Summary, error) {
	if sink == nil {
		sink = events.NoopSink{}
	}

	// Build the HTTP client and fetcher.
	c, err := client.New(cfg)
	if err != nil {
		return Summary{}, errext.Errorf("gojira: build client: %w", err)
	}
	f := fetch.New(c)

	// Hierarchy discoverer shares the same *client.Client so the Epic Link
	// auto-detection lookup is cached across all worker goroutines. When
	// cfg.IncludeChildren is false the crawl skips the per-issue JQL
	// search; the discoverer is still constructed because the cost is
	// trivial.
	hier := hierarchy.New(c, cfg)

	// Dev Status enricher likewise shares the same *client.Client. The
	// per-issue customfield_10000 zero-PR gate lives inside the
	// enricher so we always construct it; cfg.IncludeDevStatus is the
	// hard opt-out switch and is enforced both by the crawl
	// orchestrator and inside the enricher itself.
	prs := devstatus.New(c, cfg)

	return crawl.CrawlWithEnrichers(ctx, cfg, startKeys, f, sink, hier, prs)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// refsToOutbound converts extract.Reference values to render.OutboundRef
// values using the same kind-label mapping as internal/crawl.
//
// Kind labels:
//
//	classify.KindJiraKey  → "jira"
//	classify.KindJiraURL  → "jira"
//	classify.KindGitHubPR → "github-pr"
//	classify.KindExternal → "external"
func refsToOutbound(refs []extract.Reference) []render.OutboundRef {
	out := make([]render.OutboundRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, render.OutboundRef{
			Kind:     kindLabel(r.Kind),
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

// kindLabel maps a classify.Kind to the string label expected by
// render.OutboundRef.Kind.
func kindLabel(k classify.Kind) string {
	switch k {
	case classify.KindJiraKey, classify.KindJiraURL:
		return "jira"
	case classify.KindGitHubPR:
		return "github-pr"
	default:
		return "external"
	}
}
