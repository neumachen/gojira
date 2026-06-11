// Package grpcserver provides the gRPC server implementation for gojira.
package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/internal/render"
	"github.com/neumachen/gojira/pkg/client"
)

// Server implements the gojirav1.GojiraServer interface.
// It is single-tenant: one Jira identity is loaded at startup via cfg.
//
// The getIssueFn and crawlFn fields are function-field seams that default
// to closures over the gojira facade in [NewServer]. Tests in the same
// package (white-box) overwrite them with fakes so handler behavior can
// be exercised without making any network calls.
type Server struct {
	gojirav1.UnimplementedGojiraServer
	cfg gojira.Config

	// getIssueFn fetches+parses+extracts a single issue. Defaults to a
	// closure over gojira.GetIssue. Overridable in tests.
	getIssueFn func(ctx context.Context, cfg gojira.Config, key string) (parse.Issue, []extract.Reference, error)

	// crawlFn runs a recursive crawl. Defaults to a closure over
	// gojira.Crawl. Overridable in tests.
	crawlFn func(ctx context.Context, cfg gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, error)

	// crawlGraphFn runs an in-memory graph-only crawl and returns
	// the discovered [gojira.GraphModel] alongside the Summary,
	// without writing graph.json / graph.d2 to disk. Defaults to a
	// closure over gojira.CrawlGraph. Overridable in tests via
	// [WithCrawlGraphFunc].
	crawlGraphFn func(ctx context.Context, cfg gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, gojira.GraphModel, error)

	// Phase-2 write seams. Each defaults in [NewServer] to a closure
	// over the matching gojira facade function and is overridable via
	// the With*Func [Option] constructors below. Keeping them on the
	// Server (rather than embedding a separate "writes" sub-struct)
	// matches the read-side getIssueFn/crawlFn pattern and keeps the
	// test wiring (NewServer(cfg, WithXxxFunc(fake))) uniform.
	createIssueFn func(ctx context.Context, cfg gojira.Config, project, issueType string, opts ...client.CreateOption) (client.CreatedIssue, error)
	updateIssueFn func(ctx context.Context, cfg gojira.Config, key string, opts ...client.UpdateOption) error
	addCommentFn  func(ctx context.Context, cfg gojira.Config, key string, opts ...client.CommentOption) (client.Comment, error)

	// listTransitionsFn backs the ListTransitions handler AND the
	// by-name resolution path of TransitionIssue. Keeping a single
	// seam lets a test cover both paths with one fake.
	listTransitionsFn func(ctx context.Context, cfg gojira.Config, key string) ([]client.Transition, error)
	transitionIssueFn func(ctx context.Context, cfg gojira.Config, key, transitionID string, opts ...client.TransitionOption) error
}

// Option mutates a Server during construction. Options are applied
// after the production defaults are wired by [NewServer], so they
// always overwrite — never accidentally pre-empt — the default
// behavior. Options are intended primarily for tests (see
// [WithGetIssueFunc] and [WithCrawlFunc]) but are exported so
// integration tests outside the package can compose them.
type Option func(*Server)

// WithGetIssueFunc overrides the function used by [Server.GetIssue]
// to fetch+parse+extract a single issue. The default closes over
// [gojira.GetIssue]; tests inject a fake to avoid touching the
// network or a live Jira tenant.
func WithGetIssueFunc(fn func(ctx context.Context, cfg gojira.Config, key string) (parse.Issue, []extract.Reference, error)) Option {
	return func(s *Server) { s.getIssueFn = fn }
}

// WithCrawlFunc overrides the function used by [Server.Crawl] to
// run a recursive crawl. The default closes over [gojira.Crawl];
// tests inject a fake that emits events through the supplied sink
// without performing real fetches.
func WithCrawlFunc(fn func(ctx context.Context, cfg gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, error)) Option {
	return func(s *Server) { s.crawlFn = fn }
}

// WithCrawlGraphFunc overrides the function used by [Server.GetGraph]
// to run an in-memory graph-only crawl. The default closes over
// [gojira.CrawlGraph]; tests inject a fake that returns a fixed
// [gojira.GraphModel] without touching the network.
func WithCrawlGraphFunc(fn func(ctx context.Context, cfg gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, gojira.GraphModel, error)) Option {
	return func(s *Server) { s.crawlGraphFn = fn }
}

