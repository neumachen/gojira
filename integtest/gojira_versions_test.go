// Facade acceptance test for version (release) management and the Gap-B
// fixVersions issue field. It drives the gojira facade end-to-end against
// an httptest.Server (no live Jira, placeholder values only) and asserts
// on the request bodies and parsed responses across the full flow:
//
//	CreateVersion(EXAMPLE, Release-abc1234, released, releaseDate)
//	  -> resolves the project key, POSTs, returns id 20000
//	UpdateIssue(EXAMPLE-1, WithFixVersionIDsUpdate("20000"))
//	  -> PUT body carries fixVersions
//	ListVersions(EXAMPLE)
//	  -> parses the paginated list
package integtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/pkg/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersions_Facade_EndToEnd(t *testing.T) {
	t.Parallel()

	var (
		resolveHits int
		createBody  []byte
	)

	mux := http.NewServeMux()
	// ResolveProjectID: GET /rest/api/3/project/EXAMPLE -> numeric id.
	mux.HandleFunc("/rest/api/3/project/EXAMPLE", func(w http.ResponseWriter, _ *http.Request) {
		resolveHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"10000","key":"EXAMPLE","name":"Example"}`)
	})
	// CreateVersion: POST /rest/api/3/version -> created version.
	mux.HandleFunc("/rest/api/3/version", func(w http.ResponseWriter, r *http.Request) {
		createBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"20000","name":"Release-abc1234","projectId":10000,"released":true,"releaseDate":"2000-01-01","self":"https://example.atlassian.net/rest/api/3/version/20000"}`)
	})
	// ListVersions: paginated GET /rest/api/3/project/EXAMPLE/version.
	mux.HandleFunc("/rest/api/3/project/EXAMPLE/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"values":[{"id":"20000","name":"Release-abc1234","projectId":10000,"released":true,"releaseDate":"2000-01-01"}],"startAt":0,"maxResults":50,"total":1,"isLast":true}`)
	})

	var updateCap capturedRequest
	// UpdateIssue: PUT /rest/api/3/issue/EXAMPLE-1.
	mux.HandleFunc("/rest/api/3/issue/EXAMPLE-1", func(w http.ResponseWriter, r *http.Request) {
		updateCap.method = r.Method
		updateCap.path = r.URL.Path
		updateCap.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())
	ctx := context.Background()

	// --- CreateVersion (project KEY resolves to numeric id, then POST) ---
	v, err := gojira.CreateVersion(ctx, cfg, "Release-abc1234", "EXAMPLE",
		client.WithVersionReleased(true),
		client.WithVersionReleaseDate("2000-01-01"),
	)
	require.NoError(t, err)
	assert.Equal(t, 1, resolveHits, "a project key must trigger exactly one ResolveProjectID GET")
	assert.Equal(t, "20000", v.ID)
	assert.Equal(t, "Release-abc1234", v.Name)
	assert.Equal(t, 10000, v.ProjectID)
	assert.True(t, v.Released)

	created := decodeJSON(t, createBody)
	assert.Equal(t, "Release-abc1234", created["name"])
	assert.Equal(t, float64(10000), created["projectId"], "resolved projectId must be a JSON number")
	assert.Equal(t, true, created["released"])
	assert.Equal(t, "2000-01-01", created["releaseDate"])

	// --- UpdateIssue with fixVersions by id (set-replace) ---
	err = gojira.UpdateIssue(ctx, cfg, "EXAMPLE-1",
		client.WithFixVersionIDsUpdate("20000"),
	)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, updateCap.method)
	assert.Equal(t, "/rest/api/3/issue/EXAMPLE-1", updateCap.path)

	updBody := decodeJSON(t, updateCap.body)
	fields, ok := updBody["fields"].(map[string]any)
	require.True(t, ok, "update body must carry a fields object")
	fv, ok := fields["fixVersions"].([]any)
	require.True(t, ok, "fixVersions must be present, got %T", fields["fixVersions"])
	assert.Equal(t, []any{map[string]any{"id": "20000"}}, fv)

	// --- ListVersions ---
	vs, err := gojira.ListVersions(ctx, cfg, "EXAMPLE")
	require.NoError(t, err)
	require.Len(t, vs, 1)
	assert.Equal(t, "20000", vs[0].ID)
	assert.Equal(t, "Release-abc1234", vs[0].Name)
	assert.True(t, vs[0].Released)
	assert.Equal(t, "2000-01-01", vs[0].ReleaseDate)

	// self round-trips too.
	assert.Equal(t, "https://example.atlassian.net/rest/api/3/version/20000", v.Self)
}

// TestVersions_Facade_Create400_SurfacesBadRequest exercises the facade
// error path: a 400 from POST /version surfaces as client.ErrBadRequest
// (with the *APIError field details) through gojira.CreateVersion.
func TestVersions_Facade_Create400_SurfacesBadRequest(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errorMessages":["validation failed"],"errors":{"name":"A version with this name already exists."}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := testConfig(t, srv.URL, t.TempDir())
	_, err := gojira.CreateVersion(context.Background(), cfg, "Release-abc1234", "10000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrBadRequest), "facade must surface ErrBadRequest")

	var ape *client.APIError
	require.True(t, errors.As(err, &ape))
	assert.Equal(t, "A version with this name already exists.", ape.FieldErrors["name"])
}
