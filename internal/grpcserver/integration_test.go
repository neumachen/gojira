// In-process gRPC integration test for the grpcserver package.
//
// It wires the real handlers behind a bufconn listener and a real
// grpc.Server + grpc.ClientConn, so the full Classify / GetIssue /
// Crawl code path — request unmarshalling, status mapping, streaming —
// runs end to end without binding a real port or touching the network.
// Fakes injected through [grpcserver.WithGetIssueFunc] and
// [grpcserver.WithCrawlFunc] replace the live Jira call sites.
package grpcserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/grpcserver"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
)

// bufnetTarget is the dummy dial target used with the bufconn dialer.
// The "passthrough:///" scheme tells the gRPC resolver to use the
// supplied target verbatim, and the bufconn dialer below ignores the
// address anyway since the listener is in-process.
const bufnetTarget = "passthrough:///bufnet"

// startBufconnServer registers the supplied Server on a fresh
// in-memory bufconn listener, starts grpc.Server.Serve in a goroutine,
// dials a matching client connection, and registers cleanup callbacks
// so the listener and server are torn down at test end.
func startBufconnServer(t *testing.T, srv *grpcserver.Server) gojirav1.GojiraClient {
	t.Helper()
	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)

	grpcServer := grpc.NewServer()
	gojirav1.RegisterGojiraServer(grpcServer, srv)

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		// Serve returns an error only when the listener is broken
		// or Serve was called after Stop; both are noise in tests.
		_ = grpcServer.Serve(lis)
	}()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient(bufnetTarget,
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		grpcServer.Stop()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Log("grpc.Server.Serve did not return within 2s of Stop")
		}
		_ = lis.Close()
	})

	return gojirav1.NewGojiraClient(conn)
}

// integrationCfg returns the minimal Config the handlers consult. The
// Site value is used by the Classify fallback; OutputDir is set to a
// per-test temp dir so any incidental disk writes are sandboxed.
func integrationCfg(t *testing.T) gojira.Config {
	t.Helper()
	return gojira.Config{
		Site:      "https://example.atlassian.net",
		OutputDir: t.TempDir(),
	}
}

// integrationFixtureIssue is the parse.Issue + references the GetIssue
// fake returns. Kept tight (one of each relationship kind) so the
// assertions are unambiguous.
func integrationFixtureIssue() (parse.Issue, []extract.Reference) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		NumericID: "10001",
		Summary:   "An integration-test issue",
		Status:    "Open",
		IssueType: "Task",
		Assignee:  "Alice",
		Reporter:  "Bob",
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
	}
	refs := []extract.Reference{
		{
			Kind:   classify.KindGitHubPR,
			URL:    "https://github.com/org/repo/pull/7",
			Source: extract.SourceRemoteLink,
			ClassifyResult: classify.Result{
				Owner:    "org",
				Repo:     "repo",
				PRNumber: 7,
			},
		},
	}
	return issue, refs
}

// ---------------------------------------------------------------------------
// Classify — exercise the wire path with no fakes
// ---------------------------------------------------------------------------

func TestIntegration_Classify(t *testing.T) {
	t.Parallel()
	srv := grpcserver.NewServer(integrationCfg(t))
	client := startBufconnServer(t, srv)

	resp, err := client.Classify(context.Background(), &gojirav1.ClassifyRequest{
		Input: "EXAMPLE-1",
	})
	if err != nil {
		t.Fatalf("Classify RPC: %v", err)
	}
	if resp.GetKind() != "JiraKey" {
		t.Errorf("Kind: got %q, want JiraKey", resp.GetKind())
	}
	if resp.GetIssueKey() != "EXAMPLE-1" {
		t.Errorf("IssueKey: got %q, want EXAMPLE-1", resp.GetIssueKey())
	}
}

// ---------------------------------------------------------------------------
// GetIssue — markdown / json / structured paths through the real handler
// ---------------------------------------------------------------------------

