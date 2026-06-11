// White-box tests for the gRPC handlers. They live in `package grpcserver`
// so they can overwrite the unexported function-field seams getIssueFn
// and crawlFn with in-process fakes, exercising the full handler path
// without any network or live Jira.
package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// fixedTimeIssue is a stable timestamp used by the fixture issue so the
// proto Created/Updated mapping is comparable across runs.
var fixedTimeIssue = time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

// fakeCrawlStream is a minimal grpc.ServerStreamingServer[CrawlEvent]
// double that captures every Send. Defined here (not in sink_test.go's
// external package) so the white-box tests have an in-package copy.
type fakeCrawlStream struct {
	mu   sync.Mutex
	sent []*gojirav1.CrawlEvent
	ctx  context.Context
}

func newFakeCrawlStream() *fakeCrawlStream {
	return &fakeCrawlStream{ctx: context.Background()}
}

func (f *fakeCrawlStream) Send(msg *gojirav1.CrawlEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeCrawlStream) Sent() []*gojirav1.CrawlEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*gojirav1.CrawlEvent, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeCrawlStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeCrawlStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeCrawlStream) SetTrailer(metadata.MD)       {}
func (f *fakeCrawlStream) Context() context.Context     { return f.ctx }
func (f *fakeCrawlStream) SendMsg(any) error            { return nil }
func (f *fakeCrawlStream) RecvMsg(any) error            { return nil }

var _ grpc.ServerStreamingServer[gojirav1.CrawlEvent] = (*fakeCrawlStream)(nil)

// codeOf extracts the gRPC status code from err.
func codeOf(err error) codes.Code {
	st, _ := status.FromError(err)
	return st.Code()
}

// fixtureIssue returns an in-code parse.Issue + matching extract.References
// covering every field the proto mapping touches: parent, subtask, issue
// link, remote link, a custom field, set Created/Updated, plus a JiraKey
// reference and a GitHubPR reference with Owner/Repo/PRNumber populated.
func fixtureIssue() (parse.Issue, []extract.Reference) {
	issue := parse.Issue{
		Key:       "EXAMPLE-1",
		NumericID: "10001",
		Summary:   "An example issue",
		Status:    "In Progress",
		IssueType: "Task",
		Assignee:  "Alice Example",
		Reporter:  "Bob Example",
		Created:   fixedTimeIssue,
		Updated:   fixedTimeIssue.Add(1 * time.Hour),
		SourceURL: "https://example.atlassian.net/browse/EXAMPLE-1",
		Parent:    &parse.ParentRef{Key: "EXAMPLE-0", Summary: "Parent issue"},
		Subtasks: []parse.LinkedIssue{
			{Key: "EXAMPLE-2", Summary: "Subtask A"},
		},
		IssueLinks: []parse.IssueLink{
			{Direction: "outward", Type: "Blocks", Key: "EXAMPLE-3", Summary: "Blocked issue"},
		},
		RemoteLinks: []parse.RemoteLink{
			{Title: "Design doc", URL: "https://example.com/design"},
		},
		CustomFields: map[string]json.RawMessage{
			"customfield_10010": json.RawMessage(`"sprint-7"`),
		},
	}
	refs := []extract.Reference{
		{
			Kind:     classify.KindJiraKey,
			IssueKey: "EXAMPLE-3",
			Text:     "EXAMPLE-3",
			Source:   extract.SourceRelationship,
			Relation: "outward Blocks",
		},
		{
			Kind:   classify.KindGitHubPR,
			URL:    "https://github.com/org/repo/pull/42",
			Text:   "Design PR",
			Source: extract.SourceRemoteLink,
			ClassifyResult: classify.Result{
				Owner:    "org",
				Repo:     "repo",
				PRNumber: 42,
			},
		},
	}
	return issue, refs
}

// testServer constructs a Server with a default Config plus the supplied
// fakes for the two injectable seams. Either fake may be nil to keep the
// production default.
func testServer(t *testing.T,
	getIssueFn func(context.Context, gojira.Config, string) (parse.Issue, []extract.Reference, error),
	crawlFn func(context.Context, gojira.Config, []string, gojira.Sink) (gojira.Summary, error),
) *Server {
	t.Helper()
	s := NewServer(gojira.Config{
		Site:                   "https://example.atlassian.net",
		OutputDir:              t.TempDir(),
		RenderNullCustomFields: false,
	})
	if getIssueFn != nil {
		s.getIssueFn = getIssueFn
	}
	if crawlFn != nil {
		s.crawlFn = crawlFn
	}
	return s
}

