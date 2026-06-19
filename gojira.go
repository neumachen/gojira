// Package gojira is the public library facade for the gojira Jira-to-Markdown
// mirror tool. It exposes the full capability surface as one cohesive package
// so third-party programs can embed gojira without touching its internal
// packages or the CLI binary. The exported capabilities group into:
//
//   - Classification — [Classify] turns a URL or bare issue key into a typed
//     result (Jira key, Jira URL, GitHub PR, or external).
//   - Configuration — [LoadConfig], [LoadFileConfig], and [LoadAppConfig]
//     build and validate a runtime [Config] from a kv map, a YAML file, or
//     the full file < env cascade.
//   - Fetch and render — [GetIssue] returns one issue as structured typed
//     data; [FetchAndRender] is the convenience wrapper that also renders
//     Markdown; [ParseOutputFormat]/[OutputFormat] select the presentation
//     form.
//   - Crawl — [Crawl] (and [CrawlWithLogger]) run a full recursive crawl to
//     disk and return a [Summary]; [CrawlGraph] returns the discovered issue
//     graph in memory as a [GraphModel] ([GraphNode]/[GraphEdge]) instead of
//     (or in addition to) writing graph artifacts.
//   - Write operations — [CreateIssue], [UpdateIssue], [AddComment],
//     [ListTransitions], [TransitionIssue], and [TransitionIssueByStatus]
//     mutate Jira; [BuildCreateIssueBody]/[BuildUpdateIssueBody] expose the
//     request-body builders for dry-run previews.
//   - Event sinks — [Sink], [Event], [NoopSink], and [NewSlogSink] let
//     callers observe crawl progress.
//
// # Public surface invariants
//
//   - No flag parsing, CLI argument handling, or process-level signal handling.
//   - No os.Exit calls.
//   - No hard-coded credentials, Jira domains, or project keys.
//   - All internal packages are hidden; only the facade capabilities and
//     their supporting types are exported.
//   - The CLI binary (cmd/gojira) is a thin consumer of this package; it is
//     never required to use the library.
//
// # Minimal usage example (third-party consumer workflow)
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
	"strings"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/crawl"
	"github.com/neumachen/gojira/internal/devstatus"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/fetch"
	"github.com/neumachen/gojira/internal/graph"
	"github.com/neumachen/gojira/internal/hierarchy"
	"github.com/neumachen/gojira/internal/output"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/internal/render"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
)

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

// GraphModel is the in-memory issue graph produced by [CrawlGraph].
// It is a true alias of [graph.Model] so callers can pass values
// across the package boundary without conversion.
type GraphModel = graph.Model

// GraphNode is a single node in a [GraphModel]; one of three Kinds:
// "issue", "github_pr", or "external". Issue-only fields (Status,
// Type, Assignee, URL) are zero-valued for non-issue nodes.
type GraphNode = graph.Node

// GraphEdge is a directed relationship between two [GraphNode]s.
// Edge Kinds are: parent, subtask, child, link, remote, description,
// pull_request, external.
type GraphEdge = graph.Edge

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

	// ErrBadRequest is returned when Jira responds with 400 (validation
	// failure). The concrete error may be a [*client.APIError] carrying
	// the failing field names; errors.Is(err, ErrBadRequest) still
	// matches via Unwrap.
	ErrBadRequest = client.ErrBadRequest

	// ErrConflict is returned when Jira responds with 409, e.g. an
	// invalid workflow transition. As with ErrBadRequest, an *APIError
	// may wrap this sentinel while still satisfying errors.Is.
	ErrConflict = client.ErrConflict

	// ErrConfigMissingRequired is returned (wrapped) by [LoadConfig] /
	// [LoadAppConfig] when a required configuration value is absent.
	// It is a re-export of the internal config sentinel so callers can
	// errors.Is without importing the internal package.
	ErrConfigMissingRequired = config.ErrMissingRequired

	// ErrConfigInvalidValue is returned (wrapped) by [LoadConfig] /
	// [LoadAppConfig] when a configuration value fails validation
	// (bad URL, unknown enum, malformed integer, etc.). It is a
	// re-export of the internal config sentinel so callers can
	// errors.Is without importing the internal package.
	ErrConfigInvalidValue = config.ErrInvalidValue
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
// descriptive error. Use errors.Is with [ErrConfigMissingRequired] or
// [ErrConfigInvalidValue] to distinguish failure classes.
//
// LoadConfig is the legacy entry point preserved for backward compatibility
// with library consumers and the existing CLI flag-overlay pattern. New
// callers SHOULD prefer [LoadAppConfig], which loads through the full
// cascade (embedded defaults < YAML file < GOJIRA_ environment variables)
// and supports config-file discovery.
func LoadConfig(kv map[string]string) (Config, error) {
	return config.Build(kv)
}

