package hierarchy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/hierarchy"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// HierarchyCapable
// ---------------------------------------------------------------------------

func TestHierarchyCapable(t *testing.T) {
	cases := []struct {
		issueType string
		want      bool
	}{
		{"Epic", true},
		{"Story", true},
		{"Task", true},
		{"Bug", true},
		{"Improvement", true},
		{"New Feature", true},
		{"Sub-task", false},
		{"sub-task", false},
		{"SUB-TASK", false},
		{"Subtask", false},
		{"subtask", false},
		{"", true}, // unknown → default true
		{"Custom Issue Type", true},
	}
	for _, tc := range cases {
		t.Run(tc.issueType, func(t *testing.T) {
			assert.Equal(t, tc.want, hierarchy.HierarchyCapable(tc.issueType))
		})
	}
}

// ---------------------------------------------------------------------------
// Children — server helpers
// ---------------------------------------------------------------------------

type searchHandler struct {
	t         *testing.T
	parentMap map[string][]string // KEY → child keys returned for `parent = "KEY"`
	epicMap   map[string][]string // KEY → child keys returned for `"Epic Link" = "KEY"`
	fields    []map[string]any
	parentErr func() (int, string) // optional override: status, body
	epicErr   func() (int, string)
	fieldsErr func() (int, string)
	searchCnt atomic.Int64
	fieldsCnt atomic.Int64
}

