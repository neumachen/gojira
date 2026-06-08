// Package grpcserver provides the gRPC server implementation for gojira.
package grpcserver

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/client"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/internal/render"
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
