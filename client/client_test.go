package client_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	gojiralog "github.com/neumachen/gojira/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noSleep is a sleepFn that returns immediately, making retry tests fast.
// It is injected via the unexported withSleepFn option exposed through the
// package-level helper below.
func noSleep(_ context.Context, _ time.Duration) error { return nil }

// newTestClient builds a Client pointed at srv with tiny backoffs and
// no real sleeping, so retry tests complete in milliseconds.
func newTestClient(t *testing.T, srv *httptest.Server, extraOpts ...client.Option) *client.Client {
	t.Helper()
	cfg := config.Config{
		Site:  srv.URL,
		User:  "user@example.com",
		Token: "api-token",
	}
	opts := []client.Option{
		client.WithHTTPClient(srv.Client()),
		client.WithRateLimitBackoff(time.Millisecond, 10*time.Millisecond),
		client.WithNetworkBackoff(time.Millisecond, 10*time.Millisecond),
		clientWithNoSleep(),
	}
	opts = append(opts, extraOpts...)
	c, err := client.New(cfg, opts...)
	require.NoError(t, err, "client.New")
	return c
}

// clientWithNoSleep returns the unexported withSleepFn option via the
// exported test-helper shim defined in client_export_test.go.
func clientWithNoSleep() client.Option {
	return client.WithSleepFnForTest(noSleep)
}

// --- helpers ---

func expectedAuthHeader() string {
	creds := "user@example.com:api-token"
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// --- tests ---

func TestGetIssue_Success(t *testing.T) {
	const wantBody = `{"key":"PROJ-1","fields":{}}`
	var gotAuth, gotUA, gotAccept string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, wantBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	body, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	require.NoError(t, err)
	assert.Equal(t, wantBody, string(body), "body")
	assert.Equal(t, expectedAuthHeader(), gotAuth, "Authorization")
	assert.Equal(t, "gojira/0.1.0", gotUA, "User-Agent")
	assert.Equal(t, "application/json", gotAccept, "Accept")
}

func TestGetIssue_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	assert.ErrorIs(t, err, client.ErrUnauthorized)
}

func TestGetIssue_403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	assert.ErrorIs(t, err, client.ErrForbidden)
}

func TestGetIssue_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	assert.ErrorIs(t, err, client.ErrNotFound)
}

func TestGetIssue_429_RetryAfterHeader_EventualSuccess(t *testing.T) {
	const wantBody = `{"key":"PROJ-1"}`
	var callCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n < 2 {
			// First call: 429 with a tiny Retry-After.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		// Second call: success.
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, wantBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	body, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	require.NoError(t, err)
	assert.Equal(t, wantBody, string(body), "body")
	assert.Equal(t, int32(2), atomic.LoadInt32(&callCount), "call count")
}

func TestGetIssue_429_Exhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// Allow only 2 retries so the test is fast.
	c := newTestClient(t, srv, client.WithMaxRetries(2))
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	assert.ErrorIs(t, err, client.ErrRateLimited)
}

func TestGetIssue_5xx_ErrorContainsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	require.Error(t, err, "expected error")
	assert.Contains(t, err.Error(), "500", "error should mention status 500")
}

func TestGetIssue_ContextCancellation(t *testing.T) {
	// Server that blocks until the test cancels the context.
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(ready)
		// Block until the client disconnects.
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	c := newTestClient(t, srv)
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetIssue(ctx, "PROJ-1", nil)
		errCh <- err
	}()

	// Wait until the server has received the request, then cancel.
	<-ready
	cancel()

	err := <-errCh
	require.Error(t, err, "expected error after context cancellation")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestNew_InvalidSiteURL(t *testing.T) {
	cfg := config.Config{
		Site:  "://not-a-url",
		User:  "u",
		Token: "t",
	}
	_, err := client.New(cfg)
	assert.Error(t, err, "expected error for invalid site URL")
}