// LoadFileConfig runs ONLY the YAML-file layer of the configuration
// cascade (embedded defaults + optional config file, with Layer-1 schema
// validation) and returns the flattened [Config]. It does NOT read
// environment variables and does NOT run the Layer-2 semantic validator,
// so a partial/missing-required configuration is NOT an error here. An
// explicit configPath pointing at a non-existent file IS a hard error
// wrapping [ErrConfigInvalidValue].
//
// LoadFileConfig is the seam the CLI uses when it wants to overlay
// environment variables and flag values on top of the file's
// contribution using its own validation path: the CLI calls
// LoadFileConfig, flattens the result onto a kv map, merges env and
// flag values, and runs the merged map through [LoadConfig]. This
// preserves the v0.1 *ConfigError error messages downstream tests and
// users depend on while still honoring the YAML file's contribution
// to the cascade.
//
// When configPath is empty, the standard discovery chain runs (see the
// LoadAppConfig docstring).
func LoadFileConfig(configPath string) (Config, error) {
	app, err := config.LoadFileLayer(configPath, nil)
	if err != nil {
		return Config{}, err
	}
	return app.ToConfig(), nil
}

// LoadAppConfig loads configuration through the full app-level cascade and
// returns a flattened [Config] ready for [Crawl] / [FetchAndRender]. The
// cascade order, lowest-to-highest precedence, is:
//
//  1. Embedded defaults (per-entity DefaultX constructors).
//  2. YAML config file: when configPath is non-empty, that file is opened;
//     when empty, the discovery chain runs (--config-equivalent: explicit
//     path → $GOJIRA_CONFIG_FILE → ./gojira.yaml →
//     $XDG_CONFIG_HOME/gojira/config.yaml → ~/.config/gojira/config.yaml).
//     An explicit but non-existent configPath is a hard error.
//  3. GOJIRA_-prefixed environment variables from env (the caller supplies
//     this map; the CLI passes a filtered snapshot of os.Environ, while
//     library consumers may inject any map). Deprecated v0.1 flat keys
//     (GOJIRA_SITE, GOJIRA_USER, GOJIRA_TOKEN, etc.) continue to work via
//     internal alias resolution.
//
// CLI-flag overrides, if any, are the caller's responsibility to apply
// to the returned Config. Keeping flags out of LoadAppConfig keeps this
// package free of CLI-library dependencies.
//
// On failure LoadAppConfig returns a zero Config and a descriptive error.
// Use errors.Is with [ErrConfigMissingRequired] or [ErrConfigInvalidValue]
// to distinguish failure classes — the exact same sentinels [LoadConfig]
// uses.
func LoadAppConfig(configPath string, env map[string]string) (Config, error) {
	app, err := config.LoadApp(config.LoadOptions{
		ConfigPath: configPath,
		Env:        env,
	})
	if err != nil {
		return Config{}, err
	}
	return app.ToConfig(), nil
}

// ---------------------------------------------------------------------------
// Capability 3: GetIssue / FetchAndRender
// ---------------------------------------------------------------------------

