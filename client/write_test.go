package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/adf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers — capture method/path/body of the one request each test issues.
// ---------------------------------------------------------------------------

// requestCapture records the request a single-shot test handler receives.
type requestCapture struct {
	method string
	path   string
	body   []byte
}

// captureHandler returns an http.HandlerFunc that records the incoming
// request and then writes the supplied status + body. Tests pass the
// recording struct by pointer so they can assert afterwards.
func captureHandler(rec *requestCapture, status int, respBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.body, _ = io.ReadAll(r.Body)
		if respBody == "" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}
}

// decodeJSONObject unmarshals body into a generic map for structure asserts.
func decodeJSONObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoErrorf(t, json.Unmarshal(body, &got), "decode body: %s", string(body))
	return got
}

// ---------------------------------------------------------------------------
// CreateIssue
// ---------------------------------------------------------------------------

func TestCreateIssue_Success_GoldenBody(t *testing.T) {
	const respJSON = `{"id":"10001","key":"PROJ-1","self":"https://example.atlassian.net/rest/api/3/issue/10001"}`

	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusCreated, respJSON))
	defer srv.Close()

	c := newTestClient(t, srv)

	got, err := c.CreateIssue(context.Background(), "PROJ", "Task",
		client.WithSummary("S"),
		client.WithDescriptionText("hello"),
	)
	require.NoError(t, err)

	// Response parse.
	assert.Equal(t, client.CreatedIssue{
		Key:  "PROJ-1",
		ID:   "10001",
		Self: "https://example.atlassian.net/rest/api/3/issue/10001",
	}, got)

	// Request envelope.
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/rest/api/3/issue", rec.path)

	// Request body structure.
	body := decodeJSONObject(t, rec.body)
	fields, ok := body["fields"].(map[string]any)
	require.True(t, ok, `"fields" must be a JSON object, got %T`, body["fields"])
	assert.Equal(t, map[string]any{"key": "PROJ"}, fields["project"])
	assert.Equal(t, map[string]any{"name": "Task"}, fields["issuetype"])
	assert.Equal(t, "S", fields["summary"])

	desc, ok := fields["description"].(map[string]any)
	require.True(t, ok, "description must be ADF object, got %T", fields["description"])
	assert.Equal(t, "doc", desc["type"], "description must be a valid ADF doc")
	// Verify the text round-trips through the reader.
	descBytes, _ := json.Marshal(desc)
	md, _, err := adf.RenderMarkdown(descBytes)
	require.NoError(t, err)
	assert.Contains(t, md, "hello")
}

func TestCreateIssue_400_SurfacesAPIError(t *testing.T) {
	const errBody = `{"errorMessages":["validation failed"],"errors":{"summary":"Summary is required."}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	_, err := c.CreateIssue(context.Background(), "PROJ", "Task")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrBadRequest))

	var ape *client.APIError
	require.True(t, errors.As(err, &ape))
	assert.Equal(t, http.StatusBadRequest, ape.Status)
	assert.Equal(t, "Summary is required.", ape.FieldErrors["summary"])
}

func TestCreateIssue_401_SentinelStillFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	_, err := c.CreateIssue(context.Background(), "PROJ", "Task")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrUnauthorized))
}

func TestCreateIssue_ValidatesRequiredArgs(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called when args are invalid")
	})))

	_, err := c.CreateIssue(context.Background(), "", "Task")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project")

	_, err = c.CreateIssue(context.Background(), "PROJ", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issueType")
}

// ---------------------------------------------------------------------------
// UpdateIssue
// ---------------------------------------------------------------------------

func TestUpdateIssue_Success_GoldenBody(t *testing.T) {
	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusNoContent, ""))
	defer srv.Close()

	c := newTestClient(t, srv)

	err := c.UpdateIssue(context.Background(), "PROJ-1",
		client.WithSummaryUpdate("new"),
		client.WithDescriptionTextUpdate("new desc"),
	)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPut, rec.method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1", rec.path)

	body := decodeJSONObject(t, rec.body)
	fields, ok := body["fields"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new", fields["summary"])
	desc, ok := fields["description"].(map[string]any)
	require.True(t, ok, "description must be ADF object")
	assert.Equal(t, "doc", desc["type"])
}

func TestUpdateIssue_400_SurfacesAPIError(t *testing.T) {
	const errBody = `{"errors":{"assignee":"Unknown account."}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	err := c.UpdateIssue(context.Background(), "PROJ-1",
		client.WithAssigneeAccountIDUpdate("unknown"),
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrBadRequest))

	var ape *client.APIError
	require.True(t, errors.As(err, &ape))
	assert.Equal(t, "Unknown account.", ape.FieldErrors["assignee"])
}