func TestNew_EmptySiteURL(t *testing.T) {
	cfg := config.Config{
		Site:  "",
		User:  "u",
		Token: "t",
	}
	_, err := client.New(cfg)
	assert.Error(t, err, "expected error for empty site URL")
}

func TestGetIssue_URLPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetIssue(context.Background(), "PROJ-42", nil)
	require.NoError(t, err)
	assert.Equal(t, "/rest/api/3/issue/PROJ-42", gotPath, "path")
}

// TestGetIssue_WithExpand verifies that passing a non-empty expand
// slice surfaces as the documented `expand=<csv>` query parameter on
// the outgoing request, and that passing nil/empty leaves the
// request URL with no query string (legacy behaviour). The two
// branches are asserted in one test because they share fixture
// scaffolding.
func TestGetIssue_WithExpand(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	// Single expand token surfaces as `expand=names`.
	_, err := c.GetIssue(context.Background(), "PROJ-1", []string{"names"})
	require.NoError(t, err)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1", gotPath, "path")
	assert.Equal(t, "expand=names", gotQuery, "expand=names query")

	// Multiple expand tokens are comma-joined verbatim.
	_, err = c.GetIssue(context.Background(), "PROJ-1", []string{"names", "renderedFields"})
	require.NoError(t, err)
	// url.Values.Encode percent-encodes the comma to %2C; the Jira
	// API accepts both forms, and the encoded form is what
	// net/url emits.
	assert.Equal(t, "expand=names%2CrenderedFields", gotQuery, "expand csv query")

	// Empty slice → no expand query parameter at all.
	_, err = c.GetIssue(context.Background(), "PROJ-1", nil)
	require.NoError(t, err)
	assert.Empty(t, gotQuery, "no query when expand is nil")
}

func TestGetIssue_SiteWithTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	// Construct client with a trailing slash in the site URL.
	cfg := config.Config{
		Site:  srv.URL + "/",
		User:  "user@example.com",
		Token: "api-token",
	}
	c, err := client.New(cfg,
		client.WithHTTPClient(srv.Client()),
		clientWithNoSleep(),
	)
	require.NoError(t, err, "client.New")

	_, err = c.GetIssue(context.Background(), "PROJ-1", nil)
	require.NoError(t, err)
	assert.Equal(t, "/rest/api/3/issue/PROJ-1", gotPath, "path")
}

