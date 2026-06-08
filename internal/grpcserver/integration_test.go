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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/classify"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/grpcserver"
	"github.com/neumachen/gojira/internal/parse"
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