// WithCreateIssueFunc overrides the function used by [Server.CreateIssue].
// The default closes over [gojira.CreateIssue]; tests inject a fake to
// avoid touching Jira.
func WithCreateIssueFunc(fn func(ctx context.Context, cfg gojira.Config, project, issueType string, opts ...client.CreateOption) (client.CreatedIssue, error)) Option {
	return func(s *Server) { s.createIssueFn = fn }
}

// WithUpdateIssueFunc overrides the function used by [Server.UpdateIssue].
// The default closes over [gojira.UpdateIssue].
func WithUpdateIssueFunc(fn func(ctx context.Context, cfg gojira.Config, key string, opts ...client.UpdateOption) error) Option {
	return func(s *Server) { s.updateIssueFn = fn }
}

// WithAddCommentFunc overrides the function used by [Server.AddComment].
// The default closes over [gojira.AddComment].
func WithAddCommentFunc(fn func(ctx context.Context, cfg gojira.Config, key string, opts ...client.CommentOption) (client.Comment, error)) Option {
	return func(s *Server) { s.addCommentFn = fn }
}

// WithListTransitionsFunc overrides the function used by
// [Server.ListTransitions]. The default closes over
// [gojira.ListTransitions].
func WithListTransitionsFunc(fn func(ctx context.Context, cfg gojira.Config, key string) ([]client.Transition, error)) Option {
	return func(s *Server) { s.listTransitionsFn = fn }
}

// WithTransitionIssueFunc overrides the function used by the id-based
// path of [Server.TransitionIssue]. The default closes over
// [gojira.TransitionIssue]. The by-name path additionally uses
// [WithListTransitionsFunc] to resolve a target status to an id.
func WithTransitionIssueFunc(fn func(ctx context.Context, cfg gojira.Config, key, transitionID string, opts ...client.TransitionOption) error) Option {
	return func(s *Server) { s.transitionIssueFn = fn }
}