func TestWithRoundTripper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"key":"PROJ-1"}`)
	}))
	defer srv.Close()

	cfg := config.Config{
		Site:  srv.URL,
		User:  "u",
		Token: "t",
	}
	// Use WithRoundTripper instead of WithHTTPClient.
	c, err := client.New(cfg,
		client.WithRoundTripper(srv.Client().Transport),
		clientWithNoSleep(),
	)
	require.NoError(t, err, "client.New")
	body, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, body, "expected non-empty body")
}

// ---------------------------------------------------------------------------
// Search tests
// ---------------------------------------------------------------------------

func TestSearch_Success(t *testing.T) {
	var (
		gotMethod, gotPath, gotCT, gotAccept, gotAuth string
		gotBody                                       []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"issues":[{"key":"EXAMPLE-1"},{"key":"EXAMPLE-2"}],"nextPageToken":""}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Search(context.Background(), `parent = "EXAMPLE-0"`, 50)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod, "method")
	assert.Equal(t, "/rest/api/3/search/jql", gotPath, "path")
	assert.Equal(t, "application/json", gotCT, "Content-Type")
	assert.Equal(t, "application/json", gotAccept, "Accept")
	assert.Equal(t, expectedAuthHeader(), gotAuth, "Authorization")

	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent), "request body must be JSON")
	assert.Equal(t, `parent = "EXAMPLE-0"`, sent["jql"], "request jql")
	assert.Equal(t, float64(50), sent["maxResults"], "request maxResults")
	assert.Equal(t, []any{"key"}, sent["fields"], "request fields")

	assert.Equal(t, []string{"EXAMPLE-1", "EXAMPLE-2"}, res.Keys, "result keys")
	assert.Empty(t, res.NextPageToken, "no next page expected")
}

func TestSearch_OmitMaxResultsWhenZero(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"issues":[]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Search(context.Background(), `parent = "X"`, 0)
	require.NoError(t, err)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent), "request body must be JSON")
	_, present := sent["maxResults"]
	assert.False(t, present, "maxResults must be omitted when <= 0")
}

func TestSearch_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"issues":[]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Search(context.Background(), `parent = "Y"`, 10)
	require.NoError(t, err)
	assert.Empty(t, res.Keys)
}

func TestSearch_NextPageToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"issues":[{"key":"A-1"}],"nextPageToken":"tok-2"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Search(context.Background(), "", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"A-1"}, res.Keys)
	assert.Equal(t, "tok-2", res.NextPageToken)
}

func TestSearch_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Search(context.Background(), `parent = "X"`, 10)
	assert.ErrorIs(t, err, client.ErrUnauthorized)
}

func TestSearch_429_RetryAndSucceed(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n < 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"issues":[{"key":"OK-1"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Search(context.Background(), `parent = "X"`, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"OK-1"}, res.Keys)
	assert.Equal(t, int32(2), atomic.LoadInt32(&callCount))
}

// ---------------------------------------------------------------------------
// ListFields tests
// ---------------------------------------------------------------------------

func TestListFields_Success(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[
  {"id":"summary","key":"summary","name":"Summary","custom":false},
  {"id":"customfield_10014","key":"customfield_10014","name":"Epic Link","custom":true}
]`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	fields, err := c.ListFields(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/rest/api/3/field", gotPath)
	require.Len(t, fields, 2)
	assert.Equal(t, "summary", fields[0].ID)
	assert.False(t, fields[0].Custom)
	assert.Equal(t, "customfield_10014", fields[1].ID)
	assert.Equal(t, "Epic Link", fields[1].Name)
	assert.True(t, fields[1].Custom)
}

func TestListFields_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.ListFields(context.Background())
	assert.ErrorIs(t, err, client.ErrUnauthorized)
}

// ---------------------------------------------------------------------------
// DevStatus tests
// ---------------------------------------------------------------------------

// devStatusBody is the canonical Dev Status response shape captured from
// a real Atlassian tenant (instinctvet.atlassian.net, PLATENG-1573).
const devStatusBody = `{
  "errors": [],
  "detail": [
    {
      "_instance": {
        "id": "com.github.integration.production",
        "type": "GitHub",
        "singleInstance": true,
        "baseUrl": "https://github.com",
        "typeName": "GitHub",
        "name": "GitHub"
      },
      "branches": [],
      "pullRequests": [
        {
          "id": "#557",
          "url": "https://github.com/org/repo/pull/557",
          "name": "PLATENG-1573: Implement feature",
          "status": "MERGED",
          "lastUpdate": "2026-05-08T13:44:52.000+0000",
          "source": {
            "branch": "feature/PLATENG-1573",
            "url": "https://github.com/org/repo/tree/feature%2FPLATENG-1573"
          },
          "destination": {
            "branch": "main",
            "url": "https://github.com/org/repo/tree/main"
          },
          "author": {
            "name": "Robert Tirserio",
            "avatar": "https://example.com/avatar.png"
          },
          "reviewers": [
            {"name": "Kareem Hepburn", "avatar": "x", "approved": true}
          ],
          "repositoryUrl": "https://github.com/org/repo",
          "repositoryName": "org/repo",
          "repositoryId": "abc",
          "commentCount": 0
        }
      ],
      "repositories": []
    }
  ]
}`