func TestUpdateIssue_404_Sentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	err := c.UpdateIssue(context.Background(), "PROJ-999",
		client.WithSummaryUpdate("x"),
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrNotFound))
}

func TestUpdateIssue_ValidatesKey(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called when key is empty")
	})))
	err := c.UpdateIssue(context.Background(), "", client.WithSummaryUpdate("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")
}

// ---------------------------------------------------------------------------
// AddComment
// ---------------------------------------------------------------------------

func TestAddComment_Success_TextBecomesADF(t *testing.T) {
	const respJSON = `{
		"id": "10100",
		"author": {"accountId": "acc-1", "displayName": "Alice"},
		"created": "2026-01-15T10:30:00.000+0000"
	}`

	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusCreated, respJSON))
	defer srv.Close()

	c := newTestClient(t, srv)

	got, err := c.AddComment(context.Background(), "PROJ-1",
		client.WithCommentText("looks good"),
	)
	require.NoError(t, err)

	assert.Equal(t, client.Comment{
		ID:                "10100",
		AuthorAccountID:   "acc-1",
		AuthorDisplayName: "Alice",
		Created:           "2026-01-15T10:30:00.000+0000",
	}, got)

	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1/comment", rec.path)

	body := decodeJSONObject(t, rec.body)
	bodyADF, ok := body["body"].(map[string]any)
	require.True(t, ok, `request "body" must be ADF object, got %T`, body["body"])
	assert.Equal(t, "doc", bodyADF["type"])

	descBytes, _ := json.Marshal(bodyADF)
	md, _, err := adf.RenderMarkdown(descBytes)
	require.NoError(t, err)
	assert.Contains(t, md, "looks good")
}

func TestAddComment_WithCommentADF_Passthrough(t *testing.T) {
	doc := json.RawMessage(`{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"rich"}]}]}`)

	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusCreated, `{"id":"1","author":{},"created":""}`))
	defer srv.Close()

	c := newTestClient(t, srv)

	_, err := c.AddComment(context.Background(), "PROJ-1", client.WithCommentADF(doc))
	require.NoError(t, err)

	body := decodeJSONObject(t, rec.body)
	bodyADF, ok := body["body"].(map[string]any)
	require.True(t, ok)
	content, ok := bodyADF["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
}

func TestAddComment_404_Sentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	_, err := c.AddComment(context.Background(), "PROJ-999", client.WithCommentText("x"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrNotFound))
}

func TestAddComment_ValidatesKey(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called when key is empty")
	})))
	_, err := c.AddComment(context.Background(), "", client.WithCommentText("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")
}

// ---------------------------------------------------------------------------
// ListTransitions + TransitionIssue
// ---------------------------------------------------------------------------

func TestListTransitions_Success(t *testing.T) {
	const respJSON = `{
		"transitions": [
			{"id": "11", "name": "Start Progress", "to": {"name": "In Progress"}},
			{"id": "21", "name": "Done", "to": {"name": "Done"}}
		]
	}`

	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusOK, respJSON))
	defer srv.Close()

	c := newTestClient(t, srv)

	got, err := c.ListTransitions(context.Background(), "PROJ-1")
	require.NoError(t, err)

	assert.Equal(t, http.MethodGet, rec.method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1/transitions", rec.path)

	assert.Equal(t, []client.Transition{
		{ID: "11", Name: "Start Progress", ToStatus: "In Progress"},
		{ID: "21", Name: "Done", ToStatus: "Done"},
	}, got)
}