// NewServer constructs a Server with the given runtime configuration and
// the production gojira facade wired into the injectable seams. Each
// supplied [Option] is applied after the defaults, allowing tests to
// substitute fakes without touching unexported fields.
func NewServer(cfg gojira.Config, opts ...Option) *Server {
	s := &Server{
		cfg: cfg,
		getIssueFn: func(ctx context.Context, cfg gojira.Config, key string) (parse.Issue, []extract.Reference, error) {
			return gojira.GetIssue(ctx, cfg, key)
		},
		crawlFn: func(ctx context.Context, cfg gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, error) {
			return gojira.Crawl(ctx, cfg, startKeys, sink)
		},
		crawlGraphFn: func(ctx context.Context, cfg gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, gojira.GraphModel, error) {
			return gojira.CrawlGraph(ctx, cfg, startKeys, sink)
		},
		createIssueFn: func(ctx context.Context, cfg gojira.Config, project, issueType string, opts ...client.CreateOption) (client.CreatedIssue, error) {
			return gojira.CreateIssue(ctx, cfg, project, issueType, opts...)
		},
		updateIssueFn: func(ctx context.Context, cfg gojira.Config, key string, opts ...client.UpdateOption) error {
			return gojira.UpdateIssue(ctx, cfg, key, opts...)
		},
		addCommentFn: func(ctx context.Context, cfg gojira.Config, key string, opts ...client.CommentOption) (client.Comment, error) {
			return gojira.AddComment(ctx, cfg, key, opts...)
		},
		listTransitionsFn: func(ctx context.Context, cfg gojira.Config, key string) ([]client.Transition, error) {
			return gojira.ListTransitions(ctx, cfg, key)
		},
		transitionIssueFn: func(ctx context.Context, cfg gojira.Config, key, transitionID string, opts ...client.TransitionOption) error {
			return gojira.TransitionIssue(ctx, cfg, key, transitionID, opts...)
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Classify classifies a link or bare issue key into one of four kinds.
//
// The request's jira_site field overrides the server's configured Site
// when non-empty. This lets a single server instance classify URLs from
// multiple Jira tenants without reconfiguration. When jira_site is empty
// the server falls back to its configured cfg.Site, preserving the
// historical single-tenant behavior.
func (s *Server) Classify(_ context.Context, req *gojirav1.ClassifyRequest) (*gojirav1.ClassifyResponse, error) {
	site := req.GetJiraSite()
	if site == "" {
		site = s.cfg.Site
	}
	result := gojira.Classify(req.GetInput(), site)
	return &gojirav1.ClassifyResponse{
		Kind:     result.Kind.String(),
		IssueKey: result.IssueKey,
		Owner:    result.Owner,
		Repo:     result.Repo,
		PrNumber: int32(result.PRNumber),
		Url:      result.URL,
	}, nil
}

// GetIssue fetches a single Jira issue and returns it in the requested
// output format. The fetch+parse+extract step is delegated to the
// injectable getIssueFn seam so handler tests can run without network.
//
// Format selection:
//
//   - OUTPUT_FORMAT_UNSPECIFIED and OUTPUT_FORMAT_STRUCTURED: the typed
//     proto Issue (with mapped References) is returned.
//   - OUTPUT_FORMAT_MARKDOWN: a single-issue Markdown render using
//     [render.RenderIssue] (with an empty neighbours set, since no
//     crawl context is available for relative-link resolution).
//   - OUTPUT_FORMAT_JSON: an indented JSON payload via
//     [render.RenderIssueJSON].
//
// Errors from getIssueFn are mapped to gRPC status codes via
// [toStatusError]; render errors map to codes.Internal.
func (s *Server) GetIssue(ctx context.Context, req *gojirav1.GetIssueRequest) (*gojirav1.GetIssueResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "issue key is required")
	}

	issue, refs, err := s.getIssueFn(ctx, s.cfg, key)
	if err != nil {
		return nil, toStatusError(err)
	}

	switch req.GetFormat() {
	case gojirav1.OutputFormat_OUTPUT_FORMAT_UNSPECIFIED,
		gojirav1.OutputFormat_OUTPUT_FORMAT_STRUCTURED:
		return &gojirav1.GetIssueResponse{
			Result: &gojirav1.GetIssueResponse_Issue{Issue: issueToProto(issue, refs)},
		}, nil

	case gojirav1.OutputFormat_OUTPUT_FORMAT_MARKDOWN:
		md, renderErr := render.RenderIssue(issue, map[string]bool{}, s.cfg.RenderNullCustomFields)
		if renderErr != nil {
			return nil, status.Errorf(codes.Internal, "render markdown: %v", renderErr)
		}
		return &gojirav1.GetIssueResponse{
			Result: &gojirav1.GetIssueResponse_Markdown{Markdown: md},
		}, nil

	case gojirav1.OutputFormat_OUTPUT_FORMAT_JSON:
		j, renderErr := render.RenderIssueJSON(issue, refs)
		if renderErr != nil {
			return nil, status.Errorf(codes.Internal, "render json: %v", renderErr)
		}
		return &gojirav1.GetIssueResponse{
			Result: &gojirav1.GetIssueResponse_Json{Json: j},
		}, nil

	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported output format: %v", req.GetFormat())
	}
}

// Crawl runs a recursive Jira crawl seeded from req.StartKeys, streaming
// each crawl event back to the client as a CrawlEvent proto message.
//
// A per-RPC [grpcSink] is constructed for this stream so concurrent
// Crawl RPCs do not share sink state. Crawl output (rendered Markdown
// for each issue) is persisted server-side via the facade's default
// FSStore at cfg.OutputDir; delivering content over the wire is
// deferred to Phase 2.
//
// Terminal errors from the crawl orchestrator are mapped to gRPC status
// codes via [toStatusError] (e.g. ErrUnauthorized → Unauthenticated).
func (s *Server) Crawl(req *gojirav1.CrawlRequest, stream grpc.ServerStreamingServer[gojirav1.CrawlEvent]) error {
	keys := req.GetStartKeys()
	if len(keys) == 0 {
		return status.Error(codes.InvalidArgument, "at least one start key is required")
	}

	// One sink per RPC: no shared mutable state between concurrent
	// Crawl invocations. gojira.Sink is a type alias of events.Sink,
	// so the value returned by NewGRPCStreamSink is assignable directly.
	sink := NewGRPCStreamSink(stream)

	if _, err := s.crawlFn(stream.Context(), s.cfg, keys, sink); err != nil {
		return toStatusError(err)
	}
	return nil
}

// GetGraph runs an in-memory graph-only crawl and returns the
// discovered issue graph as {nodes, edges}. Mirrors the graph.json
// schema produced by the CLI's --graph flag — same node Kinds
// ("issue", "github_pr", "external") and edge Kinds
// ("parent", "subtask", "child", "link", "remote", "description",
// "pull_request", "external").
//
// Per-request crawl knobs (depth_limit, issue_cap, time_cap_seconds,
// concurrency, include_children, include_dev_status) are applied via
// a per-RPC COPY of s.cfg so concurrent GetGraph calls do not race
// on shared mutable state. Zero values fall through to the server's
// configured defaults.
//
// Errors from the underlying crawl flow through [toStatusError]; an
// empty start_keys list returns codes.InvalidArgument.
func (s *Server) GetGraph(ctx context.Context, req *gojirav1.GetGraphRequest) (*gojirav1.GetGraphResponse, error) {
	keys := req.GetStartKeys()
	if len(keys) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one start key is required")
	}

	// Per-RPC cfg copy: apply per-request knob overrides without
	// mutating s.cfg (which is read by every concurrent handler).
	// Zero values mean "use server default", so we forward them
	// only when non-zero. Bool fields are simply applied as-is —
	// proto3 cannot distinguish "unset" from "false", and the
	// underlying Crawl handler does not honor per-request bool
	// overrides today either; documenting both forms in the proto
	// keeps the door open for a future "as-is" semantic without a
	// breaking change.
	cfg := s.cfg
	if v := req.GetDepthLimit(); v != 0 {
		cfg.DepthLimit = int(v)
	}
	if v := req.GetIssueCap(); v != 0 {
		cfg.IssueCap = int(v)
	}
	if v := req.GetTimeCapSeconds(); v != 0 {
		cfg.TimeCapSeconds = int(v)
	}
	if v := req.GetConcurrency(); v != 0 {
		cfg.Concurrency = int(v)
	}
	cfg.IncludeChildren = req.GetIncludeChildren()
	cfg.IncludeDevStatus = req.GetIncludeDevStatus()

	// GetGraph is unary; events are not streamed back to the caller.
	// The underlying crawl still emits to a sink, so we pass a
	// discarding sink. gojira.CrawlGraph also accepts nil and
	// substitutes events.NoopSink internally; we pass nil so the
	// fake injected by tests sees the same shape callers do.
	sum, model, err := s.crawlGraphFn(ctx, cfg, keys, nil)
	if err != nil {
		return nil, toStatusError(err)
	}
	_ = sum // GetGraph does not surface the summary today

	return graphModelToProto(model), nil
}