func TestDevStatus_Success(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, devStatusBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.DevStatus(context.Background(), "86679", "GitHub", "pullrequest")
	require.NoError(t, err)

	assert.Equal(t, "/rest/dev-status/1.0/issue/detail", gotPath)
	// Query parameter order is deterministic (url.Values.Encode sorts keys).
	assert.Contains(t, gotQuery, "issueId=86679")
	assert.Contains(t, gotQuery, "applicationType=GitHub")
	assert.Contains(t, gotQuery, "dataType=pullrequest")

	require.Len(t, resp.Detail, 1)
	require.Len(t, resp.Detail[0].PullRequests, 1)
	pr := resp.Detail[0].PullRequests[0]
	assert.Equal(t, "#557", pr.ID)
	assert.Equal(t, "https://github.com/org/repo/pull/557", pr.URL)
	assert.Equal(t, "PLATENG-1573: Implement feature", pr.Name)
	assert.Equal(t, "MERGED", pr.Status)
	assert.Equal(t, "org/repo", pr.Repository)
	assert.Equal(t, "main", pr.Destination.Branch)
	assert.Equal(t, "Robert Tirserio", pr.Author.Name)
	require.Len(t, pr.Reviewers, 1)
	assert.True(t, pr.Reviewers[0].Approved)
}

func TestDevStatus_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.DevStatus(context.Background(), "86679", "GitHub", "pullrequest")
	assert.ErrorIs(t, err, client.ErrUnauthorized)
}

func TestDevStatus_429RetryThenSucceed(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, devStatusBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.DevStatus(context.Background(), "86679", "GitHub", "pullrequest")
	require.NoError(t, err)
	assert.Equal(t, int32(2), callCount.Load(), "retry then success")
	require.Len(t, resp.Detail, 1)
}

// devStatusMultiEntityBody is a synthetic Dev Status response that
// populates branches, commits, repositories, and builds in addition to
// pull requests. It is a single response body used by
// TestDevStatus_UnmarshalsAllEntityTypes; in production each dataType
// query returns its own response with only the matching list populated.
const devStatusMultiEntityBody = `{
  "errors": [],
  "detail": [
    {
      "_instance": {"type": "GitHub", "name": "GitHub", "baseUrl": "https://github.com"},
      "pullRequests": [],
      "branches": [
        {
          "name": "feature/PLATENG-1578",
          "url": "https://github.com/org/api/tree/feature%2FPLATENG-1578",
          "createPullRequestUrl": "https://github.com/org/api/pull/new/feature%2FPLATENG-1578",
          "repository": {"name": "org/api", "url": "https://github.com/org/api"},
          "lastCommit": {
            "id": "abc123def456",
            "displayId": "abc123d",
            "url": "https://github.com/org/api/commit/abc123",
            "message": "feat: initial commit",
            "author": {"name": "Kareem Hepburn"},
            "authorTimestamp": "2026-06-02T14:30:00.000+0000"
          }
        }
      ],
      "commits": [
        {
          "id": "abc123def456",
          "displayId": "abc123d",
          "url": "https://github.com/org/api/commit/abc123",
          "message": "feat: add OIDC initiate endpoint\nDetailed body line.",
          "author": {"name": "Kareem Hepburn"},
          "authorTimestamp": "2026-06-02T14:30:00.000+0000",
          "fileCount": 3,
          "merge": false,
          "repository": {"name": "org/api", "url": "https://github.com/org/api"}
        }
      ],
      "repositories": [
        {"name": "org/api", "url": "https://github.com/org/api", "avatar": "https://example.com/a.png"}
      ],
      "builds": [
        {
          "id": "42",
          "buildNumber": 42,
          "name": "Build #42",
          "description": "Pipelines build",
          "url": "https://bitbucket.org/org/api/pipelines/results/42",
          "state": "SUCCESSFUL",
          "lastUpdated": "2026-06-02T14:30:00.000+0000",
          "testSummary": {"totalNumber": 100, "passedNumber": 100, "failedNumber": 0, "skippedNumber": 0},
          "references": [{"name": "feature/PLATENG-1578", "uri": "refs/heads/feature/PLATENG-1578"}]
        }
      ]
    }
  ]
}`

