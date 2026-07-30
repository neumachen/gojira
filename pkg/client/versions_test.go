package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neumachen/gojira/pkg/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CreateVersion
// ---------------------------------------------------------------------------

func TestCreateVersion_Success_GoldenBody(t *testing.T) {
	const respJSON = `{
		"id": "20000",
		"name": "Release-abc1234",
		"projectId": 10000,
		"released": true,
		"releaseDate": "2000-01-01",
		"self": "https://example.atlassian.net/rest/api/3/version/20000"
	}`

	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusCreated, respJSON))
	defer srv.Close()

	c := newTestClient(t, srv)

	got, err := c.CreateVersion(context.Background(), "Release-abc1234", "10000",
		client.WithVersionReleased(true),
		client.WithVersionReleaseDate("2000-01-01"),
	)
	require.NoError(t, err)

	assert.Equal(t, client.Version{
		ID:          "20000",
		Name:        "Release-abc1234",
		ProjectID:   10000,
		Released:    true,
		ReleaseDate: "2000-01-01",
		Self:        "https://example.atlassian.net/rest/api/3/version/20000",
	}, got)

	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/rest/api/3/version", rec.path)

	body := decodeJSONObject(t, rec.body)
	assert.Equal(t, "Release-abc1234", body["name"])
	// projectId must be a JSON number (decoded as float64 in a generic map).
	assert.Equal(t, float64(10000), body["projectId"], "projectId must be a JSON number")
	assert.Equal(t, true, body["released"])
	assert.Equal(t, "2000-01-01", body["releaseDate"])
}

func TestCreateVersion_ProjectKey_ResolvesThenPosts(t *testing.T) {
	var (
		resolveHit bool
		postRec    requestCapture
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/project/EXAMPLE", func(w http.ResponseWriter, _ *http.Request) {
		resolveHit = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"10000","key":"EXAMPLE","name":"Example"}`))
	})
	mux.HandleFunc("/rest/api/3/version", captureHandler(&postRec, http.StatusCreated,
		`{"id":"20000","name":"Release-abc1234","projectId":10000,"self":"https://example.atlassian.net/rest/api/3/version/20000"}`))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv)

	got, err := c.CreateVersion(context.Background(), "Release-abc1234", "EXAMPLE")
	require.NoError(t, err)

	assert.True(t, resolveHit, "ResolveProjectID GET must be issued for a project key")
	assert.Equal(t, "20000", got.ID)

	// The rendered POST body must carry the resolved numeric projectId.
	body := decodeJSONObject(t, postRec.body)
	assert.Equal(t, float64(10000), body["projectId"], "resolved projectId must be a JSON number")
}

func TestCreateVersion_NumericProject_SkipsResolution(t *testing.T) {
	var postRec requestCapture
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/project/", func(http.ResponseWriter, *http.Request) {
		t.Fatal("ResolveProjectID must NOT be called for a numeric project id")
	})
	mux.HandleFunc("/rest/api/3/version", captureHandler(&postRec, http.StatusCreated,
		`{"id":"20000","name":"Release-abc1234","projectId":10000}`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.CreateVersion(context.Background(), "Release-abc1234", "10000")
	require.NoError(t, err)
	assert.Equal(t, "20000", got.ID)
	assert.Equal(t, http.MethodPost, postRec.method)
	assert.Equal(t, "/rest/api/3/version", postRec.path)
}

func TestCreateVersion_400_SurfacesAPIError(t *testing.T) {
	const errBody = `{"errorMessages":["validation failed"],"errors":{"name":"A version with this name already exists."}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errBody))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	_, err := c.CreateVersion(context.Background(), "Release-abc1234", "10000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrBadRequest))

	var ape *client.APIError
	require.True(t, errors.As(err, &ape))
	assert.Equal(t, http.StatusBadRequest, ape.Status)
	assert.Equal(t, "A version with this name already exists.", ape.FieldErrors["name"])
}

func TestCreateVersion_ValidatesRequiredArgs(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called when args are invalid")
	})))

	_, err := c.CreateVersion(context.Background(), "", "10000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name")

	_, err = c.CreateVersion(context.Background(), "Release-abc1234", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project")
}

// ---------------------------------------------------------------------------
// ResolveProjectID
// ---------------------------------------------------------------------------

func TestResolveProjectID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rest/api/3/project/EXAMPLE", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"10000","key":"EXAMPLE","name":"Example"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	id, err := c.ResolveProjectID(context.Background(), "EXAMPLE")
	require.NoError(t, err)
	assert.Equal(t, 10000, id)
}

func TestResolveProjectID_404_SurfacesNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["No project could be found with key 'NOPE'."]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))
	_, err := c.ResolveProjectID(context.Background(), "NOPE")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrNotFound))
}

func TestResolveProjectID_NonNumericID_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"not-a-number","key":"EXAMPLE"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.ResolveProjectID(context.Background(), "EXAMPLE")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse id")
}

func TestResolveProjectID_EmptyKey_NoHTTP(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called with an empty key")
	})))
	_, err := c.ResolveProjectID(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")
}

// ---------------------------------------------------------------------------
// UpdateVersion
// ---------------------------------------------------------------------------

func TestUpdateVersion_Success_ReleasedFlip(t *testing.T) {
	const respJSON = `{"id":"20000","name":"Release-abc1234","projectId":10000,"released":true}`

	var rec requestCapture
	srv := httptest.NewServer(captureHandler(&rec, http.StatusOK, respJSON))
	defer srv.Close()

	c := newTestClient(t, srv)

	got, err := c.UpdateVersion(context.Background(), "20000",
		client.WithVersionReleased(true),
	)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPut, rec.method)
	assert.Equal(t, "/rest/api/3/version/20000", rec.path)
	assert.True(t, got.Released)

	body := decodeJSONObject(t, rec.body)
	assert.Equal(t, true, body["released"])
	// name/project are not part of an update body.
	_, hasName := body["name"]
	_, hasProject := body["projectId"]
	assert.False(t, hasName, "update body must not carry name")
	assert.False(t, hasProject, "update body must not carry projectId")
}