// graphModelToProto converts a [gojira.GraphModel] into the wire
// [gojirav1.GetGraphResponse]. The field mapping is 1:1 and the node
// Kinds / edge Kinds are forwarded verbatim as the strings the
// graph package emits, matching the graph.json schema.
func graphModelToProto(m gojira.GraphModel) *gojirav1.GetGraphResponse {
	nodes := make([]*gojirav1.GraphNode, 0, len(m.Nodes))
	for _, n := range m.Nodes {
		nodes = append(nodes, &gojirav1.GraphNode{
			Id:       n.ID,
			Kind:     string(n.Kind),
			Label:    n.Label,
			Status:   n.Status,
			Type:     n.Type,
			Assignee: n.Assignee,
			Url:      n.URL,
			Fetched:  n.Fetched,
		})
	}
	edges := make([]*gojirav1.GraphEdge, 0, len(m.Edges))
	for _, e := range m.Edges {
		edges = append(edges, &gojirav1.GraphEdge{
			From:  e.From,
			To:    e.To,
			Kind:  string(e.Kind),
			Label: e.Label,
		})
	}
	return &gojirav1.GetGraphResponse{Nodes: nodes, Edges: edges}
}

// Compile-time assertion that *Server satisfies the GojiraServer interface.
var _ gojirav1.GojiraServer = (*Server)(nil)