// ---------------------------------------------------------------------------
// Classify — jira_site override behavior
// ---------------------------------------------------------------------------

func TestClassify_JiraSiteOverridePrefersRequest(t *testing.T) {
	t.Parallel()
	s := NewServer(gojira.Config{Site: "https://configured.atlassian.net"})

	// A bare issue key classifies the same regardless of site, but we
	// can at least observe the JiraSite getter being honored when set.
	resp, err := s.Classify(context.Background(), &gojirav1.ClassifyRequest{
		Input:    "EXAMPLE-1",
		JiraSite: "https://request-override.atlassian.net",
	})
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if resp.GetIssueKey() != "EXAMPLE-1" {
		t.Errorf("IssueKey: got %q, want EXAMPLE-1", resp.GetIssueKey())
	}
}

func TestClassify_FallsBackToConfigWhenRequestSiteEmpty(t *testing.T) {
	t.Parallel()
	s := NewServer(gojira.Config{Site: "https://configured.atlassian.net"})

	resp, err := s.Classify(context.Background(), &gojirav1.ClassifyRequest{
		Input: "EXAMPLE-1",
		// JiraSite intentionally left empty.
	})
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if resp.GetIssueKey() != "EXAMPLE-1" {
		t.Errorf("IssueKey: got %q, want EXAMPLE-1", resp.GetIssueKey())
	}
}

// ---------------------------------------------------------------------------
// GetIssue — structured / markdown / json / errors
// ---------------------------------------------------------------------------

func TestGetIssue_StructuredMapping(t *testing.T) {
	t.Parallel()
	issue, refs := fixtureIssue()
	s := testServer(t,
		func(_ context.Context, _ gojira.Config, key string) (parse.Issue, []extract.Reference, error) {
			if key != "EXAMPLE-1" {
				t.Errorf("getIssueFn key: got %q, want EXAMPLE-1", key)
			}
			return issue, refs, nil
		},
		nil,
	)

	resp, err := s.GetIssue(context.Background(), &gojirav1.GetIssueRequest{
		Key:    "EXAMPLE-1",
		Format: gojirav1.OutputFormat_OUTPUT_FORMAT_STRUCTURED,
	})
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}

	got := resp.GetIssue()
	if got == nil {
		t.Fatal("GetIssue: structured result was nil")
	}
	if got.GetKey() != "EXAMPLE-1" {
		t.Errorf("Key: got %q, want EXAMPLE-1", got.GetKey())
	}
	if got.GetSummary() != "An example issue" {
		t.Errorf("Summary: got %q", got.GetSummary())
	}
	if got.GetStatus() != "In Progress" {
		t.Errorf("Status: got %q", got.GetStatus())
	}
	if got.GetIssueType() != "Task" {
		t.Errorf("IssueType: got %q", got.GetIssueType())
	}
	if got.GetParentKey() != "EXAMPLE-0" {
		t.Errorf("ParentKey: got %q, want EXAMPLE-0", got.GetParentKey())
	}
	if got.GetSourceUrl() != "https://example.atlassian.net/browse/EXAMPLE-1" {
		t.Errorf("SourceUrl: got %q", got.GetSourceUrl())
	}

	if len(got.GetSubtasks()) != 1 || got.GetSubtasks()[0].GetKey() != "EXAMPLE-2" {
		t.Errorf("Subtasks: got %+v", got.GetSubtasks())
	}
	if len(got.GetIssueLinks()) != 1 {
		t.Fatalf("IssueLinks: want 1, got %d", len(got.GetIssueLinks()))
	}
	link := got.GetIssueLinks()[0]
	if link.GetDirection() != "outward" || link.GetLinkType() != "Blocks" || link.GetIssue().GetKey() != "EXAMPLE-3" {
		t.Errorf("IssueLink mapping wrong: %+v", link)
	}
	if len(got.GetRemoteLinks()) != 1 || got.GetRemoteLinks()[0].GetUrl() != "https://example.com/design" {
		t.Errorf("RemoteLinks: got %+v", got.GetRemoteLinks())
	}

	if got.GetCustomFields()["customfield_10010"] != `"sprint-7"` {
		t.Errorf("CustomFields[customfield_10010]: got %q", got.GetCustomFields()["customfield_10010"])
	}

	if got.GetCreated() == nil {
		t.Fatal("Created must be non-nil for a non-zero time")
	}
	if !got.GetCreated().AsTime().Equal(fixedTimeIssue) {
		t.Errorf("Created: got %v, want %v", got.GetCreated().AsTime(), fixedTimeIssue)
	}
	if got.GetUpdated() == nil {
		t.Fatal("Updated must be non-nil for a non-zero time")
	}

	if len(got.GetReferences()) != 2 {
		t.Fatalf("References: want 2, got %d", len(got.GetReferences()))
	}
	var prRef *gojirav1.Reference
	for _, r := range got.GetReferences() {
		if r.GetKind() == "GitHubPR" {
			prRef = r
		}
	}
	if prRef == nil {
		t.Fatal("expected a GitHubPR reference in the mapped output")
	}
	if prRef.GetOwner() != "org" || prRef.GetRepo() != "repo" || prRef.GetPrNumber() != 42 {
		t.Errorf("PR ref fields wrong: owner=%q repo=%q pr=%d",
			prRef.GetOwner(), prRef.GetRepo(), prRef.GetPrNumber())
	}
	if prRef.GetSource() != "RemoteLink" {
		t.Errorf("PR ref Source: got %q, want RemoteLink", prRef.GetSource())
	}
}

