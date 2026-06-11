// White-box tests for the GetGraph handler. They live in
// `package grpc` so they can install fakes via
// [WithCrawlGraphFunc] without exercising any network or live Jira.
package grpc

import (
	"context"
	"errors"
	"sort"
	"testing"

	"google.golang.org/grpc/codes"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
)

// fixtureGraphModel returns a small but representative graph: two
// fetched issues linked to each other, one referenced github_pr node,
// and one external node. Used by the happy-path mapping test.
func fixtureGraphModel() gojira.GraphModel {
	return gojira.GraphModel{
		Nodes: []gojira.GraphNode{
			{ID: "EXAMPLE-1", Kind: "issue", Label: "EXAMPLE-1: Hello",
				Status: "Open", Type: "Task", Assignee: "Alice",
				URL: "https://example.atlassian.net/browse/EXAMPLE-1", Fetched: true},
			{ID: "EXAMPLE-2", Kind: "issue", Label: "EXAMPLE-2: World",
				Status: "Done", Type: "Story", Assignee: "Bob",
				URL: "https://example.atlassian.net/browse/EXAMPLE-2", Fetched: true},
			{ID: "acme/widget#42", Kind: "github_pr",
				Label: "acme/widget#42",
				URL:   "https://github.com/acme/widget/pull/42"},
			{ID: "https://docs.example.com/foo", Kind: "external",
				Label: "external", URL: "https://docs.example.com/foo"},
		},
		Edges: []gojira.GraphEdge{
			{From: "EXAMPLE-1", To: "EXAMPLE-2", Kind: "link", Label: "blocks"},
			{From: "EXAMPLE-1", To: "acme/widget#42", Kind: "pull_request"},
			{From: "EXAMPLE-2", To: "https://docs.example.com/foo", Kind: "external"},
		},
	}
}

// ---------------------------------------------------------------------------
// happy path: handler maps GraphModel → GetGraphResponse correctly
// ---------------------------------------------------------------------------

func TestGetGraph_MapsGraphModelToResponse(t *testing.T) {
	t.Parallel()

	var gotKeys []string
	srv := NewServer(gojira.Config{Site: "https://example.atlassian.net"},
		WithCrawlGraphFunc(func(_ context.Context, _ gojira.Config, keys []string, _ gojira.Sink) (gojira.Summary, gojira.GraphModel, error) {
			gotKeys = append(gotKeys, keys...)
			return gojira.Summary{Fetched: 2}, fixtureGraphModel(), nil
		}),
	)

	resp, err := srv.GetGraph(context.Background(), &gojirav1.GetGraphRequest{
		StartKeys: []string{"EXAMPLE-1"},
	})
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if got, want := gotKeys, []string{"EXAMPLE-1"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("seed keys not forwarded: got %v, want %v", got, want)
	}

	if got, want := len(resp.GetNodes()), 4; got != want {
		t.Fatalf("node count: got %d, want %d", got, want)
	}
	if got, want := len(resp.GetEdges()), 3; got != want {
		t.Fatalf("edge count: got %d, want %d", got, want)
	}

	// Per-node field mapping (sorted by ID for stable indexing).
	nodes := resp.GetNodes()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].GetId() < nodes[j].GetId() })
	wantNodes := []struct {
		id, kind, label, status, typ, assignee, url string
		fetched                                     bool
	}{
		{"EXAMPLE-1", "issue", "EXAMPLE-1: Hello", "Open", "Task", "Alice",
			"https://example.atlassian.net/browse/EXAMPLE-1", true},
		{"EXAMPLE-2", "issue", "EXAMPLE-2: World", "Done", "Story", "Bob",
			"https://example.atlassian.net/browse/EXAMPLE-2", true},
		{"acme/widget#42", "github_pr", "acme/widget#42", "", "", "",
			"https://github.com/acme/widget/pull/42", false},
		{"https://docs.example.com/foo", "external", "external", "", "", "",
			"https://docs.example.com/foo", false},
	}
	for i, w := range wantNodes {
		n := nodes[i]
		if n.GetId() != w.id || n.GetKind() != w.kind || n.GetLabel() != w.label {
			t.Errorf("node[%d] core mismatch: got %+v, want id=%q kind=%q label=%q",
				i, n, w.id, w.kind, w.label)
		}
		if n.GetStatus() != w.status || n.GetType() != w.typ || n.GetAssignee() != w.assignee {
			t.Errorf("node[%d] issue-only mismatch: got status=%q type=%q assignee=%q want %q/%q/%q",
				i, n.GetStatus(), n.GetType(), n.GetAssignee(), w.status, w.typ, w.assignee)
		}
		if n.GetUrl() != w.url || n.GetFetched() != w.fetched {
			t.Errorf("node[%d] url/fetched mismatch: got url=%q fetched=%v, want %q/%v",
				i, n.GetUrl(), n.GetFetched(), w.url, w.fetched)
		}
	}

	// Per-edge field mapping.
	edges := resp.GetEdges()
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].GetFrom() != edges[j].GetFrom() {
			return edges[i].GetFrom() < edges[j].GetFrom()
		}
		return edges[i].GetTo() < edges[j].GetTo()
	})
	wantEdges := []struct{ from, to, kind, label string }{
		{"EXAMPLE-1", "EXAMPLE-2", "link", "blocks"},
		{"EXAMPLE-1", "acme/widget#42", "pull_request", ""},
		{"EXAMPLE-2", "https://docs.example.com/foo", "external", ""},
	}
	for i, w := range wantEdges {
		e := edges[i]
		if e.GetFrom() != w.from || e.GetTo() != w.to ||
			e.GetKind() != w.kind || e.GetLabel() != w.label {
			t.Errorf("edge[%d] mismatch: got %+v, want %+v", i, e, w)
		}
	}
}