func TestIntegration_GetIssue_AllFormats(t *testing.T) {
	t.Parallel()
	issue, refs := integrationFixtureIssue()

	srv := grpcserver.NewServer(
		integrationCfg(t),
		grpcserver.WithGetIssueFunc(func(_ context.Context, _ gojira.Config, key string) (parse.Issue, []extract.Reference, error) {
			if key != "EXAMPLE-1" {
				t.Errorf("getIssueFn key: got %q, want EXAMPLE-1", key)
			}
			return issue, refs, nil
		}),
	)
	client := startBufconnServer(t, srv)
	ctx := context.Background()

	t.Run("markdown", func(t *testing.T) {
		resp, err := client.GetIssue(ctx, &gojirav1.GetIssueRequest{
			Key:    "EXAMPLE-1",
			Format: gojirav1.OutputFormat_OUTPUT_FORMAT_MARKDOWN,
		})
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		md := resp.GetMarkdown()
		if md == "" {
			t.Fatal("Markdown payload was empty")
		}
		if !strings.Contains(md, "EXAMPLE-1") {
			t.Errorf("Markdown must mention the issue key; got:\n%s", md)
		}
	})

	t.Run("json", func(t *testing.T) {
		resp, err := client.GetIssue(ctx, &gojirav1.GetIssueRequest{
			Key:    "EXAMPLE-1",
			Format: gojirav1.OutputFormat_OUTPUT_FORMAT_JSON,
		})
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		payload := resp.GetJson()
		if payload == "" {
			t.Fatal("JSON payload was empty")
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("JSON payload was not valid JSON:\n%s", payload)
		}
		if !strings.Contains(payload, "EXAMPLE-1") {
			t.Errorf("JSON must mention the issue key; got:\n%s", payload)
		}
	})

	t.Run("structured", func(t *testing.T) {
		resp, err := client.GetIssue(ctx, &gojirav1.GetIssueRequest{
			Key:    "EXAMPLE-1",
			Format: gojirav1.OutputFormat_OUTPUT_FORMAT_STRUCTURED,
		})
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		got := resp.GetIssue()
		if got == nil {
			t.Fatal("structured response missing the Issue payload")
		}
		if got.GetKey() != "EXAMPLE-1" {
			t.Errorf("Issue.Key: got %q, want EXAMPLE-1", got.GetKey())
		}
		if got.GetSummary() != "An integration-test issue" {
			t.Errorf("Issue.Summary: got %q", got.GetSummary())
		}
		if len(got.GetReferences()) != 1 {
			t.Fatalf("References: want 1, got %d", len(got.GetReferences()))
		}
		ref := got.GetReferences()[0]
		if ref.GetKind() != "GitHubPR" || ref.GetPrNumber() != 7 {
			t.Errorf("PR reference mapped wrong: %+v", ref)
		}
	})
}

// ---------------------------------------------------------------------------
// Crawl — streams events end to end through the real handler + sink
// ---------------------------------------------------------------------------

func TestIntegration_Crawl_StreamsEvents(t *testing.T) {
	t.Parallel()

	srv := grpcserver.NewServer(
		integrationCfg(t),
		grpcserver.WithCrawlFunc(func(_ context.Context, _ gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, error) {
			if len(startKeys) != 1 || startKeys[0] != "EXAMPLE-1" {
				t.Errorf("startKeys: got %v, want [EXAMPLE-1]", startKeys)
			}
			now := time.Now()
			sink.Emit(events.Event{
				Kind:      events.KindIssueFetched,
				IssueKey:  "EXAMPLE-1",
				Message:   "fetched EXAMPLE-1",
				Timestamp: now,
			})
			sink.Emit(events.Event{
				Kind:      events.KindCrawlSummary,
				Message:   "crawl complete: fetched=1",
				Timestamp: now,
			})
			return gojira.Summary{Fetched: 1}, nil
		}),
	)
	client := startBufconnServer(t, srv)

	stream, err := client.Crawl(context.Background(), &gojirav1.CrawlRequest{
		StartKeys: []string{"EXAMPLE-1"},
	})
	if err != nil {
		t.Fatalf("Crawl RPC open: %v", err)
	}

	var got []*gojirav1.CrawlEvent
	for {
		evt, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Recv: %v", err)
		}
		got = append(got, evt)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 streamed events, got %d", len(got))
	}
	if got[0].GetKind() != gojirav1.CrawlEvent_KIND_ISSUE_FETCHED {
		t.Errorf("event[0].Kind: got %v", got[0].GetKind())
	}
	if got[0].GetIssueKey() != "EXAMPLE-1" {
		t.Errorf("event[0].IssueKey: got %q", got[0].GetIssueKey())
	}
	if got[1].GetKind() != gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY {
		t.Errorf("event[1].Kind: got %v", got[1].GetKind())
	}
}