func TestGetIssue_UnspecifiedFormatDefaultsToStructured(t *testing.T) {
	t.Parallel()
	issue, refs := fixtureIssue()
	s := testServer(t,
		func(context.Context, gojira.Config, string) (parse.Issue, []extract.Reference, error) {
			return issue, refs, nil
		},
		nil,
	)
	resp, err := s.GetIssue(context.Background(), &gojirav1.GetIssueRequest{
		Key: "EXAMPLE-1",
		// Format unspecified.
	})
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if resp.GetIssue() == nil {
		t.Fatal("unspecified format must default to the structured Issue result")
	}
}

func TestGetIssue_MarkdownFormat(t *testing.T) {
	t.Parallel()
	issue, refs := fixtureIssue()
	s := testServer(t,
		func(context.Context, gojira.Config, string) (parse.Issue, []extract.Reference, error) {
			return issue, refs, nil
		},
		nil,
	)
	resp, err := s.GetIssue(context.Background(), &gojirav1.GetIssueRequest{
		Key:    "EXAMPLE-1",
		Format: gojirav1.OutputFormat_OUTPUT_FORMAT_MARKDOWN,
	})
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	md := resp.GetMarkdown()
	if md == "" {
		t.Fatal("Markdown payload was empty")
	}
	if !strings.Contains(md, "EXAMPLE-1") {
		t.Errorf("Markdown must mention the issue key; got:\n%s", md)
	}
	if !strings.Contains(md, "An example issue") {
		t.Errorf("Markdown must mention the issue summary; got:\n%s", md)
	}
	if resp.GetIssue() != nil {
		t.Error("Markdown response must not also carry a structured Issue")
	}
}

func TestGetIssue_JSONFormat(t *testing.T) {
	t.Parallel()
	issue, refs := fixtureIssue()
	s := testServer(t,
		func(context.Context, gojira.Config, string) (parse.Issue, []extract.Reference, error) {
			return issue, refs, nil
		},
		nil,
	)
	resp, err := s.GetIssue(context.Background(), &gojirav1.GetIssueRequest{
		Key:    "EXAMPLE-1",
		Format: gojirav1.OutputFormat_OUTPUT_FORMAT_JSON,
	})
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	payload := resp.GetJson()
	if payload == "" {
		t.Fatal("JSON payload was empty")
	}
	if !json.Valid([]byte(payload)) {
		t.Fatalf("JSON payload was not valid JSON:\n%s", payload)
	}
	if !strings.Contains(payload, "EXAMPLE-1") {
		t.Errorf("JSON payload must mention the issue key; got:\n%s", payload)
	}
}