// GetIssue fetches a single Jira issue identified by key, parses its ADF
// description and relationships, and returns the structured typed data without
// rendering anything to Markdown.
//
// It is the "fetch" half of [FetchAndRender], exposed independently so callers
// such as MCP handlers or TUI components can obtain typed data without forcing
// a Markdown render pass.
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
//   - issue: the fully parsed [parse.Issue] value.
//   - refs: outbound references discovered by [extract.Extract], in extract's
//     documented order (description → parent → subtasks → issuelinks →
//     remotelinks). May contain duplicates; the caller is responsible for
//     deduplication.
//   - err: non-nil on fetch, parse, or extract failure.
func GetIssue(ctx context.Context, cfg Config, key string, opts ...client.Option) (parse.Issue, []extract.Reference, error) {
	// Build the HTTP client.
	c, err := client.New(cfg, opts...)
	if err != nil {
		return parse.Issue{}, nil, errext.Errorf("gojira: build client: %w", err)
	}

	// Fetch raw issue bytes.
	f := fetch.New(c)
	raw, err := f.Fetch(ctx, key)
	if err != nil {
		return parse.Issue{}, nil, errext.Errorf("gojira: fetch %s: %w", key, err)
	}

	// Parse.
	issue, err := parse.Parse(raw, cfg.Site)
	if err != nil {
		return parse.Issue{}, nil, errext.Errorf("gojira: parse %s: %w", key, err)
	}

	// Extract outbound references.
	refs, err := extract.Extract(issue, cfg.Site)
	if err != nil {
		return parse.Issue{}, nil, errext.Errorf("gojira: extract %s: %w", key, err)
	}

	return issue, refs, nil
}