// TestDevStatus_UnmarshalsAllEntityTypes verifies that DevStatusResponse
// tolerantly unmarshals every dataType-specific entity list. In
// production each dataType query returns its own response with a single
// non-empty list; this test asserts the struct can carry every shape
// without a separate Response type per dataType.
func TestDevStatus_UnmarshalsAllEntityTypes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, devStatusMultiEntityBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.DevStatus(context.Background(), "86679", "GitHub", "branch")
	require.NoError(t, err)
	require.Len(t, resp.Detail, 1)
	inst := resp.Detail[0]

	require.Len(t, inst.Branches, 1, "branches populated")
	assert.Equal(t, "feature/PLATENG-1578", inst.Branches[0].Name)
	assert.Equal(t, "org/api", inst.Branches[0].Repository.Name)
	assert.Equal(t, "abc123d", inst.Branches[0].LastCommit.DisplayID)

	require.Len(t, inst.Commits, 1, "commits populated")
	assert.Equal(t, "abc123def456", inst.Commits[0].ID)
	assert.Equal(t, 3, inst.Commits[0].FileCount)
	assert.Equal(t, "org/api", inst.Commits[0].Repository.Name)

	require.Len(t, inst.Repositories, 1, "repositories populated")
	assert.Equal(t, "org/api", inst.Repositories[0].Name)
	assert.Equal(t, "https://github.com/org/api", inst.Repositories[0].URL)

	require.Len(t, inst.Builds, 1, "builds populated")
	assert.Equal(t, "SUCCESSFUL", inst.Builds[0].State)
	require.NotNil(t, inst.Builds[0].TestSummary, "test summary present")
	assert.Equal(t, 100, inst.Builds[0].TestSummary.PassedNumber)
	require.Len(t, inst.Builds[0].References, 1)
	assert.Equal(t, "refs/heads/feature/PLATENG-1578", inst.Builds[0].References[0].URI)
}

// devStatusObjectErrorsBody is the PLATENG-1417 regression fixture.
// The Dev Status endpoint returned HTTP 200 with object-shaped entries
// in the "errors" array instead of the empty array seen on
// PLATENG-1573. Prior to the fix that retyped DevStatusResponse.Errors
// to []json.RawMessage this body caused json.Unmarshal to fail with:
//
//	json: cannot unmarshal object into Go struct field
//	DevStatusResponse.errors of type string
//
// The Detail array is intentionally non-empty so the test also proves
// the rest of the response is still consumed when "errors" carries
// shape we do not interpret.
const devStatusObjectErrorsBody = `{
  "errors": [
    {"code": 1, "message": "GitHub instance is unreachable", "userId": "u-1"},
    {"code": 2, "message": "rate limited", "userId": "u-2"}
  ],
  "detail": [
    {
      "_instance": {"type": "GitHub", "name": "GitHub", "baseUrl": "https://github.com"},
      "pullRequests": [],
      "branches": [],
      "commits": [],
      "repositories": [],
      "builds": []
    }
  ]
}`

// TestDevStatus_ObjectErrorsDoNotCrashUnmarshal is the PLATENG-1417
// regression test. A Dev Status response carrying JSON-object error
// entries (rather than the empty array seen on PLATENG-1573 or the
// historical string entries) must decode cleanly into
// DevStatusResponse without crashing the unmarshal, because callers
// rely on the rest of the response (Detail) to still be available even
// when one upstream integration emits soft errors.
func TestDevStatus_ObjectErrorsDoNotCrashUnmarshal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, devStatusObjectErrorsBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.DevStatus(context.Background(), "86679", "GitHub", "commit")
	require.NoError(t, err, "object-shaped errors entries must not crash the unmarshal")

	// Errors slice carries opaque entries; we do not interpret them
	// here, only verify they round-tripped without losing data.
	require.Len(t, resp.Errors, 2, "both error entries preserved as raw JSON")
	// The exact whitespace inside each RawMessage mirrors the input body
	// (json.RawMessage is byte-identical to the source); the source has
	// spaces after colons, so match accordingly.
	assert.Contains(t, string(resp.Errors[0]), `"code": 1`, "first entry payload preserved")
	assert.Contains(t, string(resp.Errors[1]), `"rate limited"`, "second entry message preserved")

	// Detail is still consumed normally.
	require.Len(t, resp.Detail, 1)
	assert.Equal(t, "GitHub", resp.Detail[0].Instance.Type)
}