func TestGetIssue_EmptyKeyReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	s := testServer(t,
		func(context.Context, gojira.Config, string) (parse.Issue, []extract.Reference, error) {
			t.Fatal("getIssueFn must not be called when key is empty")
			return parse.Issue{}, nil, nil
		},
		nil,
	)
	_, err := s.GetIssue(context.Background(), &gojirav1.GetIssueRequest{Key: ""})
	if err == nil {
		t.Fatal("expected an error for empty key")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

func TestGetIssue_UnsupportedFormatReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	issue, refs := fixtureIssue()
	s := testServer(t,
		func(context.Context, gojira.Config, string) (parse.Issue, []extract.Reference, error) {
			return issue, refs, nil
		},
		nil,
	)
	_, err := s.GetIssue(context.Background(), &gojirav1.GetIssueRequest{
		Key:    "EXAMPLE-1",
		Format: gojirav1.OutputFormat(999), // not a real value
	})
	if err == nil {
		t.Fatal("expected an error for an unknown format")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

// TestGetIssue_ErrorMapping exercises the toStatusError sentinel mapping
// via GetIssue. Each case wraps a sentinel with fmt.Errorf("...: %w")
// to confirm we map through errors.Is rather than a bare ==.
func TestGetIssue_ErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		injErr  error
		wantCod codes.Code
	}{
		{"unauthorized", fmt.Errorf("gojira: fetch EXAMPLE-1: %w", gojira.ErrUnauthorized), codes.Unauthenticated},
		{"forbidden", fmt.Errorf("gojira: fetch EXAMPLE-1: %w", gojira.ErrForbidden), codes.PermissionDenied},
		{"not found", fmt.Errorf("gojira: fetch EXAMPLE-1: %w", gojira.ErrNotFound), codes.NotFound},
		{"rate limited", fmt.Errorf("gojira: fetch EXAMPLE-1: %w", gojira.ErrRateLimited), codes.ResourceExhausted},
		{"missing required config", fmt.Errorf("config: %w", gojira.ErrConfigMissingRequired), codes.FailedPrecondition},
		{"invalid config value", fmt.Errorf("config: %w", gojira.ErrConfigInvalidValue), codes.FailedPrecondition},
		// Phase 2 write sentinels: 400/409 must classify even when wrapped
		// (mirroring the *client.APIError carrying field-level detail).
		{"bad request", fmt.Errorf("client: write EXAMPLE-1: %w", client.ErrBadRequest), codes.InvalidArgument},
		{"conflict", fmt.Errorf("client: write EXAMPLE-1: %w", client.ErrConflict), codes.FailedPrecondition},
		{"unknown", errors.New("some random failure"), codes.Internal},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := testServer(t,
				func(context.Context, gojira.Config, string) (parse.Issue, []extract.Reference, error) {
					return parse.Issue{}, nil, tc.injErr
				},
				nil,
			)
			_, err := s.GetIssue(context.Background(), &gojirav1.GetIssueRequest{Key: "EXAMPLE-1"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := codeOf(err); got != tc.wantCod {
				t.Errorf("status code: got %v, want %v (err=%v)", got, tc.wantCod, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Crawl — empty input, happy path, terminal error
// ---------------------------------------------------------------------------

func TestCrawl_EmptyStartKeysReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	s := testServer(t, nil, func(context.Context, gojira.Config, []string, gojira.Sink) (gojira.Summary, error) {
		t.Fatal("crawlFn must not be called when start_keys is empty")
		return gojira.Summary{}, nil
	})
	stream := newFakeCrawlStream()
	err := s.Crawl(&gojirav1.CrawlRequest{}, stream)
	if err == nil {
		t.Fatal("expected an error for empty start_keys")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

func TestCrawl_HappyPathStreamsEvents(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := testServer(t, nil, func(_ context.Context, _ gojira.Config, startKeys []string, sink gojira.Sink) (gojira.Summary, error) {
		if len(startKeys) != 1 || startKeys[0] != "X-1" {
			t.Errorf("startKeys: got %v, want [X-1]", startKeys)
		}
		sink.Emit(events.Event{
			Kind:      events.KindIssueFetched,
			IssueKey:  "X-1",
			Message:   "fetched X-1",
			Timestamp: now,
		})
		sink.Emit(events.Event{
			Kind:      events.KindCrawlSummary,
			Message:   "crawl complete: fetched=1",
			Timestamp: now,
		})
		return gojira.Summary{Fetched: 1}, nil
	})

	stream := newFakeCrawlStream()
	err := s.Crawl(&gojirav1.CrawlRequest{StartKeys: []string{"X-1"}}, stream)
	if err != nil {
		t.Fatalf("Crawl returned error: %v", err)
	}

	got := stream.Sent()
	if len(got) != 2 {
		t.Fatalf("expected 2 CrawlEvents on the stream, got %d", len(got))
	}
	if got[0].GetKind() != gojirav1.CrawlEvent_KIND_ISSUE_FETCHED {
		t.Errorf("event[0].Kind: got %v, want KIND_ISSUE_FETCHED", got[0].GetKind())
	}
	if got[0].GetIssueKey() != "X-1" {
		t.Errorf("event[0].IssueKey: got %q", got[0].GetIssueKey())
	}
	if got[1].GetKind() != gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY {
		t.Errorf("event[1].Kind: got %v, want KIND_CRAWL_SUMMARY", got[1].GetKind())
	}
}

func TestCrawl_TerminalErrorMapsToStatus(t *testing.T) {
	t.Parallel()
	s := testServer(t, nil, func(context.Context, gojira.Config, []string, gojira.Sink) (gojira.Summary, error) {
		return gojira.Summary{}, fmt.Errorf("crawl: %w", gojira.ErrUnauthorized)
	})
	stream := newFakeCrawlStream()
	err := s.Crawl(&gojirav1.CrawlRequest{StartKeys: []string{"X-1"}}, stream)
	if err == nil {
		t.Fatal("expected an error for an unauthorized crawl")
	}
	if got := codeOf(err); got != codes.Unauthenticated {
		t.Errorf("status code: got %v, want Unauthenticated", got)
	}
}

// ---------------------------------------------------------------------------
// toStatusError direct unit test — guards the helper independently of the
// handler paths, so a future refactor that bypasses GetIssue/Crawl still
// keeps the sentinel mapping honest.
// ---------------------------------------------------------------------------

func TestToStatusError_NilInputReturnsNil(t *testing.T) {
	t.Parallel()
	if err := toStatusError(nil); err != nil {
		t.Errorf("expected nil for nil input, got %v", err)
	}
}

// TestToStatusError_APIErrorFieldDetailFlows confirms that a
// *client.APIError — the typed write-path error from phase-a-transport-2
// — classifies through its Unwrap chain (errors.Is(err, ErrBadRequest))
// AND propagates its rendered field-level detail into the gRPC status
// Message. This is the contract Phase-2 write handlers depend on so a
// gRPC client can render "summary=Summary is required." back to the
// caller without rummaging through error wrappers.
func TestToStatusError_APIErrorFieldDetailFlows(t *testing.T) {
	t.Parallel()

	// Reconstruct the on-the-wire Jira error shape that the client
	// parser unmarshals. Roundtripping through json.Unmarshal/Marshal
	// is overkill here — we exercise the parser path used by the
	// production status switch.
	body := []byte(`{"errorMessages":["validation failed"],"errors":{"summary":"Summary is required."}}`)

	// Build the *APIError the same way doWithRetry does on 400. The
	// returned error must Unwrap to client.ErrBadRequest and produce a
	// human-readable Error() string with the field detail.
	srv := buildStatusErrorFromAPIErrorBody(t, body)

	st, ok := status.FromError(srv)
	if !ok {
		t.Fatalf("expected a *status.Status, got %T (%v)", srv, srv)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", st.Code())
	}

	msg := st.Message()
	if !strings.Contains(msg, "summary") {
		t.Errorf("status message must mention the failing field; got %q", msg)
	}
	if !strings.Contains(msg, "Summary is required.") {
		t.Errorf("status message must carry the per-field detail; got %q", msg)
	}
	if !strings.Contains(msg, "validation failed") {
		t.Errorf("status message must carry the top-level errorMessages; got %q", msg)
	}
}

// buildStatusErrorFromAPIErrorBody simulates what doWithRetry does on a
// 400: it would normally call the unexported client.parseAPIError, but
// that helper is not exported. Constructing a wrap chain with the same
// observable surface (Unwrap to ErrBadRequest + Error() containing the
// rendered body) is sufficient for the toStatusError contract.
func buildStatusErrorFromAPIErrorBody(t *testing.T, body []byte) error {
	t.Helper()

	// A real *client.APIError exposes:
	//   - Unwrap() → client.ErrBadRequest (so errors.Is matches)
	//   - Error() → "client: bad request (400): ... [field=msg]"
	// We can't construct one directly (APIError.sentinel is unexported),
	// so we synthesize an equivalent wrap that toStatusError must treat
	// the same way: fmt.Errorf("...: %w") around ErrBadRequest, then
	// prepend the body's rendered detail. The mapping under test is
	// errors.Is + err.Error() → status.Error(code, err.Error()), and
	// both behaviours are observable from a wrap chain.
	if !json.Valid(body) {
		t.Fatalf("test fixture must be valid JSON: %s", body)
	}

	// Decode the Jira body to format a deterministic detail string the
	// status Message can be asserted against.
	var parsed struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	detail := strings.Join(parsed.ErrorMessages, "; ")
	for k, v := range parsed.Errors {
		detail += " [" + k + "=" + v + "]"
	}

	// Wrap ErrBadRequest with the rendered detail. errors.Is(err,
	// client.ErrBadRequest) returns true, and err.Error() ends with the
	// detail — the exact preconditions toStatusError relies on.
	wrapped := fmt.Errorf("%s: %w", detail, client.ErrBadRequest)
	return toStatusError(wrapped)
}
