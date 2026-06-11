// Package main tests the write subcommands of the gojira CLI binary
// (create, update, comment, transitions, transition).
//
// All tests run against an httptest.Server that fakes the Jira REST v3
// endpoints — no live network. Each test drives the binary through
// run() / captureRun() so it exercises the same wiring real users see,
// including the configuration cascade and the exitErr→exit-code
// mapping.
package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Shared harness
// ---------------------------------------------------------------------------

// writeBaseEnv returns the minimal env map needed to satisfy
// gojira.LoadConfig for a write subcommand. Write commands do not need
// a real output directory, but LoadConfig requires the field to be
// non-empty, so we point it at t.TempDir() in the test harness.
func writeBaseEnv(t *testing.T, srvURL string) map[string]string {
	t.Helper()
	return map[string]string{
		"GOJIRA_SITE":       srvURL,
		"GOJIRA_USER":       "test@example.com",
		"GOJIRA_TOKEN":      "test-token",
		"GOJIRA_OUTPUT_DIR": t.TempDir(),
	}
}

// recordedRequest captures a request the test handler observed. The
// raw body is decoded into a generic any so individual tests can
// assert on whatever shape they expect.
type recordedRequest struct {
	method string
	path   string
	body   any
	raw    []byte
}

// writeServer is a tiny multiplexing httptest.Server that routes Jira
// write endpoints to per-test responses, while recording every request
// so the test can assert on the URL/method/body that reached it.
type writeServer struct {
	*httptest.Server
	mu       sync.Mutex
	recorded []recordedRequest

	// per-key transition lists returned by GET /transitions.
	transitionsByKey map[string][]map[string]any

	// status overrides per (method, path). Default is 201/204 for
	// the relevant verbs.
	statusByKey map[string]int

	// optional response body the handler writes back for POST /issue.
	createResponse []byte
}

func newWriteServer(t *testing.T) *writeServer {
	t.Helper()
	ws := &writeServer{
		transitionsByKey: map[string][]map[string]any{},
		statusByKey:      map[string]int{},
	}
	ws.Server = httptest.NewServer(http.HandlerFunc(ws.handle))
	t.Cleanup(ws.Server.Close)
	return ws
}

func (ws *writeServer) record(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	rec := recordedRequest{
		method: r.Method,
		path:   r.URL.Path,
		raw:    body,
	}
	if len(body) > 0 {
		var decoded any
		if err := json.Unmarshal(body, &decoded); err == nil {
			rec.body = decoded
		}
	}
	ws.mu.Lock()
	ws.recorded = append(ws.recorded, rec)
	ws.mu.Unlock()
	return body
}

func (ws *writeServer) records() []recordedRequest {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := make([]recordedRequest, len(ws.recorded))
	copy(out, ws.recorded)
	return out
}