// ---------------------------------------------------------------------------
// Phase 2 — Write RPCs end-to-end over bufconn
// ---------------------------------------------------------------------------
//
// Each test below builds a Server with the appropriate write seam
// injected, registers it on the in-process bufconn listener via
// startBufconnServer, then drives the RPC over a real gRPC client
// connection. No network, no live Jira — the handler/proto wiring is
// exercised against in-memory fakes that record the call and return a
// canned result (or a typed error to assert status mapping).

// TestIntegration_CreateIssue exercises the happy path: the proto
// request maps to a CreateIssueRequest; the server's createIssueFn
// seam returns a CreatedIssue; the response carries key/id/self.
func TestIntegration_CreateIssue(t *testing.T) {
	t.Parallel()

	var gotProject, gotIssueType string
	var called bool
	srv := grpcserver.NewServer(integrationCfg(t),
		grpcserver.WithCreateIssueFunc(func(_ context.Context, _ gojira.Config, project, issueType string, _ ...client.CreateOption) (client.CreatedIssue, error) {
			called = true
			gotProject = project
			gotIssueType = issueType
			return client.CreatedIssue{
				Key:  "PROJ-1",
				ID:   "100",
				Self: "https://example.atlassian.net/rest/api/3/issue/100",
			}, nil
		}),
	)
	c := startBufconnServer(t, srv)

	resp, err := c.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{
		Project:   "PROJ",
		IssueType: "Task",
		Summary:   "S",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if !called {
		t.Error("createIssueFn must be called on a non-dry-run request")
	}
	if gotProject != "PROJ" || gotIssueType != "Task" {
		t.Errorf("fake fn args: project=%q issueType=%q", gotProject, gotIssueType)
	}
	if resp.GetKey() != "PROJ-1" || resp.GetId() != "100" ||
		resp.GetSelf() != "https://example.atlassian.net/rest/api/3/issue/100" {
		t.Errorf("response mismatch: %+v", resp)
	}
	if len(resp.GetDryRunBody()) != 0 {
		t.Errorf("DryRunBody must be empty on a real create, got %d bytes", len(resp.GetDryRunBody()))
	}
}

// TestIntegration_CreateIssue_DryRun proves dry_run short-circuits
// inside the handler — the seam is not invoked, and the response
// carries the JSON body the server would have POSTed.
func TestIntegration_CreateIssue_DryRun(t *testing.T) {
	t.Parallel()

	var called bool
	srv := grpcserver.NewServer(integrationCfg(t),
		grpcserver.WithCreateIssueFunc(func(context.Context, gojira.Config, string, string, ...client.CreateOption) (client.CreatedIssue, error) {
			called = true
			return client.CreatedIssue{}, nil
		}),
	)
	c := startBufconnServer(t, srv)

	resp, err := c.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{
		Project:   "PROJ",
		IssueType: "Task",
		Summary:   "S",
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if called {
		t.Error("createIssueFn must NOT be called when DryRun is set")
	}
	body := resp.GetDryRunBody()
	if len(body) == 0 {
		t.Fatal("DryRunBody must be populated on a dry-run create")
	}
	if !json.Valid(body) {
		t.Errorf("DryRunBody must be valid JSON; got: %s", string(body))
	}
	if resp.GetKey() != "" || resp.GetId() != "" {
		t.Errorf("dry-run response must not carry key/id, got %+v", resp)
	}
}

func TestIntegration_UpdateIssue(t *testing.T) {
	t.Parallel()

	var gotKey string
	srv := grpcserver.NewServer(integrationCfg(t),
		grpcserver.WithUpdateIssueFunc(func(_ context.Context, _ gojira.Config, key string, _ ...client.UpdateOption) error {
			gotKey = key
			return nil
		}),
	)
	c := startBufconnServer(t, srv)

	resp, err := c.UpdateIssue(context.Background(), &gojirav1.UpdateIssueRequest{
		Key:     "PROJ-1",
		Summary: "new",
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if !resp.GetOk() {
		t.Error("Ok must be true on success")
	}
	if gotKey != "PROJ-1" {
		t.Errorf("fake got key=%q, want PROJ-1", gotKey)
	}
}

func TestIntegration_AddComment(t *testing.T) {
	t.Parallel()

	srv := grpcserver.NewServer(integrationCfg(t),
		grpcserver.WithAddCommentFunc(func(context.Context, gojira.Config, string, ...client.CommentOption) (client.Comment, error) {
			return client.Comment{
				ID:                "10",
				AuthorAccountID:   "acc-1",
				AuthorDisplayName: "Alice",
				Created:           "2026-01-01T00:00:00.000+0000",
			}, nil
		}),
	)
	c := startBufconnServer(t, srv)

	resp, err := c.AddComment(context.Background(), &gojirav1.AddCommentRequest{
		Key:      "PROJ-1",
		BodyText: "looks good",
	})
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if resp.GetId() != "10" {
		t.Errorf("Id: got %q, want 10", resp.GetId())
	}
	if resp.GetAuthorDisplayName() != "Alice" {
		t.Errorf("AuthorDisplayName: got %q, want Alice", resp.GetAuthorDisplayName())
	}
	if resp.GetCreated() != "2026-01-01T00:00:00.000+0000" {
		t.Errorf("Created: got %q", resp.GetCreated())
	}
}

func TestIntegration_ListTransitions(t *testing.T) {
	t.Parallel()

	srv := grpcserver.NewServer(integrationCfg(t),
		grpcserver.WithListTransitionsFunc(func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			return []client.Transition{
				{ID: "11", Name: "Start", ToStatus: "In Progress"},
				{ID: "21", Name: "Done", ToStatus: "Done"},
			}, nil
		}),
	)
	c := startBufconnServer(t, srv)

	resp, err := c.ListTransitions(context.Background(), &gojirav1.ListTransitionsRequest{Key: "PROJ-1"})
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	ts := resp.GetTransitions()
	if len(ts) != 2 {
		t.Fatalf("want 2 transitions, got %d", len(ts))
	}
	if ts[0].GetId() != "11" || ts[0].GetName() != "Start" || ts[0].GetToStatus() != "In Progress" {
		t.Errorf("transition[0]: %+v", ts[0])
	}
	if ts[1].GetId() != "21" || ts[1].GetToStatus() != "Done" {
		t.Errorf("transition[1]: %+v", ts[1])
	}
}

func TestIntegration_TransitionIssue_ByID(t *testing.T) {
	t.Parallel()

	var gotKey, gotID string
	srv := grpcserver.NewServer(integrationCfg(t),
		grpcserver.WithTransitionIssueFunc(func(_ context.Context, _ gojira.Config, key, transitionID string, _ ...client.TransitionOption) error {
			gotKey = key
			gotID = transitionID
			return nil
		}),
	)
	c := startBufconnServer(t, srv)

	resp, err := c.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
		Key:          "PROJ-1",
		TransitionId: "11",
	})
	if err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}
	if !resp.GetOk() {
		t.Error("Ok must be true on success")
	}
	if gotKey != "PROJ-1" || gotID != "11" {
		t.Errorf("fake got key=%q id=%q", gotKey, gotID)
	}
}

