// Package grpcserver provides the gRPC server implementation for gojira.
package grpcserver

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
)

// Server implements the gojirav1.GojiraServer interface.
// It is single-tenant: one Jira identity is loaded at startup via cfg.
type Server struct {
	gojirav1.UnimplementedGojiraServer
	cfg gojira.Config
}

// NewServer constructs a Server with the given runtime configuration.
func NewServer(cfg gojira.Config) *Server {
	return &Server{cfg: cfg}
}

// Classify classifies a link or bare issue key into one of four kinds.
func (s *Server) Classify(_ context.Context, req *gojirav1.ClassifyRequest) (*gojirav1.ClassifyResponse, error) {
	result := gojira.Classify(req.GetInput(), s.cfg.Site)
	return &gojirav1.ClassifyResponse{
		Kind:     result.Kind.String(),
		IssueKey: result.IssueKey,
		Owner:    result.Owner,
		Repo:     result.Repo,
		PrNumber: int32(result.PRNumber),
		Url:      result.URL,
	}, nil
}

// GetIssue is not yet implemented.
func (s *Server) GetIssue(_ context.Context, _ *gojirav1.GetIssueRequest) (*gojirav1.GetIssueResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "GetIssue not yet implemented")
}

// Crawl is not yet implemented.
func (s *Server) Crawl(_ *gojirav1.CrawlRequest, _ grpc.ServerStreamingServer[gojirav1.CrawlEvent]) error {
	return status.Errorf(codes.Unimplemented, "Crawl not yet implemented")
}

// Compile-time assertion that *Server satisfies the GojiraServer interface.
var _ gojirav1.GojiraServer = (*Server)(nil)