func (ws *writeServer) handle(w http.ResponseWriter, r *http.Request) {
	_ = ws.record(r)
	const prefix = "/rest/api/3/issue"

	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	// POST /rest/api/3/issue — create
	case rest == "" || rest == "/":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if code, ok := ws.statusByKey["__create__"]; ok && code != http.StatusCreated {
			w.WriteHeader(code)
			return
		}
		body := ws.createResponse
		if body == nil {
			body = []byte(`{"id":"10001","key":"PROJ-123","self":"` + ws.URL + `/rest/api/3/issue/10001"}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
		return

	// .../transitions
	case strings.HasSuffix(rest, "/transitions"):
		key := strings.TrimPrefix(rest, "/")
		key = strings.TrimSuffix(key, "/transitions")
		switch r.Method {
		case http.MethodGet:
			ts := ws.transitionsByKey[key]
			payload := map[string]any{"transitions": ts}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(payload)
		case http.MethodPost:
			if code, ok := ws.statusByKey[key+"/transition"]; ok && code != http.StatusNoContent {
				w.WriteHeader(code)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return

	// .../comment
	case strings.HasSuffix(rest, "/comment"):
		key := strings.TrimPrefix(rest, "/")
		key = strings.TrimSuffix(key, "/comment")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"55555","author":{"displayName":"Alice"},"created":"2026-01-01T00:00:00.000+0000"}`))
		_ = key
		return

	// PUT .../<key> — update.
	default:
		key := strings.TrimPrefix(rest, "/")
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if code, ok := ws.statusByKey[key]; ok && code != http.StatusNoContent {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func TestRun_Create_Success(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "create",
			"--project", "PROJ",
			"--type", "Task",
			"--summary", "Hello world",
			"--description", "Body text",
			"--label", "urgent",
			"--label", "backend",
		}, env)

	assert.Equal(t, 0, code, "expected exit 0; stderr=%q stdout=%q", stderr, stdout)
	assert.Contains(t, stdout, "PROJ-123", "stdout should contain the created issue key")
	assert.Contains(t, stdout, "10001", "stdout should contain the issue id")

	recs := srv.records()
	require.GreaterOrEqual(t, len(recs), 1, "expected at least one HTTP call")
	create := recs[0]
	assert.Equal(t, http.MethodPost, create.method)
	assert.Equal(t, "/rest/api/3/issue", create.path)

	body, ok := create.body.(map[string]any)
	require.True(t, ok, "expected JSON object body, got %T", create.body)
	fields, ok := body["fields"].(map[string]any)
	require.True(t, ok, "expected fields object")
	project, _ := fields["project"].(map[string]any)
	assert.Equal(t, "PROJ", project["key"])
	itype, _ := fields["issuetype"].(map[string]any)
	assert.Equal(t, "Task", itype["name"])
	assert.Equal(t, "Hello world", fields["summary"])
	labels, ok := fields["labels"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"urgent", "backend"}, labels)
}

func TestRun_Create_DryRun_NoHTTP(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "create",
			"--project", "PROJ",
			"--type", "Task",
			"--summary", "Dry",
			"--dry-run",
		}, env)
	assert.Equal(t, 0, code, "expected exit 0; stderr=%q", stderr)
	assert.Empty(t, srv.records(), "dry-run must not contact the server")

	// stdout must be JSON containing the same fields the live create
	// would have posted.
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed),
		"dry-run stdout must be parseable JSON: %q", stdout)
	fields, ok := parsed["fields"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Dry", fields["summary"])
}

func TestRun_Create_MissingProject(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "create", "--summary", "x"}, env)
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "project")
}

func TestRun_Create_MissingSummary(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "create", "--project", "PROJ"}, env)
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "summary")
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

func TestRun_Update_Success(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "update", "PROJ-1",
			"--summary", "new summary",
			"--label", "x",
			"--label", "y",
		}, env)
	assert.Equal(t, 0, code, "expected exit 0; stderr=%q", stderr)
	assert.Contains(t, stdout, "Updated PROJ-1")

	recs := srv.records()
	require.Len(t, recs, 1)
	assert.Equal(t, http.MethodPut, recs[0].method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1", recs[0].path)

	body, ok := recs[0].body.(map[string]any)
	require.True(t, ok)
	fields, ok := body["fields"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new summary", fields["summary"])
}

func TestRun_Update_NoFields_Errors(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "update", "PROJ-1"}, env)
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "nothing to update")
	assert.Empty(t, srv.records(), "no HTTP call should be made when there are no fields")
}

func TestRun_Update_DryRun_NoHTTP(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "update", "PROJ-1",
			"--summary", "dry summary",
			"--dry-run",
		}, env)
	assert.Equal(t, 0, code, "expected exit 0; stderr=%q", stderr)
	assert.Empty(t, srv.records(), "dry-run must not contact the server")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed))
	fields, ok := parsed["fields"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "dry summary", fields["summary"])
}

func TestRun_Update_MissingKey(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "update", "--summary", "x"}, env)
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "issue-key")
}

// ---------------------------------------------------------------------------
// comment
// ---------------------------------------------------------------------------

func TestRun_Comment_Success(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "comment", "PROJ-1", "--text", "Hello there"}, env)
	assert.Equal(t, 0, code, "expected exit 0; stderr=%q", stderr)
	assert.Contains(t, stdout, "55555")
	assert.Contains(t, stdout, "PROJ-1")

	recs := srv.records()
	require.Len(t, recs, 1)
	assert.Equal(t, http.MethodPost, recs[0].method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1/comment", recs[0].path)
}

func TestRun_Comment_MissingText(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "comment", "PROJ-1"}, env)
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "text")
}