// toStatusError maps a gojira facade / crawl orchestrator error onto a
// gRPC [*status.Status] using the public sentinels exported from the
// gojira package. Unknown errors map to [codes.Internal]. nil is
// returned for a nil input so callers can use it unconditionally.
func toStatusError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, gojira.ErrUnauthorized):
		return status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, gojira.ErrForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, gojira.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, gojira.ErrRateLimited):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, gojira.ErrConfigMissingRequired),
		errors.Is(err, gojira.ErrConfigInvalidValue):
		return status.Error(codes.FailedPrecondition, err.Error())
	// Phase 2 write sentinels. A *client.APIError wraps these via
	// Unwrap(), so errors.Is classifies correctly for both the bare
	// sentinel and the typed write error; err.Error() carries the
	// per-field Jira detail and flows into the status Message
	// verbatim — no special-case extraction needed.
	case errors.Is(err, client.ErrBadRequest):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, client.ErrConflict):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// ---------------------------------------------------------------------------
// parse.Issue / extract.Reference → proto mapping helpers
// ---------------------------------------------------------------------------

// issueToProto converts a parsed Jira issue and its extracted references
// to the wire-level Issue proto message. Fields not present on
// [parse.Issue] are left zero-valued (in particular Project, which has
// no equivalent on the source type and is deliberately not inferred
// from the Key prefix to avoid inventing data). DevStatus and
// DescriptionPlaceholder are reserved for future wiring.
func issueToProto(issue parse.Issue, refs []extract.Reference) *gojirav1.Issue {
	out := &gojirav1.Issue{
		Key:          issue.Key,
		NumericId:    issue.NumericID,
		Summary:      issue.Summary,
		Status:       issue.Status,
		IssueType:    issue.IssueType,
		Assignee:     issue.Assignee,
		Reporter:     issue.Reporter,
		SourceUrl:    issue.SourceURL,
		Children:     append([]string(nil), issue.Children...),
		CustomFields: customFieldsToProto(issue.CustomFields),
		References:   referencesToProto(refs),
	}

	if !issue.Created.IsZero() {
		out.Created = timestamppb.New(issue.Created)
	}
	if !issue.Updated.IsZero() {
		out.Updated = timestamppb.New(issue.Updated)
	}

	if issue.Parent != nil {
		out.ParentKey = issue.Parent.Key
	}

	if len(issue.Subtasks) > 0 {
		out.Subtasks = make([]*gojirav1.LinkedIssueRef, 0, len(issue.Subtasks))
		for _, st := range issue.Subtasks {
			out.Subtasks = append(out.Subtasks, &gojirav1.LinkedIssueRef{
				Key:     st.Key,
				Summary: st.Summary,
				// parse.LinkedIssue carries no Status; leave empty.
			})
		}
	}

	if len(issue.IssueLinks) > 0 {
		out.IssueLinks = make([]*gojirav1.IssueLinkRef, 0, len(issue.IssueLinks))
		for _, link := range issue.IssueLinks {
			out.IssueLinks = append(out.IssueLinks, &gojirav1.IssueLinkRef{
				Direction: link.Direction,
				LinkType:  link.Type,
				Issue: &gojirav1.LinkedIssueRef{
					Key:     link.Key,
					Summary: link.Summary,
				},
			})
		}
	}

	if len(issue.RemoteLinks) > 0 {
		out.RemoteLinks = make([]*gojirav1.RemoteLinkRef, 0, len(issue.RemoteLinks))
		for _, rl := range issue.RemoteLinks {
			out.RemoteLinks = append(out.RemoteLinks, &gojirav1.RemoteLinkRef{
				Title: rl.Title,
				Url:   rl.URL,
			})
		}
	}

	return out
}

// customFieldsToProto converts the raw-JSON custom-field map carried by
// [parse.Issue] into the string-valued map exposed on the wire. Each
// raw JSON value is preserved verbatim; lossless round-tripping is the
// responsibility of the caller.
func customFieldsToProto(in map[string]json.RawMessage) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = string(v)
	}
	return out
}

// referencesToProto maps a slice of extract.References to their proto
// counterparts, preserving extract's documented ordering.
func referencesToProto(refs []extract.Reference) []*gojirav1.Reference {
	if len(refs) == 0 {
		return nil
	}
	out := make([]*gojirav1.Reference, 0, len(refs))
	for _, r := range refs {
		out = append(out, referenceToProto(r))
	}
	return out
}