func TestUpdateVersion_ValidatesID(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called when id is empty")
	})))
	_, err := c.UpdateVersion(context.Background(), "", client.WithVersionReleased(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id")
}

// ---------------------------------------------------------------------------
// ListVersions — pagination
// ---------------------------------------------------------------------------

func TestListVersions_PaginatesUntilIsLast(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Query().Get("startAt") {
		case "0":
			_, _ = w.Write([]byte(`{"values":[{"id":"20000","name":"1.0","projectId":10000,"released":true,"releaseDate":"2000-01-01"}],"startAt":0,"maxResults":1,"total":2,"isLast":false}`))
		default:
			_, _ = w.Write([]byte(`{"values":[{"id":"20001","name":"2.0","projectId":10000,"released":false}],"startAt":1,"maxResults":1,"total":2,"isLast":true}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	got, err := c.ListVersions(context.Background(), "EXAMPLE")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "20000", got[0].ID)
	assert.Equal(t, "1.0", got[0].Name)
	assert.Equal(t, "2000-01-01", got[0].ReleaseDate)
	assert.Equal(t, "20001", got[1].ID)
	assert.False(t, got[1].Released)
	require.Len(t, paths, 2, "must issue two paginated requests")
}

func TestListVersions_SinglePageStopsAfterOneRequest(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"values":[{"id":"20000","name":"1.0","projectId":10000}],"startAt":0,"maxResults":50,"total":1,"isLast":true}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.ListVersions(context.Background(), "EXAMPLE")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 1, calls, "isLast=true on the first page must stop after a single request")
}

func TestListVersions_EmptyResultReturnsEmptySlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"values":[],"startAt":0,"maxResults":50,"total":0,"isLast":true}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.ListVersions(context.Background(), "EXAMPLE")
	require.NoError(t, err)
	assert.Empty(t, got, "an empty project must yield an empty slice and no error")
}

func TestListVersions_EmptyProject_NoHTTP(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called with an empty project")
	})))
	_, err := c.ListVersions(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "projectIDOrKey")
}

// ---------------------------------------------------------------------------
// RenderCreateVersionBody / RenderUpdateVersionBody — pure builder
// ---------------------------------------------------------------------------

func TestRenderCreateVersionBody_NumericProjectEmitsProjectID(t *testing.T) {
	t.Parallel()
	body, err := client.RenderCreateVersionBody("Release-abc1234", "10000",
		client.WithVersionReleased(true),
	)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "Release-abc1234", got["name"])
	assert.Equal(t, float64(10000), got["projectId"], "numeric project must emit projectId number")
	assert.Equal(t, true, got["released"])
}

func TestRenderCreateVersionBody_KeyProjectOmitsProjectID(t *testing.T) {
	t.Parallel()
	body, err := client.RenderCreateVersionBody("Release-abc1234", "EXAMPLE")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "Release-abc1234", got["name"])
	_, has := got["projectId"]
	assert.False(t, has, "a project key must omit projectId (network-free dry-run)")
}

func TestRenderCreateVersionBody_InvalidReleaseDate_Errors(t *testing.T) {
	t.Parallel()
	_, err := client.RenderCreateVersionBody("Release-abc1234", "10000",
		client.WithVersionReleaseDate("01-01-2000"),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "releaseDate")
}

func TestRenderCreateVersionBody_InvalidStartDate_Errors(t *testing.T) {
	t.Parallel()
	_, err := client.RenderCreateVersionBody("Release-abc1234", "10000",
		client.WithVersionStartDate("2000/01/01"),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startDate")
}

// TestRenderVersionBody_ExplicitFalseVsUnset pins the presence contract at
// the client render layer: an explicit false (released/archived) MUST be
// emitted so it is distinguishable from unset, while an option that is
// never supplied MUST leave the key out entirely. Also exercises
// WithVersionArchived + WithVersionStartDate.
func TestRenderVersionBody_ExplicitFalseVsUnset(t *testing.T) {
	t.Parallel()

	body, err := client.RenderUpdateVersionBody(
		client.WithVersionReleased(false),
		client.WithVersionArchived(false),
		client.WithVersionStartDate("2000-01-02"),
	)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, false, got["released"], "explicit false released must be emitted")
	assert.Equal(t, false, got["archived"], "explicit false archived must be emitted")
	assert.Equal(t, "2000-01-02", got["startDate"])

	body2, err := client.RenderUpdateVersionBody(
		client.WithVersionDescription("only description"),
	)
	require.NoError(t, err)
	var got2 map[string]any
	require.NoError(t, json.Unmarshal(body2, &got2))
	_, hasReleased := got2["released"]
	_, hasArchived := got2["archived"]
	assert.False(t, hasReleased, "unset released must be omitted")
	assert.False(t, hasArchived, "unset archived must be omitted")
}

func TestRenderUpdateVersionBody_OnlySetFields(t *testing.T) {
	t.Parallel()
	body, err := client.RenderUpdateVersionBody(
		client.WithVersionReleased(true),
		client.WithVersionDescription("done"),
	)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, true, got["released"])
	assert.Equal(t, "done", got["description"])
	_, hasArchived := got["archived"]
	assert.False(t, hasArchived, "unset options must be omitted")
}
