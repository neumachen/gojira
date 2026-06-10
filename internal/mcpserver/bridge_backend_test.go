// bridge_backend_test.go — exercise the gRPC bridge backend against
// an in-process grpcserver wired over a bufconn listener. No real
// network: bufconn provides an in-memory net.Listener pair and the
// grpcserver's WithXFunc seams stand in for the live facade.
package mcpserver

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/classify"
	"github.com/neumachen/gojira/client"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/grpcserver"
	"github.com/neumachen/gojira/internal/parse"
)

// dialBufconn returns a *grpc.ClientConn dialed over the supplied
// bufconn listener using plaintext credentials — matching what
// NewBridgeBackend does for a real address.
func dialBufconn(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// startBridgeBufconnServer brings up a grpcserver.Server with the
// supplied options on a fresh bufconn listener and returns a
// bridgeBackend dialed against it. opts are forwarded so each test
// can inject its own per-RPC fakes via the existing WithXFunc seams.
func startBridgeBufconnServer(t *testing.T, opts ...grpcserver.Option) *bridgeBackend {
	t.Helper()
	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)
	srv := grpcserver.NewServer(gojira.Config{
		Site:      "https://example.atlassian.net",
		OutputDir: t.TempDir(),
	}, opts...)

	grpcServer := grpc.NewServer()
	gojirav1.RegisterGojiraServer(grpcServer, srv)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = grpcServer.Serve(lis)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("grpc.Server.Serve did not return within 2s of Stop")
		}
		_ = lis.Close()
	})

	conn := dialBufconn(t, lis)
	return newBridgeBackendFromConn(conn)
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestBridgeBackend_Classify(t *testing.T) {
	// Classify is a server-side pure function over its input; no fake
	// needed beyond the running grpcserver.
	b := startBridgeBufconnServer(t)
	res, err := b.Classify(context.Background(), "https://example.atlassian.net/browse/PROJ-1", "")
	require.NoError(t, err)
	assert.Equal(t, classify.KindJiraURL, res.Kind)
	assert.Equal(t, "PROJ-1", res.IssueKey)
}

func TestBridgeBackend_GetIssue(t *testing.T) {
	b := startBridgeBufconnServer(t,
		grpcserver.WithGetIssueFunc(func(_ context.Context, _ gojira.Config, key string) (parse.Issue, []extract.Reference, error) {
			return parse.Issue{
				Key:       key,
				Summary:   "from grpc",
				Status:    "Open",
				IssueType: "Task",
				SourceURL: "https://example.atlassian.net/browse/" + key,
			}, nil, nil
		}),
	)
	issue, _, err := b.GetIssue(context.Background(), "PROJ-1")
	require.NoError(t, err)
	assert.Equal(t, "PROJ-1", issue.Key)
	assert.Equal(t, "from grpc", issue.Summary)
}

func TestBridgeBackend_Crawl_TranslatesStreamToProgressAndSummary(t *testing.T) {
	b := startBridgeBufconnServer(t,
		grpcserver.WithCrawlFunc(func(_ context.Context, _ gojira.Config, keys []string, sink gojira.Sink) (gojira.Summary, error) {
			now := time.Now()
			for _, k := range keys {
				sink.Emit(events.Event{
					Kind: events.KindIssueFetched, IssueKey: k,
					Message: "fetched " + k, Timestamp: now,
				})
			}
			sink.Emit(events.Event{
				Kind: events.KindCrawlSummary, Message: "done", Timestamp: now,
				Summary: &events.CrawlSummary{Fetched: len(keys)},
			})
			return gojira.Summary{Fetched: len(keys)}, nil
		}),
	)
	var progressCalls int32
	progress := func(done, total int, msg string) { atomic.AddInt32(&progressCalls, 1) }
	sum, err := b.Crawl(context.Background(), []string{"PROJ-1", "PROJ-2"}, progress)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Fetched)
	assert.Equal(t, int32(2), atomic.LoadInt32(&progressCalls),
		"each KIND_ISSUE_FETCHED stream event must produce one progress call")
}

func TestBridgeBackend_GetGraph_ForwardsAndDrivesProgress(t *testing.T) {
	b := startBridgeBufconnServer(t,
		grpcserver.WithCrawlGraphFunc(func(_ context.Context, _ gojira.Config, keys []string, _ gojira.Sink) (gojira.Summary, gojira.GraphModel, error) {
			return gojira.Summary{Fetched: 1}, gojira.GraphModel{
				Nodes: []gojira.GraphNode{{ID: "PROJ-1", Kind: "issue", Label: "PROJ-1", Fetched: true}},
				Edges: []gojira.GraphEdge{},
			}, nil
		}),
	)
	var progressCalls int32
	progress := func(done, total int, msg string) { atomic.AddInt32(&progressCalls, 1) }
	_, model, err := b.GetGraph(context.Background(), []string{"PROJ-1"}, progress)
	require.NoError(t, err)
	require.Len(t, model.Nodes, 1)
	assert.Equal(t, "PROJ-1", model.Nodes[0].ID)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&progressCalls), int32(1),
		"GetGraph should drive at least one progress callback")
}

func TestBridgeBackend_CreateIssue_Forwards(t *testing.T) {
	b := startBridgeBufconnServer(t,
		grpcserver.WithCreateIssueFunc(func(_ context.Context, _ gojira.Config, project, issueType string, opts ...client.CreateOption) (client.CreatedIssue, error) {
			assert.Equal(t, "PROJ", project)
			assert.Equal(t, "Task", issueType)
			return client.CreatedIssue{Key: "PROJ-99", ID: "10099", Self: "https://x/jira/PROJ-99"}, nil
		}),
	)
	res, err := b.CreateIssue(context.Background(), "PROJ", "Task", CreateIssueFields{Summary: "hi"})
	require.NoError(t, err)
	assert.Equal(t, "PROJ-99", res.Key)
}

func TestBridgeBackend_ListTransitions_Forwards(t *testing.T) {
	b := startBridgeBufconnServer(t,
		grpcserver.WithListTransitionsFunc(func(_ context.Context, _ gojira.Config, key string) ([]client.Transition, error) {
			return []client.Transition{{ID: "11", Name: "Start", ToStatus: "In Progress"}}, nil
		}),
	)
	ts, err := b.ListTransitions(context.Background(), "PROJ-1")
	require.NoError(t, err)
	require.Len(t, ts, 1)
	assert.Equal(t, "11", ts[0].ID)
}

func TestBridgeBackend_TransitionIssue_BothOrNeitherErrors(t *testing.T) {
	b := startBridgeBufconnServer(t)
	err := b.TransitionIssue(context.Background(), "PROJ-1", "", "", TransitionFields{})
	assert.Error(t, err)
	err = b.TransitionIssue(context.Background(), "PROJ-1", "11", "Done", TransitionFields{})
	assert.Error(t, err)
}

func TestNewBridgeBackend_EmptyAddrErrors(t *testing.T) {
	_, _, err := NewBridgeBackend("")
	require.Error(t, err)
	assert.True(t, errors.Is(err, err)) // sanity touch
}