// referenceToProto maps a single extract.Reference to its proto form.
// Owner/Repo/PrNumber are sourced from the embedded classify.Result so
// they remain populated for GitHubPR references without duplicating
// the data on extract.Reference itself.
func referenceToProto(r extract.Reference) *gojirav1.Reference {
	return &gojirav1.Reference{
		Kind:     r.Kind.String(),
		IssueKey: r.IssueKey,
		Url:      r.URL,
		Owner:    r.ClassifyResult.Owner,
		Repo:     r.ClassifyResult.Repo,
		PrNumber: int32(r.ClassifyResult.PRNumber),
		Title:    r.Text,
		Source:   r.Source.String(),
		Relation: r.Relation,
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Jira write-operation handlers
// ---------------------------------------------------------------------------
//
// Each handler stays a thin shim: validate the request, build client
// options from the proto fields, run either the dry-run body builder or
// the injectable seam, and map the result (or error) onto the proto
// response. Sentinel → gRPC code mapping flows through [toStatusError]
// (extended in phase-f-server-3 to know about ErrBadRequest/ErrConflict
// plus the *client.APIError they wrap).

// rawFieldToValue decodes a single raw_fields entry. The wire shape is
// map<string,string> where each value is a raw JSON literal — that's
// how the gRPC layer keeps the proto contract simple while still
// supporting arbitrary Jira custom-field shapes (numbers, objects,
// arrays). Decoding it into an `any` lets the [client.WithField] /
// [client.WithFieldUpdate] options produce the correct JSON wire form.
// When a value is not valid JSON we fall back to the raw string — a
// common case where the caller forgot to quote a string literal.
func rawFieldToValue(raw string) any {
	if raw == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	return v
}

// rawFieldsToCreateOpts turns the proto raw_fields map into a slice of
// CreateOption entries via WithField. Order within a single proto map
// is undefined, but the resulting fields object treats every entry
// independently, so the order is irrelevant for correctness.
func rawFieldsToCreateOpts(m map[string]string) []client.CreateOption {
	if len(m) == 0 {
		return nil
	}
	out := make([]client.CreateOption, 0, len(m))
	for k, v := range m {
		out = append(out, client.WithField(k, rawFieldToValue(v)))
	}
	return out
}

// rawFieldsToUpdateOpts is the Update-side counterpart to
// rawFieldsToCreateOpts.
func rawFieldsToUpdateOpts(m map[string]string) []client.UpdateOption {
	if len(m) == 0 {
		return nil
	}
	out := make([]client.UpdateOption, 0, len(m))
	for k, v := range m {
		out = append(out, client.WithFieldUpdate(k, rawFieldToValue(v)))
	}
	return out
}

// CreateIssue creates a new Jira issue. When dry_run is set, the
// returned response carries dry_run_body (the JSON body the server
// would have POSTed) and the createIssueFn seam is not invoked, so
// callers can preview a write before mutating.
func (s *Server) CreateIssue(ctx context.Context, req *gojirav1.CreateIssueRequest) (*gojirav1.CreateIssueResponse, error) {
	project := req.GetProject()
	if project == "" {
		return nil, status.Error(codes.InvalidArgument, "project is required")
	}
	issueType := req.GetIssueType()
	if issueType == "" {
		return nil, status.Error(codes.InvalidArgument, "issue_type is required")
	}

	opts := make([]client.CreateOption, 0, 6)
	if v := req.GetSummary(); v != "" {
		opts = append(opts, client.WithSummary(v))
	}
	if v := req.GetDescription(); v != "" {
		opts = append(opts, client.WithDescriptionText(v))
	}
	if v := req.GetLabels(); len(v) > 0 {
		opts = append(opts, client.WithLabels(v...))
	}
	if v := req.GetParentKey(); v != "" {
		opts = append(opts, client.WithParent(v))
	}
	opts = append(opts, rawFieldsToCreateOpts(req.GetRawFields())...)

	if req.GetDryRun() {
		body, err := gojira.BuildCreateIssueBody(project, issueType, opts...)
		if err != nil {
			return nil, toStatusError(err)
		}
		return &gojirav1.CreateIssueResponse{DryRunBody: body}, nil
	}

	res, err := s.createIssueFn(ctx, s.cfg, project, issueType, opts...)
	if err != nil {
		return nil, toStatusError(err)
	}
	return &gojirav1.CreateIssueResponse{
		Key:  res.Key,
		Id:   res.ID,
		Self: res.Self,
	}, nil
}

// UpdateIssue edits fields on an existing Jira issue. dry_run mirrors
// the [CreateIssue] semantics: when set, the seam is NOT invoked and
// the response carries dry_run_body with Ok=false (no change made).
func (s *Server) UpdateIssue(ctx context.Context, req *gojirav1.UpdateIssueRequest) (*gojirav1.UpdateIssueResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	opts := make([]client.UpdateOption, 0, 4)
	if v := req.GetSummary(); v != "" {
		opts = append(opts, client.WithSummaryUpdate(v))
	}
	if v := req.GetDescription(); v != "" {
		opts = append(opts, client.WithDescriptionTextUpdate(v))
	}
	opts = append(opts, rawFieldsToUpdateOpts(req.GetRawFields())...)

	if req.GetDryRun() {
		body, err := gojira.BuildUpdateIssueBody(opts...)
		if err != nil {
			return nil, toStatusError(err)
		}
		return &gojirav1.UpdateIssueResponse{Ok: false, DryRunBody: body}, nil
	}

	if err := s.updateIssueFn(ctx, s.cfg, key, opts...); err != nil {
		return nil, toStatusError(err)
	}
	return &gojirav1.UpdateIssueResponse{Ok: true}, nil
}

// AddComment appends a plain-text comment to an issue. The text is
// converted to ADF inside the client via [client.WithCommentText].
func (s *Server) AddComment(ctx context.Context, req *gojirav1.AddCommentRequest) (*gojirav1.AddCommentResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	c, err := s.addCommentFn(ctx, s.cfg, key, client.WithCommentText(req.GetBodyText()))
	if err != nil {
		return nil, toStatusError(err)
	}
	return &gojirav1.AddCommentResponse{
		Id:                c.ID,
		AuthorDisplayName: c.AuthorDisplayName,
		Created:           c.Created,
	}, nil
}

// ListTransitions lists the workflow transitions currently available
// for the given issue.
func (s *Server) ListTransitions(ctx context.Context, req *gojirav1.ListTransitionsRequest) (*gojirav1.ListTransitionsResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	ts, err := s.listTransitionsFn(ctx, s.cfg, key)
	if err != nil {
		return nil, toStatusError(err)
	}
	out := make([]*gojirav1.Transition, 0, len(ts))
	for _, t := range ts {
		out = append(out, &gojirav1.Transition{
			Id:       t.ID,
			Name:     t.Name,
			ToStatus: t.ToStatus,
		})
	}
	return &gojirav1.ListTransitionsResponse{Transitions: out}, nil
}

// TransitionIssue moves an issue through a workflow transition,
// selected either by transition_id (direct) or by target_status_name
// (resolved in this handler via the same listTransitionsFn seam used
// by [Server.ListTransitions], so a single fake covers both paths).
//
// Resolution rules: case-insensitive ToStatus match; zero matches is
// codes.NotFound, more than one match is codes.FailedPrecondition
// (gojira refuses to silently pick one). The optional comment_text is
// passed through as [client.WithTransitionCommentText].
func (s *Server) TransitionIssue(ctx context.Context, req *gojirav1.TransitionIssueRequest) (*gojirav1.TransitionIssueResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	transitionID := req.GetTransitionId()
	if transitionID == "" {
		name := req.GetTargetStatusName()
		if name == "" {
			return nil, status.Error(codes.InvalidArgument,
				"transition_id or target_status_name is required")
		}
		ts, err := s.listTransitionsFn(ctx, s.cfg, key)
		if err != nil {
			return nil, toStatusError(err)
		}
		matches := make([]client.Transition, 0, 1)
		for _, t := range ts {
			if strings.EqualFold(t.ToStatus, name) {
				matches = append(matches, t)
			}
		}
		switch len(matches) {
		case 0:
			return nil, status.Errorf(codes.NotFound,
				"no transition to status %q for %s", name, key)
		case 1:
			transitionID = matches[0].ID
		default:
			return nil, status.Errorf(codes.FailedPrecondition,
				"ambiguous transition to status %q for %s (%d matches)",
				name, key, len(matches))
		}
	}

	opts := make([]client.TransitionOption, 0, 1)
	if v := req.GetCommentText(); v != "" {
		opts = append(opts, client.WithTransitionCommentText(v))
	}

	if err := s.transitionIssueFn(ctx, s.cfg, key, transitionID, opts...); err != nil {
		return nil, toStatusError(err)
	}
	return &gojirav1.TransitionIssueResponse{Ok: true}, nil
}