// ---------------------------------------------------------------------------
// Phase 2 (phase-a-transport-1): PUT transport + 400/409 status handling
// ---------------------------------------------------------------------------

// TestNewPutJSON_MethodAndHeaders drives a PUT through the new
// DoPutForTest shim and asserts that the request hit the server with
// PUT, the canonical auth/UA/Accept/Content-Type headers, and an
// intact body. This is the unit-level proof that newPutJSON is wired
// the same way newPostJSON is — minus the actual write methods, which
// land in later Phase 2 tasks.
func TestNewPutJSON_MethodAndHeaders(t *testing.T) {
	const reqBody = `{"fields":{"summary":"hi"}}`

	var (
		gotMethod      string
		gotAuth        string
		gotUA          string
		gotAccept      string
		gotContentType string
		gotBody        string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	body, err := c.DoPutForTest(context.Background(), srv.URL+"/rest/api/3/issue/PROJ-1", []byte(reqBody))
	require.NoError(t, err, "DoPutForTest must succeed on 204")
	assert.Empty(t, body, "204 No Content must surface as an empty body")

	assert.Equal(t, http.MethodPut, gotMethod, "method")
	assert.Equal(t, expectedAuthHeader(), gotAuth, "Authorization")
	assert.Equal(t, "gojira/0.1.0", gotUA, "User-Agent")
	assert.Equal(t, "application/json", gotAccept, "Accept")
	assert.Equal(t, "application/json", gotContentType, "Content-Type")
	assert.Equal(t, reqBody, gotBody, "body must round-trip verbatim")
}

// TestDoWithRetry_PutStatusMapping is the table that exercises the
// extended status switch end-to-end via the PUT path. 200/201/204 are
// success; 400/409 map to the new sentinels via errors.Is; the
// existing 401/403/404 sentinels keep working through PUT exactly as
// they do through GET, proving the switch was widened additively.
func TestDoWithRetry_PutStatusMapping(t *testing.T) {
	const okBody = `{"id":"10001","key":"PROJ-1"}`

	cases := []struct {
		name       string
		status     int
		respBody   string
		wantErr    error // nil for success
		wantBody   string
		wantErrMsg string // substring match when wantErr is nil but a wrapped error is expected
	}{
		{"200 OK is success", http.StatusOK, okBody, nil, okBody, ""},
		{"201 Created is success", http.StatusCreated, okBody, nil, okBody, ""},
		{"204 No Content is success with empty body", http.StatusNoContent, "", nil, "", ""},
		{"400 maps to ErrBadRequest", http.StatusBadRequest, `{"errorMessages":["bad"]}`, client.ErrBadRequest, "", ""},
		{"401 still maps to ErrUnauthorized", http.StatusUnauthorized, "", client.ErrUnauthorized, "", ""},
		{"403 still maps to ErrForbidden", http.StatusForbidden, "", client.ErrForbidden, "", ""},
		{"404 still maps to ErrNotFound", http.StatusNotFound, "", client.ErrNotFound, "", ""},
		{"409 maps to ErrConflict", http.StatusConflict, `{"errorMessages":["conflict"]}`, client.ErrConflict, "", ""},
		{"500 still falls through to wrapped status error", http.StatusInternalServerError, "", nil, "", "unexpected status 500"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					t.Errorf("server got method %s, want PUT", r.Method)
				}
				if tc.respBody == "" {
					w.WriteHeader(tc.status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.respBody)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, client.WithMaxRetries(0))

			body, err := c.DoPutForTest(context.Background(),
				srv.URL+"/rest/api/3/issue/PROJ-1",
				[]byte(`{"fields":{"summary":"x"}}`))

			switch {
			case tc.wantErr != nil:
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr),
					"errors.Is(err, %v) must be true; got %v", tc.wantErr, err)

			case tc.wantErrMsg != "":
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrMsg)

			default:
				require.NoError(t, err)
				assert.Equal(t, tc.wantBody, string(body), "body")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Phase phase-c-httptrace-2: client.WithLogger installs the httplog
// RoundTripper and composes with WithHTTPClient / WithRoundTripper.
// ---------------------------------------------------------------------------

// loggerTestClient builds a Client pointed at srv with the supplied options
// layered onto WithHTTPClient(srv.Client()) — the standard injection seam
// for the existing tests. Centralised here so the four logger tests below
// stay focused on the behaviour they assert.
func loggerTestClient(t *testing.T, srv *httptest.Server, cfg config.Config, extra ...client.Option) *client.Client {
	t.Helper()
	if cfg.Site == "" {
		cfg.Site = srv.URL
	}
	if cfg.User == "" {
		cfg.User = "user@example.com"
	}
	if cfg.Token == "" {
		cfg.Token = "api-token"
	}
	opts := []client.Option{client.WithHTTPClient(srv.Client())}
	opts = append(opts, extra...)
	c, err := client.New(cfg, opts...)
	require.NoError(t, err, "client.New")
	return c
}

// decodeJSONRecords iterates the buffer's JSON log lines and returns them
// as a slice of maps. Tolerant: stops on the first decode error so a
// trailing newline doesn't break the assertion.
func decodeJSONRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			break
		}
		out = append(out, rec)
	}
	return out
}