// ---------------------------------------------------------------------------
// transitions
// ---------------------------------------------------------------------------

func TestRun_Transitions_List(t *testing.T) {
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{
		{"id": "11", "name": "Start Progress", "to": map[string]any{"name": "In Progress"}},
		{"id": "21", "name": "Done", "to": map[string]any{"name": "Done"}},
	}
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions", "PROJ-1"}, env)
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.Contains(t, stdout, "11")
	assert.Contains(t, stdout, "Start Progress")
	assert.Contains(t, stdout, "In Progress")
	assert.Contains(t, stdout, "21")
	assert.Contains(t, stdout, "Done")
}

func TestRun_Transitions_EmptyList(t *testing.T) {
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{}
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transitions", "PROJ-1"}, env)
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.Contains(t, strings.ToLower(stdout), "no transitions")
	assert.Contains(t, stdout, "PROJ-1")
}

// ---------------------------------------------------------------------------
// transition
// ---------------------------------------------------------------------------

func TestRun_Transition_ByID(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transition", "PROJ-1", "--id", "21"}, env)
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.Contains(t, stdout, "Transitioned PROJ-1")

	recs := srv.records()
	require.Len(t, recs, 1)
	assert.Equal(t, http.MethodPost, recs[0].method)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1/transitions", recs[0].path)
	body, ok := recs[0].body.(map[string]any)
	require.True(t, ok)
	tr, ok := body["transition"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "21", tr["id"])
}

func TestRun_Transition_ByStatus(t *testing.T) {
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{
		{"id": "11", "name": "Start Progress", "to": map[string]any{"name": "In Progress"}},
		{"id": "21", "name": "Done", "to": map[string]any{"name": "Done"}},
	}
	env := writeBaseEnv(t, srv.URL)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transition", "PROJ-1", "--to-status", "Done"}, env)
	assert.Equal(t, 0, code, "stderr=%q", stderr)
	assert.Contains(t, stdout, "Transitioned PROJ-1")

	recs := srv.records()
	require.Len(t, recs, 2, "expected GET /transitions then POST /transitions")
	assert.Equal(t, http.MethodGet, recs[0].method)
	assert.Equal(t, http.MethodPost, recs[1].method)
	body, _ := recs[1].body.(map[string]any)
	tr, _ := body["transition"].(map[string]any)
	assert.Equal(t, "21", tr["id"])
}

func TestRun_Transition_BothFlags_Errors(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transition", "PROJ-1",
			"--id", "21", "--to-status", "Done",
		}, env)
	assert.Equal(t, 1, code)
	combined := strings.ToLower(stderr)
	assert.True(t,
		strings.Contains(combined, "--id") && strings.Contains(combined, "--to-status"),
		"stderr should mention both --id and --to-status: %q", stderr)
	assert.Empty(t, srv.records())
}

func TestRun_Transition_NeitherFlag_Errors(t *testing.T) {
	srv := newWriteServer(t)
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transition", "PROJ-1"}, env)
	assert.Equal(t, 1, code)
	combined := strings.ToLower(stderr)
	assert.True(t,
		strings.Contains(combined, "--id") || strings.Contains(combined, "--to-status"),
		"stderr should mention --id or --to-status: %q", stderr)
}

func TestRun_Transition_ByStatus_NoMatch(t *testing.T) {
	srv := newWriteServer(t)
	srv.transitionsByKey["PROJ-1"] = []map[string]any{
		{"id": "11", "name": "Start Progress", "to": map[string]any{"name": "In Progress"}},
	}
	env := writeBaseEnv(t, srv.URL)

	_, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "transition", "PROJ-1", "--to-status", "Done"}, env)
	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(stderr), "no transition")
}

// ---------------------------------------------------------------------------
// --help wires the new commands
// ---------------------------------------------------------------------------

func TestRun_Help_ListsWriteCommands(t *testing.T) {
	stdout, _, code := captureRun(context.Background(), []string{"gojira", "--help"}, nil)
	assert.Equal(t, 0, code)
	for _, sub := range []string{"create", "update", "comment", "transitions", "transition"} {
		assert.Contains(t, stdout, sub, "--help should list %q", sub)
	}
}
