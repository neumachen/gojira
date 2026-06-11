// facade_backend_test.go — verify facadeBackend against an httptest
// fake Jira. No live network: every HTTP call must hit the
// in-process httptest server.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gojira "github.com/neumachen/gojira"
)

// minimalIssueJSON renders the same minimal Jira issue shape the
// other facade tests use, including the self URL and a fields
// block that satisfies the parser without leaving anything null
// that would break the typed Issue projection.
func minimalIssueJSON(key, site string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": %q,
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": []
  }
}`, key, site+"/rest/api/3/issue/"+key, key))
}

// issueWithOutwardLinkJSON gives issue `key` an outward link to
// `linkedKey` so the orchestrator enqueues a follow-up fetch.
func issueWithOutwardLinkJSON(key, linkedKey, site string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": %q,
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [
      {"type": {"name": "Relates", "inward": "relates to", "outward": "relates to"},
       "outwardIssue": {"key": %q, "fields": {"summary": "linked"}}}
    ],
    "remotelinks": []
  }
}`, key, site+"/rest/api/3/issue/"+key, key, linkedKey))
}

// jiraFake is a tiny httptest.Server that serves a handful of Jira
// REST endpoints exercised by the facade backend tests.
// transitionsByKey lets list_transitions tests return canned lists.
type jiraFake struct {
	*httptest.Server
	transitionsByKey map[string][]map[string]any
}

func newJiraFake(t *testing.T) *jiraFake {
	t.Helper()
	jf := &jiraFake{
		transitionsByKey: map[string][]map[string]any{},
	}
	jf.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/rest/api/3/issue"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)

		// POST /issue — create
		if rest == "" || rest == "/" {
			if r.Method != http.MethodPost {
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"id":"10001","key":"NEW-1","self":%q}`,
				jf.URL+"/rest/api/3/issue/10001")))
			return
		}
		// .../transitions
		if strings.HasSuffix(rest, "/transitions") {
			key := strings.TrimPrefix(rest, "/")
			key = strings.TrimSuffix(key, "/transitions")
			switch r.Method {
			case http.MethodGet:
				ts := jf.transitionsByKey[key]
				_ = json.NewEncoder(w).Encode(map[string]any{"transitions": ts})
			case http.MethodPost:
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
			}
			return
		}
		// PUT .../<key>
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// .../comment
		if strings.HasSuffix(rest, "/comment") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"55","author":{"displayName":"Bot"},"created":"2026-01-02"}`))
			return
		}
		// GET /issue/<key> — issue fetch
		key := strings.TrimPrefix(rest, "/")
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[:idx]
		}
		var body []byte
		switch key {
		case "PROJ-1":
			body = issueWithOutwardLinkJSON("PROJ-1", "PROJ-2", jf.URL)
		case "PROJ-2":
			body = minimalIssueJSON("PROJ-2", jf.URL)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(jf.Server.Close)
	return jf
}

func testFacadeCfg(t *testing.T, siteURL string) gojira.Config {
	t.Helper()
	cfg, err := gojira.LoadConfig(map[string]string{
		"GOJIRA_SITE":               siteURL,
		"GOJIRA_USER":               "test@example.com",
		"GOJIRA_TOKEN":              "test-token",
		"GOJIRA_OUTPUT_DIR":         t.TempDir(),
		"GOJIRA_CONCURRENCY":        "1",
		"GOJIRA_ISSUE_CAP":          "0",
		"GOJIRA_INCLUDE_CHILDREN":   "false",
		"GOJIRA_INCLUDE_DEV_STATUS": "false",
	})
	require.NoError(t, err)
	return cfg
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestFacadeBackend_Classify(t *testing.T) {
	jf := newJiraFake(t)
	b := NewFacadeBackend(testFacadeCfg(t, jf.URL))
	res, err := b.Classify(context.Background(), "PROJ-1", "")
	require.NoError(t, err)
	assert.Equal(t, "PROJ-1", res.IssueKey)
}

func TestFacadeBackend_GetIssue(t *testing.T) {
	jf := newJiraFake(t)
	b := NewFacadeBackend(testFacadeCfg(t, jf.URL))
	issue, refs, err := b.GetIssue(context.Background(), "PROJ-1")
	require.NoError(t, err)
	assert.Equal(t, "PROJ-1", issue.Key)
	assert.GreaterOrEqual(t, len(refs), 1, "expected at least the outward issue link reference")
}

func TestFacadeBackend_Crawl_DrivesProgress(t *testing.T) {
	jf := newJiraFake(t)
	b := NewFacadeBackend(testFacadeCfg(t, jf.URL))
	var progressCalls int32
	progress := func(done, total int, msg string) { atomic.AddInt32(&progressCalls, 1) }
	sum, err := b.Crawl(context.Background(), []string{"PROJ-1"}, progress)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, sum.Fetched, 1, "crawl summary should reflect at least one fetched issue")
	assert.GreaterOrEqual(t, atomic.LoadInt32(&progressCalls), int32(1),
		"progress callback must fire at least once when an issue is fetched")
}

func TestFacadeBackend_ListTransitions(t *testing.T) {
	jf := newJiraFake(t)
	jf.transitionsByKey["PROJ-1"] = []map[string]any{
		{"id": "11", "name": "Start", "to": map[string]any{"name": "In Progress"}},
	}
	b := NewFacadeBackend(testFacadeCfg(t, jf.URL))
	ts, err := b.ListTransitions(context.Background(), "PROJ-1")
	require.NoError(t, err)
	require.Len(t, ts, 1)
	assert.Equal(t, "11", ts[0].ID)
	assert.Equal(t, "In Progress", ts[0].ToStatus)
}

func TestFacadeBackend_CreateIssue(t *testing.T) {
	jf := newJiraFake(t)
	b := NewFacadeBackend(testFacadeCfg(t, jf.URL))
	res, err := b.CreateIssue(context.Background(), "PROJ", "Task", CreateIssueFields{
		Summary: "hi",
	})
	require.NoError(t, err)
	assert.Equal(t, "NEW-1", res.Key)
}

func TestFacadeBackend_AddComment(t *testing.T) {
	jf := newJiraFake(t)
	b := NewFacadeBackend(testFacadeCfg(t, jf.URL))
	c, err := b.AddComment(context.Background(), "PROJ-1", "hello")
	require.NoError(t, err)
	assert.Equal(t, "55", c.ID)
}

func TestFacadeBackend_TransitionIssue_BothOrNeitherErrors(t *testing.T) {
	jf := newJiraFake(t)
	b := NewFacadeBackend(testFacadeCfg(t, jf.URL))
	err := b.TransitionIssue(context.Background(), "PROJ-1", "", "", TransitionFields{})
	assert.Error(t, err, "neither id nor toStatus must error")
	err = b.TransitionIssue(context.Background(), "PROJ-1", "11", "Done", TransitionFields{})
	assert.Error(t, err, "both id and toStatus must error")
}