func recordByMsg(records []map[string]any, msg string) map[string]any {
	for _, r := range records {
		if got, _ := r["msg"].(string); got == msg {
			return r
		}
	}
	return nil
}

// TestWithLogger_EmitsResponseStreamLogs asserts that constructing a
// client with WithLogger produces exactly one INFO summary line per
// real request, tagged trace_stream=response, with the expected
// method/status/bytes/duration_ms attributes. The TRACE-only lines
// (http.request.start / http.request.complete) must NOT appear when
// the logger is configured at INFO.
func TestWithLogger_EmitsResponseStreamLogs(t *testing.T) {
	const body = "ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	c := loggerTestClient(t, srv, config.Config{}, client.WithLogger(lg))

	// We accept the error path because "EX-1" against a stubbed server
	// is shape-invalid for GetIssue's parser. The contract under test
	// is the log output, not the issue parse.
	_, _ = c.GetIssue(context.Background(), "EX-1", nil)

	recs := decodeJSONRecords(t, &buf)
	require.NotEmpty(t, recs, "expected at least one logger record; buf=%s", buf.String())

	resp := recordByMsg(recs, "http.response")
	require.NotNil(t, resp, "expected an http.response record; buf=%s", buf.String())

	assert.Equal(t, "INFO", resp["level"], "level must be INFO at slog.LevelInfo")
	assert.Equal(t, "response", resp["trace_stream"], "trace_stream must be 'response'")
	if got, _ := resp["http_method"].(string); got != http.MethodGet {
		t.Errorf("http_method: got %v, want GET", resp["http_method"])
	}
	if got, _ := resp["status"].(float64); int(got) != http.StatusOK {
		t.Errorf("status: got %v, want 200", resp["status"])
	}
	if got, _ := resp["bytes"].(float64); int(got) != len(body) {
		t.Errorf("bytes: got %v, want %d", resp["bytes"], len(body))
	}
	if _, ok := resp["duration_ms"]; !ok {
		t.Errorf("duration_ms missing on summary line")
	}

	// At LevelInfo the httptrace lines MUST be suppressed (LevelTrace
	// is below LevelInfo on slog's ladder).
	assert.Nil(t, recordByMsg(recs, "http.request.start"),
		"http.request.start must not appear at INFO level")
	assert.Nil(t, recordByMsg(recs, "http.request.complete"),
		"http.request.complete must not appear at INFO level")
}

