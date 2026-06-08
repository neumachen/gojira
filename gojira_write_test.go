// Facade tests for the Phase-2 write operations exposed by gojira.go.
// Each test stands up an httptest.NewServer (plain HTTP), points
// cfg.Site at srv.URL, and exercises the corresponding facade function.
// No live Jira; no WithHTTPClient injection — the default client.New
// path reaches the test server directly through its URL.
package gojira_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Tiny request-capture helper, shared by every write-facade test.
// ---------------------------------------------------------------------------

type capturedRequest struct {
	method string
	path   string
	body   []byte
}

// recordingHandler returns an http.HandlerFunc that records the
// request into cap and then replies with status/body.
func recordingHandler(cap *capturedRequest, status int, respBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.body, _ = io.ReadAll(r.Body)
		if respBody == "" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoErrorf(t, json.Unmarshal(body, &got), "decode body: %s", string(body))
	return got
}

// ---------------------------------------------------------------------------
// CreateIssue
// ---------------------------------------------------------------------------

func TestCreateIssue_Facade_Success(t *testing.T) {
	t.Parallel()
	const respJSON = `{"id":"10001","key":"PROJ-1","self":"https://example.atlassian.net/rest/api/3/issue/10001"}`

	var cap capturedRequest
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusCreated, respJSON))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())

	got, err := gojira.CreateIssue(context.Background(), cfg, "PROJ", "Task",
		client.WithSummary("S"),
	)
	require.NoError(t, err)

	assert.Equal(t, client.CreatedIssue{
		Key:  "PROJ-1",
		ID:   "10001",
		Self: "https://example.atlassian.net/rest/api/3/issue/10001",
	}, got)

	assert.Equal(t, http.MethodPost, cap.method)
	assert.Equal(t, "/rest/api/3/issue", cap.path)

	body := decodeJSON(t, cap.body)
	fields, ok := body["fields"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "S", fields["summary"])
	assert.Equal(t, map[string]any{"key": "PROJ"}, fields["project"])
	assert.Equal(t, map[string]any{"name": "Task"}, fields["issuetype"])
}

func TestCreateIssue_Facade_BadRequest_SurfacesSentinel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w,
			`{"errorMessages":["validation failed"],"errors":{"summary":"Summary is required."}}`)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())

	_, err := gojira.CreateIssue(context.Background(), cfg, "PROJ", "Task")
	require.Error(t, err)
	assert.True(t, errors.Is(err, gojira.ErrBadRequest),
		"errors.Is(err, gojira.ErrBadRequest) must hold; got %v", err)

	var ape *client.APIError
	require.True(t, errors.As(err, &ape),
		"errors.As must surface the typed APIError; got %v", err)
	assert.Equal(t, "Summary is required.", ape.FieldErrors["summary"])
}

// ---------------------------------------------------------------------------
// UpdateIssue
// ---------------------------------------------------------------------------

func TestUpdateIssue_Facade_Success(t *testing.T) {
	t.Parallel()

	var cap capturedRequest
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusNoContent, ""))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())

	err := gojira.UpdateIssue(context.Background(), cfg, "PROJ-1",
		client.WithSummaryUpdate("new"),
	)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPut, cap.method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1", cap.path)

	body := decodeJSON(t, cap.body)
	fields, _ := body["fields"].(map[string]any)
	require.NotNil(t, fields)
	assert.Equal(t, "new", fields["summary"])
}

// ---------------------------------------------------------------------------
// AddComment
// ---------------------------------------------------------------------------

func TestAddComment_Facade_Success(t *testing.T) {
	t.Parallel()
	const respJSON = `{
		"id": "10100",
		"author": {"accountId":"acc-1","displayName":"Alice"},
		"created": "2026-01-15T10:30:00.000+0000"
	}`

	var cap capturedRequest
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusCreated, respJSON))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())

	got, err := gojira.AddComment(context.Background(), cfg, "PROJ-1",
		client.WithCommentText("looks good"),
	)
	require.NoError(t, err)

	assert.Equal(t, client.Comment{
		ID:                "10100",
		AuthorAccountID:   "acc-1",
		AuthorDisplayName: "Alice",
		Created:           "2026-01-15T10:30:00.000+0000",
	}, got)

	assert.Equal(t, http.MethodPost, cap.method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1/comment", cap.path)
}

// ---------------------------------------------------------------------------
// ListTransitions + TransitionIssue + TransitionIssueByStatus
// ---------------------------------------------------------------------------

