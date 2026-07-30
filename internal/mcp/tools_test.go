// tools_test.go — end-to-end via the SDK's in-memory transport.
//
// We build a real *mcpsdk.Server with registerTools, expose it on an
// in-memory transport pair, connect a real *mcpsdk.Client to the other
// end, and assert on tools/list (gating) and a tools/call round
// trip. NO live network, NO subprocess.
package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
)

// fakeBackend implements mcpBackend with recorded calls + canned
// return values. Used to drive the tool-registration assertions
// without standing up either an httptest Jira or a bufconn gRPC.
type fakeBackend struct {
	classifyCalls int
}

func (f *fakeBackend) Classify(_ context.Context, input, _ string) (classify.Result, error) {
	f.classifyCalls++
	return classify.Result{Kind: classify.KindJiraKey, IssueKey: input}, nil
}
func (f *fakeBackend) GetIssue(_ context.Context, key string) (parse.Issue, []extract.Reference, error) {
	return parse.Issue{Key: key, Summary: "fake"}, nil, nil
}
func (f *fakeBackend) Crawl(_ context.Context, keys []string, progress ProgressFn) (gojira.Summary, error) {
	for i, k := range keys {
		progress(i+1, len(keys), "fetched "+k)
	}
	return gojira.Summary{Fetched: len(keys)}, nil
}
func (f *fakeBackend) GetGraph(_ context.Context, _ []string, _ ProgressFn) (gojira.Summary, gojira.GraphModel, error) {
	return gojira.Summary{}, gojira.GraphModel{}, nil
}
func (f *fakeBackend) CreateIssue(_ context.Context, _, _ string, _ CreateIssueFields) (client.CreatedIssue, error) {
	return client.CreatedIssue{Key: "FAKE-1"}, nil
}
func (f *fakeBackend) UpdateIssue(_ context.Context, _ string, _ UpdateIssueFields) error {
	return nil
}
func (f *fakeBackend) AddComment(_ context.Context, _, _ string) (client.Comment, error) {
	return client.Comment{ID: "c1"}, nil
}
func (f *fakeBackend) ListTransitions(_ context.Context, _ string) ([]client.Transition, error) {
	return []client.Transition{{ID: "11", Name: "Start", ToStatus: "In Progress"}}, nil
}
func (f *fakeBackend) TransitionIssue(_ context.Context, _, _, _ string, _ TransitionFields) error {
	return nil
}
func (f *fakeBackend) CreateVersion(_ context.Context, name, _ string, _ VersionFields) (client.Version, error) {
	return client.Version{ID: "20000", Name: name}, nil
}
func (f *fakeBackend) UpdateVersion(_ context.Context, id string, _ VersionFields) (client.Version, error) {
	return client.Version{ID: id}, nil
}
func (f *fakeBackend) ListVersions(_ context.Context, _ string) ([]client.Version, error) {
	return []client.Version{{ID: "20000", Name: "1.0", Released: true}}, nil
}

// startInProcessMCP connects an mcpsdk.Client to an mcpsdk.Server built
// over [NewMCPServer], using the SDK's in-memory transport pair.
// Server.Connect MUST run before Client.Connect — the SDK documents
// that the client triggers MCP initialize during connect.
func startInProcessMCP(t *testing.T, b mcpBackend, allowWrites bool) *mcpsdk.ClientSession {
	t.Helper()
	srvTransport, cliTransport := mcpsdk.NewInMemoryTransports()
	server := NewMCPServer(b, allowWrites)
	ctx := context.Background()

	srvSess, err := server.Connect(ctx, srvTransport, nil)
	require.NoError(t, err, "server.Connect")
	t.Cleanup(func() { _ = srvSess.Close() })

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	cliSess, err := client.Connect(ctx, cliTransport, nil)
	require.NoError(t, err, "client.Connect")
	t.Cleanup(func() { _ = cliSess.Close() })

	return cliSess
}