func (h *searchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/field":
		h.fieldsCnt.Add(1)
		if h.fieldsErr != nil {
			code, body := h.fieldsErr()
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(h.fields)
	case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/search/jql":
		h.searchCnt.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			JQL string `json:"jql"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		isEpic := strings.Contains(req.JQL, `"Epic Link"`)
		isParent := strings.HasPrefix(req.JQL, `parent =`) && !isEpic
		if isEpic && h.epicErr != nil {
			code, body := h.epicErr()
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
			return
		}
		if isParent && h.parentErr != nil {
			code, body := h.parentErr()
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
			return
		}
		key := extractQuotedKey(req.JQL)
		var keys []string
		switch {
		case isEpic:
			keys = h.epicMap[key]
		case isParent:
			keys = h.parentMap[key]
		}
		issues := make([]map[string]string, 0, len(keys))
		for _, k := range keys {
			issues = append(issues, map[string]string{"key": k})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues})
	default:
		http.NotFound(w, r)
	}
}

// extractQuotedKey pulls the first quoted segment out of a JQL like
// `parent = "EXAMPLE-1"` or `"Epic Link" = "EXAMPLE-1"`. It returns the
// last quoted segment, which is always the operand key in our queries.
func extractQuotedKey(jql string) string {
	parts := strings.Split(jql, `"`)
	// "Epic Link" = "KEY"  → ["", "Epic Link", " = ", "KEY", ""]
	// parent = "KEY"       → ["parent = ", "KEY", ""]
	// Find the last non-empty quoted segment that isn't "Epic Link".
	for i := len(parts) - 2; i >= 1; i -= 2 {
		seg := parts[i]
		if seg != "" && seg != "Epic Link" {
			return seg
		}
	}
	return ""
}

func newTestClient(t *testing.T, srv *httptest.Server) *client.Client {
	t.Helper()
	cfg := config.Config{
		Site:  srv.URL,
		User:  "u@example.com",
		Token: "tok",
	}
	c, err := client.New(cfg,
		client.WithHTTPClient(srv.Client()),
		client.WithRateLimitBackoff(0, 0),
	)
	require.NoError(t, err, "client.New")
	return c
}

// ---------------------------------------------------------------------------
// Children — modern parent only
// ---------------------------------------------------------------------------

func TestChildren_ParentSearchOnly(t *testing.T) {
	h := &searchHandler{
		t:         t,
		parentMap: map[string][]string{"EXAMPLE-1": {"EXAMPLE-3", "EXAMPLE-2"}},
		// No Epic Link field on tenant → epicMap unused.
		fields: []map[string]any{
			{"id": "summary", "key": "summary", "name": "Summary", "custom": false},
		},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newTestClient(t, srv)
	d := hierarchy.New(c, config.Config{ChildSearchLimit: 100})

	got, err := d.Children(context.Background(), parse.Issue{Key: "EXAMPLE-1", IssueType: "Epic"})
	require.NoError(t, err)
	// Sorted alphabetically.
	assert.Equal(t, []string{"EXAMPLE-2", "EXAMPLE-3"}, got)
	// One search (parent) + one fields call (no Epic Link → only one search).
	assert.Equal(t, int64(1), h.searchCnt.Load(), "parent search called exactly once")
	assert.Equal(t, int64(1), h.fieldsCnt.Load(), "fields fetched exactly once for auto-detect")
}

// ---------------------------------------------------------------------------
// Children — Epic Link auto-detection + merge + dedup
// ---------------------------------------------------------------------------

func TestChildren_EpicLinkAutoDetectAndMerge(t *testing.T) {
	h := &searchHandler{
		t: t,
		parentMap: map[string][]string{
			"EXAMPLE-1": {"EXAMPLE-2"},
		},
		epicMap: map[string][]string{
			"EXAMPLE-1": {"EXAMPLE-2", "EXAMPLE-99"}, // overlap with parent
		},
		fields: []map[string]any{
			{"id": "customfield_10014", "key": "customfield_10014", "name": "Epic Link", "custom": true},
		},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newTestClient(t, srv)
	d := hierarchy.New(c, config.Config{ChildSearchLimit: 100})

	got, err := d.Children(context.Background(), parse.Issue{Key: "EXAMPLE-1", IssueType: "Epic"})
	require.NoError(t, err)
	assert.Equal(t, []string{"EXAMPLE-2", "EXAMPLE-99"}, got, "dedup + sort")
	assert.Equal(t, int64(2), h.searchCnt.Load(), "parent + epic searches")
	assert.Equal(t, int64(1), h.fieldsCnt.Load(), "fields fetched exactly once")

	// Second call should NOT re-fetch fields metadata (auto-detect cached).
	_, err = d.Children(context.Background(), parse.Issue{Key: "EXAMPLE-1", IssueType: "Epic"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), h.fieldsCnt.Load(), "fields fetched still exactly once across two calls")
}

// ---------------------------------------------------------------------------
// Children — configured override skips ListFields entirely
// ---------------------------------------------------------------------------

func TestChildren_ConfiguredEpicLinkOverride(t *testing.T) {
	h := &searchHandler{
		t:         t,
		parentMap: map[string][]string{"EX-1": {}},
		epicMap:   map[string][]string{"EX-1": {"EX-9"}},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newTestClient(t, srv)
	d := hierarchy.New(c, config.Config{
		ChildSearchLimit: 100,
		EpicLinkField:    "customfield_77777",
	})

	got, err := d.Children(context.Background(), parse.Issue{Key: "EX-1", IssueType: "Epic"})
	require.NoError(t, err)
	assert.Equal(t, []string{"EX-9"}, got)
	assert.Equal(t, int64(0), h.fieldsCnt.Load(), "no fields fetch when override configured")
}

// ---------------------------------------------------------------------------
// Children — partial failure: epic query errors, parent succeeds
// ---------------------------------------------------------------------------

func TestChildren_PartialFailureEpicErrors(t *testing.T) {
	h := &searchHandler{
		t:         t,
		parentMap: map[string][]string{"EX-1": {"EX-2"}},
		fields: []map[string]any{
			{"id": "customfield_10014", "name": "Epic Link", "custom": true},
		},
		epicErr: func() (int, string) { return http.StatusInternalServerError, "" },
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newTestClient(t, srv)
	d := hierarchy.New(c, config.Config{ChildSearchLimit: 100})

	got, err := d.Children(context.Background(), parse.Issue{Key: "EX-1"})
	require.Error(t, err, "non-fatal error expected")
	assert.Equal(t, []string{"EX-2"}, got, "parent results still returned")
}

// ---------------------------------------------------------------------------
// Children — single query (no Epic Link) total failure is fatal
// ---------------------------------------------------------------------------

func TestChildren_SingleQueryFailureFatal(t *testing.T) {
	h := &searchHandler{
		t:         t,
		fields:    []map[string]any{{"id": "summary", "name": "Summary"}}, // no Epic Link
		parentErr: func() (int, string) { return http.StatusInternalServerError, "" },
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newTestClient(t, srv)
	d := hierarchy.New(c, config.Config{ChildSearchLimit: 100})

	got, err := d.Children(context.Background(), parse.Issue{Key: "EX-1"})
	require.Error(t, err)
	assert.Empty(t, got)
}

// ---------------------------------------------------------------------------
// Children — ChildSearchLimit caps merged result
// ---------------------------------------------------------------------------

func TestChildren_LimitCaps(t *testing.T) {
	h := &searchHandler{
		t: t,
		parentMap: map[string][]string{
			"E-1": {"A", "B", "C", "D", "E"},
		},
		fields: []map[string]any{{"id": "summary", "name": "Summary"}},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newTestClient(t, srv)
	d := hierarchy.New(c, config.Config{ChildSearchLimit: 3})

	got, err := d.Children(context.Background(), parse.Issue{Key: "E-1"})
	require.NoError(t, err)
	assert.Len(t, got, 3, "capped at limit")
}

// ---------------------------------------------------------------------------
// Children — auto-detect error is non-fatal; parent query still runs
// ---------------------------------------------------------------------------

func TestChildren_AutoDetectFieldsError(t *testing.T) {
	h := &searchHandler{
		t:         t,
		parentMap: map[string][]string{"E-1": {"E-2"}},
		fieldsErr: func() (int, string) { return http.StatusInternalServerError, "" },
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newTestClient(t, srv)
	d := hierarchy.New(c, config.Config{ChildSearchLimit: 100})

	got, err := d.Children(context.Background(), parse.Issue{Key: "E-1"})
	require.NoError(t, err, "parent query succeeds; no Epic Link query attempted")
	assert.Equal(t, []string{"E-2"}, got)
}
