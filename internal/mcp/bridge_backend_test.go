// bridge_backend_test.go — exercise the gRPC bridge backend against
// an in-process grpc wired over a bufconn listener. No real
// network: bufconn provides an in-memory net.Listener pair and the
// grpc's WithXFunc seams stand in for the live facade.
package mcp

import (
	"context"
	"encoding/json"
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
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	gojiragrpc "github.com/neumachen/gojira/internal/grpc"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
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

// startBridgeBufconnServer brings up a gojiragrpc.Server with the
// supplied options on a fresh bufconn listener and returns a
// bridgeBackend dialed against it. opts are forwarded so each test
// can inject its own per-RPC fakes via the existing WithXFunc seams.
func startBridgeBufconnServer(t *testing.T, opts ...gojiragrpc.Option) *bridgeBackend {
	t.Helper()
	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)
	srv := gojiragrpc.NewServer(gojira.Config{
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
	// needed beyond the running gojiragrpc.
	b := startBridgeBufconnServer(t)
	res, err := b.Classify(context.Background(), "https://example.atlassian.net/browse/PROJ-1", "")
	require.NoError(t, err)
	assert.Equal(t, classify.KindJiraURL, res.Kind)
	assert.Equal(t, "PROJ-1", res.IssueKey)
}

func TestBridgeBackend_GetIssue(t *testing.T) {
	b := startBridgeBufconnServer(t,
		gojiragrpc.WithGetIssueFunc(func(_ context.Context, _ gojira.Config, key string) (parse.Issue, []extract.Reference, error) {
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
		gojiragrpc.WithCrawlFunc(func(_ context.Context, _ gojira.Config, keys []string, sink gojira.Sink) (gojira.Summary, error) {
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
		gojiragrpc.WithCrawlGraphFunc(func(_ context.Context, _ gojira.Config, keys []string, _ gojira.Sink) (gojira.Summary, gojira.GraphModel, error) {
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
		gojiragrpc.WithCreateIssueFunc(func(_ context.Context, _ gojira.Config, project, issueType string, opts ...client.CreateOption) (client.CreatedIssue, error) {
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
		gojiragrpc.WithListTransitionsFunc(func(_ context.Context, _ gojira.Config, key string) ([]client.Transition, error) {
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

func TestBridgeBackend_Versions_Forward(t *testing.T) {
	var gotName, gotProject, gotID, gotList string
	b := startBridgeBufconnServer(t,
		gojiragrpc.WithCreateVersionFunc(func(_ context.Context, _ gojira.Config, name, project string, _ ...client.VersionOption) (client.Version, error) {
			gotName, gotProject = name, project
			return client.Version{ID: "20000", Name: name, ProjectID: 10000, Released: true}, nil
		}),
		gojiragrpc.WithUpdateVersionFunc(func(_ context.Context, _ gojira.Config, id string, _ ...client.VersionOption) (client.Version, error) {
			gotID = id
			return client.Version{ID: id}, nil
		}),
		gojiragrpc.WithListVersionsFunc(func(_ context.Context, _ gojira.Config, projectIDOrKey string) ([]client.Version, error) {
			gotList = projectIDOrKey
			return []client.Version{{ID: "20000", Name: "1.0", Released: true}}, nil
		}),
	)

	v, err := b.CreateVersion(context.Background(), "Release-abc1234", "10000",
		VersionFields{Released: boolPtr(true)})
	require.NoError(t, err)
	assert.Equal(t, "Release-abc1234", gotName)
	assert.Equal(t, "10000", gotProject)
	assert.Equal(t, "20000", v.ID)
	assert.Equal(t, 10000, v.ProjectID)
	assert.True(t, v.Released)

	_, err = b.UpdateVersion(context.Background(), "20000", VersionFields{Description: "d"})
	require.NoError(t, err)
	assert.Equal(t, "20000", gotID)

	vs, err := b.ListVersions(context.Background(), "EXAMPLE")
	require.NoError(t, err)
	require.Len(t, vs, 1)
	assert.Equal(t, "EXAMPLE", gotList)
	assert.Equal(t, "20000", vs[0].ID)
}

// TestBridgeBackend_UpdateVersion_BoolPresenceAcrossWire proves the *bool
// presence survives the whole bridge→proto(optional)→server→option chain
// over bufconn: a nil Released is NOT forwarded, while an explicit false
// IS. Observed by capturing the VersionOptions the server hands the seam
// and rendering them with the pure builder.
func TestBridgeBackend_UpdateVersion_BoolPresenceAcrossWire(t *testing.T) {
	var capturedOpts []client.VersionOption
	b := startBridgeBufconnServer(t,
		gojiragrpc.WithUpdateVersionFunc(func(_ context.Context, _ gojira.Config, _ string, opts ...client.VersionOption) (client.Version, error) {
			capturedOpts = opts
			return client.Version{ID: "20000"}, nil
		}),
	)

	// nil Released → must NOT appear in the rendered body.
	_, err := b.UpdateVersion(context.Background(), "20000", VersionFields{Description: "d"})
	require.NoError(t, err)
	body, err := client.RenderUpdateVersionBody(capturedOpts...)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	_, hasReleased := m["released"]
	assert.False(t, hasReleased, "nil Released must not cross the wire as a value")

	// explicit false Released → must appear as false.
	_, err = b.UpdateVersion(context.Background(), "20000", VersionFields{Released: boolPtr(false)})
	require.NoError(t, err)
	body, err = client.RenderUpdateVersionBody(capturedOpts...)
	require.NoError(t, err)
	m = map[string]any{}
	require.NoError(t, json.Unmarshal(body, &m))
	got, ok := m["released"]
	assert.True(t, ok, "explicit false Released must cross the wire")
	assert.Equal(t, false, got)
}

func TestBridgeBackend_CreateIssue_FixVersionsForward(t *testing.T) {
	var gotOpts []client.CreateOption
	b := startBridgeBufconnServer(t,
		gojiragrpc.WithCreateIssueFunc(func(_ context.Context, _ gojira.Config, _, _ string, opts ...client.CreateOption) (client.CreatedIssue, error) {
			gotOpts = opts
			return client.CreatedIssue{Key: "PROJ-1"}, nil
		}),
	)
	_, err := b.CreateIssue(context.Background(), "PROJ", "Task", CreateIssueFields{
		Summary:       "s",
		FixVersionIDs: []string{"10000"},
	})
	require.NoError(t, err)

	body, err := client.RenderCreateBody("PROJ", "Task", gotOpts...)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	fields := m["fields"].(map[string]any)
	assert.Equal(t, []any{map[string]any{"id": "10000"}}, fields["fixVersions"])
}

func TestNewBridgeBackend_EmptyAddrErrors(t *testing.T) {
	_, _, err := NewBridgeBackend("")
	require.Error(t, err)
	assert.True(t, errors.Is(err, err)) // sanity touch
}