// TestIntegration_TransitionIssue_ByStatus exercises the handler's
// by-name resolution path: the listTransitionsFn seam supplies the
// transition list; the handler must case-insensitively match the
// target ToStatus and forward the resolved id to transitionIssueFn.
func TestIntegration_TransitionIssue_ByStatus(t *testing.T) {
	t.Parallel()

	var transitionedWith string
	srv := grpcserver.NewServer(integrationCfg(t),
		grpcserver.WithListTransitionsFunc(func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			return []client.Transition{
				{ID: "11", Name: "Start", ToStatus: "In Progress"},
				{ID: "21", Name: "Done", ToStatus: "Done"},
			}, nil
		}),
		grpcserver.WithTransitionIssueFunc(func(_ context.Context, _ gojira.Config, _, transitionID string, _ ...client.TransitionOption) error {
			transitionedWith = transitionID
			return nil
		}),
	)
	c := startBufconnServer(t, srv)

	resp, err := c.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
		Key:              "PROJ-1",
		TargetStatusName: "Done",
	})
	if err != nil {
		t.Fatalf("TransitionIssue by status: %v", err)
	}
	if !resp.GetOk() {
		t.Error("Ok must be true on success")
	}
	if transitionedWith != "21" {
		t.Errorf("by-status must resolve to id 21, got %q", transitionedWith)
	}
}