// toolNames extracts the Name fields off ListToolsResult.Tools.
func toolNames(r *mcpsdk.ListToolsResult) []string {
	out := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		out = append(out, t.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// Tool-gating: allowWrites=false omits the four mutating tools
// ---------------------------------------------------------------------------

func TestTools_AllowWritesFalse_ExposesOnlyReadTools(t *testing.T) {
	cs := startInProcessMCP(t, &fakeBackend{}, false)
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := toolNames(res)

	readTools := []string{"classify", "get_issue", "crawl", "get_graph", "list_transitions", "list_versions"}
	writeTools := []string{"create_issue", "update_issue", "add_comment", "transition_issue", "create_version", "update_version"}

	for _, name := range readTools {
		assert.Contains(t, names, name, "read tool %q must be exposed", name)
	}
	for _, name := range writeTools {
		assert.NotContains(t, names, name,
			"write tool %q must be absent when allow_writes=false", name)
	}
	// list_versions is a read tool and must be present unconditionally.
	assert.Contains(t, names, "list_versions",
		"list_versions must be registered even when allow_writes=false")
	assert.Len(t, names, len(readTools),
		"exactly the read-tool set must be registered when allow_writes=false")
}

func TestTools_AllowWritesTrue_ExposesAllTools(t *testing.T) {
	cs := startInProcessMCP(t, &fakeBackend{}, true)
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := toolNames(res)

	all := []string{
		"classify", "get_issue", "crawl", "get_graph", "list_transitions", "list_versions",
		"create_issue", "update_issue", "add_comment", "transition_issue",
		"create_version", "update_version",
	}
	for _, name := range all {
		assert.Contains(t, names, name, "tool %q must be exposed when allow_writes=true", name)
	}
	assert.Len(t, names, len(all), "exactly the full tool set when allow_writes=true")
}

// ---------------------------------------------------------------------------
// tools/call round-trip
// ---------------------------------------------------------------------------

func TestTools_CallClassify_RoundTripsThroughHandler(t *testing.T) {
	fake := &fakeBackend{}
	cs := startInProcessMCP(t, fake, false)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "classify",
		Arguments: map[string]any{"input": "PROJ-1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "classify on a valid key must not produce an error result")
	require.NotEmpty(t, res.Content)

	// The handler wraps the classify.Result as indented JSON in a
	// text content block; assert the issue key is present.
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok, "first content block should be TextContent")
	assert.Contains(t, tc.Text, "PROJ-1", "classify result must contain the input key")
	assert.Equal(t, 1, fake.classifyCalls, "the handler should reach the backend exactly once")
}

func TestTools_CallListVersions_RoundTrip(t *testing.T) {
	cs := startInProcessMCP(t, &fakeBackend{}, false)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "list_versions",
		Arguments: map[string]any{"project": "EXAMPLE"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "20000")
	assert.Contains(t, tc.Text, "versions")
}

func TestTools_CallCreateVersion_RoundTrip(t *testing.T) {
	cs := startInProcessMCP(t, &fakeBackend{}, true)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "create_version",
		Arguments: map[string]any{"project": "10000", "name": "Release-abc1234", "released": true},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "Release-abc1234")
}

func TestTools_CallUpdateVersion_RoundTrip(t *testing.T) {
	cs := startInProcessMCP(t, &fakeBackend{}, true)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "update_version",
		Arguments: map[string]any{"id": "20000", "released": true},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "20000")
}

// ---------------------------------------------------------------------------
// Tool errors surface via IsError on the result, preserving sentinel category
// ---------------------------------------------------------------------------

type errorBackend struct {
	fakeBackend
}

func (e *errorBackend) GetIssue(_ context.Context, key string) (parse.Issue, []extract.Reference, error) {
	return parse.Issue{}, nil, gojira.ErrNotFound
}

func TestTools_HandlerError_BecomesIsErrorResult(t *testing.T) {
	cs := startInProcessMCP(t, &errorBackend{}, false)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "get_issue",
		Arguments: map[string]any{"key": "NOPE-1"},
	})
	require.NoError(t, err, "transport-level call must succeed even on tool error")
	require.True(t, res.IsError, "tool-level error must surface IsError=true")
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(tc.Text, "not_found:"),
		"sentinel category must prefix the error message; got %q", tc.Text)
}