// FetchAndRender fetches a single Jira issue identified by key, parses its
// ADF description and relationships, and returns the rendered Markdown content
// for both the issue page and its outbound reference index.
//
// It is a convenience wrapper over [GetIssue] + render. It shares all fetch,
// parse, and extract logic with [GetIssue]; only the render step is added here.
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
	// Delegate fetch→parse→extract to GetIssue.
	issue, refs, err := GetIssue(ctx, cfg, key, opts...)
	if err != nil {
		return "", "", nil, err
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
// Capability 5: Crawl
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
//
// # Output destination
//
// Crawl output is delivered through an injectable [output.Store]. This
// facade constructs the default — an [output.FSStore] that writes the
// canonical <key>/index.md and references/outbound.md layout to
// cfg.OutputDir, honoring skip-if-exists vs. refetch — and passes it
// through to the crawl orchestrator. Alternative Store implementations
// can be injected at the crawl layer for callers that need to deliver
// crawl output somewhere other than the local filesystem.
//
// # Observability
//
// Crawl emits no log output. To enable the structured observability
// instrument (per-issue spans, per-phase wall-clock measurement, HTTP
// request lifecycle traces, and the end-of-run crawl.measurement summary
// line) use [CrawlWithLogger] instead.
func Crawl(ctx context.Context, cfg Config, startKeys []string, sink Sink) (Summary, error) {
	return CrawlWithLogger(ctx, cfg, startKeys, sink, nil)
}

// CrawlWithLogger is the observability-aware sibling of [Crawl]. Identical
// behavior, plus: the supplied logger is wired through BOTH the crawl
// orchestrator (per-issue spans, per-phase measurement, crawl.measurement
// summary line) AND the underlying client (HTTP request lifecycle tracing
// via internal/httplog). The two share run_id/span_id/ticket_id correlation
// so trace_stream=stream lines and trace_stream=response lines from one
// invocation can be joined.
//
// A nil logger is equivalent to calling [Crawl] — no instrumentation is
// emitted and the on-disk behavior is unchanged. The signature is additive;
// existing callers of Crawl are unaffected.
//
// See log.LevelTrace and log.ParseLevel for the level ladder, and
// internal/trace for the correlation attribute keys (AttrRunID,
// AttrTicketID, AttrSpanID, AttrParentSpanID, AttrPhase, AttrTraceStream).
func CrawlWithLogger(ctx context.Context, cfg Config, startKeys []string, sink Sink, logger *slog.Logger) (Summary, error) {
	if sink == nil {
		sink = events.NoopSink{}
	}

	// Build the HTTP client. When a logger is supplied, install it via
	// client.WithLogger so HTTP requests emit the httplog RoundTripper's
	// response-stream lines with the same correlation context the crawl
	// orchestrator uses for stream-trace lines.
	var clientOpts []client.Option
	if logger != nil {
		clientOpts = append(clientOpts, client.WithLogger(logger))
	}
	c, err := client.New(cfg, clientOpts...)
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

	// Crawl output is delivered through an injectable output.Store. The
	// default is an FSStore that writes the canonical <key>/index.md and
	// references/outbound.md layout to cfg.OutputDir, honoring
	// skip-if-exists vs. refetch. Alternative Stores (e.g. for a future
	// service front-end) can be injected at the crawl layer.
	store := output.NewFSStore(cfg.OutputDir, cfg.Refetch)
	return crawl.CrawlWithEnrichers(ctx, cfg, startKeys, f, sink, hier, prs, store, logger)
}

// CrawlGraph runs a recursive crawl exactly like [Crawl] and returns
// the collected issue graph IN MEMORY as a [GraphModel], without ever
// writing graph.json or graph.d2 to disk. This is the entry point the
// gRPC GetGraph handler uses; it is also useful to library callers
// who want the graph programmatically.
//
// Graph collection is FORCED ON regardless of cfg.EmitGraph (the
// EmitGraph flag controls the disk-export side of the feature, which
// CrawlGraph deliberately bypasses). Per-issue Markdown is still
// produced through the normal output.Store when cfg.OutputDir is
// configured — only the graph files are suppressed.
//
// The returned [Summary] is identical to what [Crawl] would produce
// for the same inputs. Errors propagate unchanged through the
// existing sentinel surface ([ErrUnauthorized], [ErrNotFound], etc.).
func CrawlGraph(ctx context.Context, cfg Config, startKeys []string, sink Sink) (Summary, GraphModel, error) {
	if sink == nil {
		sink = events.NoopSink{}
	}
	c, err := client.New(cfg)
	if err != nil {
		return Summary{}, GraphModel{}, errext.Errorf("gojira: build client: %w", err)
	}
	f := fetch.New(c)
	hier := hierarchy.New(c, cfg)
	prs := devstatus.New(c, cfg)
	store := output.NewFSStore(cfg.OutputDir, cfg.Refetch)
	return crawl.CrawlGraphWithEnrichers(ctx, cfg, startKeys, f, sink, hier, prs, store, nil)
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

// ---------------------------------------------------------------------------
// Phase 2: Jira write operations (facade)
// ---------------------------------------------------------------------------
//
// These functions mirror [GetIssue]'s pattern: build a client from cfg,
// delegate to the client.* write method, wrap the error with errext.Errorf
// so the underlying sentinel / *client.APIError is preserved via %w.
// Field selection is supplied via the [client.CreateOption]/UpdateOption/
// CommentOption/TransitionOption sets — the extensibility seam locked in
// by phase-b-builder-2.

// CreateIssue creates a new Jira issue under the given project key with the
// supplied issue-type name. Field selection — summary, description, labels,
// assignee, parent, custom fields — flows through the [client.CreateOption]
// set. On success the returned [client.CreatedIssue] carries Jira's
// {key, id, self} response. On 400 the error is a [*client.APIError] that
// satisfies errors.Is against [ErrBadRequest] and exposes per-field
// validation messages via errors.As.
func CreateIssue(ctx context.Context, cfg Config, project, issueType string, opts ...client.CreateOption) (client.CreatedIssue, error) {
	c, err := client.New(cfg)
	if err != nil {
		return client.CreatedIssue{}, errext.Errorf("gojira: build client: %w", err)
	}
	got, err := c.CreateIssue(ctx, project, issueType, opts...)
	if err != nil {
		return client.CreatedIssue{}, errext.Errorf("gojira: create issue: %w", err)
	}
	return got, nil
}

// UpdateIssue edits fields on the issue identified by key. Field selection
// flows through [client.UpdateOption]. Jira returns 204 on success; this
// function returns nil. 400 surfaces a [*client.APIError] wrapping
// [ErrBadRequest]; 404 surfaces [ErrNotFound].
func UpdateIssue(ctx context.Context, cfg Config, key string, opts ...client.UpdateOption) error {
	c, err := client.New(cfg)
	if err != nil {
		return errext.Errorf("gojira: build client: %w", err)
	}
	if err := c.UpdateIssue(ctx, key, opts...); err != nil {
		return errext.Errorf("gojira: update issue %s: %w", key, err)
	}
	return nil
}

// AddComment appends a comment to the issue identified by key. The comment
// body is supplied via [client.WithCommentText] (plain text → ADF) or
// [client.WithCommentADF] (rich, caller-supplied ADF). The returned
// [client.Comment] carries Jira's id/author/created fields.
func AddComment(ctx context.Context, cfg Config, key string, opts ...client.CommentOption) (client.Comment, error) {
	c, err := client.New(cfg)
	if err != nil {
		return client.Comment{}, errext.Errorf("gojira: build client: %w", err)
	}
	got, err := c.AddComment(ctx, key, opts...)
	if err != nil {
		return client.Comment{}, errext.Errorf("gojira: add comment to %s: %w", key, err)
	}
	return got, nil
}

// ListTransitions returns the workflow transitions currently available for
// the issue identified by key. Jira surfaces only transitions whose
// preconditions are met for the issue's current state, so the result is
// workflow- and state-dependent.
func ListTransitions(ctx context.Context, cfg Config, key string) ([]client.Transition, error) {
	c, err := client.New(cfg)
	if err != nil {
		return nil, errext.Errorf("gojira: build client: %w", err)
	}
	got, err := c.ListTransitions(ctx, key)
	if err != nil {
		return nil, errext.Errorf("gojira: list transitions for %s: %w", key, err)
	}
	return got, nil
}

// TransitionIssue moves the issue identified by key through the workflow
// transition with id transitionID. Use [client.WithTransitionField] and
// [client.WithTransitionCommentText] to set fields or append a comment as
// part of the transition. Jira returns 204 on success.
func TransitionIssue(ctx context.Context, cfg Config, key, transitionID string, opts ...client.TransitionOption) error {
	c, err := client.New(cfg)
	if err != nil {
		return errext.Errorf("gojira: build client: %w", err)
	}
	if err := c.TransitionIssue(ctx, key, transitionID, opts...); err != nil {
		return errext.Errorf("gojira: transition %s via %s: %w", key, transitionID, err)
	}
	return nil
}

// TransitionIssueByStatus resolves the workflow transition whose target
// status name matches targetStatusName (case-insensitive) via
// [ListTransitions], then executes it.
//
// It returns a clear error when no transition matches (typically because
// the issue is not in a state from which that target is reachable) or
// when more than one transition shares the same target status — gojira
// will not silently pick one. The convenience costs one extra GET
// (the ListTransitions call) relative to passing a transition id directly
// to [TransitionIssue].
func TransitionIssueByStatus(ctx context.Context, cfg Config, key, targetStatusName string, opts ...client.TransitionOption) error {
	transitions, err := ListTransitions(ctx, cfg, key)
	if err != nil {
		return err
	}

	matches := make([]client.Transition, 0, 1)
	for _, t := range transitions {
		if strings.EqualFold(t.ToStatus, targetStatusName) {
			matches = append(matches, t)
		}
	}

	switch len(matches) {
	case 0:
		return errext.Errorf("gojira: no transition to status %q for %s", targetStatusName, key)
	case 1:
		return TransitionIssue(ctx, cfg, key, matches[0].ID, opts...)
	default:
		return errext.Errorf("gojira: ambiguous transition to status %q for %s (%d matches)",
			targetStatusName, key, len(matches))
	}
}

// ---------------------------------------------------------------------------
// Dry-run request body builders (phase-d-facade-3)
// ---------------------------------------------------------------------------

// BuildCreateIssueBody returns the JSON request body [CreateIssue] would
// POST, without contacting Jira. It is a pure pass-through over
// [client.RenderCreateBody], exposed at the facade so CLI / agent callers
// can preview a write before mutating.
func BuildCreateIssueBody(project, issueType string, opts ...client.CreateOption) ([]byte, error) {
	return client.RenderCreateBody(project, issueType, opts...)
}

// BuildUpdateIssueBody returns the JSON request body [UpdateIssue] would
// PUT, without contacting Jira. Like [BuildCreateIssueBody] it is a
// pure pass-through — no network, no client construction.
func BuildUpdateIssueBody(opts ...client.UpdateOption) ([]byte, error) {
	return client.RenderUpdateBody(opts...)
}