// listTransitionsServer stands up a server that returns the supplied
// transitions on GET .../transitions and 204 on POST. It records the
// POST body into cap for assertion. The mux pattern keeps the two
// endpoints separate so a single test can drive both calls.
func listTransitionsServer(t *testing.T, listJSON string, cap *capturedRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, listJSON)
		case http.MethodPost:
			cap.method = r.Method
			cap.path = r.URL.Path
			cap.body, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestListTransitions_Facade(t *testing.T) {
	t.Parallel()
	const respJSON = `{
		"transitions": [
			{"id":"11","name":"Start Progress","to":{"name":"In Progress"}},
			{"id":"21","name":"Done","to":{"name":"Done"}}
		]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/rest/api/3/issue/PROJ-1/transitions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, respJSON)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())

	got, err := gojira.ListTransitions(context.Background(), cfg, "PROJ-1")
	require.NoError(t, err)
	assert.Equal(t, []client.Transition{
		{ID: "11", Name: "Start Progress", ToStatus: "In Progress"},
		{ID: "21", Name: "Done", ToStatus: "Done"},
	}, got)
}

func TestTransitionIssue_Facade_ByID(t *testing.T) {
	t.Parallel()

	var cap capturedRequest
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusNoContent, ""))
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())

	err := gojira.TransitionIssue(context.Background(), cfg, "PROJ-1", "11")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, cap.method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1/transitions", cap.path)

	body := decodeJSON(t, cap.body)
	transition, _ := body["transition"].(map[string]any)
	assert.Equal(t, "11", transition["id"])
}

func TestTransitionIssueByStatus_Facade_Resolves(t *testing.T) {
	t.Parallel()
	const respJSON = `{
		"transitions": [
			{"id":"11","name":"Start Progress","to":{"name":"In Progress"}},
			{"id":"21","name":"Done","to":{"name":"Done"}}
		]
	}`

	var cap capturedRequest
	srv := listTransitionsServer(t, respJSON, &cap)
	cfg := testConfig(t, srv.URL, t.TempDir())

	// Case-insensitive match must pick "Done" → id "21".
	err := gojira.TransitionIssueByStatus(context.Background(), cfg, "PROJ-1", "done")
	require.NoError(t, err)

	body := decodeJSON(t, cap.body)
	transition, _ := body["transition"].(map[string]any)
	assert.Equal(t, "21", transition["id"], "by-status must resolve to id 21")
}

func TestTransitionIssueByStatus_Facade_NoMatch(t *testing.T) {
	t.Parallel()
	const respJSON = `{
		"transitions": [
			{"id":"11","name":"Start Progress","to":{"name":"In Progress"}}
		]
	}`

	var cap capturedRequest
	srv := listTransitionsServer(t, respJSON, &cap)
	cfg := testConfig(t, srv.URL, t.TempDir())

	err := gojira.TransitionIssueByStatus(context.Background(), cfg, "PROJ-1", "Resolved")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Resolved")
	assert.Empty(t, cap.method, "no POST must have been made when resolution fails")
}

func TestTransitionIssueByStatus_Facade_Ambiguous(t *testing.T) {
	t.Parallel()
	// Two transitions with the same ToStatus name → ambiguous.
	const respJSON = `{
		"transitions": [
			{"id":"31","name":"Resolve via QA","to":{"name":"Resolved"}},
			{"id":"32","name":"Resolve via PM","to":{"name":"Resolved"}}
		]
	}`

	var cap capturedRequest
	srv := listTransitionsServer(t, respJSON, &cap)
	cfg := testConfig(t, srv.URL, t.TempDir())

	err := gojira.TransitionIssueByStatus(context.Background(), cfg, "PROJ-1", "Resolved")
	require.Error(t, err)
	low := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(low, "ambiguous") || strings.Contains(low, "multiple"),
		"error message must indicate ambiguity; got %q", err.Error())
	assert.Empty(t, cap.method, "no POST when resolution is ambiguous")
}

// ---------------------------------------------------------------------------
// Dry-run body builders (phase-d-facade-3)
// ---------------------------------------------------------------------------

// TestBuildCreateIssueBody confirms the facade's dry-run helper is a
// thin pass-through over client.RenderCreateBody — byte-for-byte
// identical, no network, no client allocation.
func TestBuildCreateIssueBody_ParityWithClient(t *testing.T) {
	t.Parallel()

	facade, err := gojira.BuildCreateIssueBody("PROJ", "Task",
		client.WithSummary("S"),
		client.WithDescriptionText("hello"),
		client.WithLabels("urgent"),
	)
	require.NoError(t, err)

	lib, err := client.RenderCreateBody("PROJ", "Task",
		client.WithSummary("S"),
		client.WithDescriptionText("hello"),
		client.WithLabels("urgent"),
	)
	require.NoError(t, err)

	assert.Equal(t, string(lib), string(facade),
		"BuildCreateIssueBody must produce byte-for-byte the same payload as the client renderer")
}

func TestBuildUpdateIssueBody_ParityWithClient(t *testing.T) {
	t.Parallel()

	facade, err := gojira.BuildUpdateIssueBody(
		client.WithSummaryUpdate("new"),
		client.WithLabelsUpdate("a", "b"),
	)
	require.NoError(t, err)

	lib, err := client.RenderUpdateBody(
		client.WithSummaryUpdate("new"),
		client.WithLabelsUpdate("a", "b"),
	)
	require.NoError(t, err)

	assert.Equal(t, string(lib), string(facade),
		"BuildUpdateIssueBody must produce byte-for-byte the same payload as the client renderer")
}