func TestTransitionIssue_IDOnly_GoldenBody(t *testing.T) {
	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusNoContent, ""))
	defer srv.Close()

	c := newTestClient(t, srv)

	err := c.TransitionIssue(context.Background(), "PROJ-1", "11")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1/transitions", rec.path)

	body := decodeJSONObject(t, rec.body)
	transition, ok := body["transition"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "11", transition["id"])

	// No fields/update merged in for an id-only transition.
	_, hasFields := body["fields"]
	_, hasUpdate := body["update"]
	assert.False(t, hasFields, `"fields" must be absent for id-only transition`)
	assert.False(t, hasUpdate, `"update" must be absent for id-only transition`)
}

func TestTransitionIssue_WithFieldMergesFieldsObject(t *testing.T) {
	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusNoContent, ""))
	defer srv.Close()

	c := newTestClient(t, srv)

	err := c.TransitionIssue(context.Background(), "PROJ-1", "11",
		client.WithTransitionField("resolution", map[string]any{"name": "Done"}),
	)
	require.NoError(t, err)

	body := decodeJSONObject(t, rec.body)
	assert.Equal(t, map[string]any{"id": "11"}, body["transition"])

	fields, ok := body["fields"].(map[string]any)
	require.True(t, ok, `"fields" must be merged when WithTransitionField is used`)
	assert.Equal(t, map[string]any{"name": "Done"}, fields["resolution"])
}

func TestTransitionIssue_WithCommentText_AddsADFCommentUpdate(t *testing.T) {
	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusNoContent, ""))
	defer srv.Close()

	c := newTestClient(t, srv)

	err := c.TransitionIssue(context.Background(), "PROJ-1", "11",
		client.WithTransitionCommentText("shipping it"),
	)
	require.NoError(t, err)

	body := decodeJSONObject(t, rec.body)
	updateObj, ok := body["update"].(map[string]any)
	require.True(t, ok, `"update" must be present when WithTransitionCommentText is used`)

	comments, ok := updateObj["comment"].([]any)
	require.True(t, ok, `update.comment must be an array, got %T`, updateObj["comment"])
	require.Len(t, comments, 1)

	op, ok := comments[0].(map[string]any)
	require.True(t, ok)

	addOp, ok := op["add"].(map[string]any)
	require.True(t, ok, `comment op must be an "add", got: %+v`, op)

	bodyADF, ok := addOp["body"].(map[string]any)
	require.True(t, ok, "comment add.body must be ADF object")
	assert.Equal(t, "doc", bodyADF["type"])

	descBytes, _ := json.Marshal(bodyADF)
	md, _, err := adf.RenderMarkdown(descBytes)
	require.NoError(t, err)
	assert.Contains(t, md, "shipping it")
}

func TestTransitionIssue_400_SurfacesAPIError(t *testing.T) {
	const errBody = `{"errorMessages":["Transition is not valid for current status."]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	err := c.TransitionIssue(context.Background(), "PROJ-1", "999")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrBadRequest))

	var ape *client.APIError
	require.True(t, errors.As(err, &ape))
	assert.Equal(t, []string{"Transition is not valid for current status."}, ape.Messages)
}

func TestTransitionIssue_409_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	err := c.TransitionIssue(context.Background(), "PROJ-1", "11")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrConflict))
}

func TestTransitionIssue_ValidatesArgs(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called when args are invalid")
	})))
	err := c.TransitionIssue(context.Background(), "", "11")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")

	err = c.TransitionIssue(context.Background(), "PROJ-1", "")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "transition")
}

func TestListTransitions_ValidatesKey(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called when key is empty")
	})))
	_, err := c.ListTransitions(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")
}
