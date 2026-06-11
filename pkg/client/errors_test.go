package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neumachen/gojira/pkg/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 400 with field errors → *APIError, errors.Is(ErrBadRequest), errors.As works
// ---------------------------------------------------------------------------

// TestAPIError_400_WithFieldErrors drives a Jira-shaped 400 response back
// through the existing PUT test seam and asserts that the returned error
// (a) still classifies as ErrBadRequest via errors.Is, AND (b) can be
// type-asserted via errors.As to expose the per-field errors and the
// top-level errorMessages. This is the failure-introspection contract the
// Phase-2 write methods depend on so callers can tell users WHICH field
// the server rejected.
func TestAPIError_400_WithFieldErrors(t *testing.T) {
	const body = `{
        "errorMessages": ["Field 'summary' is required."],
        "errors": {
            "summary":           "Summary is required.",
            "customfield_10010": "Sprint must be active."
        }
    }`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	_, err := c.DoPutForTest(context.Background(), srv.URL+"/rest/api/3/issue/X-1", []byte(`{}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrBadRequest),
		"errors.Is(err, ErrBadRequest) must remain true; got: %v", err)

	var ape *client.APIError
	require.True(t, errors.As(err, &ape),
		"errors.As(err, *APIError) must succeed; got: %v", err)
	assert.Equal(t, http.StatusBadRequest, ape.Status, "Status")
	assert.Equal(t, []string{"Field 'summary' is required."}, ape.Messages, "Messages")
	assert.Equal(t, map[string]string{
		"summary":           "Summary is required.",
		"customfield_10010": "Sprint must be active.",
	}, ape.FieldErrors, "FieldErrors")

	msg := ape.Error()
	assert.Contains(t, msg, "400", "Error() should reference the status")
	assert.Contains(t, msg, "Summary is required.", "Error() should carry the message text")
	assert.Contains(t, msg, "summary", "Error() should mention the failing field id")
}

// ---------------------------------------------------------------------------
// 409 with errorMessages only → ErrConflict + APIError
// ---------------------------------------------------------------------------

func TestAPIError_409_Conflict(t *testing.T) {
	const body = `{"errorMessages":["Invalid workflow transition for current state."]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	_, err := c.DoPutForTest(context.Background(), srv.URL+"/rest/api/3/issue/X-1/transitions", []byte(`{}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrConflict),
		"errors.Is(err, ErrConflict) must remain true; got: %v", err)

	var ape *client.APIError
	require.True(t, errors.As(err, &ape))
	assert.Equal(t, http.StatusConflict, ape.Status, "Status")
	assert.Equal(t, []string{"Invalid workflow transition for current state."}, ape.Messages, "Messages")
	assert.Empty(t, ape.FieldErrors, "FieldErrors should be empty when only errorMessages is present")
}

// ---------------------------------------------------------------------------
// Degraded path — non-JSON body still classifies, no panic
// ---------------------------------------------------------------------------

func TestAPIError_Unparseable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A reverse proxy / WAF often interposes an HTML page instead of
		// the JSON error body. The client must still classify by status.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<html><body>boom</body></html>")
	}))
	defer srv.Close()

	c := newTestClient(t, srv, client.WithMaxRetries(0))

	_, err := c.DoPutForTest(context.Background(), srv.URL+"/rest/api/3/issue/X-1", []byte(`{}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrBadRequest),
		"unparseable body must still classify as ErrBadRequest; got: %v", err)

	var ape *client.APIError
	require.True(t, errors.As(err, &ape),
		"unparseable body must still surface an *APIError so callers can inspect Status")
	assert.Equal(t, http.StatusBadRequest, ape.Status)
	assert.Empty(t, ape.Messages, "no Messages when body is unparseable")
	assert.Empty(t, ape.FieldErrors, "no FieldErrors when body is unparseable")
}

// ---------------------------------------------------------------------------
// Error() determinism — sorted field-error keys
// ---------------------------------------------------------------------------

// TestAPIError_ErrorStringStable confirms Error() output is deterministic
// regardless of map iteration order, by building an APIError with several
// field errors, calling Error() repeatedly, and asserting the field-error
// keys appear in lexicographic order (the only ordering callers can rely
// on without coupling to map randomness).
func TestAPIError_ErrorStringStable(t *testing.T) {
	ape := &client.APIError{
		Status:   http.StatusBadRequest,
		Messages: []string{"validation failed"},
		FieldErrors: map[string]string{
			"customfield_10010": "Sprint must be active.",
			"assignee":          "Unknown account ID.",
			"summary":           "Summary is required.",
		},
	}

	first := ape.Error()
	for i := 0; i < 20; i++ {
		if got := ape.Error(); got != first {
			t.Fatalf("Error() must be deterministic; iter %d differs:\n first: %q\n  now: %q", i, first, got)
		}
	}

	// Sorted order = assignee, customfield_10010, summary. Locate each
	// key's first occurrence and confirm they ascend monotonically.
	idxAssignee := strings.Index(first, "assignee=")
	idxCustom := strings.Index(first, "customfield_10010=")
	idxSummary := strings.Index(first, "summary=")
	require.NotEqual(t, -1, idxAssignee, "Error() must mention assignee field")
	require.NotEqual(t, -1, idxCustom, "Error() must mention customfield_10010 field")
	require.NotEqual(t, -1, idxSummary, "Error() must mention summary field")
	assert.Less(t, idxAssignee, idxCustom, "assignee should precede customfield_10010 (sorted)")
	assert.Less(t, idxCustom, idxSummary, "customfield_10010 should precede summary (sorted)")
}