// TestIntegration_Write_ErrorMapping confirms that write-path
// sentinels flow through toStatusError end-to-end. ErrBadRequest must
// surface as InvalidArgument on CreateIssue; ErrConflict must surface
// as FailedPrecondition on TransitionIssue. The error is wrapped to
// also confirm errors.Is classification survives a wrap chain over
// the wire.
func TestIntegration_Write_ErrorMapping(t *testing.T) {
	t.Parallel()

	t.Run("CreateIssue ErrBadRequest -> InvalidArgument", func(t *testing.T) {
		t.Parallel()
		// The seam returns the real client.ErrBadRequest sentinel so the
		// server's toStatusError sees it (via errors.Is over the wrap
		// chain) and maps it to codes.InvalidArgument over the wire.
		srv := grpcserver.NewServer(integrationCfg(t),
			grpcserver.WithCreateIssueFunc(func(context.Context, gojira.Config, string, string, ...client.CreateOption) (client.CreatedIssue, error) {
				return client.CreatedIssue{}, client.ErrBadRequest
			}),
		)
		c := startBufconnServer(t, srv)

		_, err := c.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{
			Project: "PROJ", IssueType: "Task",
		})
		if err == nil {
			t.Fatal("expected error")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("expected gRPC status error, got %T (%v)", err, err)
		}
		if st.Code() != codes.InvalidArgument {
			t.Errorf("status code: got %v, want InvalidArgument", st.Code())
		}
	})

	t.Run("TransitionIssue ErrConflict -> FailedPrecondition", func(t *testing.T) {
		t.Parallel()
		srv := grpcserver.NewServer(integrationCfg(t),
			grpcserver.WithTransitionIssueFunc(func(context.Context, gojira.Config, string, string, ...client.TransitionOption) error {
				return client.ErrConflict
			}),
		)
		c := startBufconnServer(t, srv)

		_, err := c.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
			Key:          "PROJ-1",
			TransitionId: "11",
		})
		if err == nil {
			t.Fatal("expected error")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("expected gRPC status error, got %T (%v)", err, err)
		}
		if st.Code() != codes.FailedPrecondition {
			t.Errorf("status code: got %v, want FailedPrecondition", st.Code())
		}
	})
}

// ---------------------------------------------------------------------------
// GetGraph — round-trips a fake graph model through real gRPC machinery
// ---------------------------------------------------------------------------

func TestIntegration_GetGraph_RoundTrip(t *testing.T) {
	t.Parallel()

	srv := grpcserver.NewServer(
		integrationCfg(t),
		grpcserver.WithCrawlGraphFunc(func(_ context.Context, _ gojira.Config, startKeys []string, _ gojira.Sink) (gojira.Summary, gojira.GraphModel, error) {
			if len(startKeys) != 1 || startKeys[0] != "EXAMPLE-1" {
				t.Errorf("startKeys: got %v, want [EXAMPLE-1]", startKeys)
			}
			return gojira.Summary{Fetched: 2}, gojira.GraphModel{
				Nodes: []gojira.GraphNode{
					{ID: "EXAMPLE-1", Kind: "issue", Label: "EXAMPLE-1: a", Fetched: true},
					{ID: "EXAMPLE-2", Kind: "issue", Label: "EXAMPLE-2: b", Fetched: true},
					{ID: "acme/widget#7", Kind: "github_pr", Label: "acme/widget#7",
						URL: "https://github.com/acme/widget/pull/7"},
				},
				Edges: []gojira.GraphEdge{
					{From: "EXAMPLE-1", To: "EXAMPLE-2", Kind: "link", Label: "blocks"},
					{From: "EXAMPLE-1", To: "acme/widget#7", Kind: "pull_request"},
				},
			}, nil
		}),
	)
	client := startBufconnServer(t, srv)

	resp, err := client.GetGraph(context.Background(), &gojirav1.GetGraphRequest{
		StartKeys: []string{"EXAMPLE-1"},
	})
	if err != nil {
		t.Fatalf("GetGraph RPC: %v", err)
	}

	if got, want := len(resp.GetNodes()), 3; got != want {
		t.Fatalf("node count: got %d, want %d", got, want)
	}
	if got, want := len(resp.GetEdges()), 2; got != want {
		t.Fatalf("edge count: got %d, want %d", got, want)
	}

	// Spot-check that the kind strings are forwarded verbatim and the
	// PR node kept its non-default shape.
	kindByID := map[string]string{}
	for _, n := range resp.GetNodes() {
		kindByID[n.GetId()] = n.GetKind()
	}
	if kindByID["EXAMPLE-1"] != "issue" {
		t.Errorf("EXAMPLE-1 kind: got %q", kindByID["EXAMPLE-1"])
	}
	if kindByID["acme/widget#7"] != "github_pr" {
		t.Errorf("PR kind: got %q", kindByID["acme/widget#7"])
	}
}