// ---------------------------------------------------------------------------
// empty start_keys → InvalidArgument
// ---------------------------------------------------------------------------

func TestGetGraph_NoStartKeys_InvalidArgument(t *testing.T) {
	t.Parallel()

	srv := NewServer(gojira.Config{Site: "https://example.atlassian.net"})
	_, err := srv.GetGraph(context.Background(), &gojirav1.GetGraphRequest{})
	if err == nil {
		t.Fatal("expected error for empty start_keys")
	}
	if got, want := codeOf(err), codes.InvalidArgument; got != want {
		t.Fatalf("code: got %v, want %v (err=%v)", got, want, err)
	}
}

// ---------------------------------------------------------------------------
// underlying error → mapped via toStatusError
// ---------------------------------------------------------------------------

func TestGetGraph_PropagatesUnauthorized(t *testing.T) {
	t.Parallel()

	srv := NewServer(gojira.Config{Site: "https://example.atlassian.net"},
		WithCrawlGraphFunc(func(_ context.Context, _ gojira.Config, _ []string, _ gojira.Sink) (gojira.Summary, gojira.GraphModel, error) {
			return gojira.Summary{}, gojira.GraphModel{}, gojira.ErrUnauthorized
		}),
	)
	_, err := srv.GetGraph(context.Background(), &gojirav1.GetGraphRequest{
		StartKeys: []string{"EXAMPLE-1"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := codeOf(err), codes.Unauthenticated; got != want {
		t.Fatalf("code: got %v, want %v (err=%v)", got, want, err)
	}
	if !errors.Is(err, err) { // sanity touch — err is non-nil status err
		t.Fatal("err should be non-nil")
	}
}

// ---------------------------------------------------------------------------
// per-request knobs override server cfg via a per-RPC copy (no shared mutation)
// ---------------------------------------------------------------------------

func TestGetGraph_PerRequestKnobs_OverrideViaCfgCopy(t *testing.T) {
	t.Parallel()

	baseCfg := gojira.Config{
		Site:             "https://example.atlassian.net",
		DepthLimit:       0,
		IssueCap:         500,
		TimeCapSeconds:   0,
		Concurrency:      3,
		IncludeChildren:  true,
		IncludeDevStatus: true,
	}
	var seenCfg gojira.Config
	srv := NewServer(baseCfg,
		WithCrawlGraphFunc(func(_ context.Context, cfg gojira.Config, _ []string, _ gojira.Sink) (gojira.Summary, gojira.GraphModel, error) {
			seenCfg = cfg
			return gojira.Summary{}, gojira.GraphModel{}, nil
		}),
	)

	req := &gojirav1.GetGraphRequest{
		StartKeys:        []string{"EXAMPLE-1"},
		DepthLimit:       2,
		IssueCap:         10,
		TimeCapSeconds:   30,
		Concurrency:      8,
		IncludeChildren:  false, // explicit false overrides true default? see note below.
		IncludeDevStatus: false,
	}
	_, err := srv.GetGraph(context.Background(), req)
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}

	// Non-zero knobs override; zero knobs leave server default in place.
	// (We use non-zero overrides for the integer fields. For the bool
	// fields a zero value is `false`; we do not attempt to distinguish
	// "unset" from "explicitly false" in proto3 — the handler simply
	// applies them as-is. This matches the simple Crawl handler
	// today and is documented in the proto comments.)
	if seenCfg.DepthLimit != 2 {
		t.Errorf("DepthLimit not forwarded: %d", seenCfg.DepthLimit)
	}
	if seenCfg.IssueCap != 10 {
		t.Errorf("IssueCap not forwarded: %d", seenCfg.IssueCap)
	}
	if seenCfg.TimeCapSeconds != 30 {
		t.Errorf("TimeCapSeconds not forwarded: %d", seenCfg.TimeCapSeconds)
	}
	if seenCfg.Concurrency != 8 {
		t.Errorf("Concurrency not forwarded: %d", seenCfg.Concurrency)
	}

	// The server's original cfg must be UNCHANGED — concurrent RPCs
	// must not race on s.cfg. We verify by inspecting srv.cfg, which
	// is white-box accessible from this package.
	if srv.cfg.DepthLimit != 0 || srv.cfg.IssueCap != 500 ||
		srv.cfg.TimeCapSeconds != 0 || srv.cfg.Concurrency != 3 {
		t.Errorf("server cfg was mutated: %+v", srv.cfg)
	}
}