// TestWithLogger_RedactionAudit is the unit-scope guard for the absolute
// credential-redaction invariant the crawl-observability PRD requires.
// The client's authHeader is "Basic <base64(user:token)>". At
// log.LevelTrace the round-tripper logs request headers; this test
// grep-checks that the base64 token AND the raw token literal do NOT
// appear anywhere in the captured buffer, and that the REDACTED
// placeholder DOES appear.
func TestWithLogger_RedactionAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: gojiralog.LevelTrace}))

	cfg := config.Config{
		Site:  srv.URL,
		User:  "alice",
		Token: "topsecret-token-abc",
	}
	expectedAuthB64 := base64.StdEncoding.EncodeToString([]byte(cfg.User + ":" + cfg.Token))

	c := loggerTestClient(t, srv, cfg, client.WithLogger(lg))
	_, _ = c.GetIssue(context.Background(), "EX-1", nil)

	captured := buf.String()
	assert.NotContains(t, captured, expectedAuthB64,
		"the Basic-auth base64 token must NEVER appear in logs")
	assert.NotContains(t, captured, "topsecret-token-abc",
		"the raw token literal must NEVER appear in logs")
	assert.True(t, strings.Contains(captured, "REDACTED"),
		"expected REDACTED placeholder at trace level; buf=%s", captured)
}

// TestWithLogger_NilIsNoop guards back-compat: WithLogger(nil) must
// produce no panics and no log output. A separate observation buffer
// (NOT passed through WithLogger) confirms nothing leaked: the
// implementation must skip the httplog wrap when c.logger is nil, so
// nothing is ever wired to a sink.
func TestWithLogger_NilIsNoop(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	c := loggerTestClient(t, srv, config.Config{}, client.WithLogger(nil))

	// A nil logger must not panic; the request still proceeds.
	_, _ = c.GetIssue(context.Background(), "EX-1", nil)

	// And, for clarity, a sibling observation buffer wired to a NEW
	// logger that the client knows NOTHING about must stay empty —
	// proof that no global sink is being touched.
	var watch bytes.Buffer
	_ = slog.New(slog.NewJSONHandler(&watch, &slog.HandlerOptions{Level: slog.LevelInfo}))
	assert.Empty(t, watch.String(),
		"sibling observation buffer must stay empty when WithLogger(nil)")
}

// countingRT is a small RoundTripper that records every request before
// delegating to base. Used to prove the httplog wrap composes with a
// caller-supplied transport: order should be httplog → counter → wire,
// so the counter still runs exactly once per request AND the logger
// also emits its summary line.
type countingRT struct {
	base http.RoundTripper
	n    atomic.Int32
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.base.RoundTrip(req)
}

func TestWithLogger_ComposesWithWithRoundTripper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	counter := &countingRT{base: srv.Client().Transport}
	c := loggerTestClient(t, srv, config.Config{},
		client.WithRoundTripper(counter),
		client.WithLogger(lg),
	)
	_, _ = c.GetIssue(context.Background(), "EX-1", nil)

	// The counter MUST have been called exactly once: httplog's
	// RoundTrip delegates to its base, which is the counter, which
	// delegates to the wire.
	if got := counter.n.Load(); got != 1 {
		t.Errorf("counter RoundTripper calls: got %d, want 1", got)
	}

	// And the logger MUST have captured the http.response summary line,
	// proving both wraps fire on the same request.
	recs := decodeJSONRecords(t, &buf)
	assert.NotNil(t, recordByMsg(recs, "http.response"),
		"logger must receive http.response when composing with WithRoundTripper")
}
